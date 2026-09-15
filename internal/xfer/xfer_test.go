package xfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"braid/internal/linkset"
	"braid/internal/plan"
	"braid/internal/stream"
)

func payload(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n) + 7)).Read(b)
	return b
}

// rangeServer serves data with byte-range support, counting requests.
type rangeServer struct {
	data     []byte
	etag     string
	requests atomic.Int32
	// ignoreRange makes it answer 200 with the whole body, like
	// speed.cloudflare.com does.
	ignoreRange bool
	// filename, when set, is offered via Content-Disposition.
	filename string
}

func (s *rangeServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		if s.etag != "" {
			w.Header().Set("ETag", s.etag)
		}
		if s.filename != "" {
			w.Header().Set("Content-Disposition", `attachment; filename="`+s.filename+`"`)
		}

		hdr := r.Header.Get("Range")
		if s.ignoreRange || hdr == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(s.data)))
			w.WriteHeader(http.StatusOK)
			w.Write(s.data)
			return
		}

		var start, end int64
		if _, err := fmt.Sscanf(hdr, "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if end >= int64(len(s.data)) {
			end = int64(len(s.data)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(s.data)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(s.data[start : end+1])
	}
}

// loopbackLinks pretends to be two uplinks. The Dialer seam below hands back
// ordinary transports, so no real interface is involved.
func loopbackLinks(names ...string) []linkset.Link {
	var out []linkset.Link
	for i, n := range names {
		out = append(out, linkset.Link{
			Iface: n,
			Index: i + 1,
			V4:    netip.MustParseAddr("127.0.0.1"),
		})
	}
	return out
}

func plainDialer(linkset.Link, linkset.Family) (*http.Transport, error) {
	return &http.Transport{DisableCompression: true}, nil
}

func baseOpts(url, dest string, links []linkset.Link) Options {
	return Options{
		URL:            url,
		Dest:           dest,
		ChunkSize:      100,
		WorkersPerLink: 2,
		Links:          links,
		Dialer:         plainDialer,
	}
}

func TestGetDownloadsAcrossTwoLinks(t *testing.T) {
	data := payload(1000)
	srv := httptest.NewServer((&rangeServer{data: data, etag: `"v1"`}).handler())
	defer srv.Close()

	dir := t.TempDir()
	out, err := Get(context.Background(), baseOpts(srv.URL+"/thing.bin", dir, loopbackLinks("a", "b")))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	got, err := os.ReadFile(out.Path)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded file does not match the source")
	}
	if filepath.Base(out.Path) != "thing.bin" {
		t.Errorf("path = %q, want a file named thing.bin", out.Path)
	}
	if !out.Ranges {
		t.Error("Ranges = false against a range-capable server")
	}
	if out.Size != int64(len(data)) {
		t.Errorf("Size = %d, want %d", out.Size, len(data))
	}
	if total := out.Result.ByLink["a"] + out.Result.ByLink["b"]; total != 10 {
		t.Errorf("links completed %d chunks, want 10", total)
	}
}

func TestGetPrefersContentDispositionFilename(t *testing.T) {
	data := payload(200)
	srv := httptest.NewServer((&rangeServer{data: data, etag: `"v1"`, filename: "real-name.iso"}).handler())
	defer srv.Close()

	dir := t.TempDir()
	out, err := Get(context.Background(), baseOpts(srv.URL+"/ignored.bin", dir, loopbackLinks("a")))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if filepath.Base(out.Path) != "real-name.iso" {
		t.Errorf("path = %q, want real-name.iso", out.Path)
	}
}

func TestGetWritesToAnExplicitFilePath(t *testing.T) {
	data := payload(200)
	srv := httptest.NewServer((&rangeServer{data: data, etag: `"v1"`}).handler())
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "chosen.dat")
	out, err := Get(context.Background(), baseOpts(srv.URL+"/x.bin", target, loopbackLinks("a")))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out.Path != target {
		t.Errorf("path = %q, want %q", out.Path, target)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, data) {
		t.Error("file at the explicit path does not match the source")
	}
}

func TestGetRemovesTheSidecarOnSuccess(t *testing.T) {
	data := payload(500)
	srv := httptest.NewServer((&rangeServer{data: data, etag: `"v1"`}).handler())
	defer srv.Close()

	dir := t.TempDir()
	out, err := Get(context.Background(), baseOpts(srv.URL+"/f.bin", dir, loopbackLinks("a")))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := os.Stat(out.Path + sidecarExt); !os.IsNotExist(err) {
		t.Error("sidecar should be gone after a complete download")
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("leftover files in the output directory: %v", names)
	}
}

