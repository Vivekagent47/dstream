package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

// Front-door tests: every inbound webhook lands in handleIngest, so these
// drive it through the router with a real Postgres, real Redis and the real
// queue, and assert the status, the stored rows and the queue lane.

// logBuf is a goroutine-safe slog sink so a test can assert on what the
// handler logged for a path that returns 202 regardless.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *logBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func (e *captureEnv) logs() *logBuf {
	lb := &logBuf{}
	e.h.Log = slog.New(slog.NewTextHandler(lb, nil))
	return lb
}

// addConn gives the source one enabled connection, so a request fans out to
// one event. The org (and everything under it) is removed on cleanup.
func (e *captureEnv) addConn(t *testing.T) store.Connection {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, e.org.ID) })
	u := "https://example.invalid/hook"
	d, err := e.q.CreateDestination(ctx, store.CreateDestinationParams{
		OrgID: e.org.ID, Name: "d-" + uuid.NewString(), Type: "http", Url: &u, AuthConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("destination: %v", err)
	}
	c, err := e.q.CreateConnection(ctx, store.CreateConnectionParams{SourceID: e.src.ID, DestinationID: d.ID, Enabled: true})
	if err != nil {
		t.Fatalf("connection: %v", err)
	}
	return c
}

func (e *captureEnv) postHdr(t *testing.T, method, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/e/"+e.src.IngestToken, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *captureEnv) count(t *testing.T, sql string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, e.src.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func (e *captureEnv) requests(t *testing.T) int {
	return e.count(t, `SELECT count(*) FROM requests WHERE source_id = $1`)
}

func (e *captureEnv) events(t *testing.T) int {
	return e.count(t, `SELECT count(*) FROM events ev JOIN requests r ON r.id = ev.request_id WHERE r.source_id = $1`)
}

func (e *captureEnv) pending(t *testing.T) int64 {
	t.Helper()
	s, err := e.h.Queue.Stats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	return s.Pending
}

func (e *captureEnv) dedupKeys(t *testing.T) []string {
	t.Helper()
	k, err := e.h.Redis.Keys(context.Background(), "dedup:"+store.GoUUID(e.src.ID).String()+":*").Result()
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	return k
}

func TestIngest_FansOutOneEventPerConnectionAndIsNotTest(t *testing.T) {
	e := newCaptureEnv(t)
	e.addConn(t)
	rec := e.postHdr(t, http.MethodPost, `{"a":1}`, nil) // no Content-Type: stored as NULL
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; %s", rec.Code, rec.Body)
	}
	if e.requests(t) != 1 || e.events(t) != 1 {
		t.Fatalf("rows: requests=%d events=%d, want 1/1", e.requests(t), e.events(t))
	}
	if n := e.count(t, `SELECT count(*) FROM events ev JOIN requests r ON r.id = ev.request_id
		WHERE r.source_id = $1 AND ev.is_test = false AND ev.status = 'queued' AND r.content_type IS NULL`); n != 1 {
		t.Fatalf("fresh ingest must create a queued, non-test event for a request with NULL content_type; got %d", n)
	}
	if got := e.pending(t); got != 1 {
		t.Fatalf("pending lane = %d, want 1", got)
	}
}

