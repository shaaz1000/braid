package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(t *testing.T, url string) (Result, error) {
	t.Helper()
	return Do(context.Background(), http.DefaultClient, url)
}

func TestProbeDetectsRangeSupport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-0/1048576")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte{0})
	}))
	defer srv.Close()

	got, err := do(t, srv.URL+"/big.iso")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !got.Ranges {
		t.Error("Ranges = false, want true for a 206")
	}
	if got.Size != 1048576 {
		t.Errorf("Size = %d, want 1048576", got.Size)
	}
}

func TestProbeSendsOneByteRangedGetNotHead(t *testing.T) {
	// A HEAD is not proof: CDNs frequently misreport it. Only a real 206
	// establishes that ranges work, so the probe must be a ranged GET.
	var method, rangeHdr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, rangeHdr = r.Method, r.Header.Get("Range")
		w.Header().Set("Content-Range", "bytes 0-0/10")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte{0})
	}))
	defer srv.Close()

	if _, err := do(t, srv.URL); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if method != http.MethodGet {
		t.Errorf("method = %q, want GET", method)
	}
	if rangeHdr != "bytes=0-0" {
		t.Errorf("Range = %q, want bytes=0-0", rangeHdr)
	}
}

func TestProbeFallsBackWhenServerIgnoresRange(t *testing.T) {
	// speed.cloudflare.com behaves exactly like this: it answers 200 and sends
	// the whole body. That is not an error, it just cannot be split.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	got, err := do(t, srv.URL)
	if err != nil {
		t.Fatalf("Do should not error on a 200: %v", err)
	}
	if got.Ranges {
		t.Error("Ranges = true, want false when the server answers 200")
	}
	if got.Size != 4096 {
		t.Errorf("Size = %d, want 4096 from Content-Length", got.Size)
	}
}

func TestProbeCapturesCacheValidators(t *testing.T) {
	// Resume is only safe if we can prove the remote file has not changed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-0/99")
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Set("Last-Modified", "Mon, 15 Sep 2026 10:00:00 GMT")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte{0})
	}))
	defer srv.Close()

	got, err := do(t, srv.URL)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got.ETag != `"abc123"` {
		t.Errorf("ETag = %q", got.ETag)
	}
	if got.LastModified != "Mon, 15 Sep 2026 10:00:00 GMT" {
		t.Errorf("LastModified = %q", got.LastModified)
	}
}

func TestProbeFilename(t *testing.T) {
	cases := []struct {
		name        string
		disposition string
		path        string
		want        string
	}{
		{"from content-disposition", `attachment; filename="ubuntu.iso"`, "/d/x", "ubuntu.iso"},
		{"from url path when no disposition", "", "/go1.27.1.darwin-arm64.tar.gz", "go1.27.1.darwin-arm64.tar.gz"},
		{"disposition wins over path", `attachment; filename="real.bin"`, "/wrong.bin", "real.bin"},
		{"strips directories from disposition", `attachment; filename="/etc/passwd"`, "/x", "passwd"},
		{"refuses path traversal", `attachment; filename="../../../etc/passwd"`, "/x", "passwd"},
		{"falls back when path is bare", "", "/", "download"},
		{"ignores query string", "", "/file.zip?token=abc", "file.zip"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.disposition != "" {
					w.Header().Set("Content-Disposition", c.disposition)
				}
				w.Header().Set("Content-Range", "bytes 0-0/10")
				w.WriteHeader(http.StatusPartialContent)
				w.Write([]byte{0})
			}))
			defer srv.Close()

			got, err := do(t, srv.URL+c.path)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			if got.Filename != c.want {
				t.Errorf("Filename = %q, want %q", got.Filename, c.want)
			}
		})
	}
}

func TestProbeFollowsRedirectsAndReportsFinalURL(t *testing.T) {
	// Chunk workers must request the final URL directly; re-following a
	// redirect on every one of hundreds of chunks is wasted round trips.
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-0/512")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte{0})
	}))
	defer final.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/real.bin", http.StatusFound)
	}))
	defer redirector.Close()

	got, err := do(t, redirector.URL+"/start")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got.URL != final.URL+"/real.bin" {
		t.Errorf("URL = %q, want the redirect target %q", got.URL, final.URL+"/real.bin")
	}
	if got.Size != 512 {
		t.Errorf("Size = %d, want 512", got.Size)
	}
	if got.Filename != "real.bin" {
		t.Errorf("Filename = %q, want real.bin from the final URL", got.Filename)
	}
}

func TestProbeRejectsUnknownTotalSize(t *testing.T) {
	// "bytes 0-0/*" means the server will not say how big the file is, so
	// there is nothing to divide into chunks.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-0/*")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte{0})
	}))
	defer srv.Close()

	if _, err := do(t, srv.URL); err == nil {
		t.Fatal("an unknown total size must be an error")
	}
}

func TestProbeRejectsMalformedContentRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "pages 1-2 of many")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte{0})
	}))
	defer srv.Close()

	_, err := do(t, srv.URL)
	if err == nil {
		t.Fatal("a malformed Content-Range must be an error")
	}
	if !strings.Contains(err.Error(), "Content-Range") {
		t.Errorf("error should mention Content-Range, got %q", err)
	}
}

func TestProbeReportsServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := do(t, srv.URL)
	if err == nil {
		t.Fatal("a 403 must be an error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should carry the status, got %q", err)
	}
}

func TestProbeRejectsEmptyFile(t *testing.T) {
	// A zero-length file has no chunks and would divide by nothing downstream.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := do(t, srv.URL); err == nil {
		t.Fatal("a zero-byte file must be an error")
	}
}
