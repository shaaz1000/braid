package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"braid/internal/linkset"
)

func payload(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n) + 3)).Read(b)
	return b
}

// origin serves a file with byte-range support, standing in for a CDN.
func origin(data []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Range")
		if hdr == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			w.Write(data)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(hdr, "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[start : end+1])
	}))
}

func testLinks() []linkset.Link {
	return []linkset.Link{
		{Iface: "en0", Friendly: "Wi-Fi", Index: 1, V4: netip.MustParseAddr("127.0.0.1")},
		{Iface: "en5", Friendly: "iPhone USB", Index: 2, V4: netip.MustParseAddr("127.0.0.1"), Metered: true},
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Options{
		Token:    "secret-token",
		CacheDir: t.TempDir(),
		Links:    testLinks,
		Dialer: func(linkset.Link, linkset.Family) (*http.Transport, error) {
			return &http.Transport{DisableCompression: true}, nil
		},
		ChunkSize: 100,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Transfers are deliberately detached from the request, so without this a
	// test's fetches keep retrying against its closed origin and bleed into
	// whichever test binds that port next.
	t.Cleanup(func() { s.Close() })
	return s
}

func get(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Result()
}

// ----------------------------------------------------------------------- auth

func TestTokenIsRequired(t *testing.T) {
	// The daemon binds to the LAN so a phone can reach it, which means an open
	// endpoint would be reachable by anything on the network.
	s := newTestServer(t)

	for _, path := range []string{"/", "/api/links", "/api/events", "/stream?url=http://x/y"} {
		t.Run(path, func(t *testing.T) {
			if got := get(t, s, path).StatusCode; got != http.StatusUnauthorized {
				t.Errorf("%s without a token = %d, want 401", path, got)
			}
		})
	}
}

func TestTokenAcceptedInAQueryParameter(t *testing.T) {
	// VLC, Infuse and an Apple TV cannot set request headers, so a /stream URL
	// has to carry its own credential and stay pasteable.
	s := newTestServer(t)

	if got := get(t, s, "/api/links?t=secret-token").StatusCode; got != http.StatusOK {
		t.Errorf("query-parameter token = %d, want 200", got)
	}
}

func TestTokenAcceptedInAnAuthorizationHeader(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/links", nil)
	req.Header.Set("Authorization", "Bearer secret-token")

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("bearer token = %d, want 200", rec.Code)
	}
}

func TestAWrongTokenIsRejected(t *testing.T) {
	s := newTestServer(t)
	if got := get(t, s, "/api/links?t=guess").StatusCode; got != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", got)
	}
}

func TestNewRejectsAnEmptyToken(t *testing.T) {
	// An accidental empty token would authenticate everyone.
	_, err := New(Options{CacheDir: t.TempDir(), Links: testLinks})
	if err == nil {
		t.Fatal("New with no token must fail rather than serve the LAN unauthenticated")
	}
}

// ----------------------------------------------------------------- links API

