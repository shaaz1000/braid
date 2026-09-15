package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSpeedTestReportsPerLinkThroughput(t *testing.T) {
	// fast.com and every other speed test measures whatever single route macOS
	// picks, so it can never show a bonded figure. This endpoint runs a real
	// transfer through the engine, which is the only way to see the number
	// that braid actually delivers.
	data := payload(400_000)
	up := origin(data)
	defer up.Close()

	s := newTestServer(t)
	s.opts.SpeedTestURL = up.URL + "/speedtest.bin"
	s.opts.ChunkSize = 64 << 10

	srv := httptest.NewServer(s)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/speedtest?t=secret-token", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/speedtest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var got struct {
		Mbps    float64            `json:"mbps"`
		Bytes   int64              `json:"bytes"`
		Seconds float64            `json:"seconds"`
		PerLink map[string]float64 `json:"per_link"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if got.Bytes != int64(len(data)) {
		t.Errorf("Bytes = %d, want %d", got.Bytes, len(data))
	}
	if got.Mbps <= 0 {
		t.Errorf("Mbps = %v; a completed transfer must report a rate", got.Mbps)
	}
	if got.Seconds <= 0 {
		t.Errorf("Seconds = %v", got.Seconds)
	}
	if len(got.PerLink) == 0 {
		t.Error("no per-link breakdown; the split is the whole point of the number")
	}
	var share float64
	for _, v := range got.PerLink {
		share += v
	}
	if share < 0.99 || share > 1.01 {
		t.Errorf("per-link shares sum to %v, want 1", share)
	}
}

func TestSpeedTestLeavesNothingBehind(t *testing.T) {
	// A speed test that quietly filled the cache would be a poor trade.
	data := payload(200_000)
	up := origin(data)
	defer up.Close()

	s := newTestServer(t)
	s.opts.SpeedTestURL = up.URL + "/st.bin"
	s.opts.ChunkSize = 64 << 10
	srv := httptest.NewServer(s)
	defer srv.Close()

	if _, err := http.Post(srv.URL+"/api/speedtest?t=secret-token", "application/json", nil); err != nil {
		t.Fatalf("POST: %v", err)
	}

	entries, err := readDirNames(s.opts.CacheDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range entries {
		if name == "st.bin" || name == "st.bin"+sidecarSuffix {
			t.Errorf("speed test left %q in the cache", name)
		}
	}
}

func TestSpeedTestNeedsAToken(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/speedtest", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

var _ = fmt.Sprint
