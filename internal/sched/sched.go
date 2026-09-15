// Package sched drives a bonded transfer: many workers, spread across links,
// pulling chunks from one shared queue.
//
// The shared queue is the whole trick. Nothing is handed a fixed share, so a
// link that turns out slow simply completes fewer chunks. Splitting a file
// 50/50 up front would make every transfer wait on the slower link, and
// measurement showed link speeds are neither equal nor stable.
package sched

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"braid/internal/plan"
)

// Fetcher retrieves one inclusive byte range. Implementations are expected to
// be pinned to a single interface.
type Fetcher interface {
	Fetch(ctx context.Context, start, end int64) ([]byte, error)
}

// Link is one uplink and how many concurrent fetches to run on it.
type Link struct {
	Name    string
	Fetcher Fetcher
	Workers int
}

// Progress is reported once per chunk successfully written.
type Progress struct {
	Link        string
	ChunkIndex  int
	DoneChunks  int
	TotalChunks int
	Bytes       int64 // cumulative bytes written by this run
}

// Config describes one transfer.
type Config struct {
	Plan  plan.Plan
	Bits  *plan.Bitmap
	Out   io.WriterAt
	Links []Link

	// MaxAttempts is how many times a single link may retry one chunk before
	// giving that chunk up. Other links may still attempt it.
	MaxAttempts int
	// Backoff returns the delay before a link's next attempt.
	Backoff func(attempt int) time.Duration
	// ChunkTimeout bounds any single fetch, so a connection that stays open
	// but silent is abandoned rather than holding a chunk forever.
	ChunkTimeout time.Duration
	// TailStealAfter enables endgame tail stealing: once the queue is empty, an
	// idle worker may re-request a chunk that has been in flight this long.
	// Zero disables it, because duplicate bytes cost money on a metered link.
	TailStealAfter time.Duration
	// LinkFailureLimit retires a link after this many consecutive failures.
	LinkFailureLimit int

	// OnProgress is called once per chunk written, serialised so DoneChunks is
	// monotonic. It runs on a worker goroutine and blocks it, so it must be
	// cheap and must not block.
	OnProgress func(Progress)
}

// Result summarises what happened.
type Result struct {
	Bytes      int64
	ByLink     map[string]int
	TailSteals int
}

const (
	defaultMaxAttempts      = 5
	defaultLinkFailureLimit = 6
)

func defaultBackoff(attempt int) time.Duration {
	d := time.Second << uint(attempt)
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	return d
}

// Run fetches every outstanding chunk and writes it at its offset. It returns
// when the file is complete, when no link can make progress, or when ctx ends.
func Run(ctx context.Context, cfg Config) (Result, error) {
	if len(cfg.Links) == 0 {
		return Result{}, errors.New("no links to download over")
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if cfg.LinkFailureLimit <= 0 {
		cfg.LinkFailureLimit = defaultLinkFailureLimit
	}
	if cfg.Backoff == nil {
		cfg.Backoff = defaultBackoff
	}

	q := newQueue(cfg)
	res := Result{ByLink: map[string]int{}}
	for _, l := range cfg.Links {
		res.ByLink[l.Name] = 0
	}

	// Already complete: nothing to fetch, and no worker should make a request.
	if q.done() {
		return res, nil
	}

	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Surface cancellation of the caller's context into the queue so waiters
	// wake up instead of blocking on a job that can no longer finish.
	go func() {
		<-jobCtx.Done()
		q.abort(jobCtx.Err())
	}()

	// Tail stealing decides on elapsed time, so waiters need waking
	// periodically to re-evaluate how long a chunk has been in flight.
	if cfg.TailStealAfter > 0 {
		ticker := time.NewTicker(cfg.TailStealAfter / 2)
		defer ticker.Stop()
		go func() {
			for {
				select {
				case <-jobCtx.Done():
					return
				case <-ticker.C:
					q.wake()
				}
			}
		}()
	}

	var (
		mu sync.Mutex // guards res
		wg sync.WaitGroup
	)
	for _, l := range cfg.Links {
		workers := l.Workers
		if workers <= 0 {
			workers = 1
		}
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(l Link) {
				defer wg.Done()
				runWorker(jobCtx, cfg, q, l, &mu, &res)
			}(l)
		}
	}

	err := q.wait()

	// Unblock any fetch still sitting on a wedged connection, then let the
	// workers finish so nothing writes after Run returns.
	cancel()
	wg.Wait()

	mu.Lock()
	res.TailSteals = q.steals()
	out := res
	mu.Unlock()

	if err != nil {
		return out, err
	}
	return out, nil
}