func TestLinksReportsWhatCanBeUsed(t *testing.T) {
	s := newTestServer(t)
	resp := get(t, s, "/api/links?t=secret-token")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}

	var got struct {
		Links []struct {
			Iface    string `json:"iface"`
			Label    string `json:"label"`
			Metered  bool   `json:"metered"`
			Families []int  `json:"families"`
		} `json:"links"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(got.Links) != 2 {
		t.Fatalf("got %d links, want 2", len(got.Links))
	}
	if got.Links[0].Label != "Wi-Fi" || got.Links[0].Metered {
		t.Errorf("first link = %+v", got.Links[0])
	}
	if got.Links[1].Label != "iPhone USB" || !got.Links[1].Metered {
		t.Errorf("second link = %+v; the tether must be reported as metered", got.Links[1])
	}
}

func TestLinksIsReReadEachTime(t *testing.T) {
	// Interfaces come and go — a phone gets unplugged mid-session — so the
	// answer must not be captured once at startup.
	calls := 0
	s, err := New(Options{
		Token:    "tok",
		CacheDir: t.TempDir(),
		Links: func() []linkset.Link {
			calls++
			return testLinks()
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	get(t, s, "/api/links?t=tok")
	get(t, s, "/api/links?t=tok")
	if calls < 2 {
		t.Errorf("link discovery ran %d times for 2 requests; it is being cached", calls)
	}
}

// -------------------------------------------------------------------- /stream

func TestStreamServesTheWholeFile(t *testing.T) {
	data := payload(1000)
	up := origin(data)
	defer up.Close()

	s := newTestServer(t)
	srv := httptest.NewServer(s)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/stream?t=secret-token&url=" + url.QueryEscape(up.URL+"/movie.bin"))
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.ContentLength; got != int64(len(data)) {
		t.Errorf("Content-Length = %d, want %d", got, len(data))
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes; players need to seek", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("streamed %d bytes that do not match the source", len(body))
	}
}

func TestStreamHonoursARangeRequest(t *testing.T) {
	// This is what makes seeking in a video player work.
	data := payload(1000)
	up := origin(data)
	defer up.Close()

	s := newTestServer(t)
	srv := httptest.NewServer(s)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/stream?t=secret-token&url="+url.QueryEscape(up.URL+"/movie.bin"), nil)
	req.Header.Set("Range", "bytes=250-499")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ranged GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 250-499/1000" {
		t.Errorf("Content-Range = %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, data[250:500]) {
		t.Error("ranged response does not match the requested slice")
	}
}

func TestStreamHonoursAnOpenEndedRange(t *testing.T) {
	// "bytes=800-" is what a player sends when resuming playback.
	data := payload(1000)
	up := origin(data)
	defer up.Close()

	s := newTestServer(t)
	srv := httptest.NewServer(s)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/stream?t=secret-token&url="+url.QueryEscape(up.URL+"/m.bin"), nil)
	req.Header.Set("Range", "bytes=800-")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, data[800:]) {
		t.Error("open-ended range does not match the tail of the file")
	}
}

func TestStreamRejectsAnUnsatisfiableRange(t *testing.T) {
	data := payload(500)
	up := origin(data)
	defer up.Close()

	s := newTestServer(t)
	srv := httptest.NewServer(s)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/stream?t=secret-token&url="+url.QueryEscape(up.URL+"/m.bin"), nil)
	req.Header.Set("Range", "bytes=9000-9999")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status = %d, want 416", resp.StatusCode)
	}
}

func TestStreamSuggestsAFilename(t *testing.T) {
	data := payload(200)
	up := origin(data)
	defer up.Close()

	s := newTestServer(t)
	resp := get(t, s, "/stream?t=secret-token&url="+url.QueryEscape(up.URL+"/holiday.mp4"))

	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "holiday.mp4") {
		t.Errorf("Content-Disposition = %q, want it to name holiday.mp4", got)
	}
}

func TestStreamRequiresAURL(t *testing.T) {
	s := newTestServer(t)
	if got := get(t, s, "/stream?t=secret-token").StatusCode; got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
}

func TestStreamRejectsANonHTTPURL(t *testing.T) {
	// Without this, "file:///etc/passwd" would turn the daemon into a file
	// server for its own host.
	s := newTestServer(t)

	for _, bad := range []string{"file:///etc/passwd", "ftp://example.com/x", "gopher://x"} {
		t.Run(bad, func(t *testing.T) {
			got := get(t, s, "/stream?t=secret-token&url="+url.QueryEscape(bad)).StatusCode
			if got != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", got)
			}
		})
	}
}

func TestStreamReportsAnUnreachableOrigin(t *testing.T) {
	s := newTestServer(t)
	resp := get(t, s, "/stream?t=secret-token&url="+url.QueryEscape("http://127.0.0.1:1/nope"))

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for an origin that cannot be reached", resp.StatusCode)
	}
}

// --------------------------------------------------------------------- events

func TestEventsStreamsSnapshots(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events?t=secret-token", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// The first frame arrives immediately so a freshly-opened page is never blank.
	sc := bufio.NewScanner(resp.Body)
	var sawData bool
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			var snap struct {
				Links []struct {
					Label string `json:"label"`
				} `json:"links"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(sc.Text(), "data: ")), &snap); err != nil {
				t.Fatalf("frame is not JSON: %v", err)
			}
			if len(snap.Links) != 2 {
				t.Errorf("snapshot carries %d links, want 2", len(snap.Links))
			}
			sawData = true
			break
		}
	}
	if !sawData {
		t.Fatal("no data frame arrived on the event stream")
	}
}

// ------------------------------------------------------------------------- ui

func TestRootServesTheDashboard(t *testing.T) {
	s := newTestServer(t)
	resp := get(t, s, "/?t=secret-token")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("braid")) {
		t.Error("the dashboard does not mention braid")
	}
	if !bytes.Contains(body, []byte("EventSource")) {
		t.Error("the dashboard does not subscribe to the event stream")
	}
}

func TestGeneratedTokenIsLongEnoughToBeWorthHaving(t *testing.T) {
	a, b := NewToken(), NewToken()
	if len(a) < 20 {
		t.Errorf("token %q is only %d characters", a, len(a))
	}
	if a == b {
		t.Error("NewToken returned the same value twice")
	}
}
