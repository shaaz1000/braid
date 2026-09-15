// Package xfer performs one bonded transfer end to end: probe the URL, decide
// how to divide it, honour any resumable progress, and drive the scheduler.
//
// Two entry points. Get downloads to disk and returns when finished. Start
// returns a handle to a transfer that is still filling, so a caller can serve
// the bytes in order while the links are still fetching them.
package xfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"braid/internal/dial"
	"braid/internal/linkset"
	"braid/internal/plan"
	"braid/internal/probe"
	"braid/internal/sched"
)

// sidecarExt is appended to the output path to hold resume state.
const sidecarExt = ".braid"

// ErrRangeIgnored means a server answered 206 but did not honour the range it
// promised. Observed in the wild from cdn.jsdelivr.net, which reports the
// compressed length in Content-Range and then sends the whole uncompressed
// body. Splitting such a response corrupts the file, so the transfer falls
// back to a single stream.
var ErrRangeIgnored = errors.New("server answered 206 but ignored the byte range")

const (
	defaultWorkersPerLink = 4
	probeTimeout          = 30 * time.Second
	streamBufSize         = 256 << 10
)

// Dialer builds a transport pinned to one link and family. Injectable so the
// transfer logic can be tested over loopback without real interfaces.
type Dialer func(linkset.Link, linkset.Family) (*http.Transport, error)

// Options describes one download.
type Options struct {
	URL  string
	Dest string // a directory, an explicit file path, or "" for the cwd

	ChunkSize      int64
	WorkersPerLink int
	Links          []linkset.Link

	TailStealAfter time.Duration
	ChunkTimeout   time.Duration

	// Dialer defaults to the real interface-pinning dialer.
	Dialer Dialer

	// OnProgress is called as chunks land. It must be cheap; it blocks a worker.
	OnProgress func(doneChunks, totalChunks int, bytesDone int64, link string)
}

// Outcome reports what happened.
type Outcome struct {
	Path    string
	Size    int64
	Ranges  bool
	Resumed bool
	Result  sched.Result
}

// linkClient is one link with a client already pinned to it.
type linkClient struct {
	link   linkset.Link
	family linkset.Family
	client *http.Client
}

// Transfer is a download that has started and may still be filling.
//
// Plan and Bits together say which bytes are already on disk, which is what a
// reader needs to serve the file in order while chunks arrive out of order.
type Transfer struct {
	Info    probe.Result
	Plan    plan.Plan
	Bits    *plan.Bitmap
	Path    string
	Resumed bool

	opts    Options
	clients []linkClient
	file    *os.File
	sidecar string
	state   plan.State

	done chan struct{}
	mu   sync.Mutex
	res  sched.Result
	err  error
}

// Start probes the URL, prepares the output file, and begins fetching in the
// background. The returned Transfer may be read through immediately.
func Start(ctx context.Context, o Options) (*Transfer, error) {
	o, clients, err := prepare(o)
	if err != nil {
		return nil, err
	}

	probeCtx, cancelProbe := context.WithTimeout(ctx, probeTimeout)
	defer cancelProbe()
	info, err := probe.Do(probeCtx, clients[0].client, o.URL)
	if err != nil {
		return nil, fmt.Errorf("probing %s: %w", o.URL, err)
	}

	path, err := destPath(o.Dest, info.Filename)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	// Size the file up front so every chunk can be written at its true offset.
	// It stays sparse until the bytes arrive, so this costs nothing.
	if err := f.Truncate(info.Size); err != nil {
		f.Close()
		return nil, fmt.Errorf("sizing %s: %w", path, err)
	}

	t := &Transfer{
		Info:    info,
		Plan:    plan.New(info.Size, o.ChunkSize),
		Path:    path,
		opts:    o,
		clients: clients,
		file:    f,
		sidecar: path + sidecarExt,
		done:    make(chan struct{}),
	}
	t.state = plan.State{
		URL:          info.URL,
		Filename:     info.Filename,
		Size:         info.Size,
		ChunkSize:    t.Plan.ChunkSize,
		ETag:         info.ETag,
		LastModified: info.LastModified,
	}

	t.Bits = plan.NewBitmap(t.Plan.Chunks)
	if saved, err := plan.Load(t.sidecar); err == nil && saved.Resumable(t.state) {
		t.Bits = plan.BitmapFromBytes(t.Plan.Chunks, saved.Bits)
		t.Resumed = true
	} else {
		// Either there is no saved progress or it belongs to a different file.
		// Refusing to reuse it is the point: resuming across a changed remote
		// file would silently stitch two files together.
		os.Remove(t.sidecar)
	}

	go t.fill(ctx)
	return t, nil
}

