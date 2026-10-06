package deliver

import (
	"fmt"
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

// TestRetryDelay_Arithmetic pins the exact delay of every strategy (jitter off).
func TestRetryDelay_Arithmetic(t *testing.T) {
	ms := func(n int64) time.Duration { return time.Duration(n) * time.Millisecond }
	custom := func(sched string) store.Connection {
		c := conn("custom", 500, 60000)
		c.CustomRetrySchedule = []byte(sched)
		return c
	}
	cases := []struct {
		name    string
		c       store.Connection
		attempt int
		want    time.Duration
	}{
		{"exp a1 = base", conn("exponential", 1000, 3600000), 1, ms(1000)},
		{"exp a2 = 2*base", conn("exponential", 1000, 3600000), 2, ms(2000)},
		{"exp a5 = 16*base", conn("exponential", 1000, 3600000), 5, ms(16000)},
		{"exp just under cap", conn("exponential", 1000, 20000), 5, ms(16000)},
		{"exp past cap", conn("exponential", 1000, 20000), 6, ms(20000)},
		{"unknown strategy falls to exponential", conn("bogus", 1000, 3600000), 3, ms(4000)},
		{"linear a1", conn("linear", 1000, 3600000), 1, ms(1000)},
		{"linear a4", conn("linear", 1000, 3600000), 4, ms(4000)},
		{"linear exactly at cap", conn("linear", 1000, 4000), 4, ms(4000)},
		{"linear past cap", conn("linear", 1000, 4000), 5, ms(4000)},
		{"fixed ignores attempt", conn("fixed", 700, 3600000), 9, ms(700)},
		{"fixed above cap clamps", conn("fixed", 9000, 5000), 1, ms(5000)},
		{"custom a1", custom(`[100,200,300]`), 1, ms(100)},
		{"custom a3", custom(`[100,200,300]`), 3, ms(300)},
		{"custom shorter than attempt reuses last", custom(`[100,200,300]`), 9, ms(300)},
		{"custom above cap clamps", custom(`[999999]`), 1, ms(60000)},
		{"custom negative entry is zero", custom(`[-5]`), 1, 0},
		{"custom empty schedule falls back to base", custom(`[]`), 4, ms(500)},
		{"custom malformed falls back to base", custom(`{nope`), 4, ms(500)},
		{"custom missing falls back to base", conn("custom", 500, 60000), 4, ms(500)},
		{"zero base floors to 1s", conn("fixed", 0, 3600000), 1, time.Second},
		{"zero cap floors to 1h", conn("fixed", 2*3600*1000, 0), 1, time.Hour},
	}
	for _, c := range cases {
		if got := RetryDelay(c.c, c.attempt); got != c.want {
			t.Errorf("%s: RetryDelay = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestClampToCap(t *testing.T) {
	cp := 10 * time.Second
	cases := []struct {
		ns   float64
		want time.Duration
	}{
		{float64(cp) - 1, cp - 1},
		{float64(cp), cp},
		{float64(cp) + 1e3, cp},
		{1e300, cp},
		{0, 0},
		{-5, 0},
		{1, 1},
	}
	for _, c := range cases {
		if got := clampToCap(c.ns, cp); got != c.want {
			t.Errorf("clampToCap(%v) = %v, want %v", c.ns, got, c.want)
		}
	}
}

func TestCustomDelay(t *testing.T) {
	fb := 7 * time.Second
	const year = 365 * 24 * time.Hour
	cases := []struct {
		raw     string
		attempt int
		want    time.Duration
	}{
		{``, 1, fb},
		{`not json`, 1, fb},
		{`[]`, 1, fb},
		{`[10,20]`, 1, 10 * time.Millisecond},
		{`[10,20]`, 2, 20 * time.Millisecond},
		{`[10,20]`, 3, 20 * time.Millisecond}, // schedule shorter than attempt
		{`[10,20]`, 1000, 20 * time.Millisecond},
		{`[-1]`, 1, 0},
		{`[999999999999999]`, 1, year}, // operator typo bounded to one year
	}
	for _, c := range cases {
		if got := customDelay([]byte(c.raw), c.attempt, fb); got != c.want {
			t.Errorf("customDelay(%q,%d) = %v, want %v", c.raw, c.attempt, got, c.want)
		}
	}
}

// TestApplyJitter asserts the bounds (never a specific random value): the
// result stays within +/-pct of d, pct is capped at 100, and it really varies.
func TestApplyJitter(t *testing.T) {
	d := 10 * time.Second
	for _, pct := range []int{0, -5} {
		if got := applyJitter(d, pct); got != d {
			t.Errorf("pct %d must be a no-op, got %v", pct, got)
		}
	}
	for _, c := range []struct{ pct, eff int }{{20, 20}, {100, 100}, {500, 100}} {
		lo := d - d*time.Duration(c.eff)/100
		hi := d + d*time.Duration(c.eff)/100
		seen := map[time.Duration]bool{}
		for i := 0; i < 2000; i++ {
			got := applyJitter(d, c.pct)
			if got < lo || got > hi {
				t.Fatalf("pct %d: %v outside [%v, %v]", c.pct, got, lo, hi)
			}
			seen[got] = true
		}
		if len(seen) < 100 {
			t.Errorf("pct %d: only %d distinct values over 2000 draws; jitter not applied", c.pct, len(seen))
		}
	}
}

// RetryDelay with jitter stays inside [d-pct, d+pct] and never above the cap.
func TestRetryDelay_JitterBounds(t *testing.T) {
	c := conn("fixed", 10000, 3600000)
	c.RetryJitterPct = 25
	seen := map[time.Duration]bool{}
	for i := 0; i < 500; i++ {
		d := RetryDelay(c, 1)
		if d < 7500*time.Millisecond || d > 12500*time.Millisecond {
			t.Fatal(fmt.Sprintf("jittered delay %v outside +/-25%% of 10s", d))
		}
		seen[d] = true
	}
	// Identical backoffs across a fleet (thundering herd) is what jitter exists
	// to prevent: RetryDelay must actually apply it, not merely stay in range.
	if len(seen) <= 50 {
		t.Fatalf("only %d distinct delays over 500 draws; jitter is not wired into RetryDelay", len(seen))
	}
}
