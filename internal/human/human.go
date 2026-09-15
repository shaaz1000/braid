// Package human formats numbers for people watching a transfer.
package human

import (
	"fmt"
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