// fill runs the transfer to completion and publishes the outcome.
func (t *Transfer) fill(ctx context.Context) {
	defer close(t.done)

	if !t.Info.Ranges {
		// Nothing to split. One sequential stream, but chunks are still marked
		// as the write passes each boundary, so a reader gets bytes as they
		// arrive rather than all at the end.
		n, err := t.single(ctx)
		t.publish(sched.Result{Bytes: n, ByLink: map[string]int{t.clients[0].link.Iface: 1}}, err)
		return
	}

	res, err := sched.Run(ctx, sched.Config{
		Plan:           t.Plan,
		Bits:           t.Bits,
		Out:            t.file,
		Links:          schedLinks(t.clients, t.Info.URL, t.opts.WorkersPerLink),
		TailStealAfter: t.opts.TailStealAfter,
		ChunkTimeout:   t.opts.ChunkTimeout,
		OnProgress: func(pr sched.Progress) {
			// Persisted as chunks land, so an interrupted transfer resumes from
			// where it actually got to.
			t.saveSidecar()
			if t.opts.OnProgress != nil {
				t.opts.OnProgress(pr.DoneChunks, pr.TotalChunks, pr.Bytes, pr.Link)
			}
		},
	})
	t.publish(res, err)
}

func (t *Transfer) publish(res sched.Result, err error) {
	t.mu.Lock()
	t.res, t.err = res, err
	t.mu.Unlock()
}

// Wait blocks until the transfer finishes and reports what happened.
func (t *Transfer) Wait() (sched.Result, error) {
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.res, t.err
}

// Done reports whether the transfer has finished, without blocking.
func (t *Transfer) Done() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// ReaderAt reads the bytes already on disk. Pair it with Bits so nothing reads
// a hole.
func (t *Transfer) ReaderAt() io.ReaderAt { return t.file }

// Close releases the output file. It does not delete anything.
func (t *Transfer) Close() error { return t.file.Close() }

// Sync flushes the output file. Get does this itself; a caller that only
// streams a transfer must call it once the fetch completes, or a crash can
// lose bytes the bitmap claims are on disk.
func (t *Transfer) Sync() error { return t.file.Sync() }

func (t *Transfer) saveSidecar() {
	t.state.Bits = t.Bits.Bytes()
	_ = plan.Save(t.sidecar, t.state)
}

// single fetches an unsplittable URL sequentially, marking chunks complete as
// the write passes each boundary so a reader can follow along.
func (t *Transfer) single(ctx context.Context) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.Info.URL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := t.clients[0].client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("single-stream GET returned HTTP %d", resp.StatusCode)
	}

	var written int64
	nextChunk := 0
	buf := make([]byte, streamBufSize)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := t.file.WriteAt(buf[:n], written); err != nil {
				return written, err
			}
			written += int64(n)
			for nextChunk < t.Plan.Chunks {
				_, end := t.Plan.Range(nextChunk)
				if written <= end {
					break
				}
				t.Bits.Set(nextChunk)
				if t.opts.OnProgress != nil {
					t.opts.OnProgress(t.Bits.Done(), t.Plan.Chunks, written, t.clients[0].link.Iface)
				}
				nextChunk++
			}
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

// Get downloads o.URL, splitting it across every link when the server permits,
// and returns when the file is complete on disk.
func Get(ctx context.Context, o Options) (Outcome, error) {
	t, err := Start(ctx, o)
	if err != nil {
		return Outcome{}, err
	}
	defer t.Close()

	out := Outcome{Path: t.Path, Size: t.Info.Size, Ranges: t.Info.Ranges, Resumed: t.Resumed}
	res, runErr := t.Wait()
	out.Result = res

	if errors.Is(runErr, ErrRangeIgnored) {
		// The server promised slices and sent whole bodies. Nothing fetched so
		// far can be trusted, so discard it and take the one path that yields a
		// correct file.
		os.Remove(t.sidecar)
		out.Ranges, out.Resumed = false, false
		n, err := t.single(ctx)
		if err != nil {
			return out, err
		}
		return t.finishUnsplittable(out, n)
	}
	if runErr != nil {
		t.saveSidecar() // keep it: this is what makes the next attempt cheap
		return out, runErr
	}

	if !t.Info.Ranges {
		// A server that ignores ranges often misreports its length too, so the
		// file is sized to what actually arrived.
		return t.finishUnsplittable(out, res.Bytes)
	}
	if err := t.file.Sync(); err != nil {
		return out, err
	}
	os.Remove(t.sidecar)
	return out, nil
}

