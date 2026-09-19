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
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/opevents"
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
// delivery id + the message id + the org id. Thin wrapper over seedDeliveryFull.
func seedDelivery(t *testing.T, q *store.Queries, url string, secret string) (delID, msgID uuid.UUID, orgID uuid.UUID) {
	t.Helper()
	delID, msgID, orgID, _, _ = seedDeliveryFull(t, q, url, secret)
	return delID, msgID, orgID
}

// seedDeliveryFull is seedDelivery but also returns the endpoint + app ids, for
// tests that assert on endpoint-level state (auto-disable, failure counters).
func seedDeliveryFull(t *testing.T, q *store.Queries, url string, secret string) (delID, msgID, orgID, endpointID, appID uuid.UUID) {
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
		AppID: app.ID, OrgID: o.ID, Url: url, Secret: secret, Headers: []byte("{}"),
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
	return store.GoUUID(dels[0].ID), store.GoUUID(msg.ID), store.GoUUID(o.ID), store.GoUUID(ep.ID), store.GoUUID(app.ID)
}

// seedDeliveryWithPrev is seedDelivery with the endpoint's current secret set
// to `secret` and a live previous secret `prevSecret` expiring at prevExp
// (created by rotating prevSecret→secret via the store).
func seedDeliveryWithPrev(t *testing.T, q *store.Queries, url, secret, prevSecret string, prevExp time.Time) (delID, msgID, orgID uuid.UUID) {
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
	// Create with the OLD secret, then rotate: prev_secret←prevSecret, secret←secret.
	ep, err := q.CreateEndpoint(ctx, store.CreateEndpointParams{
		AppID: app.ID, OrgID: o.ID, Url: url, Secret: prevSecret, Headers: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if _, err := q.RotateEndpointSecret(ctx, store.RotateEndpointSecretParams{
		PrevExpiresAt: pgtype.Timestamptz{Time: prevExp, Valid: true},
		NewSecret:     secret, ID: ep.ID, AppID: app.ID,
	}); err != nil {
		t.Fatalf("rotate: %v", err)
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

func TestDeliverAppliesCustomHeadersReservedWin(t *testing.T) {
	q := testPool(t)
	dq, _, _ := testQueue(t)
	ctx := context.Background()

	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	secret, _ := GenerateSecret()
	delID, msgID, orgID, epID, appID := seedDeliveryFull(t, q, srv.URL, secret)
	// Stash a custom header AND a rogue webhook-id (write-path blocks the latter,
	// this is defense-in-depth: the send must still prefer the real reserved value).
	if _, err := q.UpdateEndpoint(ctx, store.UpdateEndpointParams{
		ID: store.UUID(epID), AppID: store.UUID(appID),
		SetHeaders: true, Headers: []byte(`{"X-Consumer-Auth":"Bearer t","webhook-id":"rogue"}`),
	}); err != nil {
		t.Fatalf("set headers: %v", err)
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
	if got := captured.Get("X-Consumer-Auth"); got != "Bearer t" {
		t.Errorf("custom header: got %q", got)
	}
	if got := captured.Get("webhook-id"); got != msgID.String() {
		t.Errorf("reserved header overwritten: got %q want %q", got, msgID)
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

func TestAutoDisableAfterConsecutiveDead(t *testing.T) {
	q := testPool(t)
	dq, _, _ := testQueue(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	secret, _ := GenerateSecret()
	delID, _, orgID, epID, appID := seedDeliveryFull(t, q, srv.URL, secret)

	// threshold 1 → a single dead delivery disables the endpoint.
	h := Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true), MaxConsecutiveFailures: 1}
	// burn the retry budget so this Process call dead-letters in one shot
	for i := 0; i < len(retrySchedule); i++ {
		_ = q.MarkDeliveryInFlight(ctx, store.UUID(delID))
	}
	enqueueDelivery(t, dq, delID, orgID)
	raw, p, ok, _ := dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}
	ep, err := q.GetEndpointForApp(ctx, store.GetEndpointForAppParams{ID: store.UUID(epID), AppID: store.UUID(appID)})
	if err != nil {
		t.Fatal(err)
	}
	if !ep.Disabled || ep.ConsecutiveFailures < 1 {
		t.Fatalf("endpoint should be auto-disabled: disabled=%v failures=%d", ep.Disabled, ep.ConsecutiveFailures)
	}
	if !ep.DisabledAt.Valid {
		t.Fatalf("disabled_at should be stamped")
	}
}

func TestConsecutiveFailuresResetOnSuccess(t *testing.T) {
	q := testPool(t)
	dq, _, _ := testQueue(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	secret, _ := GenerateSecret()
	delID, _, orgID, epID, appID := seedDeliveryFull(t, q, srv.URL, secret)

	// Pre-load a failure run (high threshold so it doesn't auto-disable).
	if _, err := q.IncrEndpointFailures(ctx, store.IncrEndpointFailuresParams{ID: store.UUID(epID), Threshold: 100}); err != nil {
		t.Fatal(err)
	}
	h := Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true), MaxConsecutiveFailures: 5}
	enqueueDelivery(t, dq, delID, orgID)
	raw, p, ok, _ := dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}
	ep, err := q.GetEndpointForApp(ctx, store.GetEndpointForAppParams{ID: store.UUID(epID), AppID: store.UUID(appID)})
	if err != nil {
		t.Fatal(err)
	}
	if ep.ConsecutiveFailures != 0 {
		t.Fatalf("successful delivery must reset failures, got %d", ep.ConsecutiveFailures)
	}
}

func fmtSscan(s string, out *int64) (int, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	*out = v
	return 1, err
}

func TestDeliverDualSignsDuringGrace(t *testing.T) {
	q := testPool(t)
	dq, rdb, prefix := testQueue(t)
	_ = rdb
	ctx := context.Background()

	var gotSig, gotTs string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("webhook-signature")
		gotTs = r.Header.Get("webhook-timestamp")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	newSecret, _ := GenerateSecret()
	prevSecret, _ := GenerateSecret()
	// seed with current=newSecret AND a live prev=prevSecret (expires +1h)
	delID, msgID, orgID := seedDeliveryWithPrev(t, q, srv.URL, newSecret, prevSecret, time.Now().Add(time.Hour))
	_ = orgID
	enqueueDelivery(t, dq, delID, orgID)

	h := Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true)}
	raw, p, ok, _ := dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}

	var ts int64
	_, _ = fmtSscan(gotTs, &ts)
	wantNew, _ := Sign(newSecret, msgID.String(), ts, gotBody)
	wantPrev, _ := Sign(prevSecret, msgID.String(), ts, gotBody)
	// header must contain BOTH signatures, space-separated
	if !strings.Contains(gotSig, wantNew) || !strings.Contains(gotSig, wantPrev) {
		t.Fatalf("header must carry new + prev sig during grace: %q", gotSig)
	}
	_ = prefix
}

