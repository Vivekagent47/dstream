package ingest

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/opevents"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"
)

// --- quota harness, layered on captureEnv (real DB + Redis, no connections) ---

func quotaLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// setQuotas writes this org's events quota pair and period straight onto the
// row. There is no query for it yet — the super-admin PATCH /admin/orgs/{org_id}/plan
// is a later task — and the gate reads these through ListOrgQuotas.
func (e *captureEnv) setQuotas(t *testing.T, soft, hard int64, period string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE organizations
		    SET quota_events_soft = $2, quota_events_hard = $3, quota_period = $4
		  WHERE id = $1`,
		e.org.ID, soft, hard, period); err != nil {
		t.Fatalf("set quotas: %v", err)
	}
}

// withQuota attaches a real gate to the handler and loads the limits snapshot
// synchronously, so the first request under test already sees the limits. In
// production the snapshot is refreshed off the request path on a TTL, which is
// exactly why the gate costs no query per request.
func (e *captureEnv) withQuota(t *testing.T, rdb *redis.Client) *usage.Gate {
	t.Helper()
	g := &usage.Gate{Log: quotaLog(), Queries: e.q, Redis: rdb, Queue: e.h.Queue}
	if err := g.Reload(context.Background()); err != nil {
		t.Fatalf("gate reload: %v", err)
	}
	e.h.Quota = g
	t.Cleanup(func() {
		ctx := context.Background()
		// The org cascades away, so the next sweep in the next run doesn't
		// re-count this test's rows, and the counters/latches it wrote don't
		// outlive it with a 62-day TTL.
		keys, _ := e.h.Redis.Keys(ctx, "usage:*"+e.orgID().String()+"*").Result()
		if len(keys) > 0 {
			e.h.Redis.Del(ctx, keys...)
		}
		_, _ = e.pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, e.org.ID)
	})
	return g
}

func (e *captureEnv) orgID() uuid.UUID { return store.GoUUID(e.org.ID) }

// counter reads the live events counter for the current period. -1 = no key.
func (e *captureEnv) counter(t *testing.T, metric, period string) int64 {
	t.Helper()
	key := usage.CounterKey(e.orgID(), metric, usage.PeriodStart(time.Now(), period))
	v, err := e.h.Redis.Get(context.Background(), key).Int64()
	if err == redis.Nil {
		return -1
	}
	if err != nil {
		t.Fatalf("read counter %s: %v", key, err)
	}
	return v
}

// opMessages counts the org's operational messages by event type.
func (e *captureEnv) opMessages(t *testing.T) map[string]int {
	t.Helper()
	ctx := context.Background()
	appID, err := opevents.SeedOperationalApp(ctx, e.q, e.orgID())
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

// deadRedis is a client pointed at a closed port: every command errors without
// retrying or waiting on a pool of connections that will never dial.
func deadRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, PoolSize: 1})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// --- the enforcement ladder, end to end through the real handler ---

// Below the soft limit: accepted, counted, and silent.
func TestIngestQuota_UnderSoft_Accepted(t *testing.T) {
	e := newCaptureEnv(t)
	e.setQuotas(t, 10, 100, "month")
	e.withQuota(t, e.h.Redis)

	if rec := e.post(t, `{"n":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if got := e.counter(t, "events", "month"); got != 1 {
		t.Errorf("events counter = %d, want 1", got)
	}
	if m := e.opMessages(t); m["usage.quota_warning"] != 0 || m["usage.quota_exceeded"] != 0 {
		t.Errorf("under the soft limit must alert nothing, got %v", m)
	}
}

// At or above the soft limit the request is still accepted — it is overage, not
// a rejection — and usage.quota_warning fires at most once per period however
// long the overage lasts.
func TestIngestQuota_OverSoft_AcceptedAndAlertsOnce(t *testing.T) {
	e := newCaptureEnv(t)
	e.setQuotas(t, 1, 0, "month") // soft 1, NO ceiling
	g := e.withQuota(t, e.h.Redis)

	// Distinct bodies: identical ones would be deduped, which is a different
	// code path and would hide nothing useful here.
	for i, body := range []string{`{"n":1}`, `{"n":2}`} {
		if rec := e.post(t, body); rec.Code != http.StatusAccepted {
			t.Fatalf("request %d: status = %d, want 202 (over soft is accepted); body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	g.WaitAlerts()

	m := e.opMessages(t)
	if m["usage.quota_warning"] != 1 {
		t.Errorf("usage.quota_warning = %d, want exactly 1 (latched once per period)", m["usage.quota_warning"])
	}
	if m["usage.quota_exceeded"] != 0 {
		t.Errorf("usage.quota_exceeded = %d, want 0 (no ceiling configured)", m["usage.quota_exceeded"])
	}
}

// At the hard ceiling: 429 with Retry-After, and usage.quota_exceeded once.
func TestIngestQuota_OverHard_429WithRetryAfter(t *testing.T) {
	e := newCaptureEnv(t)
	e.setQuotas(t, 1, 2, "month")
	g := e.withQuota(t, e.h.Redis)

	if rec := e.post(t, `{"n":1}`); rec.Code != http.StatusAccepted { // count 1 → over soft
		t.Fatalf("first: status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	rec := e.post(t, `{"n":2}`) // count 2 → at the ceiling
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second: status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("429 without Retry-After: senders that retry on 429 are the reason the ceiling is survivable")
	}
	if rec := e.post(t, `{"n":3}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third: status = %d, want 429", rec.Code)
	}
	g.WaitAlerts()

	m := e.opMessages(t)
	if m["usage.quota_exceeded"] != 1 {
		t.Errorf("usage.quota_exceeded = %d, want exactly 1", m["usage.quota_exceeded"])
	}
	if m["usage.quota_warning"] != 1 {
		t.Errorf("usage.quota_warning = %d, want 1 (fired on the way past the soft limit)", m["usage.quota_warning"])
	}
}

// 0 means unlimited on a tier, regardless of what an org's defaults are: an
// org explicitly configured with 0/0 must reject nothing, however high the
// count.
func TestIngestQuota_ZeroLimitsNeverReject(t *testing.T) {
	e := newCaptureEnv(t)
	// Zeros set explicitly, not inherited: the organizations defaults are
	// the free tier (8000/10000) since plan presets landed, so an org that
	// configures nothing is limited, not unlimited. 0 still means unlimited
	// per tier — that is what this test pins.
	e.setQuotas(t, 0, 0, "month")
	e.withQuota(t, e.h.Redis)
	cur := usage.PeriodStart(time.Now(), "month")
	if err := usage.SetCounter(context.Background(), e.h.Redis, e.orgID(), "events", cur, 10_000); err != nil {
		t.Fatalf("seed counter: %v", err)
	}

	if rec := e.post(t, `{"n":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 with unlimited quotas; body=%s", rec.Code, rec.Body.String())
	}
	if m := e.opMessages(t); len(m) != 0 {
		t.Errorf("unlimited org must never be alerted, got %v", m)
	}
	// An unlimited org also pays no Redis cost: the counter is untouched.
	if got := e.counter(t, "events", "month"); got != 10_000 {
		t.Errorf("events counter = %d, want 10000 untouched (no limit configured → no counter work)", got)
	}
}

// Fail-open: refusing webhooks because an internal cache is down is a worse
// outage than the overage, so a Redis error accepts. Limits here would reject
// on the very first request if the counter could be read at all.
func TestIngestQuota_RedisDown_Accepts(t *testing.T) {
	e := newCaptureEnv(t)
	e.setQuotas(t, 1, 1, "month")
	e.withQuota(t, deadRedis(t)) // gate's Redis is dead; the handler's is live

	if rec := e.post(t, `{"n":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (quota must fail open); body=%s", rec.Code, rec.Body.String())
	}
}

// Attempts are metered but NEVER enforced: rejecting a retry because the
// customer's own endpoint is down punishes them for their outage. The gate is
// active for this org, its attempts and requests meters are astronomically
// over, and nothing is refused — enforcement neither reads nor writes them.
func TestAttemptsAreNeverEnforced(t *testing.T) {
	e := newCaptureEnv(t)
	e.setQuotas(t, 5, 10, "month") // gate ACTIVE, events count still 0
	e.withQuota(t, e.h.Redis)

	ctx := context.Background()
	cur := usage.PeriodStart(time.Now(), "month")
	for _, metric := range []string{"attempts", "requests"} {
		if err := usage.SetCounter(ctx, e.h.Redis, e.orgID(), metric, cur, 1_000_000_000); err != nil {
			t.Fatalf("seed %s counter: %v", metric, err)
		}
	}

	if rec := e.post(t, `{"n":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (attempts/requests are not gating metrics); body=%s", rec.Code, rec.Body.String())
	}
	for _, metric := range []string{"attempts", "requests"} {
		if got := e.counter(t, metric, "month"); got != 1_000_000_000 {
			t.Errorf("%s counter = %d, want it untouched at 1000000000", metric, got)
		}
	}
}

// Ruling #1: the gate must derive the period from organizations.quota_period.
// A 'day' org whose gate computed a hardcoded "month" key would read a key the
// sweep never reconciles, so the counter would drift upward only and surface as
// spurious 429s with nothing in the logs.
func TestIngestQuota_UsesOrgQuotaPeriod(t *testing.T) {
	e := newCaptureEnv(t)
	e.setQuotas(t, 10, 100, "day")
	e.withQuota(t, e.h.Redis)

	if rec := e.post(t, `{"n":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if got := e.counter(t, "events", "day"); got != 1 {
		t.Errorf("day-period counter = %d, want 1 — the gate must key by the org's own quota_period", got)
	}
	day, month := usage.PeriodStart(time.Now(), "day"), usage.PeriodStart(time.Now(), "month")
	if !day.Equal(month) { // identical on the 1st of the month
		if got := e.counter(t, "events", "month"); got != -1 {
			t.Errorf("month-period counter = %d, want no key at all for a 'day' org", got)
		}
	}
}
