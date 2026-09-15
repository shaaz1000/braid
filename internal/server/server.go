// Package server exposes a bonded transfer to every device on the network.
//
// The point is that clients need nothing installed. A phone, a laptop or a TV
// asks for an ordinary sequential HTTP response; behind it the bytes are being
// fetched concurrently over every uplink. Range requests are honoured so
// players can seek.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"braid/internal/linkset"
	"braid/internal/stream"
	"braid/internal/xfer"
)

//go:embed ui.html
var uiFS embed.FS

// snapshotInterval is how often the dashboard is pushed a fresh snapshot.
// Fast enough to feel live, slow enough not to flood a phone on Wi-Fi.
const snapshotInterval = 250 * time.Millisecond

// Options configures the daemon.
type Options struct {
	// Token is required on every request. There is no unauthenticated mode:
	// the daemon binds to the LAN so a phone can reach it, which means an open
	// endpoint would be reachable by anything on the network.
	Token string
	// CacheDir is where streamed files are materialised.
	CacheDir string
	// Links is re-read per request, because interfaces come and go.
	Links func() []linkset.Link

	Dialer         xfer.Dialer
	ChunkSize      int64
	WorkersPerLink int
	TailStealAfter time.Duration
	ChunkTimeout   time.Duration
}

// Server is the HTTP surface. It implements http.Handler.
type Server struct {
	opts Options
	mux  *http.ServeMux

	// ctx outlives individual requests, because a transfer must survive the
	// client that triggered it: a player reconnecting should find the work
	// already done. Close cancels it, which is how the daemon shuts down
	// without leaving fetches running.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	byURL   map[string]*entry
	ordered []*entry
}

// entry is one transfer the daemon is serving, plus the live detail the
// dashboard needs.
type entry struct {
	id   string
	url  string
	name string
	size int64

	tr *xfer.Transfer

	mu      sync.Mutex
	owners  []string // chunk index -> the link that fetched it, "" while pending
	done    int
	bytes   int64
	started time.Time
	ended   time.Time
	failed  string
}

func New(o Options) (*Server, error) {
	if strings.TrimSpace(o.Token) == "" {
		return nil, errors.New("a token is required; the daemon must not serve the network unauthenticated")
	}
	if o.CacheDir == "" {
		return nil, errors.New("a cache directory is required")
	}
	if o.Links == nil {
		return nil, errors.New("no link source configured")
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		opts: o, mux: http.NewServeMux(), byURL: map[string]*entry{},
		ctx: ctx, cancel: cancel,
	}
	s.mux.HandleFunc("/api/links", s.handleLinks)
	s.mux.HandleFunc("/api/events", s.handleEvents)
	s.mux.HandleFunc("/stream", s.handleStream)
	s.mux.HandleFunc("/", s.handleUI)
	return s, nil
}

