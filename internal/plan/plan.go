// Package plan divides a file into chunks, tracks which have arrived, and
// persists just enough state to resume safely.
package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// DefaultChunkSize is a compromise: large enough that per-request overhead is
// negligible, small enough that many chunks remain for a fast link to steal
// while a slow one is still working.
const DefaultChunkSize int64 = 4 << 20

// Plan is the fixed division of a file into chunks. Chunks are the unit of
// leasing, retry, and accounting.
type Plan struct {
	Size      int64
	ChunkSize int64
	Chunks    int
}

// New divides size into chunks. The final chunk is short whenever the size is
// not a multiple of chunkSize.
func New(size, chunkSize int64) Plan {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	chunks := int((size + chunkSize - 1) / chunkSize)
	return Plan{Size: size, ChunkSize: chunkSize, Chunks: chunks}
}

// Range returns the inclusive byte range of chunk i, as HTTP Range uses
// inclusive ends.
func (p Plan) Range(i int) (start, end int64) {
	start = int64(i) * p.ChunkSize
	end = start + p.ChunkSize - 1
	if end > p.Size-1 {
		end = p.Size - 1
	}
	return start, end
}

// Len is the length in bytes of chunk i.
func (p Plan) Len(i int) int64 {
	start, end := p.Range(i)
	return end - start + 1
}

// RangeHeader is the Range request header value for chunk i.
func (p Plan) RangeHeader(i int) string {
	start, end := p.Range(i)
	return fmt.Sprintf("bytes=%d-%d", start, end)
}

// Bitmap records which chunks have landed. Safe for concurrent use: every
// worker touches it.
type Bitmap struct {
	mu    sync.Mutex
	words []uint64
	n     int
	done  int
	// changed is closed and replaced on every completed chunk. Waiters select
	// on it alongside their context, which a sync.Cond cannot offer — and a
	// reader whose client has hung up must not stay parked forever.
	changed chan struct{}
}

func NewBitmap(n int) *Bitmap {
	return &Bitmap{
		words:   make([]uint64, (n+63)/64),
		n:       n,
		changed: make(chan struct{}),
	}
}

// BitmapFromBytes restores a bitmap from its serialised form. Input shorter
// than n bits is tolerated — the missing bits simply read as incomplete, which
// is the safe direction for a truncated sidecar.
func BitmapFromBytes(n int, b []byte) *Bitmap {
	bm := NewBitmap(n)
	for i := 0; i < n; i++ {
		byteIdx := i / 8
		if byteIdx >= len(b) {
			break
		}
		if b[byteIdx]&(1<<uint(i%8)) != 0 {
			bm.set(i)
		}
	}
	return bm
}

// Set marks chunk i complete and reports whether this call was the one that
// changed it. A false return means someone else got there first, which is how
// a tail-stealing loser learns to discard its buffer.
func (b *Bitmap) Set(i int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := b.set(i)
	if changed {
		// Wake every waiter. Each re-checks its own chunk and sleeps again if
		// this was not the one it needed.
		close(b.changed)
		b.changed = make(chan struct{})
	}
	return changed
}

// WaitFor blocks until chunk i has landed, or ctx ends.
//
// This is what lets a client read a file in order while chunks arrive out of
// order: the reader parks on the next byte it needs rather than polling.
func (b *Bitmap) WaitFor(ctx context.Context, i int) error {
	if i < 0 || i >= b.n {
		return fmt.Errorf("chunk %d is outside a file of %d chunks", i, b.n)
	}
	for {
		b.mu.Lock()
		if b.words[i/64]&(1<<uint(i%64)) != 0 {
			b.mu.Unlock()
			return nil
		}
		changed := b.changed
		b.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// set is the unlocked core, for use during construction.
func (b *Bitmap) set(i int) bool {
	if i < 0 || i >= b.n {
		return false
	}
	word, bit := i/64, uint(i%64)
	if b.words[word]&(1<<bit) != 0 {
		return false
	}
	b.words[word] |= 1 << bit
	b.done++
	return true
}

func (b *Bitmap) Get(i int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i < 0 || i >= b.n {
		return false
	}
	return b.words[i/64]&(1<<uint(i%64)) != 0
}

func (b *Bitmap) Done() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.done
}

func (b *Bitmap) Complete() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.done == b.n
}

// Bytes serialises the bitmap little-endian by bit index, for the sidecar.
func (b *Bitmap) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, (b.n+7)/8)
	for i := 0; i < b.n; i++ {
		if b.words[i/64]&(1<<uint(i%64)) != 0 {
			out[i/8] |= 1 << uint(i%8)
		}
	}
	return out
}

// State is the resume sidecar: what was being fetched, how it was divided, and
// how far it got.
type State struct {
	URL          string `json:"url"`
	Filename     string `json:"filename"`
	Size         int64  `json:"size"`
	ChunkSize    int64  `json:"chunk_size"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Bits         []byte `json:"bits"`
}

// Resumable reports whether saved progress may be applied to what the server
// is serving now.
//
// Deliberately strict. Both validators must match exactly, including the case
// where a validator is absent from both sides, and at least one must actually
// be present. A validator that has disappeared since the sidecar was written
// is treated as a mismatch: resuming against a changed file would silently
// stitch two different files together, which is far worse than downloading
// again.
func (s State) Resumable(remote State) bool {
	if s.Size != remote.Size || s.ChunkSize != remote.ChunkSize {
		return false
	}
	if s.ETag != remote.ETag || s.LastModified != remote.LastModified {
		return false
	}
	return s.ETag != "" || s.LastModified != ""
}

// Save writes the sidecar atomically: a temporary file in the same directory,
// fsynced, then renamed over the target. It is written on every chunk
// completion, so a crash mid-write must never leave an unparseable file.
func Save(path string, s State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".braid-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// On any failure past this point the temporary file must not survive.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func Load(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("unparsable resume file %s: %w", path, err)
	}
	return s, nil
}