// After the grace window expires, the header must carry ONLY the current
// secret's signature — the old/leaked secret must stop working.
func TestDeliverSingleSignAfterGraceExpiry(t *testing.T) {
	q := testPool(t)
	dq, rdb, prefix := testQueue(t)
	_ = rdb
	ctx := context.Background()

	var gotSig, gotTs string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("webhook-signature")
		gotTs = r.Header.Get("webhook-timestamp")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	newSecret, _ := GenerateSecret()
	prevSecret, _ := GenerateSecret()
	// seed with current=newSecret AND a prev=prevSecret that ALREADY expired
	delID, msgID, orgID := seedDeliveryWithPrev(t, q, srv.URL, newSecret, prevSecret, time.Now().Add(-time.Hour))
	enqueueDelivery(t, dq, delID, orgID)

	h := Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true)}
	raw, p, ok, _ := dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}

	var ts int64
	_, _ = fmtSscan(gotTs, &ts)
	wantNew, _ := Sign(newSecret, msgID.String(), ts, gotBody)
	wantPrev, _ := Sign(prevSecret, msgID.String(), ts, gotBody)
	// header must be EXACTLY the current sig (one entry, no space) ...
	if gotSig != wantNew {
		t.Fatalf("post-expiry header must carry ONLY current sig: got %q want %q", gotSig, wantNew)
	}
	// ... and must NOT include the expired prev secret's signature.
	if strings.Contains(gotSig, wantPrev) {
		t.Fatalf("post-expiry header must not carry expired prev sig: %q", gotSig)
	}
	_ = prefix
}