func TestIngest_HopLimit(t *testing.T) {
	cases := []struct {
		name string
		max  int
		hdr  map[string]string
		want int
	}{
		{"below limit", 3, map[string]string{"Dstream-Webhook-Hops": "2"}, http.StatusAccepted},
		{"at limit", 3, map[string]string{"Dstream-Webhook-Hops": "3"}, http.StatusForbidden},
		{"above limit", 3, map[string]string{"Dstream-Webhook-Hops": "4"}, http.StatusForbidden},
		{"missing header", 3, nil, http.StatusAccepted},
		{"malformed header counts as zero", 3, map[string]string{"Dstream-Webhook-Hops": "lots"}, http.StatusAccepted},
		{"negative header counts as zero", 3, map[string]string{"Dstream-Webhook-Hops": "-9"}, http.StatusAccepted},
		{"guard disabled at 0", 0, map[string]string{"Dstream-Webhook-Hops": "999"}, http.StatusAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCaptureEnv(t)
			e.addConn(t)
			e.h.MaxWebhookHops = tc.max
			lb := e.logs()
			rec := e.postHdr(t, http.MethodPost, `{"x":1}`, tc.hdr)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; %s", rec.Code, tc.want, rec.Body)
			}
			wantRows := 0
			if tc.want == http.StatusForbidden {
				if !strings.Contains(rec.Body.String(), "loop detected") {
					t.Errorf("body = %q, want loop detected message", rec.Body)
				}
				if !strings.Contains(lb.String(), "loop guard tripped") {
					t.Errorf("trip not logged: %s", lb)
				}
			} else {
				wantRows = 1
			}
			// The guard fires before the body is read: a refused request leaves
			// no request row, no event, nothing queued and no dedup claim.
			if e.requests(t) != wantRows || e.events(t) != wantRows || e.pending(t) != int64(wantRows) {
				t.Errorf("requests=%d events=%d pending=%d, want %d each", e.requests(t), e.events(t), e.pending(t), wantRows)
			}
			if got := len(e.dedupKeys(t)); got != wantRows {
				t.Errorf("dedup keys = %d, want %d", got, wantRows)
			}
		})
	}
}

