// Package stream serves a file in order while its chunks are still arriving
// out of order.
//
// This is what lets any device on the network benefit from bonding without
// installing anything: point a player or a browser at an endpoint, and it
// receives an ordinary sequential response whose bytes are being fetched
// concurrently over every uplink behind the scenes.
package stream

import (
	"context"
	"fmt"
	"io"

	"braid/internal/plan"
)

// bufSize is the slab moved per write. Large enough to keep syscalls down,
// small enough that a player receives its first bytes promptly.
const bufSize = 256 << 10

// Copy writes bytes [start, end] of the file to w, in order, blocking on each
// chunk until it lands. It returns when the range is served, when ctx ends, or
// on the first write error.
//
// end beyond the last byte is clamped, since "bytes=400-" is ordinary.
func Copy(ctx context.Context, w io.Writer, p plan.Plan, bits *plan.Bitmap, src io.ReaderAt, start, end int64) (int64, error) {
	if start < 0 {
		return 0, fmt.Errorf("start offset %d is negative", start)
	}
	if start >= p.Size {
		return 0, fmt.Errorf("start offset %d is past the end of a %d byte file", start, p.Size)
	}
	if end >= p.Size {
		end = p.Size - 1
	}
	if end < start {
		return 0, fmt.Errorf("range %d-%d ends before it starts", start, end)
	}

	buf := make([]byte, bufSize)
	var written int64

	for pos := start; pos <= end; {
		chunk := int(pos / p.ChunkSize)
		// Park until this chunk exists on disk. Only the chunks actually
		// covered by the requested range are waited for, so a request for the
		// tail of a file does not block on its head.
		if err := bits.WaitFor(ctx, chunk); err != nil {
			return written, err
		}

		_, chunkEnd := p.Range(chunk)
		last := chunkEnd
		if last > end {
			last = end
		}

		for pos <= last {
			slab := buf
			if n := last - pos + 1; n < int64(len(slab)) {
				slab = buf[:n]
			}
			read, err := src.ReadAt(slab, pos)
			if read > 0 {
				n, werr := w.Write(slab[:read])
				written += int64(n)
				pos += int64(n)
				if werr != nil {
					return written, werr
				}
			}
			if err != nil && read == 0 {
				return written, fmt.Errorf("reading at offset %d: %w", pos, err)
			}
			// A short read inside a landed chunk means the file is not yet what
			// the bitmap claims; stopping is safer than looping forever.
			if read == 0 {
				return written, fmt.Errorf("no bytes available at offset %d despite chunk %d being complete", pos, chunk)
			}
		}
	}
	return written, nil
}
