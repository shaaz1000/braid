package xfer

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"braid/internal/linkset"
	"braid/internal/plan"
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
	_, err := f.Fetch(context.Background(), 100, 199)
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
	got, err := f.Fetch(context.Background(), 100, 199)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(got, data[100:200]) {
		t.Error("Fetch returned the wrong bytes")
	}
}
