package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, dir, name string, size int, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCacheEvictsTheOldestFilesFirst(t *testing.T) {
	// Streamed files are kept so a repeat request costs no mobile data, but
	// kept forever they quietly fill the disk.
	dir := t.TempDir()
	oldest := write(t, dir, "oldest.bin", 400, 72*time.Hour)
	middle := write(t, dir, "middle.bin", 400, 24*time.Hour)
	newest := write(t, dir, "newest.bin", 400, time.Minute)

	freed, err := evictCache(dir, 900)
	if err != nil {
		t.Fatalf("evictCache: %v", err)
	}
	if freed == 0 {
		t.Error("nothing was freed despite being over the limit")
	}

	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Error("the oldest file should have gone first")
	}
	if _, err := os.Stat(newest); err != nil {
		t.Error("the newest file should have been kept")
	}
	_ = middle // may go either way depending on how much had to be freed
}

func TestCacheLeavesEverythingAloneWhenUnderTheLimit(t *testing.T) {
	dir := t.TempDir()
	keep := write(t, dir, "keep.bin", 100, time.Hour)

	freed, err := evictCache(dir, 10_000)
	if err != nil {
		t.Fatalf("evictCache: %v", err)
	}
	if freed != 0 {
		t.Errorf("freed %d bytes while under the limit", freed)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("a file was deleted despite being under the limit")
	}
}

func TestCacheRemovesASidecarWithItsFile(t *testing.T) {
	// A resume sidecar without its file is useless and would make the next
	// attempt think it had progress it does not have.
	dir := t.TempDir()
	write(t, dir, "big.bin", 900, 48*time.Hour)
	sidecar := write(t, dir, "big.bin.braid", 20, 48*time.Hour)
	write(t, dir, "recent.bin", 100, time.Minute)

	if _, err := evictCache(dir, 500); err != nil {
		t.Fatalf("evictCache: %v", err)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Error("the sidecar outlived the file it describes")
	}
}

func TestCacheToleratesAMissingDirectory(t *testing.T) {
	if _, err := evictCache(filepath.Join(t.TempDir(), "absent"), 100); err != nil {
		t.Errorf("a missing cache directory should not be an error: %v", err)
	}
}
