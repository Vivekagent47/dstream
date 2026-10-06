package dqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// freshClient is a queue on a private keyspace prefix of the real Redis. Only
// that prefix is removed on cleanup; nothing else on the server is touched.
func freshClient(t *testing.T) *Client {
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
	prefix := "dqlife-" + uuid.NewString()[:8]
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+":*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
		_ = rdb.Close()
	})
	return NewClient(rdb).WithPrefix(prefix)
}

func (c *Client) mustStats(t *testing.T) Stats {
	t.Helper()
	s, err := c.Stats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	return s
}

func (c *Client) mustPick(t *testing.T, lease int64) (string, Payload) {
	t.Helper()
	raw, p, ok, err := c.FairPick(context.Background(), lease)
	if err != nil || !ok {
		t.Fatalf("fairpick ok=%v err=%v", ok, err)
	}
	return raw, p
}

func TestLifecycle_EnqueuePickAckLeavesNothingBehind(t *testing.T) {
	c := freshClient(t)
	ctx := context.Background()
	org, ev := uuid.New(), uuid.New()
	if err := c.Enqueue(ctx, Payload{EventID: ev, OrgID: org, Attempt: 3, RetryStrategy: "fixed"}); err != nil {
		t.Fatal(err)
	}
	if s := c.mustStats(t); s.Pending != 1 || s.Processing != 0 {
		t.Fatalf("after enqueue %+v, want pending 1", s)
	}
	before := time.Now().UnixMilli()
	raw, p := c.mustPick(t, 60_000)
	if p.EventID != ev || p.OrgID != org || p.Attempt != 3 || p.RetryStrategy != "fixed" {
		t.Fatalf("payload did not round-trip: %+v", p)
	}
	if s := c.mustStats(t); s.Pending != 0 || s.Processing != 1 {
		t.Fatalf("after pick %+v, want processing 1", s)
	}
	items, _, _ := c.Items(ctx, "processing", "", 10)
	if len(items) != 1 || items[0].LeaseMs < before+60_000 || items[0].LeaseMs > time.Now().UnixMilli()+60_000 {
		t.Fatalf("lease deadline = %+v, want now+60s", items)
	}
	if err := c.Ack(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if s := c.mustStats(t); s.Pending+s.Processing+s.Scheduled+s.Dead != 0 {
		t.Fatalf("after ack %+v, want an empty queue", s)
	}
	if _, _, ok, _ := c.FairPick(ctx, 1000); ok {
		t.Fatal("an acked event must not be picked again")
	}
}

func TestVisibilityTimeout_OnlyExpiredLeasesAreRecoveredAndRedelivered(t *testing.T) {
	c := freshClient(t)
	ctx := context.Background()
	org := uuid.New()
	short, long := uuid.New(), uuid.New()
	for _, ev := range []uuid.UUID{short, long} {
		if err := c.Enqueue(ctx, Payload{EventID: ev, OrgID: org}); err != nil {
			t.Fatal(err)
		}
	}
	_, a := c.mustPick(t, 1_000)    // leased for a second
	_, b := c.mustPick(t, 3600_000) // leased for an hour
	if a.EventID != short || b.EventID != long {
		t.Fatalf("pick order: %v then %v", a.EventID, b.EventID)
	}
	items, _, _ := c.Items(ctx, "processing", "", 10)
	var shortDeadline int64
	for _, it := range items {
		if it.EventID == short {
			shortDeadline = it.LeaseMs
		}
	}
	if shortDeadline == 0 {
		t.Fatalf("short lease missing from processing: %+v", items)
	}

	// The clock is an argument, so the timeout is crossed without sleeping.
	if n, err := c.Recover(ctx, shortDeadline-1); err != nil || n != 0 {
		t.Fatalf("Recover before the deadline = %d, %v, want 0", n, err)
	}
	if n, err := c.Recover(ctx, shortDeadline); err != nil || n != 1 {
		t.Fatalf("Recover at the deadline = %d, %v, want 1", n, err)
	}
	if s := c.mustStats(t); s.Processing != 1 || s.Scheduled != 1 || s.Pending != 0 {
		t.Fatalf("after recover %+v, want the hour lease still held and one scheduled", s)
	}
	if n, err := c.PromoteDue(ctx, shortDeadline, 10); err != nil || n != 1 {
		t.Fatalf("PromoteDue = %d, %v, want 1", n, err)
	}
	_, again := c.mustPick(t, 60_000)
	if again.EventID != short {
		t.Fatalf("redelivered %v, want the expired-lease event %v", again.EventID, short)
	}
}

func TestDeadLetter_MovesFromProcessingToDeadAndIsInspectable(t *testing.T) {
	c := freshClient(t)
	ctx := context.Background()
	org, ev := uuid.New(), uuid.New()
	_ = c.Enqueue(ctx, Payload{EventID: ev, OrgID: org, Attempt: 8})
	raw, _ := c.mustPick(t, 60_000)
	if err := c.DeadLetter(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if s := c.mustStats(t); s.Processing != 0 || s.Dead != 1 {
		t.Fatalf("%+v, want processing 0 dead 1", s)
	}
	dead, _, _ := c.Items(ctx, "dead", "", 10)
	if len(dead) != 1 || dead[0].EventID != ev || dead[0].Attempt != 8 {
		t.Fatalf("dead items = %+v", dead)
	}
}

func TestPromoteDue_HonoursTimeAndLimit(t *testing.T) {
	c := freshClient(t)
	ctx := context.Background()
	org := uuid.New()
	now := int64(1_000_000)
	for i := 0; i < 3; i++ {
		_ = c.Schedule(ctx, Payload{EventID: uuid.New(), OrgID: org}, now-int64(i))
	}
	future := uuid.New()
	_ = c.Schedule(ctx, Payload{EventID: future, OrgID: org}, now+1)
	if n, _ := c.PromoteDue(ctx, now, 2); n != 2 {
		t.Fatalf("first PromoteDue = %d, want the limit (2)", n)
	}
	if n, _ := c.PromoteDue(ctx, now, 2); n != 1 {
		t.Fatalf("second PromoteDue = %d, want the remaining due one", n)
	}
	if n, _ := c.PromoteDue(ctx, now, 2); n != 0 {
		t.Fatalf("third PromoteDue = %d, want 0 (the future event is not due)", n)
	}
	s := c.mustStats(t)
	if s.Pending != 3 || s.Scheduled != 1 {
		t.Fatalf("%+v, want pending 3 scheduled 1", s)
	}
	sched, _, _ := c.Items(context.Background(), "scheduled", "", 10)
	if len(sched) != 1 || sched[0].EventID != future {
		t.Fatalf("scheduled = %+v, want only the future event", sched)
	}
}

func TestFairPick_BigBacklogDoesNotStarveALaterOrg(t *testing.T) {
	c := freshClient(t)
	ctx := context.Background()
	big, small := uuid.New(), uuid.New()
	for i := 0; i < 20; i++ {
		_ = c.Enqueue(ctx, Payload{EventID: uuid.New(), OrgID: big})
	}
	smallEv := uuid.New()
	_ = c.Enqueue(ctx, Payload{EventID: smallEv, OrgID: small})
	if n, _ := c.rdb.LLen(ctx, c.prefix+":orgs").Result(); n != 2 {
		t.Fatalf("ring length = %d, want each org in it once", n)
	}
	_, first := c.mustPick(t, 60_000)
	_, second := c.mustPick(t, 60_000)
	if first.OrgID != big || second.OrgID != small || second.EventID != smallEv {
		t.Fatalf("picks %v then %v: the second org must get the very next turn", first.OrgID, second.OrgID)
	}
	if n, _ := c.rdb.LLen(ctx, c.prefix+":orgs").Result(); n != 1 {
		t.Fatalf("ring length = %d, want the drained org removed", n)
	}
}

func TestFairPick_UndecodableMemberSurfacesAnError(t *testing.T) {
	c := freshClient(t)
	ctx := context.Background()
	org := uuid.New().String()
	c.rdb.RPush(ctx, c.prefix+":pending:"+org, "not json")
	c.rdb.RPush(ctx, c.prefix+":orgs", org)
	raw, p, ok, err := c.FairPick(ctx, 60_000)
	if err == nil || ok || raw != "" || p.EventID != uuid.Nil {
		t.Fatalf("FairPick = %q %+v ok=%v err=%v, want an error and nothing picked", raw, p, ok, err)
	}
}

func TestStats_ReportsTopTenOrgsBusiestFirst(t *testing.T) {
	c := freshClient(t)
	ctx := context.Background()
	busiest := uuid.New()
	for i := 0; i < 12; i++ {
		org := uuid.New()
		if i == 11 {
			org = busiest
		}
		for j := 0; j <= i; j++ { // org i holds i+1 events
			_ = c.Enqueue(ctx, Payload{EventID: uuid.New(), OrgID: org})
		}
	}
	s := c.mustStats(t)
	if s.Pending != 78 || len(s.TopOrgs) != 10 {
		t.Fatalf("pending=%d topOrgs=%d, want 78 and the top 10", s.Pending, len(s.TopOrgs))
	}
	if s.TopOrgs[0].OrgID != busiest.String() || s.TopOrgs[0].Pending != 12 || s.TopOrgs[9].Pending != 3 {
		t.Fatalf("topOrgs = %+v, want 12 first and 3 last", s.TopOrgs)
	}
	all, err := c.AllOrgPending(ctx)
	if err != nil || len(all) != 12 {
		t.Fatalf("AllOrgPending = %d orgs, %v, want all 12", len(all), err)
	}
}

// A key of the wrong type under the queue's prefix is a real Redis failure the
// admin endpoints must surface rather than swallow.
func TestAdminReads_SurfaceRedisTypeErrors(t *testing.T) {
	for _, bad := range []string{"pending:" + uuid.NewString(), "scheduled", "processing", "dead"} {
		t.Run(bad[:strings.IndexAny(bad+":", ":")], func(t *testing.T) {
			c := freshClient(t)
			ctx := context.Background()
			c.rdb.Set(ctx, c.prefix+":"+bad, "x", time.Minute)
			if _, err := c.Stats(ctx); err == nil || !strings.Contains(err.Error(), "WRONGTYPE") {
				t.Errorf("Stats err = %v, want WRONGTYPE", err)
			}
			if strings.HasPrefix(bad, "pending") {
				if _, err := c.AllOrgPending(ctx); err == nil || !strings.Contains(err.Error(), "WRONGTYPE") {
					t.Errorf("AllOrgPending err = %v, want WRONGTYPE", err)
				}
				return
			}
			if _, _, err := c.Items(ctx, bad, "", 10); err == nil || !strings.Contains(err.Error(), "WRONGTYPE") {
				t.Errorf("Items(%s) err = %v, want WRONGTYPE", bad, err)
			}
		})
	}
}

func TestItems_LimitsAndLaneValidation(t *testing.T) {
	c := freshClient(t)
	ctx := context.Background()
	raw, _ := json.Marshal(Payload{EventID: uuid.New(), OrgID: uuid.New()})
	vals := make([]any, 201)
	zs := make([]redis.Z, 3)
	for i := range vals {
		vals[i] = string(raw)
	}
	c.rdb.RPush(ctx, c.prefix+":dead", vals...)
	for i := range zs {
		zs[i] = redis.Z{Score: float64(i), Member: fmt.Sprintf(`{"event_id":"%s","org_id":"%s"}`, uuid.New(), uuid.New())}
	}
	c.rdb.ZAdd(ctx, c.prefix+":scheduled", zs...)

	if items, trunc, err := c.Items(ctx, "dead", "", 0); err != nil || len(items) != 100 || !trunc {
		t.Fatalf("limit 0: %d items trunc=%v err=%v, want the default 100, truncated", len(items), trunc, err)
	}
	if items, trunc, err := c.Items(ctx, "dead", "", 5000); err != nil || len(items) != 200 || !trunc {
		t.Fatalf("limit 5000: %d items trunc=%v err=%v, want the 200 cap, truncated", len(items), trunc, err)
	}
	if items, trunc, err := c.Items(ctx, "scheduled", "", 2); err != nil || len(items) != 2 || !trunc {
		t.Fatalf("scheduled limit 2: %d items trunc=%v err=%v, want 2, truncated", len(items), trunc, err)
	}
	if _, _, err := c.Items(ctx, "pending", "", 10); err == nil || !strings.Contains(err.Error(), "requires org") {
		t.Fatalf("pending without org err = %v", err)
	}
	if _, _, err := c.Items(ctx, "bogus", "", 10); err == nil || !strings.Contains(err.Error(), `unknown lane "bogus"`) {
		t.Fatalf("unknown lane err = %v", err)
	}
}
