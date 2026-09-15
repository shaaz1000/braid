// Package human formats numbers for people watching a transfer.
package human

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// unknown is shown wherever a value cannot be computed yet, rather than a
// misleading zero.
const unknown = "—"

// Bytes formats a byte count. Precision grows with magnitude so the number
// stays informative without getting wide.
func Bytes(n int64) string {
	switch {
	case n < 0:
		return "-" + Bytes(-n)
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	}
}

// Rate formats throughput in megabits per second, the unit ISPs and speed
// tests use, so the number is comparable to what the user expects.
func Rate(bytes int64, elapsed time.Duration) string {
	if elapsed <= 0 {
		return unknown
	}
	mbps := float64(bytes) * 8 / elapsed.Seconds() / 1e6
	return fmt.Sprintf("%.1f Mbps", mbps)
}

// Duration formats a short, fixed-ish width span.
func Duration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// ETA estimates the time remaining from the rate achieved so far.
func ETA(done, total int64, elapsed time.Duration) string {
	if done >= total {
		return Duration(0)
	}
	if done <= 0 || elapsed <= 0 {
		return unknown
	}
	perByte := float64(elapsed) / float64(done)
	return Duration(time.Duration(perByte * float64(total-done)))
}

// Bar renders a fixed-width progress bar.
func Bar(done, total int) string {
	if total <= 0 {
		return ""
	}
	filled := done
	if filled < 0 {
		filled = 0
	}
	if filled > total {
		filled = total
	}
	// Scale to the requested width, which here is the total itself.
	return strings.Repeat("█", filled) + strings.Repeat("░", total-filled)
}

// DefaultInterval throttles repainting. A 4 MB chunk can land every few
// milliseconds on a fast link, and repainting that often is only flicker.
const DefaultInterval = 100 * time.Millisecond

// Painter decides when a progress line is worth redrawing.
type Painter struct {
	// Interval is the minimum gap between redraws; zero means DefaultInterval.
	Interval time.Duration

	last    time.Time
	started bool
}

// Should reports whether to redraw now. Completion always draws, so the final
// state is never lost to throttling.
func (p *Painter) Should(done, total int, now time.Time) bool {
	if total > 0 && done >= total {
		p.last, p.started = now, true
		return true
	}
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	if p.started && now.Sub(p.last) < interval {
		return false
	}
	p.last, p.started = now, true
	return true
}

// IsTerminal reports whether f is a character device, i.e. whether carriage
// returns will redraw a line rather than pile up in a log file.
func IsTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