func TestIngest_RateLimit_AcceptsThenRefuses(t *testing.T) {
	e := newCaptureEnv(t)
	e.addConn(t)
	sid := store.GoUUID(e.src.ID).String()
	t.Cleanup(func() { _ = e.h.Redis.Del(context.Background(), "rate:ingest:src:"+sid).Err() })
	e.h.Limiter = redis_rate.NewLimiter(e.h.Redis)
	e.h.RateLimitRPS = 2 // Burst unset: falls back to RPS
	for i := 0; i < 2; i++ {
		if rec := e.postHdr(t, http.MethodPost, fmt.Sprintf(`{"i":%d}`, i), nil); rec.Code != http.StatusAccepted {
			t.Fatalf("req %d status = %d, want 202", i, rec.Code)
		}
	}
	rec := e.postHdr(t, http.MethodPost, `{"i":3}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit status = %d, want 429", rec.Code)
	}
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 1 || ra > 2 {
		t.Errorf("Retry-After = %q, want 1..2 seconds", rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Errorf("body = %q", rec.Body)
	}
	if e.requests(t) != 2 || e.events(t) != 2 || e.pending(t) != 2 {
		t.Errorf("refused request left state: requests=%d events=%d pending=%d, want 2 each", e.requests(t), e.events(t), e.pending(t))
	}
}

func TestIngest_RateLimit_ExplicitBurstAdmitsBurst(t *testing.T) {
	e := newCaptureEnv(t)
	sid := store.GoUUID(e.src.ID).String()
	t.Cleanup(func() { _ = e.h.Redis.Del(context.Background(), "rate:ingest:src:"+sid).Err() })
	e.h.Limiter = redis_rate.NewLimiter(e.h.Redis)
	e.h.RateLimitRPS, e.h.RateLimitBurst = 1, 3
	for i := 0; i < 3; i++ {
		if rec := e.postHdr(t, http.MethodPost, fmt.Sprintf(`{"i":%d}`, i), nil); rec.Code != http.StatusAccepted {
			t.Fatalf("burst req %d status = %d, want 202", i, rec.Code)
		}
	}
	if rec := e.postHdr(t, http.MethodPost, `{"i":4}`, nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("past burst status = %d, want 429", rec.Code)
	}
}

func TestIngest_RateLimiterDown_FailsOpenAndLogs(t *testing.T) {
	e := newCaptureEnv(t)
	e.addConn(t)
	lb := e.logs()
	e.h.Limiter = redis_rate.NewLimiter(deadRedis(t))
	e.h.RateLimitRPS = 1
	for i := 0; i < 3; i++ { // would be refused by a working limiter
		if rec := e.postHdr(t, http.MethodPost, fmt.Sprintf(`{"i":%d}`, i), nil); rec.Code != http.StatusAccepted {
			t.Fatalf("req %d status = %d, want 202 (fail-open)", i, rec.Code)
		}
	}
	if !strings.Contains(lb.String(), "rate limiter error (fail-open)") {
		t.Errorf("limiter outage not logged: %s", lb)
	}
	if e.events(t) != 3 {
		t.Errorf("events = %d, want 3", e.events(t))
	}
}

func TestIngest_OversizedBodyIs413AndStoresNothing(t *testing.T) {
	e := newCaptureEnv(t)
	e.addConn(t)
	rec := e.postHdr(t, http.MethodPost, strings.Repeat("x", MaxBodyBytes+1), nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if e.requests(t) != 0 || e.events(t) != 0 || len(e.dedupKeys(t)) != 0 || e.pending(t) != 0 {
		t.Errorf("oversize left state: requests=%d events=%d dedup=%v", e.requests(t), e.events(t), e.dedupKeys(t))
	}
	// Exactly at the cap is accepted.
	if rec := e.postHdr(t, http.MethodPost, strings.Repeat("y", MaxBodyBytes), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("body at cap: status = %d, want 202", rec.Code)
	}
}

func TestIngest_UnknownAndDisabledSourceAre404(t *testing.T) {
	e := newCaptureEnv(t)
	e.addConn(t)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/e/no-such-"+uuid.NewString(), strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unknown source") {
		t.Fatalf("unknown token: %d %q, want 404 unknown source", rec.Code, rec.Body)
	}

	// Resolves and caches while enabled...
	if rec := e.postHdr(t, http.MethodPost, `{"n":1}`, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("enabled source status = %d", rec.Code)
	}
	off := false
	if _, err := e.q.UpdateSource(context.Background(), store.UpdateSourceParams{ID: e.src.ID, OrgID: e.org.ID, Enabled: &off}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	// ...the cache keeps serving it until invalidated (documented TTL trade-off)...
	if rec := e.postHdr(t, http.MethodPost, `{"n":2}`, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("cached disabled source status = %d, want 202 until invalidated", rec.Code)
	}
	// ...and InvalidateSource makes the disable take effect at once.
	e.h.InvalidateSource(e.src.IngestToken)
	if rec := e.postHdr(t, http.MethodPost, `{"n":3}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled source status = %d, want 404", rec.Code)
	}
	if e.requests(t) != 2 {
		t.Errorf("requests = %d, want 2 (the 404 stored nothing)", e.requests(t))
	}
}

