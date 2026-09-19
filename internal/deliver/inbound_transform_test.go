package deliver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
)

func inboundLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func inboundDB(t *testing.T) (*store.Queries, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := store.NewPool(ctx, dsn, 2)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool), pool
}

func inboundQueue(t *testing.T) (*dqueue.Client, *redis.Client) {
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
	prefix := "dltest-" + uuid.NewString()[:8]
	c := dqueue.NewClient(rdb).WithPrefix(prefix)
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+":*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
		_ = rdb.Close()
	})
	return c, rdb
}

func strp(s string) *string { return &s }

// seedInbound builds org→source→destination(http url)→connection→request(+body)
// →event, with the connection's filter_expr/transform_js set to the given
// pointers (nil = unset). Returns the queued event id + org id.
func seedInbound(t *testing.T, q *store.Queries, pool *pgxpool.Pool, destURL string, payload []byte, filterExpr, transformJS *string) (eventID, orgID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{Name: "T", Slug: "t-" + uuid.NewString()})
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	src, err := q.CreateSource(ctx, store.CreateSourceParams{
		OrgID: o.ID, Name: "s-" + uuid.NewString(), Type: "generic",
		IngestToken: uuid.NewString(), SigningConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	dst, err := q.CreateDestination(ctx, store.CreateDestinationParams{
		OrgID: o.ID, Name: "d-" + uuid.NewString(), Type: "http",
		Url: strp(destURL), AuthConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("destination: %v", err)
	}
	conn, err := q.CreateConnection(ctx, store.CreateConnectionParams{
		SourceID: src.ID, DestinationID: dst.ID, Enabled: true,
	})
	if err != nil {
		t.Fatalf("connection: %v", err)
	}
	// CreateConnection has no filter/transform columns; set them directly (no
	// store setter exists yet — Phase 3 edits arrive via the API in a later task).
	if _, err := pool.Exec(ctx, `UPDATE connections SET filter_expr=$1, transform_js=$2 WHERE id=$3`,
		filterExpr, transformJS, conn.ID); err != nil {
		t.Fatalf("set filter/transform: %v", err)
	}

	reqID := uuid.Must(uuid.NewV7())
	req, err := q.CreateRequest(ctx, store.CreateRequestParams{
		ID: store.UUID(reqID), SourceID: src.ID, HTTPMethod: "POST", HTTPPath: "/e/x",
		Headers:  []byte(`{"Content-Type":["application/json"]}`),
		BodyHash: "h", BodyRef: "pg:" + reqID.String(), BodySize: int32(len(payload)),
		ContentType: strp("application/json"),
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	bs := ingest.NewPostgresBodyStore(q)
	if _, err := bs.Put(ctx, store.GoUUID(req.ID), payload); err != nil {
		t.Fatalf("body: %v", err)
	}
	events, err := q.CreateEventsBatch(ctx, store.CreateEventsBatchParams{
		RequestID: req.ID, OrgID: o.ID, ConnectionIds: []pgtype.UUID{conn.ID},
	})
	if err != nil || len(events) != 1 {
		t.Fatalf("events: %v (%d)", err, len(events))
	}
	return store.GoUUID(events[0].ID), store.GoUUID(o.ID)
}

func runOneInbound(t *testing.T, h *Handler, dq *dqueue.Client, eventID, orgID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := dq.Enqueue(ctx, dqueue.Payload{EventID: eventID, OrgID: orgID, EnqueuedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	raw, p, ok, err := dq.FairPick(ctx, 10000)
	if err != nil || !ok {
		t.Fatalf("fairpick ok=%v err=%v", ok, err)
	}
	if err := h.Process(ctx, p, raw); err != nil {
		t.Fatalf("process: %v", err)
	}
}

// A connection filter that does NOT match drops the event: status becomes
// 'filtered' and the destination is never called.
func TestDeliverFilteredNoMatch(t *testing.T) {
	q, pool := inboundDB(t)
	dq, rdb := inboundQueue(t)
	ctx := context.Background()

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	eventID, orgID := seedInbound(t, q, pool, srv.URL, []byte(`{"amount":1}`), strp("payload.amount > 1000"), nil)
	h := New(inboundLog(), q, rdb, ingest.NewPostgresBodyStore(q), dq, true)
	runOneInbound(t, h, dq, eventID, orgID)

	if hits != 0 {
		t.Fatalf("filtered event must NOT hit the destination, got %d hits", hits)
	}
	row, err := q.GetEventForDelivery(ctx, store.UUID(eventID))
	if err != nil {
		t.Fatalf("load event: %v", err)
	}
	if row.Status != "filtered" {
		t.Fatalf("want event status 'filtered', got %q", row.Status)
	}
}

// A connection transform reshapes the payload: the destination receives the
// mutated body, not the original.
func TestDeliverTransformMutatesBody(t *testing.T) {
	q, pool := inboundDB(t)
	dq, rdb := inboundQueue(t)

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	js := `function transform(payload, headers) { return {tagged: true}; }`
	eventID, orgID := seedInbound(t, q, pool, srv.URL, []byte(`{"amount":1}`), nil, strp(js))
	h := New(inboundLog(), q, rdb, ingest.NewPostgresBodyStore(q), dq, true)
	runOneInbound(t, h, dq, eventID, orgID)

	if got := string(gotBody); got != `{"tagged":true}` {
		t.Fatalf("destination must receive the transformed body, got %q", got)
	}
}
