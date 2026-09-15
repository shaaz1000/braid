package human

import (
	"testing"
	"time"
)

func TestBytes(t *testing.T) {
	cases := map[int64]string{
		0:                 "0 B",
		512:               "512 B",
		1023:              "1023 B",
		1024:              "1.0 KB",
		1536:              "1.5 KB",
		1 << 20:           "1.0 MB",
		64_900_000:        "61.9 MB",
		1 << 30:           "1.00 GB",
		(1 << 30) * 3 / 2: "1.50 GB",
		1 << 40:           "1024.00 GB",
	}
	for in, want := range cases {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestBytesHandlesNegative(t *testing.T) {
	// Never shown in practice, but a formatter that panics on odd input is a
	// bad neighbour for a progress loop.
	if got := Bytes(-1); got == "" {
		t.Error("Bytes(-1) returned empty")
	}
}

func TestRate(t *testing.T) {
	// 12.5 MB in one second is 100 Mbps, which is roughly this Mac's Wi-Fi.
	if got := Rate(12_500_000, time.Second); got != "100.0 Mbps" {
		t.Errorf("Rate = %q, want 100.0 Mbps", got)
	}
	if got := Rate(1_250_000, time.Second); got != "10.0 Mbps" {
		t.Errorf("Rate = %q, want 10.0 Mbps", got)
	}
}

func TestRateHandlesZeroElapsed(t *testing.T) {
	// The first progress callback can arrive in well under a microsecond.
	if got := Rate(1000, 0); got != "—" {
		t.Errorf("Rate with zero elapsed = %q, want an em dash", got)
	}
}

func TestDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                      "0s",
		900 * time.Millisecond: "0s",
		5 * time.Second:        "5s",
		65 * time.Second:       "1m05s",
		3600 * time.Second:     "1h00m",
		3725 * time.Second:     "1h02m",
	}
	for in, want := range cases {
		if got := Duration(in); got != want {
			t.Errorf("Duration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestETA(t *testing.T) {
	// Half of 1000 bytes done in 1s means about another second to go.
	if got := ETA(500, 1000, time.Second); got != "1s" {
		t.Errorf("ETA = %q, want 1s", got)
	}
}

func TestETAIsUnknownWithoutProgress(t *testing.T) {
	if got := ETA(0, 1000, time.Second); got != "—" {
		t.Errorf("ETA with nothing done = %q, want an em dash", got)
	}
}

func TestETAIsZeroWhenComplete(t *testing.T) {
	if got := ETA(1000, 1000, time.Second); got != "0s" {
		t.Errorf("ETA when complete = %q, want 0s", got)
	}
}

func TestBar(t *testing.T) {
	if got := Bar(0, 10); got != "░░░░░░░░░░" {
		t.Errorf("Bar(0,10) = %q", got)
	}
	if got := Bar(10, 10); got != "██████████" {
		t.Errorf("Bar(10,10) = %q", got)
	}
	if got := Bar(5, 10); got != "█████░░░░░" {
		t.Errorf("Bar(5,10) = %q", got)
	}
}

func TestBarClampsOutOfRange(t *testing.T) {
	// Tail stealing can momentarily report more done than expected; the bar
	// must not overflow its width or go negative.
	if got := Bar(99, 10); len([]rune(got)) != 10 {
		t.Errorf("Bar(99,10) has %d runes, want 10", len([]rune(got)))
	}
	if got := Bar(-5, 10); len([]rune(got)) != 10 {
		t.Errorf("Bar(-5,10) has %d runes, want 10", len([]rune(got)))
	}
}

func TestBarWithZeroTotalDoesNotDivideByZero(t *testing.T) {
	if got := Bar(0, 0); got != "" {
		t.Errorf("Bar(0,0) = %q, want empty", got)
	}
}
