package server

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultCacheLimit is how much disk the streamed-file cache may use.
//
// The cache exists so that asking for the same URL twice costs no mobile data
// the second time. Kept without a bound it quietly fills the disk instead.
const DefaultCacheLimit int64 = 5 << 30 // 5 GiB

// evictCache deletes the least recently used files until the directory fits
// within limit, and reports how many bytes it freed.
//
// A resume sidecar is removed with the file it describes: on its own it would
// tell the next attempt it has progress that is no longer on disk.
func evictCache(dir string, limit int64) (int64, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	type item struct {
		path    string
		size    int64
		modTime int64
	}
	var files []item
	var total int64

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Sidecars are accounted with their file, not on their own, so a tiny
		// sidecar is never chosen as the thing to evict.
		if strings.HasSuffix(e.Name(), sidecarSuffix) {
			total += info.Size()
			continue
		}
		files = append(files, item{
			path:    filepath.Join(dir, e.Name()),
			size:    info.Size(),
			modTime: info.ModTime().UnixNano(),
		})
		total += info.Size()
	}

	if total <= limit {
		return 0, nil
	}

	// Oldest first.
	sort.Slice(files, func(i, j int) bool { return files[i].modTime < files[j].modTime })

	var freed int64
	for _, f := range files {
		if total-freed <= limit {
			break
		}
		if err := os.Remove(f.path); err != nil {
			continue
		}
		freed += f.size

		// Take the sidecar with it.
		sidecar := f.path + sidecarSuffix
		if info, err := os.Stat(sidecar); err == nil {
			if os.Remove(sidecar) == nil {
				freed += info.Size()
			}
		}
	}
	return freed, nil
}

// sidecarSuffix mirrors the resume file the transfer package writes.
const sidecarSuffix = ".braid"
