package deliver

import (
	"encoding/json"
	"math"
	"math/rand"
	"time"

	"github.com/Vivekagent47/dstream/internal/store"
)

// RetryDelay computes the next retry delay given a connection's policy and
// the current attempt number (1-based: attempt 1 = the first try).
func RetryDelay(c store.Connection, attempt int) time.Duration {
	base := time.Duration(c.RetryBaseMs) * time.Millisecond
	cap := time.Duration(c.RetryCapMs) * time.Millisecond

	var d time.Duration
	switch c.RetryStrategy {
	case "linear":
		// Clamp in float space: base*attempt can overflow int64 for a large
		// max_retries, wrapping negative and skipping the cap below.
		d = clampToCap(float64(base)*float64(attempt), cap)
	case "fixed":
		d = base
	case "custom":
		d = customDelay(c.CustomRetrySchedule, attempt, base)
	default: // exponential
		// 2^(attempt-1) so the first retry waits `base`. Clamp in float space —
		// float64(base)*2^n overflows int64 at high attempts, which would wrap
		// negative and, after the (now-skipped) cap check, floor to 0 → a retry
		// storm hammering the destination.
		d = clampToCap(float64(base)*math.Pow(2, float64(attempt-1)), cap)
	}

	if d > cap {
		d = cap
	}
	d = applyJitter(d, int(c.RetryJitterPct))
	if d < 0 {
		d = 0
	}
	return d
}

// clampToCap converts a nanosecond count (as float64, to survive intermediate
// overflow) to a Duration bounded to [0, cap].
func clampToCap(ns float64, cap time.Duration) time.Duration {
	if ns >= float64(cap) {
		return cap
	}
	if ns <= 0 {
		return 0
	}
	return time.Duration(ns)
}

func customDelay(raw []byte, attempt int, fallback time.Duration) time.Duration {
	if len(raw) == 0 {
		return fallback
	}
	var schedule []int64 // milliseconds
	if err := json.Unmarshal(raw, &schedule); err != nil || len(schedule) == 0 {
		return fallback
	}
	idx := attempt - 1
	if idx >= len(schedule) {
		idx = len(schedule) - 1
	}
	ms := schedule[idx]
	if ms < 0 {
		ms = 0
	}
	// Bound an operator typo so ms*time.Millisecond can't overflow Duration.
	const maxMs = int64(365 * 24 * 60 * 60 * 1000) // 1 year
	if ms > maxMs {
		ms = maxMs
	}
	return time.Duration(ms) * time.Millisecond
}

func applyJitter(d time.Duration, pct int) time.Duration {
	if pct <= 0 {
		return d
	}
	if pct > 100 {
		pct = 100
	}
	// jitter ∈ [-pct%, +pct%]
	spread := float64(d) * float64(pct) / 100.0
	delta := (rand.Float64()*2 - 1) * spread
	return d + time.Duration(delta)
}