func TestIngest_MethodNotAllowed(t *testing.T) {
	e := newCaptureEnv(t)
	if _, err := e.q.UpdateSource(context.Background(), store.UpdateSourceParams{ID: e.src.ID, OrgID: e.org.ID, AllowedMethods: []string{"POST"}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if rec := e.postHdr(t, http.MethodPut, `{}`, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT status = %d, want 405", rec.Code)
	}
	if rec := e.postHdr(t, http.MethodPost, `{}`, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", rec.Code)
	}
	if e.requests(t) != 1 {
		t.Errorf("requests = %d, want 1", e.requests(t))
	}
}

func TestIngest_Dedup_SameBodyOnceInsideWindowTwiceOutside(t *testing.T) {
	e := newCaptureEnv(t)
	e.addConn(t)
	first := e.postHdr(t, http.MethodPost, `{"same":true}`, nil)
	dup := e.postHdr(t, http.MethodPost, `{"same":true}`, nil)
	other := e.postHdr(t, http.MethodPost, `{"same":false}`, nil)
	for _, r := range []*httptest.ResponseRecorder{first, dup, other} {
		if r.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", r.Code)
		}
	}
	if !strings.Contains(dup.Body.String(), `"deduped":true`) || !strings.Contains(dup.Body.String(), `"event_ids":null`) {
		t.Errorf("duplicate body = %s, want deduped with no event ids", dup.Body)
	}
	if strings.Contains(first.Body.String(), "deduped") {
		t.Errorf("first body flagged deduped: %s", first.Body)
	}
	// Inside the window: the duplicate still records its request (replayable)
	// but creates no events and enqueues nothing: 3 requests, 2 events.
	if e.requests(t) != 3 || e.events(t) != 2 || e.pending(t) != 2 {
		t.Fatalf("inside window: requests=%d events=%d pending=%d, want 3/2/2", e.requests(t), e.events(t), e.pending(t))
	}

	// The marker lives for the 60s window; assert that, then let it lapse for
	// real by shortening the TTL rather than sleeping the window.
	keys := e.dedupKeys(t)
	if len(keys) != 2 {
		t.Fatalf("dedup keys = %v, want 2 (one per distinct body)", keys)
	}
	ttl, err := e.h.Redis.PTTL(context.Background(), keys[0]).Result()
	if err != nil || ttl <= 55*time.Second || ttl > DedupWindow {
		t.Fatalf("dedup TTL = %v err=%v, want within (55s, %v]", ttl, err, DedupWindow)
	}
	for _, k := range keys {
		if err := e.h.Redis.PExpire(context.Background(), k, 20*time.Millisecond).Err(); err != nil {
			t.Fatalf("pexpire: %v", err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(e.dedupKeys(t)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("dedup keys did not expire")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec := e.postHdr(t, http.MethodPost, `{"same":true}`, nil); strings.Contains(rec.Body.String(), "deduped") {
		t.Fatalf("outside the window the same body must not be deduped: %s", rec.Body)
	}
	if e.events(t) != 3 || e.pending(t) != 3 {
		t.Errorf("outside window: events=%d pending=%d, want 3/3", e.events(t), e.pending(t))
	}
}

// The dedup key covers method+path+body: same body under a different method is
// a distinct request and must be accepted; identical method+body still dedups.
func TestIngest_Dedup_KeyIncludesMethod(t *testing.T) {
	e := newCaptureEnv(t)
	e.addConn(t)
	const body = `{"same":true}`
	post := e.postHdr(t, http.MethodPost, body, nil)
	put := e.postHdr(t, http.MethodPut, body, nil)
	del := e.postHdr(t, http.MethodDelete, body, nil)
	for name, r := range map[string]*httptest.ResponseRecorder{"post": post, "put": put, "delete": del} {
		if r.Code != http.StatusAccepted || strings.Contains(r.Body.String(), "deduped") {
			t.Errorf("%s: status=%d body=%s, want accepted and not deduped", name, r.Code, r.Body)
		}
	}
	if dup := e.postHdr(t, http.MethodPut, body, nil); !strings.Contains(dup.Body.String(), `"deduped":true`) {
		t.Errorf("same method+body repeat = %s, want deduped", dup.Body)
	}
	if e.events(t) != 3 {
		t.Errorf("events = %d, want 3", e.events(t))
	}
}

func TestIngest_DedupDown_FailsOpenAndLogs(t *testing.T) {
	e := newCaptureEnv(t)
	e.addConn(t)
	lb := e.logs()
	e.h.Redis = deadRedis(t) // queue keeps its own live client
	for i := 0; i < 2; i++ {
		if rec := e.postHdr(t, http.MethodPost, `{"same":1}`, nil); rec.Code != http.StatusAccepted || strings.Contains(rec.Body.String(), "deduped") {
			t.Fatalf("req %d: %d %s, want plain 202", i, rec.Code, rec.Body)
		}
	}
	if !strings.Contains(lb.String(), "dedup check failed (ignored)") {
		t.Errorf("dedup outage not logged: %s", lb)
	}
	if e.events(t) != 2 {
		t.Errorf("events = %d, want 2 (no dedup without Redis)", e.events(t))
	}
}

func TestIngest_NoConnections_PersistsRequestAndBody(t *testing.T) {
	e := newCaptureEnv(t)
	rec := e.postHdr(t, http.MethodPost, `{"keep":"me"}`, map[string]string{"Authorization": "secret"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	var id pgtype.UUID
	var ref string
	var size int32
	if err := e.pool.QueryRow(context.Background(), `SELECT id, body_ref, body_size FROM requests WHERE source_id = $1`, e.src.ID).Scan(&id, &ref, &size); err != nil {
		t.Fatalf("request row: %v", err)
	}
	body, err := e.h.BodyStore.Get(context.Background(), ref)
	if err != nil || string(body) != `{"keep":"me"}` || size != int32(len(body)) {
		t.Fatalf("stored body = %q err=%v size=%d", body, err, size)
	}
	if e.events(t) != 0 || e.pending(t) != 0 {
		t.Errorf("events=%d pending=%d, want 0", e.events(t), e.pending(t))
	}
	if len(e.dedupKeys(t)) != 1 {
		t.Errorf("dedup key must be kept on the zero-connection success path")
	}
}

// --- failure paths, with real faults ---

// A source row deleted while its cache entry is still live: the request insert
// hits the real FK and fails, the sender gets a 500, and the dedup claim is
// rolled back so their retry is not silently deduped.
func TestIngest_CreateRequestFails_500AndDedupRolledBack(t *testing.T) {
	e := newCaptureEnv(t)
	e.addConn(t)
	if rec := e.postHdr(t, http.MethodPost, `{"warm":1}`, nil); rec.Code != http.StatusAccepted { // fills the cache
		t.Fatalf("warm status = %d", rec.Code)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM sources WHERE id = $1`, e.src.ID); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	rec := e.postHdr(t, http.MethodPost, `{"doomed":1}`, nil)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "internal error") {
		t.Fatalf("status = %d %q, want 500 internal error", rec.Code, rec.Body)
	}
	hash := fmt.Sprintf("%x", sha256sum(`{"doomed":1}`))
	if n, _ := e.h.Redis.Exists(context.Background(), dedupKey(store.GoUUID(e.src.ID), hash)).Result(); n != 0 {
		t.Fatal("dedup key must be rolled back after a failed insert")
	}
}

func TestIngest_BodyStoreFails_500KeepsNoEventsAndRollsBackDedup(t *testing.T) {
	e := newCaptureEnv(t)
	e.addConn(t)
	e.h.BodyStore = NewPostgresBodyStore(store.New(closedPool(t)))
	lb := e.logs()
	rec := e.postHdr(t, http.MethodPost, `{"b":1}`, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(lb.String(), "ingest: store body") {
		t.Errorf("store failure not logged: %s", lb)
	}
	if e.events(t) != 0 || e.pending(t) != 0 || len(e.dedupKeys(t)) != 0 {
		t.Errorf("events=%d pending=%d dedup=%v, want none", e.events(t), e.pending(t), e.dedupKeys(t))
	}
	// The sender's retry (store healthy again) is not deduped and succeeds.
	e.h.BodyStore = NewPostgresBodyStore(e.q)
	if rec := e.postHdr(t, http.MethodPost, `{"b":1}`, nil); rec.Code != http.StatusAccepted || strings.Contains(rec.Body.String(), "deduped") {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	if e.events(t) != 1 {
		t.Errorf("events after retry = %d, want 1", e.events(t))
	}
}

func TestIngest_EnqueueFails_EventStaysQueuedForReaper(t *testing.T) {
	e := newCaptureEnv(t)
	e.addConn(t)
	lb := e.logs()
	live := e.h.Queue
	e.h.Queue = dqueue.NewClient(deadRedis(t))
	rec := e.postHdr(t, http.MethodPost, `{"q":1}`, nil)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"event_ids":null`) {
		t.Fatalf("status = %d %s, want 202 with no event ids", rec.Code, rec.Body)
	}
	if !strings.Contains(lb.String(), "enqueue delivery") {
		t.Errorf("enqueue failure not logged: %s", lb)
	}
	if n := e.count(t, `SELECT count(*) FROM events ev JOIN requests r ON r.id = ev.request_id WHERE r.source_id = $1 AND ev.status = 'queued'`); n != 1 {
		t.Errorf("queued events = %d, want 1 (reaper will re-queue)", n)
	}
	if got := func() int64 { e.h.Queue = live; return e.pending(t) }(); got != 0 {
		t.Errorf("pending = %d, want 0", got)
	}
	if len(e.dedupKeys(t)) != 1 {
		t.Errorf("dedup key must be kept once events are durable")
	}
}

// failOn makes the handler's queries fail for one named sqlc statement, over
// the real pool.
func (e *captureEnv) failOn(name string) {
	e.h.Queries = store.New(faultDB{DBTX: e.pool, match: "-- name: " + name + " "})
}

func TestIngest_DBFaults(t *testing.T) {
	cases := []struct {
		query    string
		status   int
		logs     string
		warmup   bool // resolve the source first so the fault hits a later query
		wantReqs int
	}{
		{"GetSourceByIngestToken", 500, "ingest: resolve source", false, 0},
		{"ListEnabledConnectionsBySource", 500, "list connections", true, 1},
		{"CreateEventsBatch", 500, "create events batch", true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			e := newCaptureEnv(t)
			e.addConn(t)
			lb := e.logs()
			if tc.warmup {
				e.postHdr(t, http.MethodPost, `{"warm":1}`, nil)
				// warm request created 1 request + 1 event; clear its marker so
				// the faulted request is a first sight, not a duplicate.
				for _, k := range e.dedupKeys(t) {
					_ = e.h.Redis.Del(context.Background(), k).Err()
				}
			}
			e.failOn(tc.query)
			rec := e.postHdr(t, http.MethodPost, `{"fault":1}`, nil)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), "internal error") {
				t.Fatalf("status = %d %q, want %d internal error", rec.Code, rec.Body, tc.status)
			}
			if !strings.Contains(lb.String(), tc.logs) {
				t.Errorf("log %q missing: %s", tc.logs, lb)
			}
			if tc.query == "CreateEventsBatch" || tc.query == "ListEnabledConnectionsBySource" {
				// request row is persisted before fan-out; no extra event exists.
				if e.events(t) != 1 {
					t.Errorf("events = %d, want only the warm-up event", e.events(t))
				}
				// The faulted request's marker is rolled back: its retry is not a dup.
				if len(e.dedupKeys(t)) != 0 {
					t.Errorf("dedup keys = %v, want rolled back", e.dedupKeys(t))
				}
			}
		})
	}
}

func TestIngest_CaptureRuleListFails_TreatedAsNone(t *testing.T) {
	e := newCaptureEnv(t)
	lb := e.logs()
	if _, err := e.q.CreateCaptureRule(context.Background(), store.CreateCaptureRuleParams{
		OrgID: e.org.ID, SourceID: e.src.ID, Name: "r-" + uuid.NewString(), Cap: 5, Enabled: true,
	}); err != nil {
		t.Fatalf("rule: %v", err)
	}
	e.failOn("ListEnabledCaptureRulesBySource")
	// Source lookup also goes through the faulted wrapper, which only fails the rules query.
	if rec := e.post(t, `{"x":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if !strings.Contains(lb.String(), "list capture rules (treating as none)") {
		t.Errorf("not logged: %s", lb)
	}
	if e.totalBookmarks(t) != 0 {
		t.Error("capture must be disabled when the rules cannot be loaded")
	}
}

func TestIngest_BadCaptureFilterSkippedOthersStillCapture(t *testing.T) {
	e := newCaptureEnv(t)
	lb := e.logs()
	bad := "this is ((( not cel"
	if _, err := e.q.CreateCaptureRule(context.Background(), store.CreateCaptureRuleParams{
		OrgID: e.org.ID, SourceID: e.src.ID, Name: "bad-" + uuid.NewString(), FilterExpr: &bad, Cap: 5, Enabled: true,
	}); err != nil {
		t.Fatalf("bad rule: %v", err)
	}
	good, err := e.q.CreateCaptureRule(context.Background(), store.CreateCaptureRuleParams{
		OrgID: e.org.ID, SourceID: e.src.ID, Name: "good-" + uuid.NewString(), Cap: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("good rule: %v", err)
	}
	if rec := e.post(t, `{"x":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(lb.String(), "bad filter, skipping") {
		t.Errorf("bad filter not logged: %s", lb)
	}
	if e.bookmarksForRule(t, good.ID) != 1 || e.totalBookmarks(t) != 1 {
		t.Errorf("want exactly the good rule's bookmark; total=%d", e.totalBookmarks(t))
	}
}

func TestIngest_CaptureAfterFanOut(t *testing.T) {
	e := newCaptureEnv(t)
	e.addConn(t)
	rule, err := e.q.CreateCaptureRule(context.Background(), store.CreateCaptureRuleParams{
		OrgID: e.org.ID, SourceID: e.src.ID, Name: "r-" + uuid.NewString(), Cap: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("rule: %v", err)
	}
	if rec := e.post(t, `{"x":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	if e.bookmarksForRule(t, rule.ID) != 1 || e.events(t) != 1 {
		t.Errorf("bookmarks=%d events=%d, want 1/1", e.bookmarksForRule(t, rule.ID), e.events(t))
	}
}

func TestCapture_FailuresAreSwallowedAndIngestStill202(t *testing.T) {
	for _, tc := range []struct{ query, log string }{
		{"CreateAutoBookmark", "create bookmark (ignored)"},
		{"EvictCaptureBookmarks", "evict (ignored)"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			e := newCaptureEnv(t)
			lb := e.logs()
			if _, err := e.q.CreateCaptureRule(context.Background(), store.CreateCaptureRuleParams{
				OrgID: e.org.ID, SourceID: e.src.ID, Name: "r-" + uuid.NewString(), Cap: 5, Enabled: true,
			}); err != nil {
				t.Fatalf("rule: %v", err)
			}
			// Fail only after the source (and its rules) are cached.
			if rec := e.post(t, `{"warm":1}`); rec.Code != http.StatusAccepted {
				t.Fatalf("warm: %d", rec.Code)
			}
			e.failOn(tc.query)
			if rec := e.post(t, `{"x":2}`); rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", rec.Code)
			}
			if !strings.Contains(lb.String(), tc.log) {
				t.Errorf("log %q missing: %s", tc.log, lb)
			}
			want := 1 // warm-up bookmark only, or warm + the evict-failed one
			if tc.query == "EvictCaptureBookmarks" {
				want = 2
			}
			if e.totalBookmarks(t) != want {
				t.Errorf("bookmarks = %d, want %d", e.totalBookmarks(t), want)
			}
		})
	}
}

func TestCapture_PanicIsRecoveredAndLogged(t *testing.T) {
	lb := &logBuf{}
	h := &Handler{Log: slog.New(slog.NewTextHandler(lb, nil))} // nil Queries: CreateAutoBookmark panics
	h.capture(context.Background(), []compiledRule{{id: uuid.New(), cap: 1, name: "r"}}, pgtype.UUID{}, uuid.New(), nil, nil)
	if !strings.Contains(lb.String(), "capture panic (ignored)") {
		t.Fatalf("panic not recovered+logged: %q", lb)
	}
}

func TestParseRemoteAddr_Unparseable(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/e/x", nil)
	r.RemoteAddr = "not-an-ip:99"
	if got := parseRemoteAddr(r); got != nil {
		t.Fatalf("parseRemoteAddr = %v, want nil", got)
	}
}

func TestHopCount(t *testing.T) {
	for in, want := range map[string]int{"": 0, "x": 0, "0": 0, "-1": 0, "1": 1, "12": 12} {
		r := httptest.NewRequest(http.MethodPost, "/e/x", nil)
		if in != "" {
			r.Header.Set("Dstream-Webhook-Hops", in)
		}
		if got := hopCount(r); got != want {
			t.Errorf("hopCount(%q) = %d, want %d", in, got, want)
		}
	}
}

// --- body store ---

func TestBodyStore_RoundTripExpungedAndBadRefs(t *testing.T) {
	e := newCaptureEnv(t)
	ctx := context.Background()
	e.addConn(t) // org cleanup
	bs := NewPostgresBodyStore(e.q)

	rec := e.post(t, `{"stored":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ingest: %d", rec.Code)
	}
	var ref string
	if err := e.pool.QueryRow(ctx, `SELECT body_ref FROM requests WHERE source_id = $1`, e.src.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if got, err := bs.Get(ctx, ref); err != nil || string(got) != `{"stored":1}` {
		t.Fatalf("Get = %q, %v", got, err)
	}

	// Expunge (retention): the body is gone, the request row stays.
	if _, err := e.pool.Exec(ctx, `UPDATE request_bodies SET body = NULL WHERE request_id = $1`, strings.TrimPrefix(ref, "pg:")); err != nil {
		t.Fatal(err)
	}
	if _, err := bs.Get(ctx, ref); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expunged Get err = %v, want pgx.ErrNoRows", err)
	}
	if e.requests(t) != 1 {
		t.Error("expunging a body must not remove the request row")
	}

	if _, err := bs.Get(ctx, "s3:abc"); !errors.Is(err, ErrUnknownBodyRef) {
		t.Errorf("foreign scheme err = %v, want ErrUnknownBodyRef", err)
	}
	if _, err := bs.Get(ctx, "pg:"); !errors.Is(err, ErrUnknownBodyRef) {
		t.Errorf("empty id err = %v, want ErrUnknownBodyRef", err)
	}
	if _, err := bs.Get(ctx, "pg:not-a-uuid"); err == nil || errors.Is(err, ErrUnknownBodyRef) {
		t.Errorf("malformed uuid err = %v, want a uuid parse error", err)
	}
	if _, err := bs.Get(ctx, "pg:"+uuid.NewString()); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("unknown id err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := NewPostgresBodyStore(store.New(closedPool(t))).Put(ctx, uuid.New(), []byte("x")); err == nil {
		t.Error("Put on a closed pool must fail")
	}
}

// --- helpers ---

// closedPool is a real pool that has been closed: every query on it fails.
func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("DSTREAM_TEST_DB_URL"), 1)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	pool.Close()
	return pool
}

func sha256sum(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

type faultDB struct {
	store.DBTX
	match string
}

var errInjected = errors.New("injected db failure")

type errRow struct{}

func (errRow) Scan(...any) error { return errInjected }

func (f faultDB) Exec(ctx context.Context, sql string, a ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, f.match) {
		return pgconn.CommandTag{}, errInjected
	}
	return f.DBTX.Exec(ctx, sql, a...)
}
func (f faultDB) Query(ctx context.Context, sql string, a ...any) (pgx.Rows, error) {
	if strings.Contains(sql, f.match) {
		return nil, errInjected
	}
	return f.DBTX.Query(ctx, sql, a...)
}
func (f faultDB) QueryRow(ctx context.Context, sql string, a ...any) pgx.Row {
	if strings.Contains(sql, f.match) {
		return errRow{}
	}
	return f.DBTX.QueryRow(ctx, sql, a...)
}