func TestOutboundRateLimitDefers(t *testing.T) {
	q := testPool(t)
	dq, rdb, prefix := testQueue(t)
	ctx := context.Background()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	secret, _ := GenerateSecret()
	del1, _, orgID, epID, appID := seedDeliveryFull(t, q, srv.URL, secret)

	// rate_limit = 1/s on the endpoint.
	if _, err := q.UpdateEndpoint(ctx, store.UpdateEndpointParams{
		ID: store.UUID(epID), AppID: store.UUID(appID), RateLimit: int32Ptr(1),
	}); err != nil {
		t.Fatalf("set rate_limit: %v", err)
	}
	// second delivery to the SAME endpoint, within the same second.
	del2 := seedExtraDelivery(t, q, orgID, appID, epID)

	h := Handler{
		Log: discardLog(), Queries: q,
		HTTP:    deliver.NewSafeHTTPClient(10*time.Second, true),
		Redis:   rdb,
		Limiter: redis_rate.NewLimiter(rdb),
	}
	// First: allowed → sends.
	enqueueDelivery(t, dq, del1, orgID)
	raw, p, ok, _ := dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick del1")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("first delivery should send, got %d hits", hits)
	}

	// Second: over-limit within the second → deferred (no send, no attempt).
	enqueueDelivery(t, dq, del2, orgID)
	raw, p, ok, _ = dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick del2")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("rate-limited delivery must NOT hit the endpoint, got %d hits", hits)
	}
	if n, _ := rdb.ZCard(ctx, prefix+":scheduled").Result(); n != 1 {
		t.Fatalf("deferred delivery should be rescheduled, scheduled=%d", n)
	}
	if n, _ := rdb.ZCard(ctx, prefix+":processing").Result(); n != 0 {
		t.Fatalf("deferred delivery should be acked, processing=%d", n)
	}
	// Defer consumes no attempt: del2 attempt_count still 0.
	row, err := q.GetMessageDeliveryForSend(ctx, store.UUID(del2))
	if err != nil {
		t.Fatal(err)
	}
	if row.AttemptCount != 0 {
		t.Fatalf("deferred delivery must not consume an attempt, attempt_count=%d", row.AttemptCount)
	}
}

func int32Ptr(v int32) *int32 { return &v }

// seedExtraDelivery creates a fresh message + one delivery to an existing
// endpoint (same org/app), for tests that need two deliveries to one endpoint.
func seedExtraDelivery(t *testing.T, q *store.Queries, orgID, appID, epID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	msg, err := q.CreateMessage(ctx, store.CreateMessageParams{
		AppID: store.UUID(appID), OrgID: store.UUID(orgID), EventType: "invoice.paid",
		Payload: []byte(`{"x":2}`), PayloadHash: "h2",
	})
	if err != nil {
		t.Fatalf("message: %v", err)
	}
	dels, err := q.CreateMessageDeliveriesBatch(ctx, store.CreateMessageDeliveriesBatchParams{
		MessageID: msg.ID, OrgID: store.UUID(orgID), EndpointIds: []pgtype.UUID{store.UUID(epID)},
	})
	if err != nil || len(dels) != 1 {
		t.Fatalf("deliveries: %v (%d)", err, len(dels))
	}
	return store.GoUUID(dels[0].ID)
}

// opMessageCounts returns the per-event-type message count on an op app.
func opMessageCounts(t *testing.T, q *store.Queries, appID uuid.UUID) map[string]int {
	t.Helper()
	msgs, err := q.ListMessagesByApp(context.Background(), store.ListMessagesByAppParams{
		AppID:    store.UUID(appID),
		CursorTs: pgtype.Timestamptz{Time: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), Valid: true},
		CursorID: store.UUID(uuid.Max),
		Lim:      100,
	})
	if err != nil {
		t.Fatalf("list op messages: %v", err)
	}
	byType := map[string]int{}
	for _, m := range msgs {
		byType[m.EventType]++
	}
	return byType
}

// A normal delivery that dead-letters AND crosses the auto-disable threshold
// emits both operational events (message.attempt.exhausted + endpoint.disabled)
// to its org's operational app.
func TestExhaustEmitsOperationalEvents(t *testing.T) {
	q := testPool(t)
	dq, _, _ := testQueue(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	secret, _ := GenerateSecret()

	// NORMAL app + endpoint + message + delivery.
	delID, _, orgID, _, _ := seedDeliveryFull(t, q, srv.URL, secret)
	// The org's operational app + an endpoint on it, so op events fan out.
	opAppID, err := opevents.SeedOperationalApp(ctx, q, orgID)
	if err != nil {
		t.Fatalf("seed op app: %v", err)
	}
	if _, err := q.CreateEndpoint(ctx, store.CreateEndpointParams{
		AppID: store.UUID(opAppID), OrgID: store.UUID(orgID),
		Url: "https://example.test/ops", Secret: secret, Headers: []byte("{}"),
	}); err != nil {
		t.Fatalf("op endpoint: %v", err)
	}

	// Burn the retry budget so this Process dead-letters in one shot; threshold 1
	// so the same failure also auto-disables the endpoint.
	for i := 0; i < len(retrySchedule); i++ {
		_ = q.MarkDeliveryInFlight(ctx, store.UUID(delID))
	}
	enqueueDelivery(t, dq, delID, orgID)
	h := Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true), MaxConsecutiveFailures: 1}
	raw, p, ok, _ := dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}

	byType := opMessageCounts(t, q, opAppID)
	if byType["message.attempt.exhausted"] != 1 {
		t.Fatalf("want 1 message.attempt.exhausted op message, got %d", byType["message.attempt.exhausted"])
	}
	if byType["endpoint.disabled"] != 1 {
		t.Fatalf("want exactly 1 endpoint.disabled op message, got %d", byType["endpoint.disabled"])
	}
}

