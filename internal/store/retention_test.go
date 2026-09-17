package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/store"
)

// A far-past, unique window so these sweeps only ever touch rows this test
// backdates — never a sibling test's now()-stamped rows in the shared DB.
var (
	backdatedAt = time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	sweepCutoff = pgtype.Timestamptz{Time: time.Date(1991, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}
)

func TestExpireOldMessagePayloads(t *testing.T) {
	pool := isolationPool(t)
	q := store.New(pool)
	ctx := context.Background()
	org := seedIsolationOrg(t, q, "ret-msg")

	app, err := q.CreateApplication(ctx, store.CreateApplicationParams{
		OrgID: store.UUID(org.OrgID), Name: "A", Metadata: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	mk := func() store.Message {
		m, err := q.CreateMessage(ctx, store.CreateMessageParams{
			AppID: app.ID, OrgID: store.UUID(org.OrgID), EventType: "invoice.paid",
			Payload: []byte(`{"x":1}`), PayloadHash: "h",
		})
		if err != nil {
			t.Fatalf("message: %v", err)
		}
		return m
	}
	oldMsg := mk()
	newMsg := mk()
	// Backdate the old one past the cutoff.
	if _, err := pool.Exec(ctx, `UPDATE messages SET created_at = $1 WHERE id = $2`, backdatedAt, oldMsg.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := q.ExpireOldMessagePayloads(ctx, sweepCutoff)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n < 1 {
		t.Fatalf("first sweep should null >=1 payload, got %d", n)
	}

	got := func(id pgtype.UUID) []byte {
		m, err := q.GetMessageForApp(ctx, store.GetMessageForAppParams{ID: id, AppID: app.ID})
		if err != nil {
			t.Fatalf("get message: %v", err)
		}
		return m.Payload
	}
	if p := got(oldMsg.ID); len(p) != 0 {
		t.Fatalf("old message payload should be NULL, got %q", p)
	}
	if p := got(newMsg.ID); len(p) == 0 {
		t.Fatalf("recent message payload must remain intact, got empty")
	}

	// Idempotent: nothing left below the cutoff with a non-null payload.
	if n2, err := q.ExpireOldMessagePayloads(ctx, sweepCutoff); err != nil {
		t.Fatalf("second expire: %v", err)
	} else if n2 != 0 {
		t.Fatalf("second sweep must affect 0 rows, got %d", n2)
	}
}

func TestExpireOldAttemptBodies(t *testing.T) {
	pool := isolationPool(t)
	q := store.New(pool)
	ctx := context.Background()
	org := seedIsolationOrg(t, q, "ret-att")

	app, err := q.CreateApplication(ctx, store.CreateApplicationParams{
		OrgID: store.UUID(org.OrgID), Name: "A", Metadata: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	ep, err := q.CreateEndpoint(ctx, store.CreateEndpointParams{
		AppID: app.ID, OrgID: store.UUID(org.OrgID), Url: "https://ex.test/a", Secret: "s", Headers: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	msg, err := q.CreateMessage(ctx, store.CreateMessageParams{
		AppID: app.ID, OrgID: store.UUID(org.OrgID), EventType: "invoice.paid",
		Payload: []byte(`{"x":1}`), PayloadHash: "h",
	})
	if err != nil {
		t.Fatalf("message: %v", err)
	}
	dels, err := q.CreateMessageDeliveriesBatch(ctx, store.CreateMessageDeliveriesBatchParams{
		MessageID: msg.ID, OrgID: store.UUID(org.OrgID), EndpointIds: []pgtype.UUID{ep.ID},
	})
	if err != nil || len(dels) != 1 {
		t.Fatalf("deliveries: %v (%d)", err, len(dels))
	}
	mkAttempt := func(num int32, body string) store.MessageDeliveryAttempt {
		a, err := q.CreateMessageDeliveryAttempt(ctx, store.CreateMessageDeliveryAttemptParams{
			DeliveryID: dels[0].ID, AttemptNum: num, ResponseBody: []byte(body),
		})
		if err != nil {
			t.Fatalf("attempt: %v", err)
		}
		return a
	}
	oldAtt := mkAttempt(1, `{"ok":1}`)
	mkAttempt(2, `{"ok":2}`)
	if _, err := pool.Exec(ctx, `UPDATE message_delivery_attempts SET attempted_at = $1 WHERE id = $2`, backdatedAt, oldAtt.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := q.ExpireOldAttemptBodies(ctx, sweepCutoff)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n < 1 {
		t.Fatalf("first sweep should null >=1 body, got %d", n)
	}

	atts, err := q.ListAttemptsByMessage(ctx, store.ListAttemptsByMessageParams{MessageID: msg.ID, Lim: 10})
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	byNum := map[int32][]byte{}
	for _, a := range atts {
		byNum[a.AttemptNum] = a.ResponseBody
	}
	if b := byNum[1]; len(b) != 0 {
		t.Fatalf("old attempt body should be NULL, got %q", b)
	}
	if b := byNum[2]; len(b) == 0 {
		t.Fatalf("recent attempt body must remain intact, got empty")
	}

	if n2, err := q.ExpireOldAttemptBodies(ctx, sweepCutoff); err != nil {
		t.Fatalf("second expire: %v", err)
	} else if n2 != 0 {
		t.Fatalf("second sweep must affect 0 rows, got %d", n2)
	}
}
