package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"braid/internal/plan"
)

func payload(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n) + 11)).Read(b)
	return b
}

// syncWriter records what it was handed and when, so a test can prove bytes
// were flushed progressively rather than all at the end.
type syncWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	flushes int
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushes++
	return w.buf.Write(p)
}

func (w *syncWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, w.buf.Len())
	copy(out, w.buf.Bytes())
	return out
}

func (w *syncWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushes
}

func TestCopyServesTheWholeFile(t *testing.T) {
	data := payload(1000)
	p := plan.New(int64(len(data)), 100)
	bits := plan.NewBitmap(p.Chunks)
	for i := 0; i < p.Chunks; i++ {
		bits.Set(i)
	}

	var out bytes.Buffer
	n, err := Copy(context.Background(), &out, p, bits, bytes.NewReader(data), 0, int64(len(data))-1)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("wrote %d bytes, want %d", n, len(data))
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Error("output does not match the source")
	}
}

func TestCopyWaitsForChunksArrivingOutOfOrder(t *testing.T) {
	// The whole point of the endpoint: the fetcher completes chunks in whatever
	// order the links deliver them, while the client reads front to back.
	data := payload(500)
	p := plan.New(int64(len(data)), 100) // 5 chunks
	bits := plan.NewBitmap(p.Chunks)

	out := &syncWriter{}
	done := make(chan error, 1)
	go func() {
		_, err := Copy(context.Background(), out, p, bits, bytes.NewReader(data), 0, int64(len(data))-1)
		done <- err
	}()

	// Land them back to front. Nothing may be served until chunk 0 arrives.
	for _, i := range []int{4, 3, 2, 1} {
		bits.Set(i)
	}
	time.Sleep(20 * time.Millisecond)
	if got := len(out.bytes()); got != 0 {
		t.Fatalf("served %d bytes before chunk 0 landed", got)
	}

	bits.Set(0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Copy: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Copy did not finish once every chunk had landed")
	}
	if !bytes.Equal(out.bytes(), data) {
		t.Error("output does not match the source")
	}
}

func TestCopyFlushesProgressively(t *testing.T) {
	// A video player needs the first bytes immediately, not at the end.
	data := payload(300)
	p := plan.New(int64(len(data)), 100) // 3 chunks
	bits := plan.NewBitmap(p.Chunks)
	bits.Set(0)

	out := &syncWriter{}
	done := make(chan error, 1)
	go func() {
		_, err := Copy(context.Background(), out, p, bits, bytes.NewReader(data), 0, int64(len(data))-1)
		done <- err
	}()

	// Chunk 0 is available, so its bytes must appear without waiting for 1 or 2.
	deadline := time.Now().Add(2 * time.Second)
	for len(out.bytes()) < 100 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(out.bytes()); got != 100 {
		t.Fatalf("served %d bytes of the first chunk, want 100 before later chunks landed", got)
	}

	bits.Set(1)
	bits.Set(2)
	if err := <-done; err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if !bytes.Equal(out.bytes(), data) {
		t.Error("output does not match the source")
	}
	if out.count() < 2 {
		t.Errorf("wrote in %d calls; progressive delivery should take several", out.count())
	}
}

func TestCopyHonoursAStartOffsetInsideAChunk(t *testing.T) {
	// Players seek to arbitrary byte offsets, which rarely align to a chunk.
	data := payload(500)
	p := plan.New(int64(len(data)), 100)
	bits := plan.NewBitmap(p.Chunks)
	for i := 0; i < p.Chunks; i++ {
		bits.Set(i)
	}

	var out bytes.Buffer
	n, err := Copy(context.Background(), &out, p, bits, bytes.NewReader(data), 250, int64(len(data))-1)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n != 250 {
		t.Errorf("wrote %d bytes, want 250", n)
	}
	if !bytes.Equal(out.Bytes(), data[250:]) {
		t.Error("output does not match the source from offset 250")
	}
}

func TestCopyHonoursAnEndOffset(t *testing.T) {
	data := payload(500)
	p := plan.New(int64(len(data)), 100)
	bits := plan.NewBitmap(p.Chunks)
	for i := 0; i < p.Chunks; i++ {
		bits.Set(i)
	}

	var out bytes.Buffer
	n, err := Copy(context.Background(), &out, p, bits, bytes.NewReader(data), 120, 349)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n != 230 {
		t.Errorf("wrote %d bytes, want 230", n)
	}
	if !bytes.Equal(out.Bytes(), data[120:350]) {
		t.Error("output does not match the requested range")
	}
}

func TestCopyOnlyWaitsForChunksItNeeds(t *testing.T) {
	// A range request for the tail must not block on the head of the file.
	data := payload(500)
	p := plan.New(int64(len(data)), 100)
	bits := plan.NewBitmap(p.Chunks)
	bits.Set(4) // only the final chunk

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	n, err := Copy(ctx, &out, p, bits, bytes.NewReader(data), 400, 499)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n != 100 || !bytes.Equal(out.Bytes(), data[400:]) {
		t.Errorf("wrote %d bytes; output correct: %v", n, bytes.Equal(out.Bytes(), data[400:]))
	}
}

func TestCopyStopsWhenTheClientDisconnects(t *testing.T) {
	// The context is the client. When it goes away the reader must not stay
	// parked on a chunk nobody will deliver.
	data := payload(300)
	p := plan.New(int64(len(data)), 100)
	bits := plan.NewBitmap(p.Chunks)
	bits.Set(0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Copy(ctx, io.Discard, p, bits, bytes.NewReader(data), 0, int64(len(data))-1)
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Copy ignored the disconnect")
	}
}

func TestCopyRejectsAnImpossibleRange(t *testing.T) {
	data := payload(300)
	p := plan.New(int64(len(data)), 100)
	bits := plan.NewBitmap(p.Chunks)

	cases := []struct {
		name       string
		start, end int64
	}{
		{"start past the end of the file", 500, 600},
		{"end before start", 200, 100},
		{"negative start", -1, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Copy(context.Background(), io.Discard, p, bits, bytes.NewReader(data), c.start, c.end); err == nil {
				t.Error("want an error, got nil")
			}
		})
	}
}

func TestCopyClampsAnEndBeyondTheFile(t *testing.T) {
	// "bytes=400-" on a 500-byte file is legitimate and common.
	data := payload(500)
	p := plan.New(int64(len(data)), 100)
	bits := plan.NewBitmap(p.Chunks)
	for i := 0; i < p.Chunks; i++ {
		bits.Set(i)
	}

	var out bytes.Buffer
	n, err := Copy(context.Background(), &out, p, bits, bytes.NewReader(data), 400, 99999)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n != 100 {
		t.Errorf("wrote %d bytes, want 100", n)
	}
}
