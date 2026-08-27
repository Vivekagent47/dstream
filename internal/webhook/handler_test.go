package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testPool(t *testing.T) *store.Queries {
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
	return store.New(pool)
}

func testQueue(t *testing.T) (*dqueue.Client, *redis.Client, string) {
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
	prefix := "whtest-" + uuid.NewString()[:8]
	c := dqueue.NewClient(rdb).WithPrefix(prefix)
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+":*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
		_ = rdb.Close()
	})
	return c, rdb, prefix
}

// seedDelivery builds org→app→endpoint(url)→message→delivery and returns the
// delivery id + the message id + the org id.
func seedDelivery(t *testing.T, q *store.Queries, url string, secret string) (delID, msgID uuid.UUID, orgID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	u, err := q.CreateUser(ctx, store.CreateUserParams{Email: "t+" + uuid.NewString() + "@example.test"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{Name: "T", Slug: "t-" + uuid.NewString()})
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	_ = q.AddOrgMember(ctx, store.AddOrgMemberParams{OrgID: o.ID, UserID: u.ID, Role: "owner"})
	app, err := q.CreateApplication(ctx, store.CreateApplicationParams{OrgID: o.ID, Name: "A", Metadata: []byte(`{}`)})
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	ep, err := q.CreateEndpoint(ctx, store.CreateEndpointParams{
		AppID: app.ID, OrgID: o.ID, Url: url, Secret: secret,
	})
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	msg, err := q.CreateMessage(ctx, store.CreateMessageParams{
		AppID: app.ID, OrgID: o.ID, EventType: "invoice.paid",
		Payload: []byte(`{"x":1}`), PayloadHash: "h",
	})
	if err != nil {
		t.Fatalf("message: %v", err)
	}
	dels, err := q.CreateMessageDeliveriesBatch(ctx, store.CreateMessageDeliveriesBatchParams{
		MessageID: msg.ID, OrgID: o.ID, EndpointIds: []pgtype.UUID{ep.ID},
	})
	if err != nil || len(dels) != 1 {
		t.Fatalf("deliveries: %v (%d)", err, len(dels))
	}
	return store.GoUUID(dels[0].ID), store.GoUUID(msg.ID), store.GoUUID(o.ID)
}

func enqueueDelivery(t *testing.T, dq *dqueue.Client, delID, orgID uuid.UUID) {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"delivery_id": delID.String()})
	if err := dq.Enqueue(context.Background(), dqueue.Payload{
		Kind: "message", OrgID: orgID, EnqueuedAt: time.Now().UnixMilli(), Data: data,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
}

func TestDeliverSignedOKAcks(t *testing.T) {
	q := testPool(t)
	dq, rdb, prefix := testQueue(t)
	ctx := context.Background()

	var gotSig, gotID, gotTs string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("webhook-signature")
		gotID = r.Header.Get("webhook-id")
		gotTs = r.Header.Get("webhook-timestamp")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	secret, _ := GenerateSecret()
	delID, msgID, orgID := seedDelivery(t, q, srv.URL, secret)
	enqueueDelivery(t, dq, delID, orgID)

	// allowPrivate=true so the guard permits 127.0.0.1 (httptest).
	h := Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true)}
	raw, p, ok, err := dq.FairPick(ctx, 10000)
	if err != nil || !ok {
		t.Fatalf("fairpick ok=%v err=%v", ok, err)
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatalf("process: %v", err)
	}

	// Server was hit and headers verify.
	if gotID != msgID.String() {
		t.Fatalf("webhook-id: got %q want %q", gotID, msgID)
	}
	// Recompute the expected signature from the received timestamp + body.
	var ts int64
	_, _ = fmtSscan(gotTs, &ts)
	want, _ := Sign(secret, msgID.String(), ts, gotBody)
	if gotSig != want {
		t.Fatalf("signature: got %q want %q", gotSig, want)
	}
	// Acked (processing drained) + delivery marked delivered.
	if n, _ := rdb.ZCard(ctx, prefix+":processing").Result(); n != 0 {
		t.Fatalf("want acked, %d in processing", n)
	}
}

func TestDeliverRetryOn500Schedules(t *testing.T) {
	q := testPool(t)
	dq, rdb, prefix := testQueue(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	secret, _ := GenerateSecret()
	delID, _, orgID := seedDelivery(t, q, srv.URL, secret)
	enqueueDelivery(t, dq, delID, orgID)
	h := Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true)}
	raw, p, ok, _ := dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}
	// Rescheduled (1 in scheduled) and acked (0 in processing).
	if n, _ := rdb.ZCard(ctx, prefix+":scheduled").Result(); n != 1 {
		t.Fatalf("want 1 scheduled, got %d", n)
	}
	if n, _ := rdb.ZCard(ctx, prefix+":processing").Result(); n != 0 {
		t.Fatalf("want acked, got %d", n)
	}
}

func TestDeliverDeadLettersWhenExhausted(t *testing.T) {
	q := testPool(t)
	dq, rdb, prefix := testQueue(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	secret, _ := GenerateSecret()
	delID, _, orgID := seedDelivery(t, q, srv.URL, secret)
	// Burn the retry budget: bump attempt_count to len(retrySchedule) so the
	// next failure exhausts it. 7 in-flight marks → attempt_count = 7.
	for i := 0; i < len(retrySchedule); i++ {
		if err := q.MarkDeliveryInFlight(ctx, store.UUID(delID)); err != nil {
			t.Fatal(err)
		}
	}
	enqueueDelivery(t, dq, delID, orgID)
	h := Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true)}
	raw, p, ok, _ := dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.LLen(ctx, prefix+":dead").Result(); n != 1 {
		t.Fatalf("want 1 dead-lettered, got %d", n)
	}
}

func fmtSscan(s string, out *int64) (int, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	*out = v
	return 1, err
}