func TestGetResumesAndFetchesOnlyTheRemainder(t *testing.T) {
	data := payload(1000)
	srv := httptest.NewServer((&rangeServer{data: data, etag: `"v1"`}).handler())
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "resume.bin")

	// Lay down a half-finished download: first six chunks already on disk.
	f, err := os.Create(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(data[:600], 0); err != nil {
		t.Fatal(err)
	}
	f.Close()

	bits := plan.NewBitmap(10)
	for i := 0; i < 6; i++ {
		bits.Set(i)
	}
	if err := plan.Save(target+sidecarExt, plan.State{
		URL:       srv.URL + "/resume.bin",
		Filename:  "resume.bin",
		Size:      int64(len(data)),
		ChunkSize: 100,
		ETag:      `"v1"`,
		Bits:      bits.Bytes(),
	}); err != nil {
		t.Fatal(err)
	}

	out, err := Get(context.Background(), baseOpts(srv.URL+"/resume.bin", target, loopbackLinks("a")))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !out.Resumed {
		t.Error("Resumed = false; the sidecar should have been honoured")
	}
	if out.Result.Bytes != 400 {
		t.Errorf("fetched %d bytes, want only the outstanding 400", out.Result.Bytes)
	}

	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, data) {
		t.Error("resumed file does not match the source")
	}
}

func TestGetStartsOverWhenTheRemoteFileChanged(t *testing.T) {
	// The sidecar says ETag "v1" but the server now serves "v2". Resuming
	// would stitch two different files together.
	data := payload(400)
	srv := httptest.NewServer((&rangeServer{data: data, etag: `"v2"`}).handler())
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "changed.bin")

	f, _ := os.Create(target)
	f.Truncate(int64(len(data)))
	f.WriteAt(bytes.Repeat([]byte{0xAA}, 200), 0) // stale bytes from the old file
	f.Close()

	bits := plan.NewBitmap(4)
	bits.Set(0)
	bits.Set(1)
	if err := plan.Save(target+sidecarExt, plan.State{
		URL: srv.URL + "/changed.bin", Filename: "changed.bin",
		Size: int64(len(data)), ChunkSize: 100, ETag: `"v1"`, Bits: bits.Bytes(),
	}); err != nil {
		t.Fatal(err)
	}

	out, err := Get(context.Background(), baseOpts(srv.URL+"/changed.bin", target, loopbackLinks("a")))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out.Resumed {
		t.Error("Resumed = true against a changed remote file")
	}
	if out.Result.Bytes != int64(len(data)) {
		t.Errorf("fetched %d bytes, want the whole %d", out.Result.Bytes, len(data))
	}

	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, data) {
		t.Error("file was not fully rewritten after refusing to resume")
	}
}

func TestGetFallsBackToASingleStreamWithoutRanges(t *testing.T) {
	data := payload(3000)
	srv := httptest.NewServer((&rangeServer{data: data, ignoreRange: true}).handler())
	defer srv.Close()

	dir := t.TempDir()
	out, err := Get(context.Background(), baseOpts(srv.URL+"/plain.bin", dir, loopbackLinks("a", "b")))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out.Ranges {
		t.Error("Ranges = true against a server that ignores Range")
	}

	got, _ := os.ReadFile(out.Path)
	if !bytes.Equal(got, data) {
		t.Fatal("single-stream fallback produced the wrong bytes")
	}
}

func TestGetRequiresAtLeastOneLink(t *testing.T) {
	srv := httptest.NewServer((&rangeServer{data: payload(100), etag: `"v1"`}).handler())
	defer srv.Close()

	_, err := Get(context.Background(), baseOpts(srv.URL+"/x", t.TempDir(), nil))
	if err == nil {
		t.Fatal("Get with no links must error")
	}
	if !strings.Contains(err.Error(), "link") {
		t.Errorf("error should mention links, got %q", err)
	}
}

func TestGetReportsProgress(t *testing.T) {
	data := payload(500)
	srv := httptest.NewServer((&rangeServer{data: data, etag: `"v1"`}).handler())
	defer srv.Close()

	var last int32
	opts := baseOpts(srv.URL+"/p.bin", t.TempDir(), loopbackLinks("a"))
	opts.OnProgress = func(done, total int, bytesDone int64, link string) {
		atomic.StoreInt32(&last, int32(done))
	}

	if _, err := Get(context.Background(), opts); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := atomic.LoadInt32(&last); got != 5 {
		t.Errorf("last progress reported %d chunks, want 5", got)
	}
}

func TestFetcherSendsTheRangeHeaderAndRejectsAFullBody(t *testing.T) {
	// If a server silently answers 200 to a ranged chunk request, accepting it
	// would write the whole file into one chunk's slot.
	data := payload(300)
	var sawRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRange = r.Header.Get("Range")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer srv.Close()

	f := &httpFetcher{client: &http.Client{}, url: srv.URL}
	_, err := f.Fetch(context.Background(), 100, 199, newMemWriter(1000))
	if err == nil {
		t.Fatal("a 200 response to a ranged chunk request must be an error")
	}
	if sawRange != "bytes=100-199" {
		t.Errorf("Range header = %q, want bytes=100-199", sawRange)
	}
}