func runWorker(ctx context.Context, cfg Config, q *queue, l Link, mu *sync.Mutex, res *Result) {
	for {
		if ctx.Err() != nil {
			return
		}
		idx, ok := q.next(l.Name, cfg.TailStealAfter)
		if !ok {
			return
		}

		start, end := cfg.Plan.Range(idx)
		want := end - start + 1

		fetchCtx := ctx
		var cancelFetch context.CancelFunc
		if cfg.ChunkTimeout > 0 {
			fetchCtx, cancelFetch = context.WithTimeout(ctx, cfg.ChunkTimeout)
		}
		body, err := l.Fetcher.Fetch(fetchCtx, start, end)
		if cancelFetch != nil {
			cancelFetch()
		}

		// A body shorter than the range asked for would leave a hole that the
		// bitmap claims is filled, so it counts as a failure.
		if err == nil && int64(len(body)) != want {
			err = fmt.Errorf("chunk %d: got %d bytes, want %d", idx, len(body), want)
		}

		if err != nil {
			// A cancelled job is not the link's fault; stop rather than
			// burning this chunk's attempt budget.
			if ctx.Err() != nil {
				q.release(idx)
				return
			}
			attempt := q.failed(idx, l.Name, err)
			if d := cfg.Backoff(attempt); d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
					return
				}
			}
			continue
		}

		// Claim the chunk before writing. Under tail stealing two workers hold
		// the same range; the bitmap decides which one owns it, so the loser
		// discards its buffer instead of repeating the write.
		if !cfg.Bits.Set(idx) {
			q.release(idx)
			continue
		}
		if _, err := cfg.Out.WriteAt(body, start); err != nil {
			// The bytes are good but the disk refused them. This is not
			// retryable on another link.
			q.abort(fmt.Errorf("writing chunk %d at offset %d: %w", idx, start, err))
			return
		}

		// Progress is reported while the lock is held, which serialises delivery
		// so DoneChunks never goes backwards. Reporting outside the lock let a
		// worker that computed 4/10 deliver after one that computed 10/10, which
		// would make a progress bar jump backwards.
		mu.Lock()
		res.Bytes += want
		res.ByLink[l.Name]++
		if cfg.OnProgress != nil {
			cfg.OnProgress(Progress{
				Link:        l.Name,
				ChunkIndex:  idx,
				DoneChunks:  cfg.Bits.Done(),
				TotalChunks: cfg.Plan.Chunks,
				Bytes:       res.Bytes,
			})
		}
		mu.Unlock()

		q.completed(idx, l.Name)
	}
}

// chunkLink keys an attempt counter per chunk *and* link. A chunk is only
// given up once every live link has exhausted its attempts on it, so one
// broken link cannot condemn the transfer.
type chunkLink struct {
	idx  int
	link string
}

type queue struct {
	mu   sync.Mutex
	cond *sync.Cond

	pending  []int
	inflight map[int]time.Time
	stolen   map[int]bool

	attempts  map[chunkLink]int
	linkFails map[string]int
	liveLinks map[string]bool

	remaining        int
	maxAttempts      int
	linkFailureLimit int
	tailSteals       int
	failErr          error
}

func newQueue(cfg Config) *queue {
	q := &queue{
		inflight:         map[int]time.Time{},
		stolen:           map[int]bool{},
		attempts:         map[chunkLink]int{},
		linkFails:        map[string]int{},
		liveLinks:        map[string]bool{},
		maxAttempts:      cfg.MaxAttempts,
		linkFailureLimit: cfg.LinkFailureLimit,
	}
	q.cond = sync.NewCond(&q.mu)
	for _, l := range cfg.Links {
		q.liveLinks[l.Name] = true
	}
	for i := 0; i < cfg.Plan.Chunks; i++ {
		if !cfg.Bits.Get(i) {
			q.pending = append(q.pending, i)
			q.remaining++
		}
	}
	return q
}

func (q *queue) done() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.remaining == 0
}

func (q *queue) steals() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.tailSteals
}

