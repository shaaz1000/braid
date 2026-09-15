package sched

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"braid/internal/plan"
)

// ---------------------------------------------------------------- test doubles

// source is the pretend remote file every fake link serves slices of.
func source(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewSource(int64(n)))
	r.Read(b)
	return b
}

// fakeLink serves ranges out of a byte slice, with optional per-call delay,
// failures, and a hard block. It records every range it was asked for.
type fakeLink struct {
	data  []byte
	delay time.Duration

	// failFirst counts down; while positive, Fetch returns an error.
	failFirst atomic.Int32
	// failAlways makes every Fetch fail.
	failAlways bool
	// blockFrom, when non-nil, makes any fetch starting at that offset wait
	// until the context is cancelled, simulating a wedged transfer.
	blockFrom *int64
	// blockAll wedges every fetch, whichever chunk it is handed.
	blockAll bool

	mu        sync.Mutex
	asked     [][2]int64
	callCount int
}

func (f *fakeLink) Fetch(ctx context.Context, start, end int64) ([]byte, error) {
	f.mu.Lock()
	f.asked = append(f.asked, [2]int64{start, end})
	f.callCount++
	f.mu.Unlock()

	if f.blockAll || (f.blockFrom != nil && start == *f.blockFrom) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.failAlways {
		return nil, errors.New("link is down")
	}
	if f.failFirst.Load() > 0 {
		f.failFirst.Add(-1)
		return nil, errors.New("transient failure")
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	out := make([]byte, end-start+1)
	copy(out, f.data[start:end+1])
	return out, nil
}

func (f *fakeLink) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCount
}

func (f *fakeLink) ranges() [][2]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][2]int64, len(f.asked))
	copy(out, f.asked)
	return out
}

// sink is an in-memory io.WriterAt that records what landed where.
type sink struct {
	mu  sync.Mutex
	buf []byte
}

func newSink(n int) *sink { return &sink{buf: make([]byte, n)} }

func (s *sink) WriteAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy(s.buf[off:], p)
	return len(p), nil
}

func (s *sink) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, len(s.buf))
	copy(out, s.buf)
	return out
}

// fastOpts keeps retry backoff negligible so tests stay quick.
func fastOpts(p plan.Plan, bits *plan.Bitmap, out *sink, links ...Link) Config {
	return Config{
		Plan:        p,
		Bits:        bits,
		Out:         out,
		Links:       links,
		MaxAttempts: 4,
		Backoff:     func(int) time.Duration { return time.Millisecond },
	}
}

// ------------------------------------------------------------------- the tests

func TestRunAssemblesTheWholeFileFromTwoLinks(t *testing.T) {
	data := source(1000)
	p := plan.New(int64(len(data)), 100)
	bits := plan.NewBitmap(p.Chunks)
	out := newSink(len(data))

	a := &fakeLink{data: data}
	b := &fakeLink{data: data}

	res, err := Run(context.Background(), fastOpts(p, bits, out,
		Link{Name: "a", Fetcher: a, Workers: 2},
		Link{Name: "b", Fetcher: b, Workers: 2},
	))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Equal(out.bytes(), data) {
		t.Fatal("assembled file does not match the source")
	}
	if !bits.Complete() {
		t.Errorf("bitmap incomplete: %d/%d", bits.Done(), p.Chunks)
	}
	if res.Bytes != int64(len(data)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(data))
	}
	if got := res.ByLink["a"] + res.ByLink["b"]; got != p.Chunks {
		t.Errorf("per-link chunk counts sum to %d, want %d", got, p.Chunks)
	}
}

func TestRunWorksWithASingleLink(t *testing.T) {
	data := source(500)
	p := plan.New(int64(len(data)), 64)
	out := newSink(len(data))

	_, err := Run(context.Background(), fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "only", Fetcher: &fakeLink{data: data}, Workers: 3},
	))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Equal(out.bytes(), data) {
		t.Error("single-link output does not match the source")
	}
}

