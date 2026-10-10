package dqueue

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func testClient(t *testing.T) *Client {
	t.Helper()
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
	c := NewClient(rdb).WithPrefix("dqtest")
	// clean any leftovers from a prior run
	keys, _ := rdb.Keys(context.Background(), "dqtest:*").Result()
	if len(keys) > 0 {
		rdb.Del(context.Background(), keys...)
	}
	return c
}

func TestFairPickRoundRobin(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	orgs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	// org[0] gets 3 events, org[1] gets 2, org[2] gets 1 — enqueued grouped.
	counts := []int{3, 2, 1}
	for i, org := range orgs {
		for j := 0; j < counts[i]; j++ {
			if err := c.Enqueue(ctx, Payload{EventID: uuid.New(), OrgID: org}); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		}
	}
	// Draining must round-robin across orgs, NOT drain org[0] first.
	var order []uuid.UUID
	for {
		_, p, ok, err := c.FairPick(ctx, 60000)
		if err != nil {
			t.Fatalf("fairpick: %v", err)
		}
		if !ok {
			break
		}
		order = append(order, p.OrgID)
	}
	if len(order) != 6 {
		t.Fatalf("drained %d events, want 6", len(order))
	}
	// First 3 picks must be the 3 distinct orgs (one turn each), not org0,org0,org0.
	first3 := map[uuid.UUID]bool{order[0]: true, order[1]: true, order[2]: true}
	if len(first3) != 3 {
		t.Errorf("first 3 picks not round-robin across orgs: %v", order[:3])
	}
}

// TestFairPickFencingToken guards the lease fencing fix: when a stalled worker's
// lease is recovered and the event is re-leased to another worker, the stale
// worker's Ack (on its old handle) must NOT remove the new lease. Before the fix
// the processing member was the raw payload, identical across re-leases, so the
// stale Ack removed the live lease — a lost message on a later crash.
func TestFairPickFencingToken(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	org := uuid.New()
	if err := c.Enqueue(ctx, Payload{EventID: uuid.New(), OrgID: org}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Worker A picks with a tiny lease, then stalls.
	rawA, _, ok, err := c.FairPick(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("pick A: ok=%v err=%v", ok, err)
	}

	// A's lease expires: recover it to scheduled, then promote back to pending.
	now := time.Now().UnixMilli()
	if _, err := c.Recover(ctx, now+1000); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if _, err := c.PromoteDue(ctx, now+2000, 100); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// Worker B re-leases the same event — a fresh lease under a new token.
	rawB, _, ok, err := c.FairPick(ctx, 60000)
	if err != nil || !ok {
		t.Fatalf("pick B: ok=%v err=%v", ok, err)
	}
	if rawA == rawB {
		t.Fatal("re-lease produced an identical member — no fencing token")
	}

	// The stale worker A finishes and Acks its OLD handle; B's live lease stays.
	if err := c.Ack(ctx, rawA); err != nil {
		t.Fatalf("stale ack: %v", err)
	}
	st, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Processing != 1 {
		t.Fatalf("stale Ack removed the live lease: processing=%d, want 1", st.Processing)
	}

	// B acks its own handle → processing drains.
	if err := c.Ack(ctx, rawB); err != nil {
		t.Fatalf("ack B: %v", err)
	}
	if st, _ := c.Stats(ctx); st.Processing != 0 {
		t.Fatalf("B Ack did not drain processing: got %d, want 0", st.Processing)
	}
}

func TestPayloadEmailRoundTrip(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if err := c.Enqueue(ctx, Payload{Kind: "email", OrgID: uuid.Nil, Data: []byte(`{"template":"magic_link"}`)}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, p, ok, err := c.FairPick(ctx, 10000)
	if err != nil || !ok {
		t.Fatalf("fairpick ok=%v err=%v", ok, err)
	}
	if p.Kind != "email" {
		t.Fatalf("kind=%q, want email", p.Kind)
	}
	if string(p.Data) != `{"template":"magic_link"}` {
		t.Fatalf("data=%s", p.Data)
	}
}

// TestDeadLetterCap dead-letters more entries than deadListCap and asserts the
// dead list is LTRIM-bounded to the cap (not growing without bound).
func TestDeadLetterCap(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	for i := 0; i < deadListCap+50; i++ {
		if err := c.DeadLetter(ctx, "evt-"+strconv.Itoa(i)); err != nil {
			t.Fatalf("deadletter %d: %v", i, err)
		}
	}
	n, err := c.rdb.LLen(ctx, c.prefix+":dead").Result()
	if err != nil {
		t.Fatalf("llen: %v", err)
	}
	if n != int64(deadListCap) {
		t.Errorf("dead list len = %d, want cap %d", n, deadListCap)
	}
}
