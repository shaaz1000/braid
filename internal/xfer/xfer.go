// Package xfer performs one bonded transfer end to end: probe the URL, decide
// how to divide it, honour any resumable progress, and drive the scheduler.
package xfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"braid/internal/dial"
	"braid/internal/linkset"
	"braid/internal/plan"
	"braid/internal/probe"
	"braid/internal/sched"
)

// sidecarExt is appended to the output path to hold resume state.
const sidecarExt = ".braid"

const (
	defaultWorkersPerLink = 4
	probeTimeout          = 30 * time.Second
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

// Get downloads o.URL, splitting it across every link when the server permits.
func Get(ctx context.Context, o Options) (Outcome, error) {
	if len(o.Links) == 0 {
		return Outcome{}, errors.New("no usable link to download over")
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
	if err != nil {
		return Outcome{}, err
	}

	probeCtx, cancelProbe := context.WithTimeout(ctx, probeTimeout)
	defer cancelProbe()
	info, err := probe.Do(probeCtx, clients[0].client, o.URL)
	if err != nil {
		return Outcome{}, fmt.Errorf("probing %s: %w", o.URL, err)
	}

	path, err := destPath(o.Dest, info.Filename)
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{Path: path, Size: info.Size, Ranges: info.Ranges}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return out, err
	}
	defer f.Close()
	// Size the file up front so every chunk can be written at its true offset.
	// The file is sparse until the bytes arrive, so this costs nothing.
	if err := f.Truncate(info.Size); err != nil {
		return out, fmt.Errorf("sizing %s: %w", path, err)
	}

	if !info.Ranges {
		// The server ignored the Range header, so there is nothing to split.
		// One stream on one link, reported honestly rather than pretending.
		n, err := singleStream(ctx, clients[0].client, info.URL, f)
		if err != nil {
			return out, err
		}
		out.Result = sched.Result{
			Bytes:  n,
			ByLink: map[string]int{clients[0].link.Iface: 1},
		}
		return out, f.Sync()
	}

	p := plan.New(info.Size, o.ChunkSize)
	current := plan.State{
		URL:          info.URL,
		Filename:     info.Filename,
		Size:         info.Size,
		ChunkSize:    p.ChunkSize,
		ETag:         info.ETag,
		LastModified: info.LastModified,
	}

	sidecar := path + sidecarExt
	bits := plan.NewBitmap(p.Chunks)
	if saved, err := plan.Load(sidecar); err == nil && saved.Resumable(current) {
		bits = plan.BitmapFromBytes(p.Chunks, saved.Bits)
		out.Resumed = true
	} else {
		// Either there is no saved progress or it belongs to a different file.
		// Refusing to reuse it is the whole point: resuming across a changed
		// remote file would silently stitch two files together.
		os.Remove(sidecar)
	}

	cfg := sched.Config{
		Plan:           p,
		Bits:           bits,
		Out:            f,
		Links:          schedLinks(clients, info.URL, o.WorkersPerLink),
		TailStealAfter: o.TailStealAfter,
		ChunkTimeout:   o.ChunkTimeout,
		OnProgress: func(pr sched.Progress) {
			// Persisted as chunks land, so an interrupted transfer resumes from
			// where it actually got to.
			current.Bits = bits.Bytes()
			_ = plan.Save(sidecar, current)
			if o.OnProgress != nil {
				o.OnProgress(pr.DoneChunks, pr.TotalChunks, pr.Bytes, pr.Link)
			}
		},
	}

	res, runErr := sched.Run(ctx, cfg)
	out.Result = res

	if runErr != nil {
		// Keep the sidecar: it is what makes the next attempt cheap.
		current.Bits = bits.Bytes()
		_ = plan.Save(sidecar, current)
		return out, runErr
	}
	if err := f.Sync(); err != nil {
		return out, err
	}
	os.Remove(sidecar)
	return out, nil
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

func singleStream(ctx context.Context, client *http.Client, url string, w io.WriterAt) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("single-stream GET returned HTTP %d", resp.StatusCode)
	}

	var written int64
	buf := make([]byte, 256<<10)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := w.WriteAt(buf[:n], written); err != nil {
				return written, err
			}
			written += int64(n)
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
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

	// Anything but 206 means we did not get the slice we asked for. Accepting
	// a 200 here would write the whole file into one chunk's slot.
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
	if int64(len(body)) != want {
		return nil, fmt.Errorf("chunk %d-%d: got %d bytes, want %d", start, end, len(body), want)
	}
	return body, nil
}