func TestFastLinkTakesMoreChunksThanSlowLink(t *testing.T) {
	// The point of a shared queue: nobody is handed a fixed share, so a link
	// that is 20x slower simply completes fewer chunks. A static 50/50 split
	// would make the whole transfer wait on the slow link.
	data := source(4000)
	p := plan.New(int64(len(data)), 100) // 40 chunks
	out := newSink(len(data))

	fast := &fakeLink{data: data}
	slow := &fakeLink{data: data, delay: 20 * time.Millisecond}

	res, err := Run(context.Background(), fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "fast", Fetcher: fast, Workers: 2},
		Link{Name: "slow", Fetcher: slow, Workers: 2},
	))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Equal(out.bytes(), data) {
		t.Fatal("output does not match the source")
	}
	if res.ByLink["fast"] <= res.ByLink["slow"] {
		t.Errorf("fast link took %d chunks, slow took %d; work stealing is not happening",
			res.ByLink["fast"], res.ByLink["slow"])
	}
}

func TestChunksFromAFailingLinkAreCompletedByAnother(t *testing.T) {
	data := source(800)
	p := plan.New(int64(len(data)), 100)
	out := newSink(len(data))

	good := &fakeLink{data: data}
	dead := &fakeLink{data: data, failAlways: true}

	res, err := Run(context.Background(), fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "good", Fetcher: good, Workers: 2},
		Link{Name: "dead", Fetcher: dead, Workers: 2},
	))
	if err != nil {
		t.Fatalf("Run should survive one dead link: %v", err)
	}
	if !bytes.Equal(out.bytes(), data) {
		t.Fatal("output does not match the source")
	}
	if res.ByLink["dead"] != 0 {
		t.Errorf("dead link credited with %d chunks", res.ByLink["dead"])
	}
	if res.ByLink["good"] != p.Chunks {
		t.Errorf("good link completed %d chunks, want all %d", res.ByLink["good"], p.Chunks)
	}
}

func TestTransientFailuresAreRetried(t *testing.T) {
	data := source(300)
	p := plan.New(int64(len(data)), 100) // 3 chunks
	out := newSink(len(data))

	flaky := &fakeLink{data: data}
	flaky.failFirst.Store(2) // first two fetches fail, then it recovers

	_, err := Run(context.Background(), fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "flaky", Fetcher: flaky, Workers: 1},
	))
	if err != nil {
		t.Fatalf("Run should retry through transient failures: %v", err)
	}
	if !bytes.Equal(out.bytes(), data) {
		t.Error("output does not match the source")
	}
	if flaky.calls() < p.Chunks+2 {
		t.Errorf("only %d fetches; expected at least %d with retries", flaky.calls(), p.Chunks+2)
	}
}

func TestRunFailsWhenEveryLinkIsDown(t *testing.T) {
	data := source(200)
	p := plan.New(int64(len(data)), 100)
	out := newSink(len(data))

	_, err := Run(context.Background(), fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "dead1", Fetcher: &fakeLink{data: data, failAlways: true}, Workers: 1},
		Link{Name: "dead2", Fetcher: &fakeLink{data: data, failAlways: true}, Workers: 1},
	))
	if err == nil {
		t.Fatal("Run must fail loudly when no link can complete a chunk")
	}
}

func TestAlreadyCompleteChunksAreNotRefetched(t *testing.T) {
	// Resume: the bitmap arrives partly filled and those bytes are already on
	// disk. Re-fetching them would spend metered data for nothing.
	data := source(1000)
	p := plan.New(int64(len(data)), 100) // 10 chunks
	bits := plan.NewBitmap(p.Chunks)
	for i := 0; i < 6; i++ {
		bits.Set(i)
	}
	out := newSink(len(data))

	link := &fakeLink{data: data}
	res, err := Run(context.Background(), fastOpts(p, bits, out,
		Link{Name: "l", Fetcher: link, Workers: 2},
	))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if link.calls() != 4 {
		t.Errorf("fetched %d chunks, want only the 4 outstanding", link.calls())
	}
	if res.Bytes != 400 {
		t.Errorf("Bytes = %d, want 400 for the resumed remainder only", res.Bytes)
	}
	for _, r := range link.ranges() {
		if r[0] < 600 {
			t.Errorf("refetched an already-complete range %d-%d", r[0], r[1])
		}
	}
}