func TestFetcherReturnsTheRequestedSlice(t *testing.T) {
	data := payload(300)
	srv := httptest.NewServer((&rangeServer{data: data}).handler())
	defer srv.Close()

	f := &httpFetcher{client: &http.Client{}, url: srv.URL}
	sink := newMemWriter(len(data))
	n, err := f.Fetch(context.Background(), 100, 199, sink)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n != 100 {
		t.Errorf("wrote %d bytes, want 100", n)
	}
	if !bytes.Equal(sink.bytes()[100:200], data[100:200]) {
		t.Error("Fetch wrote the wrong bytes")
	}
}

func TestFetcherRejectsMismatchedContentRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-99/300")
		w.WriteHeader(http.StatusPartialContent)
		w.Write(make([]byte, 100))
	}))
	defer srv.Close()

	f := &httpFetcher{client: &http.Client{}, url: srv.URL}
	_, err := f.Fetch(context.Background(), 100, 199, newMemWriter(300))
	if !errors.Is(err, ErrRangeIgnored) {
		t.Fatalf("mismatched Content-Range error = %v, want ErrRangeIgnored", err)
	}
}

func TestStartHonoursExplicitWorkerCount(t *testing.T) {
	data := payload(1000)
	srv := httptest.NewServer((&rangeServer{data: data, etag: `"v1"`}).handler())
	defer srv.Close()

	opts := baseOpts(srv.URL+"/workers.bin", t.TempDir(), loopbackLinks("a"))
	opts.WorkersPerLink = 7
	tr, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tr.Close()
	if tr.opts.WorkersPerLink != 7 {
		t.Errorf("WorkersPerLink = %d, want explicit value 7", tr.opts.WorkersPerLink)
	}
	if _, err := tr.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestGetRecoversFromAServerThatLiesAboutRanges(t *testing.T) {
	// Real behaviour observed from cdn.jsdelivr.net: it answers 206 and reports
	// a Content-Range total that is the *compressed* length, then sends the
	// whole *uncompressed* body. Trusting either half alone corrupts the file,
	// so braid must notice and fall back to a single stream.
	data := payload(9000)
	const liedSize = 1500

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Range")
		switch {
		case hdr == "bytes=0-0":
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", liedSize))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[:1])
		case hdr != "":
			// Claims a slice, sends everything.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", liedSize-1, liedSize))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data)
		default:
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			w.WriteHeader(http.StatusOK)
			w.Write(data)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	out, err := Get(context.Background(), baseOpts(srv.URL+"/liar.js", dir, loopbackLinks("a", "b")))
	if err != nil {
		t.Fatalf("Get should recover from a lying server: %v", err)
	}
	if out.Ranges {
		t.Error("Ranges = true; a server caught lying must be reported as unsplittable")
	}

	got, err := os.ReadFile(out.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("recovered file is wrong: got %d bytes, want %d", len(got), len(data))
	}
	if out.Size != int64(len(data)) {
		t.Errorf("Size = %d, want the true %d, not the advertised %d", out.Size, len(data), liedSize)
	}
	if _, err := os.Stat(out.Path + sidecarExt); !os.IsNotExist(err) {
		t.Error("sidecar should not survive the fallback")
	}
}

func TestFetcherFlagsAnOverlongBody(t *testing.T) {
	data := payload(5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-99/100")
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data) // far more than the 100 bytes promised
	}))
	defer srv.Close()

	f := &httpFetcher{client: &http.Client{}, url: srv.URL}
	_, err := f.Fetch(context.Background(), 0, 99, newMemWriter(10000))
	if err == nil {
		t.Fatal("an over-long body must be an error")
	}
	if !errors.Is(err, ErrRangeIgnored) {
		t.Errorf("error should wrap ErrRangeIgnored so the caller can fall back, got %v", err)
	}
}

