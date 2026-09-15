package plan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewDividesEvenly(t *testing.T) {
	p := New(8, 4)
	if p.Chunks != 2 {
		t.Fatalf("Chunks = %d, want 2", p.Chunks)
	}
	assertRange(t, p, 0, 0, 3)
	assertRange(t, p, 1, 4, 7)
}

func TestNewLeavesAShortFinalChunk(t *testing.T) {
	p := New(10, 4)
	if p.Chunks != 3 {
		t.Fatalf("Chunks = %d, want 3", p.Chunks)
	}
	assertRange(t, p, 0, 0, 3)
	assertRange(t, p, 1, 4, 7)
	assertRange(t, p, 2, 8, 9) // inclusive end, so 2 bytes
}

func TestNewHandlesFileSmallerThanOneChunk(t *testing.T) {
	p := New(1, 4)
	if p.Chunks != 1 {
		t.Fatalf("Chunks = %d, want 1", p.Chunks)
	}
	assertRange(t, p, 0, 0, 0)
}

func TestChunkLenSumsToTotalSize(t *testing.T) {
	// Any off-by-one in the range maths corrupts the output file, so check the
	// invariant across sizes that straddle chunk boundaries.
	for _, size := range []int64{1, 3, 4, 5, 7, 8, 9, 1 << 20, (1 << 20) + 1} {
		p := New(size, 4096)
		var total int64
		for i := 0; i < p.Chunks; i++ {
			start, end := p.Range(i)
			if start > end {
				t.Fatalf("size %d chunk %d: start %d > end %d", size, i, start, end)
			}
			total += end - start + 1
		}
		if total != size {
			t.Errorf("size %d: chunk lengths sum to %d", size, total)
		}
	}
}

func TestRangeHeaderFormat(t *testing.T) {
	p := New(10, 4)
	if got := p.RangeHeader(1); got != "bytes=4-7" {
		t.Errorf("RangeHeader(1) = %q, want bytes=4-7", got)
	}
}

func assertRange(t *testing.T, p Plan, i int, wantStart, wantEnd int64) {
	t.Helper()
	start, end := p.Range(i)
	if start != wantStart || end != wantEnd {
		t.Errorf("Range(%d) = %d-%d, want %d-%d", i, start, end, wantStart, wantEnd)
	}
}

func TestBitmapTracksCompletion(t *testing.T) {
	b := NewBitmap(3)

	if b.Done() != 0 || b.Complete() {
		t.Fatalf("fresh bitmap: Done=%d Complete=%v", b.Done(), b.Complete())
	}
	if !b.Set(1) {
		t.Error("Set(1) on a fresh bit should report true")
	}
	if !b.Get(1) {
		t.Error("Get(1) after Set(1) should be true")
	}
	if b.Get(0) {
		t.Error("Get(0) should still be false")
	}
	if b.Done() != 1 {
		t.Errorf("Done = %d, want 1", b.Done())
	}

	b.Set(0)
	b.Set(2)
	if !b.Complete() || b.Done() != 3 {
		t.Errorf("Complete=%v Done=%d, want true/3", b.Complete(), b.Done())
	}
}

func TestBitmapSetIsIdempotentAndReportsIt(t *testing.T) {
	// Tail stealing has two workers racing for the same bytes. The loser must
	// learn it lost so it can discard its buffer instead of writing again.
	b := NewBitmap(2)

	if !b.Set(0) {
		t.Fatal("first Set should report true")
	}
	if b.Set(0) {
		t.Error("second Set of the same bit must report false")
	}
	if b.Done() != 1 {
		t.Errorf("Done = %d; a repeated Set must not double-count", b.Done())
	}
}

func TestBitmapHandlesMoreThanOneWord(t *testing.T) {
	// 64 bits per word, so this catches word-indexing bugs.
	const n = 200
	b := NewBitmap(n)
	for i := 0; i < n; i++ {
		b.Set(i)
	}
	if !b.Complete() || b.Done() != n {
		t.Errorf("Complete=%v Done=%d, want true/%d", b.Complete(), b.Done(), n)
	}
	for i := 0; i < n; i++ {
		if !b.Get(i) {
			t.Fatalf("bit %d not set", i)
		}
	}
}

func TestBitmapRoundTripsThroughBytes(t *testing.T) {
	b := NewBitmap(130)
	for _, i := range []int{0, 63, 64, 129} {
		b.Set(i)
	}

	restored := BitmapFromBytes(130, b.Bytes())

	if restored.Done() != 4 {
		t.Errorf("restored Done = %d, want 4", restored.Done())
	}
	for _, i := range []int{0, 63, 64, 129} {
		if !restored.Get(i) {
			t.Errorf("restored bit %d should be set", i)
		}
	}
	for _, i := range []int{1, 62, 65, 128} {
		if restored.Get(i) {
			t.Errorf("restored bit %d should be clear", i)
		}
	}
}

func TestBitmapFromBytesToleratesShortInput(t *testing.T) {
	// A truncated sidecar must not panic; it should just mean less progress.
	restored := BitmapFromBytes(130, []byte{0xFF})
	if restored.Done() != 8 {
		t.Errorf("Done = %d, want 8", restored.Done())
	}
	if restored.Get(100) {
		t.Error("bit beyond the supplied bytes should be clear")
	}
}