func TestRunReportsNothingToDoWhenAlreadyComplete(t *testing.T) {
	data := source(300)
	p := plan.New(int64(len(data)), 100)
	bits := plan.NewBitmap(p.Chunks)
	for i := 0; i < p.Chunks; i++ {
		bits.Set(i)
	}

	link := &fakeLink{data: data}
	if _, err := Run(context.Background(), fastOpts(p, bits, newSink(len(data)),
		Link{Name: "l", Fetcher: link, Workers: 2},
	)); err != nil {
		t.Fatalf("Run on a complete bitmap should succeed: %v", err)
	}
	if link.calls() != 0 {
		t.Errorf("made %d fetches on an already-complete file", link.calls())
	}
}

func TestTailStealingRescuesAWedgedChunk(t *testing.T) {
	// The endgame problem: the queue is empty, every other link is idle, and
	// one chunk is stuck on a slow link. Without tail stealing the whole
	// transfer waits for it.
	//
	// The wedged link blocks on whatever chunk it is handed and never returns,
	// so exactly one chunk is always stuck no matter how the queue is drained.
	// Blocking only one specific offset made this test racy: the healthy
	// worker could take every chunk itself and no steal would be needed.
	data := source(300)
	p := plan.New(int64(len(data)), 100) // 3 chunks

	wedged := &fakeLink{data: data, blockAll: true}
	healthy := &fakeLink{data: data, delay: 5 * time.Millisecond}

	out := newSink(len(data))
	cfg := fastOpts(p, plan.NewBitmap(p.Chunks), out,
		// One worker each, so the wedged link genuinely holds a chunk leased
		// and cannot pick up anything else.
		Link{Name: "wedged", Fetcher: wedged, Workers: 1},
		Link{Name: "healthy", Fetcher: healthy, Workers: 1},
	)
	cfg.TailStealAfter = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := Run(ctx, cfg)
	if err != nil {
		t.Fatalf("tail stealing should have completed the file: %v", err)
	}
	if !bytes.Equal(out.bytes(), data) {
		t.Fatal("output does not match the source")
	}
	if res.TailSteals == 0 {
		t.Error("TailSteals = 0; the wedged chunk was not stolen")
	}
	if res.ByLink["wedged"] != 0 {
		t.Errorf("wedged link credited with %d chunks; its fetches never return",
			res.ByLink["wedged"])
	}
	if res.ByLink["healthy"] != p.Chunks {
		t.Errorf("healthy link completed %d chunks, want all %d",
			res.ByLink["healthy"], p.Chunks)
	}
}

func TestNothingIsStolenWhenEveryLinkIsHealthy(t *testing.T) {
	// Rescuing is always available now, but it is arithmetic: it only happens
	// when refetching would beat waiting. With healthy links nothing should be
	// duplicated, because duplicated bytes on a metered link cost money.
	data := source(300)
	p := plan.New(int64(len(data)), 100)
	out := newSink(len(data))

	res, err := Run(context.Background(), fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "l", Fetcher: &fakeLink{data: data}, Workers: 2},
	))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TailSteals != 0 {
		t.Errorf("TailSteals = %d with no TailStealAfter configured", res.TailSteals)
	}
}

func TestCancellingTheContextStopsTheJob(t *testing.T) {
	data := source(10000)
	p := plan.New(int64(len(data)), 100)
	out := newSink(len(data))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before we start

	_, err := Run(ctx, fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "l", Fetcher: &fakeLink{data: data, delay: time.Second}, Workers: 2},
	))
	if err == nil {
		t.Fatal("Run must report the cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
}

func TestChunkTimeoutBoundsASilentFetch(t *testing.T) {
	// A connection that stays open but sends nothing must be abandoned rather
	// than holding a chunk forever.
	data := source(200)
	p := plan.New(int64(len(data)), 100)
	out := newSink(len(data))

	var stuckAt int64 = 0
	slow := &fakeLink{data: data, blockFrom: &stuckAt}
	good := &fakeLink{data: data}

	cfg := fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "slow", Fetcher: slow, Workers: 1},
		Link{Name: "good", Fetcher: good, Workers: 1},
	)
	cfg.ChunkTimeout = 30 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := Run(ctx, cfg); err != nil {
		t.Fatalf("Run should recover from a silent fetch: %v", err)
	}
	if !bytes.Equal(out.bytes(), data) {
		t.Error("output does not match the source")
	}
}