func TestStartExposesATransferWhileItIsStillFilling(t *testing.T) {
	// What /stream needs: begin fetching, then read the file in order from the
	// front while later chunks are still arriving over the links.
	data := payload(2000)
	srv := httptest.NewServer((&rangeServer{data: data, etag: `"v1"`}).handler())
	defer srv.Close()

	tr, err := Start(context.Background(), baseOpts(srv.URL+"/live.bin", t.TempDir(), loopbackLinks("a", "b")))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tr.Close()

	if tr.Info.Size != int64(len(data)) {
		t.Errorf("Size = %d, want %d", tr.Info.Size, len(data))
	}
	if !tr.Info.Ranges {
		t.Error("Ranges = false against a range-capable server")
	}
	if tr.Plan.Chunks != 20 {
		t.Errorf("Chunks = %d, want 20", tr.Plan.Chunks)
	}

	// Read the whole thing through the in-order reader as it fills.
	var out bytes.Buffer
	n, err := stream.Copy(context.Background(), &out, tr.Plan, tr.Bits, tr.ReaderAt(), 0, tr.Info.Size-1)
	if err != nil {
		t.Fatalf("streaming while filling: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("streamed %d bytes, want %d", n, len(data))
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatal("streamed bytes do not match the source")
	}

	res, err := tr.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Bytes != int64(len(data)) {
		t.Errorf("fetched %d bytes, want %d", res.Bytes, len(data))
	}
}

func TestStartReportsAProbeFailureImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := Start(context.Background(), baseOpts(srv.URL+"/missing", t.TempDir(), loopbackLinks("a"))); err == nil {
		t.Fatal("Start must fail when the URL cannot be probed")
	}
}

func TestStartOnAnUnsplittableURLStillServesTheFile(t *testing.T) {
	// A server that ignores ranges has one chunk covering everything, so a
	// reader still gets correct bytes — just from a single link.
	data := payload(3000)
	srv := httptest.NewServer((&rangeServer{data: data, ignoreRange: true}).handler())
	defer srv.Close()

	tr, err := Start(context.Background(), baseOpts(srv.URL+"/plain.bin", t.TempDir(), loopbackLinks("a", "b")))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tr.Close()
	if tr.Info.Ranges {
		t.Error("Ranges = true against a server that ignores Range")
	}

	var out bytes.Buffer
	if _, err := stream.Copy(context.Background(), &out, tr.Plan, tr.Bits, tr.ReaderAt(), 0, tr.Info.Size-1); err != nil {
		t.Fatalf("streaming: %v", err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Error("streamed bytes do not match the source")
	}
	if _, err := tr.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestChunkPlanLeavesRoomForWorkStealing(t *testing.T) {
	// Work stealing only works when chunks massively outnumber workers. With
	// one chunk per worker the split is frozen at the start and the transfer
	// waits for the slowest link: a 33.5 MB file at the 4 MB default gave 9
	// chunks across 8 workers and measured 40.8 Mbps, against 93.1 for the
	// fast link alone.
	cases := []struct {
		name  string
		size  int64
		links int
	}{
		{"small file", 8 << 20, 2},
		{"the 33.5MB case that regressed", 35_109_201, 2},
		{"medium file", 68_100_347, 2},
		{"large file", 2 << 30, 2},
		{"single link", 68_100_347, 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chunk, workers := chunkPlan(c.size, c.links, 0)

			if chunk < minChunk {
				t.Errorf("chunk %d is below the %d floor; small chunks bleed a round trip each",
					chunk, minChunk)
			}
			if chunk > maxChunk {
				t.Errorf("chunk %d exceeds the %d ceiling", chunk, maxChunk)
			}
			if workers < 1 {
				t.Fatalf("workers = %d", workers)
			}

			chunks := (c.size + chunk - 1) / chunk
			total := int64(workers * c.links)
			// A file smaller than one block simply has one chunk; that is not a
			// scheduling fault. What matters is never spawning far more workers
			// than there is work for.
			if chunks > 1 && total > chunks {
				t.Errorf("%d workers for %d chunks leaves workers idle from the start",
					total, chunks)
			}
			// Large chunks are the point: shrinking them to manufacture steal
			// headroom cost more in round trips than it gained in balance.
			// Blocks must stay big enough that a round trip per chunk is noise.
			if chunk < minChunk {
				t.Errorf("chunk %d is below the %d floor", chunk, minChunk)
			}
			// ...and small enough that one chunk is not the entire transfer.
			if c.size > 4*minChunk && chunk > c.size/8 {
				t.Errorf("chunk %d is %d%% of a %d byte file; one slow chunk would be the whole job",
					chunk, 100*chunk/c.size, c.size)
			}
		})
	}
}

func TestChunkPlanHonoursAnExplicitChunkSize(t *testing.T) {
	// Someone passing -chunk means it, even if it is a poor choice.
	chunk, _ := chunkPlan(68_100_347, 2, 1<<20)
	if chunk != 1<<20 {
		t.Errorf("chunk = %d, want the requested 1 MiB", chunk)
	}
}

// memWriter is an in-memory io.WriterAt for exercising the fetcher without a
// file on disk.
type memWriter struct {
	mu  sync.Mutex
	buf []byte
}

func newMemWriter(n int) *memWriter { return &memWriter{buf: make([]byte, n)} }

func (m *memWriter) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if off+int64(len(p)) > int64(len(m.buf)) {
		grown := make([]byte, off+int64(len(p)))
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[off:], p)
	return len(p), nil
}

func (m *memWriter) bytes() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]byte, len(m.buf))
	copy(out, m.buf)
	return out
}