func (t *Transfer) finishUnsplittable(out Outcome, written int64) (Outcome, error) {
	if err := t.file.Truncate(written); err != nil {
		return out, fmt.Errorf("resizing %s to the %d bytes received: %w", t.Path, written, err)
	}
	out.Size = written
	out.Ranges = false
	out.Result = sched.Result{Bytes: written, ByLink: map[string]int{t.clients[0].link.Iface: 1}}
	os.Remove(t.sidecar)
	return out, t.file.Sync()
}

// prepare applies defaults and builds one pinned client per link.
func prepare(o Options) (Options, []linkClient, error) {
	if len(o.Links) == 0 {
		return o, nil, errors.New("no usable link to download over")
	}
	if o.Dialer == nil {
		o.Dialer = dial.Transport
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = plan.DefaultChunkSize
	}
	if o.WorkersPerLink <= 0 {
		o.WorkersPerLink = defaultWorkersPerLink
	}
	clients, err := buildClients(o)
	return o, clients, err
}

func buildClients(o Options) ([]linkClient, error) {
	var out []linkClient
	var firstErr error

	for _, l := range o.Links {
		fams := l.Families()
		if len(fams) == 0 {
			continue
		}
		// Families() lists IPv4 first. Which family is actually faster flips
		// between runs, so picking by measured rate is a later refinement;
		// what matters here is never assuming a family the link lacks.
		fam := fams[0]
		tr, err := o.Dialer(l, fam)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, linkClient{link: l, family: fam, client: &http.Client{Transport: tr}})
	}

	if len(out) == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("no link could be dialed: %w", firstErr)
		}
		return nil, errors.New("no link has a usable address")
	}
	return out, nil
}

func schedLinks(clients []linkClient, url string, workers int) []sched.Link {
	out := make([]sched.Link, 0, len(clients))
	for _, c := range clients {
		out = append(out, sched.Link{
			Name:    c.link.Iface,
			Fetcher: &httpFetcher{client: c.client, url: url},
			Workers: workers,
		})
	}
	return out
}

// destPath resolves where to write. An existing directory means "put the
// probed filename inside it"; anything else is taken as the exact file path.
func destPath(dest, filename string) (string, error) {
	if dest == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, filename), nil
	}
	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		return filepath.Join(dest, filename), nil
	}
	return dest, nil
}

// httpFetcher fetches one byte range over an already-pinned client.
type httpFetcher struct {
	client *http.Client
	url    string
}

func (f *httpFetcher) Fetch(ctx context.Context, start, end int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	// Anything but 206 means we did not get the slice we asked for. Accepting a
	// 200 here would write the whole file into one chunk's slot.
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("chunk %d-%d: expected 206, got HTTP %d", start, end, resp.StatusCode)
	}

	want := end - start + 1
	// Bounded by one extra byte so an over-long body is detected rather than
	// read into memory without limit.
	body, err := io.ReadAll(io.LimitReader(resp.Body, want+1))
	if err != nil {
		return nil, err
	}
	switch {
	case int64(len(body)) > want:
		// More than we asked for means the range was not honoured, whatever the
		// status line claimed. Retrying cannot help, and each attempt may cost a
		// whole file's worth of bytes, so this also wraps sched.ErrFatal to stop
		// the scheduler rather than let it retry.
		return nil, fmt.Errorf("chunk %d-%d: got at least %d bytes for a %d byte range: %w: %w",
			start, end, len(body), want, ErrRangeIgnored, sched.ErrFatal)
	case int64(len(body)) < want:
		return nil, fmt.Errorf("chunk %d-%d: got %d bytes, want %d", start, end, len(body), want)
	}
	return body, nil
}