func TestProgressIsReported(t *testing.T) {
	data := source(1000)
	p := plan.New(int64(len(data)), 100)
	out := newSink(len(data))

	var mu sync.Mutex
	var seen []Progress

	cfg := fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "l", Fetcher: &fakeLink{data: data}, Workers: 2},
	)
	cfg.OnProgress = func(pr Progress) {
		mu.Lock()
		seen = append(seen, pr)
		mu.Unlock()
	}

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != p.Chunks {
		t.Fatalf("got %d progress callbacks, want %d", len(seen), p.Chunks)
	}
	last := seen[len(seen)-1]
	if last.DoneChunks != p.Chunks || last.TotalChunks != p.Chunks {
		t.Errorf("final progress = %d/%d", last.DoneChunks, last.TotalChunks)
	}
	if last.Link == "" {
		t.Error("progress should name the link that delivered the chunk")
	}
}

func TestRunRejectsAConfigWithNoLinks(t *testing.T) {
	p := plan.New(100, 10)
	_, err := Run(context.Background(), fastOpts(p, plan.NewBitmap(p.Chunks), newSink(100)))
	if err == nil {
		t.Fatal("Run with no links must error")
	}
}

func TestShortFetchIsRejected(t *testing.T) {
	// A server that returns fewer bytes than the range asked for would leave a
	// hole that the bitmap claims is filled.
	data := source(200)
	p := plan.New(int64(len(data)), 100)
	out := newSink(len(data))

	_, err := Run(context.Background(), fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "truncating", Fetcher: truncatingFetcher{data}, Workers: 1},
	))
	if err == nil {
		t.Fatal("a short chunk body must be an error, not a silent hole")
	}
}

type truncatingFetcher struct{ data []byte }

func (f truncatingFetcher) Fetch(ctx context.Context, start, end int64) ([]byte, error) {
	full := end - start + 1
	if full < 2 {
		return nil, fmt.Errorf("range too small to truncate")
	}
	return f.data[start : start+full-1], nil // one byte short, every time
}

func TestFatalErrorsAreNotRetried(t *testing.T) {
	// Some failures cannot be fixed by trying again: a server that answers 206
	// and sends the whole body will do it every time. Retrying costs a full
	// file download per attempt, which on a metered link is real money.
	data := source(400)
	p := plan.New(int64(len(data)), 100)
	out := newSink(len(data))

	f := &fatalFetcher{}
	_, err := Run(context.Background(), fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "liar", Fetcher: f, Workers: 1},
	))
	if err == nil {
		t.Fatal("a fatal fetch error must fail the run")
	}
	if !errors.Is(err, ErrFatal) {
		t.Errorf("error should wrap ErrFatal, got %v", err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("fetcher called %d times; a fatal error must not be retried", n)
	}
}

type fatalFetcher struct{ calls atomic.Int32 }

func (f *fatalFetcher) Fetch(ctx context.Context, start, end int64) ([]byte, error) {
	f.calls.Add(1)
	return nil, fmt.Errorf("server ignored the range: %w", ErrFatal)
}

// orderCheckingSink asserts the load-bearing invariant for streaming: a
// chunk's bit must never be set before its bytes are on disk. A reader that
// wakes on the bitmap reads whatever is at that offset, so publishing early
// hands it zeros.
type orderCheckingSink struct {
	*sink
	bits       *plan.Bitmap
	p          plan.Plan
	violations atomic.Int32
}

func (s *orderCheckingSink) WriteAt(b []byte, off int64) (int, error) {
	if s.bits.Get(int(off / s.p.ChunkSize)) {
		s.violations.Add(1)
	}
	return s.sink.WriteAt(b, off)
}

