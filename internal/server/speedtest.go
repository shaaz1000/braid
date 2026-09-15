package server

import (
	"context"
	"net/http"
	"os"
	"time"

	"braid/internal/xfer"
)

// DefaultSpeedTestURL is a large file on a CDN that honours byte ranges.
// Ranges are required: without them there is nothing to split, and the test
// would measure a single link no matter how many are attached.
const DefaultSpeedTestURL = "https://dl.google.com/go/go1.27.1.darwin-arm64.tar.gz"

type speedTestResult struct {
	Mbps    float64            `json:"mbps"`
	Bytes   int64              `json:"bytes"`
	Seconds float64            `json:"seconds"`
	PerLink map[string]float64 `json:"per_link"`
	Labels  map[string]string  `json:"labels"`
	Links   int                `json:"links"`
}

// handleSpeedTest runs a real transfer through the engine and reports what it
// achieved.
//
// This exists because fast.com and every other speed test measures whatever
// single route macOS happens to pick, so none of them can ever show a bonded
// figure. The only way to see what braid delivers is to measure braid.
func (s *Server) handleSpeedTest(w http.ResponseWriter, r *http.Request) {
	url := s.opts.SpeedTestURL
	if url == "" {
		url = DefaultSpeedTestURL
	}

	dir, err := os.MkdirTemp("", "braid-speedtest-")
	if err != nil {
		http.Error(w, "could not prepare a scratch directory", http.StatusInternalServerError)
		return
	}
	// Nothing is kept: a speed test that quietly filled the cache would be a
	// poor trade for a number.
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	started := time.Now()
	out, err := xfer.Get(ctx, xfer.Options{
		URL:            url,
		Dest:           dir,
		Links:          s.opts.Links(),
		Dialer:         s.opts.Dialer,
		ChunkSize:      s.opts.ChunkSize,
		WorkersPerLink: s.opts.WorkersPerLink,
		TailStealAfter: s.opts.TailStealAfter,
		ChunkTimeout:   s.opts.ChunkTimeout,
	})
	if err != nil {
		http.Error(w, "the speed test could not complete: "+err.Error(), http.StatusBadGateway)
		return
	}

	elapsed := time.Since(started)
	result := speedTestResult{
		Bytes:   out.Result.Bytes,
		Seconds: elapsed.Seconds(),
		PerLink: map[string]float64{},
		Labels:  map[string]string{},
	}
	if elapsed > 0 {
		result.Mbps = float64(out.Result.Bytes) * 8 / elapsed.Seconds() / 1e6
	}

	total := 0
	for _, n := range out.Result.ByLink {
		total += n
	}
	for iface, n := range out.Result.ByLink {
		if total > 0 {
			result.PerLink[iface] = float64(n) / float64(total)
		}
	}
	for _, l := range s.opts.Links() {
		result.Labels[l.Iface] = l.Label()
		if l.Metered {
			result.Labels[l.Iface] += " · billed"
		}
	}
	result.Links = len(result.PerLink)

	writeJSON(w, result)
}

// readDirNames lists a directory's entries, tolerating its absence.
func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}