// next leases a chunk for one link, or reports that this link has nothing left
// to do. It blocks while the queue is empty but chunks are still in flight.
func (q *queue) next(link string, tailStealAfter time.Duration) (int, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for {
		if q.failErr != nil || q.remaining == 0 || !q.liveLinks[link] {
			return 0, false
		}

		// Prefer untried work, skipping chunks this link has already exhausted.
		if idx, ok := q.takePendingFor(link); ok {
			return idx, true
		}
		// Chunks remain queued but this link may not attempt any of them.
		if len(q.pending) > 0 {
			q.retireLocked(link)
			return 0, false
		}

		// Endgame: the queue is drained and this worker is idle while someone
		// else is still labouring over a chunk.
		if tailStealAfter > 0 {
			if idx, ok := q.takeStealFor(link, tailStealAfter); ok {
				return idx, true
			}
		}

		q.cond.Wait()
	}
}

func (q *queue) takePendingFor(link string) (int, bool) {
	for i, idx := range q.pending {
		if q.attempts[chunkLink{idx, link}] < q.maxAttempts {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			q.inflight[idx] = time.Now()
			return idx, true
		}
	}
	return 0, false
}

func (q *queue) takeStealFor(link string, after time.Duration) (int, bool) {
	now := time.Now()
	for idx, started := range q.inflight {
		if q.stolen[idx] || now.Sub(started) < after {
			continue
		}
		if q.attempts[chunkLink{idx, link}] >= q.maxAttempts {
			continue
		}
		// One steal per chunk. A crowd of workers re-requesting the same bytes
		// would multiply cost without improving the outcome.
		q.stolen[idx] = true
		q.tailSteals++
		return idx, true
	}
	return 0, false
}

// completed records a chunk as finished and owned by link.
func (q *queue) completed(idx int, link string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.inflight, idx)
	q.linkFails[link] = 0
	if q.remaining > 0 {
		q.remaining--
	}
	q.cond.Broadcast()
}

// release gives a chunk back without blaming anyone: the caller lost a steal
// race, or the job is shutting down.
func (q *queue) release(idx int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.inflight, idx)
	q.cond.Broadcast()
}

// failed records a failed attempt and requeues the chunk. It returns the
// attempt number for backoff, and gives up on the whole transfer only when no
// live link could still try this chunk.
func (q *queue) failed(idx int, link string, cause error) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	key := chunkLink{idx, link}
	q.attempts[key]++
	attempt := q.attempts[key]

	q.linkFails[link]++
	if q.linkFails[link] >= q.linkFailureLimit {
		q.retireLocked(link)
	}

	delete(q.inflight, idx)
	delete(q.stolen, idx)
	q.requeueLocked(idx)

	if !q.anyLinkCanTry(idx) {
		q.failErr = fmt.Errorf("chunk %d unrecoverable on every link: %w", idx, cause)
	}
	q.cond.Broadcast()
	return attempt
}

// requeueLocked re-adds a chunk unless it is already queued. Under tail
// stealing two workers can fail the same chunk, and a duplicate entry would
// have it fetched twice.
func (q *queue) requeueLocked(idx int) {
	for _, existing := range q.pending {
		if existing == idx {
			return
		}
	}
	q.pending = append(q.pending, idx)
}

func (q *queue) anyLinkCanTry(idx int) bool {
	for link, live := range q.liveLinks {
		if live && q.attempts[chunkLink{idx, link}] < q.maxAttempts {
			return true
		}
	}
	return false
}

func (q *queue) retireLocked(link string) {
	if !q.liveLinks[link] {
		return
	}
	q.liveLinks[link] = false
	for _, live := range q.liveLinks {
		if live {
			q.cond.Broadcast()
			return
		}
	}
	if q.remaining > 0 && q.failErr == nil {
		q.failErr = errors.New("every link failed; no way to fetch the remaining chunks")
	}
	q.cond.Broadcast()
}

func (q *queue) abort(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failErr == nil && err != nil && q.remaining > 0 {
		q.failErr = err
	}
	q.cond.Broadcast()
}

func (q *queue) wake() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cond.Broadcast()
}

// wait blocks until the transfer finishes or cannot continue.
func (q *queue) wait() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.failErr == nil && q.remaining > 0 {
		q.cond.Wait()
	}
	return q.failErr
}