func TestChunkBytesAreOnDiskBeforeTheBitIsSet(t *testing.T) {
	data := source(2000)
	p := plan.New(int64(len(data)), 100) // 20 chunks
	bits := plan.NewBitmap(p.Chunks)
	out := &orderCheckingSink{sink: newSink(len(data)), bits: bits, p: p}

	// Tail stealing off, so there is exactly one write per chunk and any
	// violation is a genuine ordering fault rather than a duplicate writer.
	cfg := Config{
		Plan: p, Bits: bits, Out: out,
		Links: []Link{
			{Name: "a", Fetcher: &fakeLink{data: data}, Workers: 3},
			{Name: "b", Fetcher: &fakeLink{data: data}, Workers: 3},
		},
		MaxAttempts: 4,
		Backoff:     func(int) time.Duration { return time.Millisecond },
	}

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Equal(out.sink.bytes(), data) {
		t.Fatal("output does not match the source")
	}
	if n := out.violations.Load(); n != 0 {
		t.Errorf("%d chunk(s) had their bit set before the bytes were written; a streaming reader would serve zeros", n)
	}
}

func TestAFewLargeChunksDoNotStrandTheTransferOnASlowLink(t *testing.T) {
	// The real-world failure, reproduced. Large blocks mean few chunks, and
	// with few chunks one bad assignment is catastrophic: a 33.5 MB download
	// over Wi-Fi at 102 Mbps plus cellular at 7 took 13 seconds instead of 2,
	// because cellular grabbed one 8 MB block and held it while Wi-Fi idled.
	//
	// A plentiful queue hides this — the shared queue alone is enough when
	// there are forty chunks. It is exactly the few-chunk case that needs
	// rescuing, which is what tail stealing is for.
	data := source(500)
	p := plan.New(int64(len(data)), 100) // only 5 chunks
	out := newSink(len(data))

	fast := &fakeLink{data: data}
	crawling := &fakeLink{data: data, delay: 3 * time.Second}

	cfg := fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "fast", Fetcher: fast, Workers: 1},
		Link{Name: "crawling", Fetcher: crawling, Workers: 1},
	)
	cfg.TailStealAfter = 150 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	res, err := Run(ctx, cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Equal(out.bytes(), data) {
		t.Fatal("output does not match the source")
	}
	// Without rescue this waits the full 3 seconds for the slow link's block.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("took %v; the transfer is still stranded on the slow link", elapsed)
	}
	if res.TailSteals == 0 {
		t.Error("nothing was stolen, so the slow link was simply waited on")
	}
}

func TestASingleSlowLinkIsStillUsedWhenItIsAllThereIs(t *testing.T) {
	// Demotion must never strand a transfer. With nothing faster to compare
	// against, a slow link is simply the link.
	data := source(600)
	p := plan.New(int64(len(data)), 100)
	out := newSink(len(data))

	slow := &fakeLink{data: data, delay: 5 * time.Millisecond}
	if _, err := Run(context.Background(), fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "slow", Fetcher: slow, Workers: 2},
	)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Equal(out.bytes(), data) {
		t.Error("output does not match the source")
	}
}

func TestRescueHappensAsSoonAsRefetchingBeatsWaiting(t *testing.T) {
	// A fixed rescue delay is a guess. With Wi-Fi at 102 Mbps beside cellular
	// at 7, waiting three seconds before rescuing wastes almost all of the
	// three seconds: the fast link could have refetched the whole block in a
	// fraction of that.
	//
	// The rule should be arithmetic, not a timer: steal when refetching would
	// finish sooner than waiting for the current holder.
	data := source(500)
	p := plan.New(int64(len(data)), 100) // 5 chunks
	out := newSink(len(data))

	fast := &fakeLink{data: data}
	crawling := &fakeLink{data: data, delay: 4 * time.Second}

	cfg := fastOpts(p, plan.NewBitmap(p.Chunks), out,
		Link{Name: "fast", Fetcher: fast, Workers: 1},
		Link{Name: "crawling", Fetcher: crawling, Workers: 1},
	)
	// Deliberately NOT setting TailStealAfter: the scheduler should work this
	// out from observed rates rather than being told a delay.

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	start := time.Now()
	res, err := Run(ctx, cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Equal(out.bytes(), data) {
		t.Fatal("output does not match the source")
	}
	// The fast link needs milliseconds for all five chunks. Anything close to
	// the slow link's four seconds means we waited instead of deciding.
	if elapsed > 900*time.Millisecond {
		t.Errorf("took %v; the rescue is still waiting on a timer rather than "+
			"comparing refetch against wait", elapsed)
	}
	if res.TailSteals == 0 {
		t.Error("nothing was rescued")
	}
}