// NewToken makes a shared secret for a fresh install.
func NewToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// Without randomness a predictable token is worse than none, so fall
		// back to something nobody will mistake for a secret.
		return "braid-insecure-token-please-set-one"
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Close stops every transfer still in flight and waits for them to stop
// before releasing their files. Safe to call more than once.
//
// The wait is the important part. Cancelling alone returns while workers are
// still mid-write, which leaves files appearing in a directory the caller
// believes it has finished with.
func (s *Server) Close() error {
	s.cancel()

	s.mu.Lock()
	entries := make([]*entry, len(s.ordered))
	copy(entries, s.ordered)
	s.mu.Unlock()

	for _, e := range entries {
		if e.tr == nil {
			continue
		}
		// A cancelled context unwinds the scheduler promptly, but a wedged
		// fetch must not be able to hang shutdown for ever.
		settled := make(chan struct{})
		go func(t *xfer.Transfer) {
			t.Wait()
			close(settled)
		}(e.tr)
		select {
		case <-settled:
		case <-time.After(5 * time.Second):
		}
		e.tr.Close()
	}
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="braid"`)
		http.Error(w, "a valid token is required", http.StatusUnauthorized)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// authorised accepts the token as a bearer header or a ?t= parameter. The
// query form exists because the clients that matter most — VLC, Infuse, an
// Apple TV — cannot set request headers, so a /stream URL has to carry its own
// credential and stay pasteable.
func (s *Server) authorised(r *http.Request) bool {
	if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); constantTimeEqual(got, s.opts.Token) {
		return true
	}
	return constantTimeEqual(r.URL.Query().Get("t"), s.opts.Token)
}

func constantTimeEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// ------------------------------------------------------------------ links API

type linkJSON struct {
	Iface    string `json:"iface"`
	Label    string `json:"label"`
	Metered  bool   `json:"metered"`
	Families []int  `json:"families"`
}

func (s *Server) links() []linkJSON {
	var out []linkJSON
	for _, l := range s.opts.Links() {
		fams := make([]int, 0, 2)
		for _, f := range l.Families() {
			fams = append(fams, int(f))
		}
		out = append(out, linkJSON{Iface: l.Iface, Label: l.Label(), Metered: l.Metered, Families: fams})
	}
	return out
}

func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"links": s.links()})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// --------------------------------------------------------------------- events

type transferJSON struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	Chunks int    `json:"chunks"`
	Done   int    `json:"done"`
	Bytes  int64  `json:"bytes"`
	// Owners is the heart of the dashboard: which link fetched each chunk, in
	// file order, so the page can draw the file as a woven band.
	Owners   []string `json:"owners"`
	Mbps     float64  `json:"mbps"`
	Seconds  float64  `json:"seconds"`
	Finished bool     `json:"finished"`
	// Cached means every byte was already on disk, so nothing was fetched and
	// no throughput figure is meaningful.
	Cached bool   `json:"cached"`
	Failed string `json:"failed,omitempty"`
}

func (s *Server) snapshot() map[string]any {
	s.mu.Lock()
	entries := make([]*entry, len(s.ordered))
	copy(entries, s.ordered)
	s.mu.Unlock()

	transfers := make([]transferJSON, 0, len(entries))
	for _, e := range entries {
		transfers = append(transfers, e.json())
	}
	return map[string]any{"links": s.links(), "transfers": transfers}
}

func (e *entry) json() transferJSON {
	e.mu.Lock()
	defer e.mu.Unlock()

	elapsed := time.Since(e.started)
	if !e.ended.IsZero() {
		elapsed = e.ended.Sub(e.started)
	}
	mbps := 0.0
	if elapsed > 0 {
		mbps = float64(e.bytes) * 8 / elapsed.Seconds() / 1e6
	}

	owners := make([]string, len(e.owners))
	copy(owners, e.owners)

	return transferJSON{
		ID: e.id, Name: e.name, URL: e.url, Size: e.size,
		Chunks: len(e.owners), Done: e.done, Bytes: e.bytes,
		Owners: owners, Mbps: mbps, Seconds: elapsed.Seconds(),
		Finished: !e.ended.IsZero(), Cached: e.bytes == 0 && e.done == len(e.owners) && len(e.owners) > 0,
		Failed: e.failed,
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	send := func() bool {
		payload, err := json.Marshal(s.snapshot())
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Send at once so a freshly-opened page is never blank.
	if !send() {
		return
	}

	ticker := time.NewTicker(snapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

// --------------------------------------------------------------------- stream

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		http.Error(w, "a url parameter is required", http.StatusBadRequest)
		return
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		http.Error(w, "that url could not be parsed", http.StatusBadRequest)
		return
	}
	// Anything but http(s) would make the daemon fetch from its own host —
	// file:///etc/passwd would turn this into a local file server.
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		http.Error(w, "only http and https urls can be streamed", http.StatusBadRequest)
		return
	}

	e, err := s.transferFor(r.Context(), raw)
	if err != nil {
		http.Error(w, "could not reach that url: "+err.Error(), http.StatusBadGateway)
		return
	}

	start, end, satisfiable, partial := parseRange(r.Header.Get("Range"), e.size)
	if !satisfiable {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", e.size))
		http.Error(w, "that range is not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	// inline, so a player plays it rather than a browser saving it.
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", e.name))
	if ct := contentType(e.name); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, e.size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	// Copy blocks per chunk until it lands, so the response is sequential even
	// though the fetch is not. A client hanging up cancels the context, which
	// unparks the reader.
	_, _ = stream.Copy(r.Context(), &flushWriter{w: w}, e.tr.Plan, e.tr.Bits, e.tr.ReaderAt(), start, end)
}

// transferFor returns the transfer for a URL, starting one if needed.
//
// Reuse matters: a video player issues many ranged requests for the same URL,
// and starting a fresh transfer for each would re-download the file every
// time — expensive in seconds and, on a metered link, in money.
func (s *Server) transferFor(ctx context.Context, raw string) (*entry, error) {
	s.mu.Lock()
	if e, ok := s.byURL[raw]; ok && e.failed == "" {
		s.mu.Unlock()
		return e, nil
	}
	s.mu.Unlock()

	e := &entry{id: NewToken()[:8], url: raw, started: time.Now()}

	// Detached from the request and tied to the server's lifetime: the transfer
	// must outlive the client that triggered it, but must not outlive the
	// daemon.
	tr, err := xfer.Start(s.ctx, xfer.Options{
		URL:            raw,
		Dest:           s.opts.CacheDir,
		ChunkSize:      s.opts.ChunkSize,
		WorkersPerLink: s.opts.WorkersPerLink,
		Links:          s.opts.Links(),
		Dialer:         s.opts.Dialer,
		TailStealAfter: s.opts.TailStealAfter,
		ChunkTimeout:   s.opts.ChunkTimeout,
		OnProgress: func(done, total int, bytes int64, link string) {
			e.record(done, total, bytes, link)
		},
	})
	if err != nil {
		return nil, err
	}

	e.tr = tr
	e.name = filepath.Base(tr.Path)
	e.size = tr.Info.Size
	e.owners = make([]string, tr.Plan.Chunks)
	// Chunks already on disk from a previous run have no recorded owner.
	for i := 0; i < tr.Plan.Chunks; i++ {
		if tr.Bits.Get(i) {
			e.owners[i] = "resumed"
			e.done++
		}
	}

	s.mu.Lock()
	s.byURL[raw] = e
	s.ordered = append(s.ordered, e)
	s.mu.Unlock()

	go func() {
		_, err := tr.Wait()
		if err == nil {
			// The sidecar is deliberately left in place: it is what lets a
			// repeat request for this URL be served from disk instead of
			// spending metered data again. Flushing is not optional though —
			// the bitmap promises these bytes survive a crash.
			err = tr.Sync()
		}
		e.finish(err)
	}()
	return e, nil
}

// record is called as each chunk lands. The owner array is what the dashboard
// draws, so it has to be filled in by chunk index, not appended to.
func (e *entry) record(done, total int, bytes int64, link string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.done, e.bytes = done, bytes
	// The progress callback reports totals rather than which chunk landed, so
	// fill the next unattributed slot. Order within a link is not meaningful;
	// the proportion is what the page shows.
	for i := range e.owners {
		if e.owners[i] == "" {
			e.owners[i] = link
			return
		}
	}
}

func (e *entry) finish(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ended = time.Now()
	if err != nil {
		e.failed = err.Error()
	}
}

// parseRange reads an HTTP Range header against a known size.
func parseRange(header string, size int64) (start, end int64, satisfiable, partial bool) {
	if header == "" {
		return 0, size - 1, true, false
	}
	spec, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !ok {
		return 0, size - 1, true, false
	}
	// Multiple ranges are legal but no player braid serves needs them, and
	// answering the first alone would be wrong. Serve the whole file instead.
	if strings.Contains(spec, ",") {
		return 0, size - 1, true, false
	}

	from, to, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false, false
	}

	switch {
	case from == "" && to != "":
		// "bytes=-500" means the last 500 bytes.
		n, err := strconv.ParseInt(to, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, true

	case from != "":
		s, err := strconv.ParseInt(from, 10, 64)
		if err != nil || s < 0 || s >= size {
			return 0, 0, false, false
		}
		if to == "" {
			return s, size - 1, true, true
		}
		e, err := strconv.ParseInt(to, 10, 64)
		if err != nil || e < s {
			return 0, 0, false, false
		}
		if e >= size {
			e = size - 1
		}
		return s, e, true, true
	}
	return 0, 0, false, false
}

// contentType guesses from the extension, limited to what players care about.
// A wrong guess is worse than none, so anything unrecognised is left unset.
func contentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".flac":
		return "audio/flac"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	}
	return ""
}

// flushWriter pushes each slab to the client instead of letting it sit in a
// buffer, which is what makes playback start promptly.
type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// ------------------------------------------------------------------------- ui

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, err := uiFS.ReadFile("ui.html")
	if err != nil {
		http.Error(w, "dashboard missing from the binary", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(body)
}