// LOOP GUARD: a delivery whose endpoint lives on the OPERATIONAL app, driven to
// exhaustion + auto-disable, must emit NO new operational message (else op
// events would recurse forever).
func TestOperationalDeliveryDoesNotRecurse(t *testing.T) {
	q := testPool(t)
	dq, _, _ := testQueue(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	secret, _ := GenerateSecret()

	// org + its operational app + an endpoint ON the op app (failing server).
	u, err := q.CreateUser(ctx, store.CreateUserParams{Email: "t+" + uuid.NewString() + "@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{Name: "T", Slug: "t-" + uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	_ = q.AddOrgMember(ctx, store.AddOrgMemberParams{OrgID: o.ID, UserID: u.ID, Role: "owner"})
	orgID := store.GoUUID(o.ID)
	opAppID, err := opevents.SeedOperationalApp(ctx, q, orgID)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := q.CreateEndpoint(ctx, store.CreateEndpointParams{
		AppID: store.UUID(opAppID), OrgID: o.ID, Url: srv.URL, Secret: secret, Headers: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// One message on the op app + a delivery to the op endpoint.
	msg, err := q.CreateMessage(ctx, store.CreateMessageParams{
		AppID: store.UUID(opAppID), OrgID: o.ID, EventType: "endpoint.disabled",
		Payload: []byte(`{"x":1}`), PayloadHash: "h",
	})
	if err != nil {
		t.Fatal(err)
	}
	dels, err := q.CreateMessageDeliveriesBatch(ctx, store.CreateMessageDeliveriesBatchParams{
		MessageID: msg.ID, OrgID: o.ID, EndpointIds: []pgtype.UUID{ep.ID},
	})
	if err != nil || len(dels) != 1 {
		t.Fatalf("deliveries: %v (%d)", err, len(dels))
	}
	delID := store.GoUUID(dels[0].ID)

	total := func(m map[string]int) int {
		s := 0
		for _, v := range m {
			s += v
		}
		return s
	}
	before := total(opMessageCounts(t, q, opAppID)) // exactly the 1 message seeded

	// Drive it to exhaustion + would-be auto-disable.
	for i := 0; i < len(retrySchedule); i++ {
		_ = q.MarkDeliveryInFlight(ctx, store.UUID(delID))
	}
	enqueueDelivery(t, dq, delID, orgID)
	h := Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true), MaxConsecutiveFailures: 1}
	raw, p, ok, _ := dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}

	after := total(opMessageCounts(t, q, opAppID))
	if after != before {
		t.Fatalf("operational delivery must not create new op messages: before=%d after=%d", before, after)
	}
}

// nullMessagePayload expunges one message's payload directly, simulating the
// retention sweep having nulled it.
func nullMessagePayload(t *testing.T, msgID uuid.UUID) {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("DSTREAM_TEST_DB_URL"), 2)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(), `UPDATE messages SET payload = NULL WHERE id = $1`, store.UUID(msgID)); err != nil {
		t.Fatalf("null payload: %v", err)
	}
}

