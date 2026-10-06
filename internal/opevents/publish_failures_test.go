package opevents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

// failTracer cancels the context of any statement containing marker, so that
// statement fails on the real pool while every other one runs normally.
type failTracer struct{ marker string }

func (f failTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(strings.ToLower(d.SQL), strings.ToLower(f.marker)) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		return c
	}
	return ctx
}
func (failTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// failingQ is a real pool on which statements containing marker fail.
func failingQ(t *testing.T, marker string) *store.Queries {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.Tracer = failTracer{marker: marker}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

// countOps returns how many op messages and deliveries the org has.
func countOps(t *testing.T, q *store.Queries, org uuid.UUID, appID uuid.UUID) (msgs, dels int) {
	t.Helper()
	ms, err := q.ListMessagesByApp(context.Background(), store.ListMessagesByAppParams{
		AppID: store.UUID(appID), CursorTs: maxTs(), CursorID: store.UUID(uuid.Max), Lim: 10,
	})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	for _, m := range ms {
		d, err := q.ListDeliveriesForMessage(context.Background(), m.ID)
		if err != nil {
			t.Fatalf("list deliveries: %v", err)
		}
		dels += len(d)
	}
	return len(ms), dels
}

// seedWithEndpoint seeds an org, its op app and one subscribed endpoint on a
// clean connection, and returns them with a live queue.
func seedWithEndpoint(t *testing.T) (q *store.Queries, org, appID uuid.UUID, dq *dqueue.Client) {
	t.Helper()
	q = testQ(t)
	org = seedOrg(t, q)
	var err error
	if appID, err = SeedOperationalApp(context.Background(), q, org); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err = q.CreateEndpoint(context.Background(), store.CreateEndpointParams{
		AppID: store.UUID(appID), OrgID: store.UUID(org), Url: "https://example.test/ops",
		Secret: "whsec_" + base64Std24(), Headers: []byte("{}"),
	}); err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	return q, org, appID, newTestQueue(t)
}

// Each step of Publish is best-effort for the CALLER (it logs and moves on) but
// must report its own failure faithfully, and must not leave a later step's
// rows behind.
func TestPublishReportsEachStepsFailure(t *testing.T) {
	cases := []struct {
		name     string
		marker   string
		wantMsgs int
		wantDels int
	}{
		{"op app cannot be ensured", "insert into applications", 0, 0},
		{"event types cannot be seeded", "insert into event_types", 0, 0},
		{"message insert fails", "insert into messages", 0, 0},
		{"endpoint lookup fails after the message exists", "cardinality(filter_event_types)", 1, 0},
		{"delivery batch insert fails", "insert into message_deliveries", 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, org, appID, dq := seedWithEndpoint(t)
			err := Publish(context.Background(), failingQ(t, tc.marker), dq, org, "endpoint.disabled", map[string]any{"k": "v"})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Publish err = %v, want the injected driver error (context canceled)", err)
			}
			msgs, dels := countOps(t, clean, org, appID)
			if msgs != tc.wantMsgs || dels != tc.wantDels {
				t.Fatalf("rows after failure: messages=%d deliveries=%d, want %d and %d", msgs, dels, tc.wantMsgs, tc.wantDels)
			}
		})
	}
}

// A payload that cannot be JSON-encoded is rejected before anything is written.
func TestPublishUnencodablePayloadWritesNothing(t *testing.T) {
	q, org, appID, dq := seedWithEndpoint(t)
	err := Publish(context.Background(), q, dq, org, "endpoint.disabled", map[string]any{"bad": func() {}})
	var ute *json.UnsupportedTypeError
	if !errors.As(err, &ute) {
		t.Fatalf("Publish err = %v, want *json.UnsupportedTypeError", err)
	}
	if msgs, dels := countOps(t, q, org, appID); msgs != 0 || dels != 0 {
		t.Fatalf("rows written for an unencodable payload: messages=%d deliveries=%d", msgs, dels)
	}
}

// If the queue is down the delivery rows are already committed as 'queued' and
// Publish still succeeds: the reaper re-enqueues queued rows, so the event is
// delayed, not lost.
func TestPublishQueueDownLeavesQueuedDelivery(t *testing.T) {
	q, org, appID, _ := seedWithEndpoint(t)
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, PoolSize: 1})
	t.Cleanup(func() { _ = dead.Close() })
	if err := Publish(context.Background(), q, dqueue.NewClient(dead), org, "endpoint.disabled", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("Publish with the queue down = %v, want nil (reaper recovers queued rows)", err)
	}
	ms, err := q.ListMessagesByApp(context.Background(), store.ListMessagesByAppParams{
		AppID: store.UUID(appID), CursorTs: maxTs(), CursorID: store.UUID(uuid.Max), Lim: 10,
	})
	if err != nil || len(ms) != 1 {
		t.Fatalf("messages = %d, %v; want 1", len(ms), err)
	}
	dels, err := q.ListDeliveriesForMessage(context.Background(), ms[0].ID)
	if err != nil || len(dels) != 1 {
		t.Fatalf("deliveries = %d, %v; want 1", len(dels), err)
	}
	if dels[0].Status != "queued" {
		t.Fatalf("delivery status = %q, want queued", dels[0].Status)
	}
}