func stateFixture() State {
	return State{
		URL:          "https://dl.google.com/go/go1.27.1.darwin-arm64.tar.gz",
		Size:         68061234,
		ChunkSize:    4 << 20,
		ETag:         `"abc123"`,
		LastModified: "Mon, 15 Sep 2026 10:00:00 GMT",
		Filename:     "go1.27.1.darwin-arm64.tar.gz",
		Bits:         []byte{0x0F},
	}
}

func TestStateSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.braid")
	want := stateFixture()

	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.URL != want.URL || got.Size != want.Size || got.ChunkSize != want.ChunkSize {
		t.Errorf("identity fields differ: %+v", got)
	}
	if got.ETag != want.ETag || got.LastModified != want.LastModified {
		t.Errorf("validators differ: %+v", got)
	}
	if got.Filename != want.Filename {
		t.Errorf("Filename = %q", got.Filename)
	}
	if len(got.Bits) != 1 || got.Bits[0] != 0x0F {
		t.Errorf("Bits = %v", got.Bits)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	// Saved on every chunk completion, so a crash mid-write must not leave a
	// half-written sidecar that fails to parse on resume.
	dir := t.TempDir()
	path := filepath.Join(dir, "out.braid")

	if err := Save(path, stateFixture()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Save(path, stateFixture()); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("Save left temporary files behind: %v", names)
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.braid")); err == nil {
		t.Fatal("Load of a missing sidecar must error")
	}
}

func TestResumableRequiresMatchingValidators(t *testing.T) {
	// Resuming against a changed remote file would stitch two different files
	// together, which is worse than starting over.
	base := stateFixture()

	cases := []struct {
		name string
		mut  func(*State)
		want bool
	}{
		{"identical", func(s *State) {}, true},
		{"different etag", func(s *State) { s.ETag = `"different"` }, false},
		{"different size", func(s *State) { s.Size = 999 }, false},
		{"different last-modified", func(s *State) { s.LastModified = "Tue, 16 Sep 2026 10:00:00 GMT" }, false},
		{"different chunk size", func(s *State) { s.ChunkSize = 1 << 20 }, false},
		{"etag disappeared remotely", func(s *State) { s.ETag = "" }, false},
		{"no validators at all", func(s *State) { s.ETag, s.LastModified = "", "" }, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			remote := base
			c.mut(&remote)
			if got := base.Resumable(remote); got != c.want {
				t.Errorf("Resumable = %v, want %v", got, c.want)
			}
		})
	}
}

func TestResumableAcceptsLastModifiedWhenNoETag(t *testing.T) {
	// Plenty of servers send only one validator. One matching validator plus a
	// matching size is enough.
	saved := stateFixture()
	saved.ETag = ""
	remote := saved

	if !saved.Resumable(remote) {
		t.Error("matching Last-Modified and size should permit resume when neither side has an ETag")
	}
}

func TestBitmapWaitForReturnsImmediatelyWhenAlreadySet(t *testing.T) {
	b := NewBitmap(4)
	b.Set(2)

	if err := b.WaitFor(context.Background(), 2); err != nil {
		t.Errorf("WaitFor on a set bit should return at once, got %v", err)
	}
}

func TestBitmapWaitForBlocksUntilTheChunkLands(t *testing.T) {
	// This is what lets a client read the file in order while chunks arrive out
	// of order: the reader parks on the next byte it needs instead of polling.
	b := NewBitmap(4)

	done := make(chan error, 1)
	go func() { done <- b.WaitFor(context.Background(), 3) }()

	select {
	case <-done:
		t.Fatal("WaitFor returned before the chunk landed")
	case <-time.After(20 * time.Millisecond):
	}

	b.Set(3)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("WaitFor: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitFor did not wake when the chunk landed")
	}
}

func TestBitmapWaitForWakesOnAnUnrelatedChunk(t *testing.T) {
	// Chunks land in any order, so a waiter is woken by every Set and must go
	// back to sleep unless its own chunk arrived.
	b := NewBitmap(4)

	done := make(chan error, 1)
	go func() { done <- b.WaitFor(context.Background(), 1) }()

	b.Set(0)
	b.Set(2)
	select {
	case <-done:
		t.Fatal("WaitFor returned for chunk 1 when only 0 and 2 landed")
	case <-time.After(20 * time.Millisecond):
	}

	b.Set(1)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("WaitFor: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitFor did not wake for its own chunk")
	}
}

func TestBitmapWaitForHonoursContextCancellation(t *testing.T) {
	// A client that closes the connection must not leave a goroutine parked
	// forever on a chunk nobody is fetching any more.
	b := NewBitmap(4)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- b.WaitFor(ctx, 3) }()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitFor ignored cancellation")
	}
}

func TestBitmapWaitForRejectsAnImpossibleChunk(t *testing.T) {
	b := NewBitmap(4)
	if err := b.WaitFor(context.Background(), 99); err == nil {
		t.Fatal("waiting for a chunk outside the file must error, not block forever")
	}
}
