package scheduler

import (
	"testing"
	"time"
)

// An unsatisfied torrent only frees its slot after 72 hours of seeding, so
// a run that finds the account at target tells us there is nothing to do
// for hours. These tests pin the resulting backoff curve.
func TestNextDelayBacksOffWhenIdle(t *testing.T) {
	const base = 30 // minutes, the default

	tests := []struct {
		name string
		idle int
		want time.Duration
	}{
		{"first run", 0, 30 * time.Minute},
		{"one idle run holds the base interval", 1, 30 * time.Minute},
		{"two idle runs still hold", 2, 30 * time.Minute},
		{"threshold reached, still base", 3, 30 * time.Minute},
		{"first backoff", 4, time.Hour},
		{"second backoff", 5, 2 * time.Hour},
		{"third backoff", 6, 4 * time.Hour},
		{"capped", 7, 6 * time.Hour},
		{"stays capped", 20, 6 * time.Hour},
		{"stays capped after a very long idle spell", 1000, 6 * time.Hour},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextDelay(base, tc.idle); got != tc.want {
				t.Errorf("nextDelay(%d, %d) = %v, want %v", base, tc.idle, got, tc.want)
			}
		})
	}
}

// The live instance recorded 48 consecutive idle runs over 24 hours at a
// fixed 30-minute interval. Check the backoff would have collapsed that
// into a handful of runs instead.
func TestNextDelayCollapsesALongIdleSpell(t *testing.T) {
	const base = 30
	var elapsed time.Duration
	runs := 0
	for idle := 0; elapsed < 24*time.Hour; idle++ {
		elapsed += nextDelay(base, idle)
		runs++
	}
	if runs > 10 {
		t.Errorf("covering 24h of idle took %d runs, want far fewer than the 48 a fixed interval needed", runs)
	}
	t.Logf("24h of idle covered in %d runs (fixed interval needed 48)", runs)
}

// Backing off must never stretch past the point where packrat would miss
// slots opening up. The 72-hour seeding window is the natural bound.
func TestNextDelayNeverExceedsCap(t *testing.T) {
	for _, base := range []int{1, 5, 30, 120, 600} {
		for idle := 0; idle < 50; idle++ {
			got := nextDelay(base, idle)
			if got > maxIdleDelay && got != time.Duration(base)*time.Minute {
				t.Fatalf("nextDelay(%d, %d) = %v, exceeds the %v cap", base, idle, got, maxIdleDelay)
			}
		}
	}
}

// A base interval longer than the cap is the user's explicit choice and
// must be honoured rather than silently shortened.
func TestNextDelayRespectsBaseLongerThanCap(t *testing.T) {
	const base = 12 * 60 // 12 hours in minutes
	if got := nextDelay(base, 0); got != 12*time.Hour {
		t.Errorf("nextDelay(%d, 0) = %v, want 12h: a long configured interval is not shortened", base, got)
	}
	// Once backing off, the cap applies rather than doubling further.
	if got := nextDelay(base, 10); got != maxIdleDelay {
		t.Errorf("nextDelay(%d, 10) = %v, want the %v cap", base, got, maxIdleDelay)
	}
}

// A zero or negative configured interval would otherwise mean a run loop
// with no delay at all, hammering MAM.
func TestNextDelayGuardsAgainstZeroInterval(t *testing.T) {
	for _, base := range []int{0, -1, -600} {
		if got := nextDelay(base, 0); got != 30*time.Minute {
			t.Errorf("nextDelay(%d, 0) = %v, want the 30m fallback", base, got)
		}
	}
}
