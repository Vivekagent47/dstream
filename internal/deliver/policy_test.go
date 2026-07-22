package deliver

import (
	"testing"
	"time"

	"github.com/Vivekagent47/dstream/internal/store"
)

// conn builds a Connection with jitter disabled so delays are deterministic.
func conn(strategy string, baseMs, capMs int32) store.Connection {
	return store.Connection{
		RetryStrategy:  strategy,
		RetryBaseMs:    baseMs,
		RetryCapMs:     capMs,
		RetryJitterPct: 0,
	}
}

func TestRetryDelay_ExponentialClampsToCap(t *testing.T) {
	c := conn("exponential", 30000, 3600000) // base 30s, cap 1h
	// A high attempt would overflow float64(base)*2^(attempt-1) into int64 and
	// wrap to a near-zero delay without the clamp. Must return the cap instead.
	for _, attempt := range []int{40, 100, 1000} {
		d := RetryDelay(c, attempt)
		if d != time.Hour {
			t.Errorf("attempt %d: got %v, want cap %v", attempt, d, time.Hour)
		}
	}
	// Sanity: early attempts still grow normally and stay >= base.
	if d := RetryDelay(c, 1); d != 30*time.Second {
		t.Errorf("attempt 1: got %v, want 30s", d)
	}
}

func TestRetryDelay_LinearClampsToCap(t *testing.T) {
	c := conn("linear", 1_000_000_000, 3600000) // absurd base to force overflow at high attempt
	if d := RetryDelay(c, 1_000_000); d != time.Hour {
		t.Errorf("linear overflow: got %v, want cap %v", d, time.Hour)
	}
}

func TestRetryDelay_NeverNegative(t *testing.T) {
	for _, s := range []string{"exponential", "linear", "fixed", "custom"} {
		if d := RetryDelay(conn(s, 30000, 3600000), 5000); d < 0 {
			t.Errorf("strategy %s produced negative delay %v", s, d)
		}
	}
}

// TestRetryDelay_JitterNeverExceedsCap: jitter is applied after the cap clamp,
// so without a re-clamp positive jitter (up to +100% at pct=100) pushes the
// delay above cap. base==cap makes d start exactly at the cap where overshoot
// is most likely; every sample must still be <= cap.
func TestRetryDelay_JitterNeverExceedsCap(t *testing.T) {
	c := store.Connection{
		RetryStrategy:  "exponential",
		RetryBaseMs:    1000,
		RetryCapMs:     1000, // base == cap → d starts at the cap
		RetryJitterPct: 100,  // full jitter would reach 2x cap without the re-clamp
	}
	for i := 0; i < 1000; i++ {
		if d := RetryDelay(c, 1); d > time.Second {
			t.Fatalf("jitter pushed delay %v above cap %v", d, time.Second)
		}
	}
}

// TestRetryDelay_ZeroConfigFloored: base/cap = 0 (the schema has no CHECK > 0)
// must not collapse backoff to a zero/negative delay — the floors keep it > 0.
func TestRetryDelay_ZeroConfigFloored(t *testing.T) {
	for _, s := range []string{"exponential", "linear", "fixed", "custom"} {
		for _, attempt := range []int{1, 5, 100} {
			if d := RetryDelay(conn(s, 0, 0), attempt); d <= 0 {
				t.Errorf("strategy %s attempt %d: got %v, want > 0 (floored)", s, attempt, d)
			}
		}
	}
}

// TestCustomDelay_AttemptBelowOneNoPanic: attempt<1 makes idx = attempt-1 < 0,
// which would panic on schedule[-1] without the guard. It must clamp to idx 0.
func TestCustomDelay_AttemptBelowOneNoPanic(t *testing.T) {
	schedule := []byte(`[1000, 2000, 3000]`) // ms
	for _, attempt := range []int{0, -1, -100} {
		if d := customDelay(schedule, attempt, 5*time.Second); d != time.Second {
			t.Errorf("attempt %d: got %v, want 1s (guarded to schedule[0])", attempt, d)
		}
	}
}
