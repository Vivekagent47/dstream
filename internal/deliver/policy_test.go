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