// A delivery whose message payload was expunged by retention must dead-letter
// with no send and an attempt recorded, never sign+POST an empty body.
func TestDeliverExpungedPayloadDeadLetters(t *testing.T) {
	q := testPool(t)
	dq, rdb, prefix := testQueue(t)
	ctx := context.Background()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	secret, _ := GenerateSecret()
	delID, msgID, orgID := seedDelivery(t, q, srv.URL, secret)
	nullMessagePayload(t, msgID)
	enqueueDelivery(t, dq, delID, orgID)

	h := Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true)}
	raw, p, ok, _ := dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatalf("expunged payload must not send, got %d hits", hits)
	}
	if n, _ := rdb.LLen(ctx, prefix+":dead").Result(); n != 1 {
		t.Fatalf("want 1 dead-lettered, got %d", n)
	}
	row, err := q.GetMessageDeliveryForSend(ctx, store.UUID(delID))
	if err != nil {
		t.Fatal(err)
	}
	if row.DeliveryStatus != "dead" {
		t.Fatalf("want delivery status dead, got %q", row.DeliveryStatus)
	}
	atts, err := q.ListAttemptsByMessage(ctx, store.ListAttemptsByMessageParams{MessageID: store.UUID(msgID), Lim: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range atts {
		if a.ErrorMessage != nil && strings.Contains(*a.ErrorMessage, "expunged") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an attempt recorded with the expunged error, got %d attempts", len(atts))
	}
}

// execRaw runs one SQL statement against the test DB, for seeding columns the
// store's typed queries don't expose (filter_expr/transform_js, custom payload).
// Mirrors nullMessagePayload's fresh-pool pattern.
func execRaw(t *testing.T, sql string, args ...any) {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("DSTREAM_TEST_DB_URL"), 2)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// A non-matching endpoint filter drops the delivery: status 'filtered', no send.
func TestOutboundFilteredNoMatch(t *testing.T) {
	q := testPool(t)
	dq, _, _ := testQueue(t)
	ctx := context.Background()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	secret, _ := GenerateSecret()
	delID, msgID, orgID, epID, _ := seedDeliveryFull(t, q, srv.URL, secret)
	// payload.amount=1 fails `payload.amount > 1000` → filtered (the field must
	// exist, or a missing-key eval error would fail open and deliver).
	execRaw(t, `UPDATE messages SET payload=$1 WHERE id=$2`, []byte(`{"amount":1}`), store.UUID(msgID))
	execRaw(t, `UPDATE endpoints SET filter_expr='payload.amount > 1000' WHERE id=$1`, store.UUID(epID))
	enqueueDelivery(t, dq, delID, orgID)

	h := Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true)}
	raw, p, ok, _ := dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatalf("filtered delivery must not send, got %d hits", hits)
	}
	row, err := q.GetMessageDeliveryForSend(ctx, store.UUID(delID))
	if err != nil {
		t.Fatal(err)
	}
	if row.DeliveryStatus != "filtered" {
		t.Fatalf("want status filtered, got %q", row.DeliveryStatus)
	}
}

// THE load-bearing test: the transform mutates the payload BEFORE signing, so
// the webhook-signature the receiver gets verifies over the MUTATED bytes.
func TestOutboundTransformSignedAfterMutation(t *testing.T) {
	q := testPool(t)
	dq, _, _ := testQueue(t)
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
	delID, msgID, orgID, epID, _ := seedDeliveryFull(t, q, srv.URL, secret)
	// reshape: add {"tagged":true} to the object.
	js := `function transform(payload, headers) { payload.tagged = true; return payload; }`
	execRaw(t, `UPDATE endpoints SET transform_js=$1 WHERE id=$2`, js, store.UUID(epID))
	enqueueDelivery(t, dq, delID, orgID)

	h := Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true)}
	raw, p, ok, _ := dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}
	if gotID != msgID.String() {
		t.Fatalf("webhook-id: got %q want %q", gotID, msgID)
	}
	// (a) captured body is the MUTATED JSON, not the original {"x":1}.
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, gotBody)
	}
	if body["tagged"] != true {
		t.Fatalf("delivered body was not transformed: %s", gotBody)
	}
	// (b) the signature verifies over the transformed bytes.
	var ts int64
	_, _ = fmtSscan(gotTs, &ts)
	want, _ := Sign(secret, msgID.String(), ts, gotBody)
	if gotSig != want {
		t.Fatalf("signature not over transformed bytes: got %q want %q", gotSig, want)
	}
}

func TestOutboundInflightDefers(t *testing.T) {
	q := testPool(t)
	dq, rdb, prefix := testQueue(t)
	ctx := context.Background()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	secret, _ := GenerateSecret()
	delID, _, orgID, _, _ := seedDeliveryFull(t, q, srv.URL, secret)

	// pre-fill the org's in-flight counter to the cap → next delivery must defer
	rdb.Set(ctx, "inflight:org:"+orgID.String(), 1, time.Minute)
	enqueueDelivery(t, dq, delID, orgID)
	h := Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true), Redis: rdb, PerOrgMaxInflight: 1}
	raw, p, ok, _ := dq.FairPick(ctx, 10000)
	if !ok {
		t.Fatal("fairpick")
	}
	if err := h.Process(ctx, p, raw, dq); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatalf("over-cap delivery must NOT hit the endpoint, got %d hits", hits)
	}
	if n, _ := rdb.ZCard(ctx, prefix+":scheduled").Result(); n != 1 {
		t.Fatalf("deferred delivery should be rescheduled, scheduled=%d", n)
	}
	if n, _ := rdb.ZCard(ctx, prefix+":processing").Result(); n != 0 {
		t.Fatalf("deferred delivery should be acked, processing=%d", n)
	}
}
