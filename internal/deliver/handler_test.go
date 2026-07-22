package deliver

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
)

// TestIsTerminalStatus guards the delivery idempotency check (audit #6): a
// re-injected event in an end state must be recognised as terminal so Process
// Acks-and-skips instead of re-firing it. Non-terminal states must fall through.
func TestIsTerminalStatus(t *testing.T) {
	terminal := []string{"delivered", "failed", "discarded", "dead"}
	for _, s := range terminal {
		if !isTerminalStatus(s) {
			t.Errorf("status %q should be terminal", s)
		}
	}
	nonTerminal := []string{"queued", "in_flight", "paused", "", "unknown"}
	for _, s := range nonTerminal {
		if isTerminalStatus(s) {
			t.Errorf("status %q should NOT be terminal", s)
		}
	}
}

// TestDeadLetterStopsRecoverLoop is the regression guard for audit #5: on a
// process panic the worker dead-letters the leased member. Dead-lettering must
// remove it from dq:processing so the lease recoverer can never re-inject it
// into an endless panic loop. Redis-gated (skips when none is reachable), same
// pattern as dqueue_test.
func TestDeadLetterStopsRecoverLoop(t *testing.T) {
	addr := os.Getenv("DSTREAM_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skip("no redis at " + addr)
	}
	dq := dqueue.NewClient(rdb).WithPrefix("dqdltest")
	if keys, _ := rdb.Keys(context.Background(), "dqdltest:*").Result(); len(keys) > 0 {
		rdb.Del(context.Background(), keys...)
	}

	bg := context.Background()
	if err := dq.Enqueue(bg, dqueue.Payload{EventID: uuid.New(), OrgID: uuid.New()}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	raw, _, ok, err := dq.FairPick(bg, 60000)
	if err != nil || !ok {
		t.Fatalf("fairpick: ok=%v err=%v", ok, err)
	}
	// The panic path: dead-letter the leased member.
	if err := dq.DeadLetter(bg, raw); err != nil {
		t.Fatalf("deadletter: %v", err)
	}
	// Recover far in the future must find nothing to reinject — the member is no
	// longer leased in dq:processing, so no poison-pill loop.
	n, err := dq.Recover(bg, time.Now().Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 0 {
		t.Errorf("dead-lettered member was re-injected (%d recovered); poison-pill loop not broken", n)
	}
}

// TestInflightReleaseUsesBackgroundCtx is the regression guard for audit #13:
// the inflight-slot release (Decr) must run on a background ctx. go-redis
// rejects commands on a done ctx, so releasing on the cancelled delivery ctx
// (worker shutdown / Do() returning context.Canceled) would leak the slot until
// its lease TTL. Redis-gated (skips when none is reachable).
func TestInflightReleaseUsesBackgroundCtx(t *testing.T) {
	addr := os.Getenv("DSTREAM_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ping, cancelPing := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelPing()
	if err := rdb.Ping(ping).Err(); err != nil {
		t.Skip("no redis at " + addr)
	}
	key := "inflight:test:" + uuid.NewString()
	defer rdb.Del(context.Background(), key)

	// Acquire a slot on a live delivery ctx (the acquire side stays on it).
	reqCtx, cancel := context.WithCancel(context.Background())
	if _, err := inflightIncrScript.Run(reqCtx, rdb, []string{key}, 150).Int64(); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Delivery ctx is now cancelled (shutdown / Do() context.Canceled).
	cancel()

	// A release on the cancelled ctx is rejected — this is the slot leak the fix
	// avoids; if it were allowed, the release below would drive the counter to -1.
	if err := rdb.Decr(reqCtx, key).Err(); err == nil {
		t.Fatal("expected Decr on cancelled ctx to be rejected (demonstrates the leak)")
	}
	// The fix: release on a background ctx always executes.
	if err := rdb.Decr(context.Background(), key).Err(); err != nil {
		t.Fatalf("background release: %v", err)
	}
	n, err := rdb.Get(context.Background(), key).Int64()
	if err != nil {
		t.Fatalf("get counter: %v", err)
	}
	if n != 0 {
		t.Errorf("slot leaked: counter = %d, want 0", n)
	}
}
