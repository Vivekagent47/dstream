package deliver

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

// The reaper selects across the whole events table. Everything here is scoped
// to events this test seeded: rows are backdated 50 years so they sort first
// under ORDER BY updated_at ASC, and every assertion names OUR event ids, never
// a global count (rows other packages leave behind may be claimed alongside).

func (f *fx) backdate(status string, ago string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE events SET status=$1, updated_at = now() - $2::interval WHERE id=$3`,
		status, ago, store.UUID(f.ev)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) updatedAt() time.Time {
	f.t.Helper()
	var u time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT updated_at FROM events WHERE id=$1`, store.UUID(f.ev)).Scan(&u); err != nil {
		f.t.Fatal(err)
	}
	return u
}

// share points g at f's queue so one handler's reaper output is visible to both.
func (g *fx) share(f *fx) { g.dq = f.dq; g.h.Queue = f.dq }

func (f *fx) pendingFor(ev *fx) []string {
	items, _, err := f.dq.Items(context.Background(), "pending", ev.org.String(), 50)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.EventID.String())
	}
	return out
}

func TestReapStuckEvents(t *testing.T) {
	const ancient = "50 years"
	mk := func(o seedOpt) *fx { o.url = strp("http://x.invalid"); return seed(t, o) }
	stuckQueued := mk(seedOpt{strategy: "linear", baseMs: 123, capMs: 4567, custom: []byte(`[5,6]`), jitter: 37})
	stuckCLI := seed(t, seedOpt{destType: "cli"})
	recent := mk(seedOpt{})
	httpInFlight := mk(seedOpt{})
	delivered := mk(seedOpt{})
	for _, g := range []*fx{stuckCLI, recent, httpInFlight, delivered} {
		g.share(stuckQueued)
	}
	stuckQueued.backdate("queued", ancient)
	stuckCLI.backdate("in_flight", ancient)
	recent.backdate("queued", "1 minute") // far inside the 15m window
	httpInFlight.backdate("in_flight", ancient)
	delivered.backdate("delivered", ancient)
	recentBefore := recent.updatedAt()

	n, err := stuckQueued.h.ReapStuckEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Fatalf("reaped %d, want at least our 2 stuck events", n)
	}
	q := stuckQueued
	for name, g := range map[string]*fx{"queued": stuckQueued, "cli in_flight": stuckCLI} {
		if got := q.pendingFor(g); len(got) != 1 || got[0] != g.ev.String() {
			t.Errorf("%s: pending lane for its org = %v, want [%s]", name, got, g.ev)
		}
		g.wantState("queued", 0)
		if age := time.Since(g.updatedAt()); age > time.Minute {
			t.Errorf("%s: updated_at still %v old; claim must bump it", name, age)
		}
		if _, _, nr := g.state(); nr == nil {
			t.Errorf("%s: next_retry_at not set by the claim", name)
		}
	}
	items, _, _ := q.dq.Items(context.Background(), "pending", stuckQueued.org.String(), 5)
	if len(items) != 1 || items[0].Attempt != 0 {
		t.Fatalf("items = %+v", items)
	}
	// Payload carries the connection's retry policy so backoff needs no DB read.
	var p dqueue.Payload
	if err := json.Unmarshal([]byte(items[0].Raw), &p); err != nil {
		t.Fatal(err)
	}
	if p.RetryJitterPct != 37 || !jsonEq(p.CustomRetrySchedule, "[5,6]") ||
		p.RetryStrategy != "linear" || p.RetryBaseMs != 123 || p.RetryCapMs != 4567 ||
		p.OrgID != stuckQueued.org || p.EventID != stuckQueued.ev || p.EnqueuedAt == 0 {
		t.Errorf("re-enqueued payload = %+v", p)
	}

	// Not reaped: inside the window, HTTP in_flight (owned by the dqueue), terminal.
	if got := q.pendingFor(recent); len(got) != 0 {
		t.Errorf("recent queued event reaped: %v", got)
	}
	recent.wantState("queued", 0)
	if !recent.updatedAt().Equal(recentBefore) {
		t.Error("recent event's updated_at was touched")
	}
	if got := q.pendingFor(httpInFlight); len(got) != 0 {
		t.Errorf("http in_flight reaped (would double-deliver): %v", got)
	}
	httpInFlight.wantState("in_flight", 0)
	if got := q.pendingFor(delivered); len(got) != 0 {
		t.Errorf("terminal event reaped: %v", got)
	}
	delivered.wantState("delivered", 0)
}

