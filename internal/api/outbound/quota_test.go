package outbound

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/opevents"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"
)

// quotaEnv is the publish path wired to a real quota gate.
type quotaEnv struct {
	q    *store.Queries
	pool *pgxpool.Pool
	rdb  *redis.Client
	gate *usage.Gate
	uid  uuid.UUID
	oid  uuid.UUID
	base string // /api/applications/{id}
	post func(path string, body any) *httptest.ResponseRecorder
}

// newQuotaEnv seeds an org with the given messages quota pair + period, an
// application and one registered event type, and returns the publish helper.
// gateRedis is the client the gate counts on — a dead one exercises fail-open.
func newQuotaEnv(t *testing.T, soft, hard int64, period string, gateRedis *redis.Client) *quotaEnv {
	t.Helper()
	pool := testPool(t)
	q := store.New(pool)
	rdb := testRedis(t)
	dq := dqueue.NewClient(rdb).WithPrefix("quotatest-" + uuidNewShort())
	uid, oid := seedOrg(t, q)

	if _, err := pool.Exec(context.Background(),
		`UPDATE organizations
		    SET quota_messages_soft = $2, quota_messages_hard = $3, quota_period = $4
		  WHERE id = $1`,
		store.UUID(oid), soft, hard, period); err != nil {
		t.Fatalf("set quotas: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		keys, _ := rdb.Keys(ctx, "usage:*"+oid.String()+"*").Result()
		if len(keys) > 0 {
			rdb.Del(ctx, keys...)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, store.UUID(oid))
	})

	if gateRedis == nil {
		gateRedis = rdb
	}
	gate := &usage.Gate{Log: discardLog(), Queries: q, Redis: gateRedis, Queue: dq}
	// Load the snapshot synchronously so the first request under test sees the
	// limits; in production it refreshes off the request path on a TTL.
	if err := gate.Reload(context.Background()); err != nil {
		t.Fatalf("gate reload: %v", err)
	}

	h := Handlers{Log: discardLog(), Queries: q, Pool: pool, Queue: dq, Quota: gate}
	r := newRouterWith(h, sign(t), msgRoutes)
	post := func(path string, body any) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, path, uid, oid, body))
		return rec
	}

	post("/api/event-types", map[string]any{"name": "invoice.paid"})
	var app map[string]any
	_ = json.Unmarshal(post("/api/applications", map[string]any{"name": "A"}).Body.Bytes(), &app)
	appID, ok := app["id"].(string)
	if !ok {
		t.Fatalf("create app: %v", app)
	}
	return &quotaEnv{
		q: q, pool: pool, rdb: rdb, gate: gate, uid: uid, oid: oid,
		base: "/api/applications/" + appID, post: post,
	}
}

func (e *quotaEnv) publish(n int) *httptest.ResponseRecorder {
	return e.post(e.base+"/messages", map[string]any{
		"event_type": "invoice.paid", "payload": map[string]any{"n": n},
	})
}

func (e *quotaEnv) counter(t *testing.T, period string) int64 {
	t.Helper()
	key := usage.CounterKey(e.oid, "messages", usage.PeriodStart(time.Now(), period))
	v, err := e.rdb.Get(context.Background(), key).Int64()
	if err == redis.Nil {
		return -1
	}
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return v
}

func (e *quotaEnv) opMessages(t *testing.T) map[string]int {
	t.Helper()
	ctx := context.Background()
	appID, err := opevents.SeedOperationalApp(ctx, e.q, e.oid)
	if err != nil {
		t.Fatalf("seed op app: %v", err)
	}
	msgs, err := e.q.ListMessagesByApp(ctx, store.ListMessagesByAppParams{
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

// Over soft on publish: accepted, with one warning per period.
func TestPublishQuota_OverSoft_AcceptedAndAlertsOnce(t *testing.T) {
	e := newQuotaEnv(t, 1, 0, "month", nil) // soft 1, no ceiling
	for i := 1; i <= 2; i++ {
		if rec := e.publish(i); rec.Code != http.StatusAccepted {
			t.Fatalf("publish %d: status = %d, want 202; body=%s", i, rec.Code, rec.Body.String())
		}
	}
	e.gate.WaitAlerts()
	m := e.opMessages(t)
	if m["usage.quota_warning"] != 1 {
		t.Errorf("usage.quota_warning = %d, want exactly 1", m["usage.quota_warning"])
	}
	if m["usage.quota_exceeded"] != 0 {
		t.Errorf("usage.quota_exceeded = %d, want 0 (no ceiling)", m["usage.quota_exceeded"])
	}
}

// At the ceiling on publish: 429 with Retry-After, and one quota_exceeded.
func TestPublishQuota_OverHard_429WithRetryAfter(t *testing.T) {
	e := newQuotaEnv(t, 1, 2, "month", nil)
	if rec := e.publish(1); rec.Code != http.StatusAccepted {
		t.Fatalf("first: status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	rec := e.publish(2)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second: status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("429 without Retry-After")
	}
	e.gate.WaitAlerts()
	if got := e.opMessages(t)["usage.quota_exceeded"]; got != 1 {
		t.Errorf("usage.quota_exceeded = %d, want exactly 1", got)
	}
}

// Unlimited (the default) never rejects, and costs no counter work.
func TestPublishQuota_ZeroLimitsNeverReject(t *testing.T) {
	e := newQuotaEnv(t, 0, 0, "month", nil)
	cur := usage.PeriodStart(time.Now(), "month")
	if err := usage.SetCounter(context.Background(), e.rdb, e.oid, "messages", cur, 10_000); err != nil {
		t.Fatalf("seed counter: %v", err)
	}
	if rec := e.publish(1); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if got := e.counter(t, "month"); got != 10_000 {
		t.Errorf("messages counter = %d, want 10000 untouched", got)
	}
	if m := e.opMessages(t); len(m) != 0 {
		t.Errorf("unlimited org must never be alerted, got %v", m)
	}
}

// Fail-open on the publish path too: a dead counter accepts.
func TestPublishQuota_RedisDown_Accepts(t *testing.T) {
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, PoolSize: 1})
	t.Cleanup(func() { _ = dead.Close() })
	e := newQuotaEnv(t, 1, 1, "month", dead)
	if rec := e.publish(1); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (quota must fail open); body=%s", rec.Code, rec.Body.String())
	}
}
