package dqueue

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestItemsDeadAndScheduled(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	org := uuid.New()

	// one dead, one scheduled
	if err := c.Enqueue(ctx, Payload{EventID: uuid.New(), OrgID: org}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	raw, _, ok, err := c.FairPick(ctx, 60000)
	if err != nil || !ok {
		t.Fatalf("pick ok=%v err=%v", ok, err)
	}
	if err := c.DeadLetter(ctx, raw); err != nil {
		t.Fatalf("deadletter: %v", err)
	}
	if err := c.Schedule(ctx, Payload{EventID: uuid.New(), OrgID: org, Attempt: 2}, 9_999_999_999_999); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	dead, trunc, err := c.Items(ctx, "dead", "", 100)
	if err != nil || len(dead) != 1 || trunc {
		t.Fatalf("dead items=%d trunc=%v err=%v", len(dead), trunc, err)
	}
	if dead[0].Raw == "" || dead[0].OrgID != org || dead[0].DecodeError != "" {
		t.Fatalf("dead item bad: %+v", dead[0])
	}
	sched, _, err := c.Items(ctx, "scheduled", "", 100)
	if err != nil || len(sched) != 1 {
		t.Fatalf("sched items=%d err=%v", len(sched), err)
	}
	if sched[0].NextRunMs != 9_999_999_999_999 || sched[0].Attempt != 2 {
		t.Fatalf("sched score/attempt wrong: %+v", sched[0])
	}
}

func TestItemsPendingRequiresOrg(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	org := uuid.New()
	if err := c.Enqueue(ctx, Payload{EventID: uuid.New(), OrgID: org}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	items, _, err := c.Items(ctx, "pending", org.String(), 100)
	if err != nil || len(items) != 1 {
		t.Fatalf("pending items=%d err=%v", len(items), err)
	}
}

func TestItemsTruncated(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	org := uuid.New()
	for i := 0; i < 5; i++ {
		if err := c.Enqueue(ctx, Payload{EventID: uuid.New(), OrgID: org}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	items, trunc, err := c.Items(ctx, "pending", org.String(), 3)
	if err != nil || len(items) != 3 || !trunc {
		t.Fatalf("items=%d trunc=%v err=%v", len(items), trunc, err)
	}
}

func TestItemsDecodeError(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	// inject a non-JSON member straight into the dead list
	if err := c.rdb.RPush(ctx, c.prefix+":dead", "not-json").Err(); err != nil {
		t.Fatalf("rpush: %v", err)
	}
	items, _, err := c.Items(ctx, "dead", "", 100)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%d err=%v", len(items), err)
	}
	if items[0].Raw != "not-json" || items[0].DecodeError == "" {
		t.Fatalf("expected decode_error surfaced: %+v", items[0])
	}
}

func TestAllOrgPending(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	o1, o2 := uuid.New(), uuid.New()
	c.Enqueue(ctx, Payload{EventID: uuid.New(), OrgID: o1})
	c.Enqueue(ctx, Payload{EventID: uuid.New(), OrgID: o1})
	c.Enqueue(ctx, Payload{EventID: uuid.New(), OrgID: o2})
	rows, err := c.AllOrgPending(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
}

func TestRequeueDead(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	org := uuid.New()
	// dead-letter a payload with attempt > 0 so requeue's reset-to-0 is proven.
	raw, _ := json.Marshal(Payload{EventID: uuid.New(), OrgID: org, Attempt: 5})
	if err := c.rdb.RPush(ctx, c.prefix+":dead", string(raw)).Err(); err != nil {
		t.Fatalf("rpush: %v", err)
	}

	ok, err := c.RequeueDead(ctx, string(raw))
	if err != nil || !ok {
		t.Fatalf("requeue ok=%v err=%v", ok, err)
	}
	// dead now empty, pending has 1 with attempt reset to 0
	if n, _ := c.rdb.LLen(ctx, c.prefix+":dead").Result(); n != 0 {
		t.Fatalf("dead not empty: %d", n)
	}
	items, _, _ := c.Items(ctx, "pending", org.String(), 100)
	if len(items) != 1 || items[0].Attempt != 0 {
		t.Fatalf("pending after requeue: %+v", items)
	}
	// second call is a no-op (already moved)
	ok2, err := c.RequeueDead(ctx, string(raw))
	if err != nil || ok2 {
		t.Fatalf("second requeue ok=%v err=%v (want false,nil)", ok2, err)
	}
}

func TestPromoteScheduled(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	org := uuid.New()
	p := Payload{EventID: uuid.New(), OrgID: org, Attempt: 3}
	if err := c.Schedule(ctx, p, 9_999_999_999_999); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	sched, _, _ := c.Items(ctx, "scheduled", "", 100)
	raw := sched[0].Raw

	ok, err := c.PromoteScheduled(ctx, raw)
	if err != nil || !ok {
		t.Fatalf("promote ok=%v err=%v", ok, err)
	}
	if n, _ := c.rdb.ZCard(ctx, c.prefix+":scheduled").Result(); n != 0 {
		t.Fatalf("scheduled not empty: %d", n)
	}
	items, _, _ := c.Items(ctx, "pending", org.String(), 100)
	if len(items) != 1 || items[0].Attempt != 3 { // attempt unchanged
		t.Fatalf("pending after promote: %+v", items)
	}
	ok2, _ := c.PromoteScheduled(ctx, raw)
	if ok2 {
		t.Fatalf("second promote should be false")
	}
}

func TestDrainDead(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	org := uuid.New()
	for i := 0; i < 3; i++ {
		c.Enqueue(ctx, Payload{EventID: uuid.New(), OrgID: org})
		raw, _, _, _ := c.FairPick(ctx, 60000)
		c.DeadLetter(ctx, raw)
	}
	n, err := c.DrainDead(ctx)
	if err != nil || n != 3 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}
	if ln, _ := c.rdb.LLen(ctx, c.prefix+":dead").Result(); ln != 0 {
		t.Fatalf("dead not cleared: %d", ln)
	}
}

func TestStatsEmptyQueueHasNonNilTopOrgs(t *testing.T) {
	st, err := testClient(t).Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.TopOrgs == nil || len(st.TopOrgs) != 0 {
		t.Errorf("TopOrgs = %#v, want a non-nil empty slice", st.TopOrgs)
	}
	if b, _ := json.Marshal(st); !strings.Contains(string(b), `"top_orgs":[]`) {
		t.Errorf("json = %s, want top_orgs as []", b)
	}
}