func TestReapStuckEvents_Errors(t *testing.T) {
	ctx := context.Background()
	t.Run("claim query fails", func(t *testing.T) {
		f := seed(t, seedOpt{url: strp("http://x.invalid")})
		f.failQuery("ClaimStuckEvents")
		n, err := f.h.ReapStuckEvents(ctx)
		if n != 0 || err != errInjected {
			t.Fatalf("= %d, %v", n, err)
		}
	})
	t.Run("connection lookup fails", func(t *testing.T) {
		f := seed(t, seedOpt{url: strp("http://x.invalid")})
		f.backdate("queued", "50 years")
		f.failQuery("GetConnectionByID")
		n, err := f.h.ReapStuckEvents(ctx)
		if err != nil || n != 0 {
			t.Fatalf("= %d, %v (every candidate must be skipped)", n, err)
		}
		if !strings.Contains(f.log.String(), "reaper: load connection") || !strings.Contains(f.log.String(), f.ev.String()) {
			t.Errorf("not logged: %s", f.log.String())
		}
		if got := f.pendingFor(f); len(got) != 0 {
			t.Errorf("enqueued despite the failed lookup: %v", got)
		}
	})
	t.Run("enqueue fails", func(t *testing.T) {
		f := seed(t, seedOpt{url: strp("http://x.invalid")})
		f.backdate("queued", "50 years")
		f.h.Queue = deadQueue(t)
		n, err := f.h.ReapStuckEvents(ctx)
		if err != nil || n != 0 {
			t.Fatalf("= %d, %v", n, err)
		}
		if !strings.Contains(f.log.String(), "reaper: re-enqueue") || !strings.Contains(f.log.String(), "redis: client is closed") {
			t.Errorf("not logged: %s", f.log.String())
		}
	})
}

func TestRunReaper(t *testing.T) {
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	stop := func(cancel context.CancelFunc, done chan struct{}) {
		t.Helper()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("reaper did not stop after ctx cancel")
		}
	}

	t.Run("ticks sweep and re-queue", func(t *testing.T) {
		f := seed(t, seedOpt{url: strp("http://x.invalid")})
		f.backdate("queued", "50 years")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { f.h.runReaper(ctx, 10*time.Millisecond); close(done) }()
		waitFor("reaper to re-queue the stuck event", func() bool { return len(f.pendingFor(f)) == 1 })
		stop(cancel, done)
		if got := f.pendingFor(f); len(got) != 1 || got[0] != f.ev.String() {
			t.Fatalf("pending = %v", got)
		}
	})
	t.Run("sweep error is logged and the loop survives", func(t *testing.T) {
		f := seed(t, seedOpt{url: strp("http://x.invalid")})
		f.failQuery("ClaimStuckEvents")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { f.h.runReaper(ctx, 10*time.Millisecond); close(done) }()
		waitFor("two failed sweeps", func() bool {
			return strings.Count(f.log.String(), "reaper: sweep failed") >= 2
		})
		stop(cancel, done)
	})
	t.Run("exported entry point stops on cancel", func(t *testing.T) {
		f := seed(t, seedOpt{url: strp("http://x.invalid")})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { f.h.RunReaper(ctx); close(done) }()
		stop(cancel, done)
	})
}

// jsonEq compares JSON semantically (jsonb normalises whitespace on storage).
func jsonEq(got []byte, want string) bool {
	var a, b any
	return json.Unmarshal(got, &a) == nil && json.Unmarshal([]byte(want), &b) == nil && reflect.DeepEqual(a, b)
}
