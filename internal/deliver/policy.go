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
		d = base * time.Duration(attempt)
	case "fixed":
		d = base
	case "custom":
		d = customDelay(c.CustomRetrySchedule, attempt, base)
	default: // exponential
		// 2^(attempt-1) so the first retry waits `base`.
		exp := math.Pow(2, float64(attempt-1))
		d = time.Duration(float64(base) * exp)
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
	return time.Duration(schedule[idx]) * time.Millisecond
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
