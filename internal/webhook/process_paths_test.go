package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/opevents"
	"github.com/Vivekagent47/dstream/internal/store"
)

// wenv is a real Postgres + real Redis (per-test queue prefix) around one
// Handler, with a small receiver that counts what actually reaches the wire.
type wenv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	q      *store.Queries
	dq     *dqueue.Client
	rdb    *redis.Client
	prefix string
	h      Handler
	hits   atomic.Int32
	status atomic.Int32 // what the receiver answers
	srv    *httptest.Server
}

func newWenv(t *testing.T) *wenv {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	pool, err := store.NewPool(context.Background(), dsn, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	dq, rdb, prefix := testQueue(t)
	e := &wenv{t: t, pool: pool, q: q, dq: dq, rdb: rdb, prefix: prefix}
	e.status.Store(http.StatusOK)
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		e.hits.Add(1)
		w.WriteHeader(int(e.status.Load()))
	}))
	t.Cleanup(e.srv.Close)
	e.h = Handler{Log: discardLog(), Queries: q, HTTP: deliver.NewSafeHTTPClient(10*time.Second, true)}
	return e
}

// seed builds org -> app -> endpoint(url) -> message -> delivery, and removes
// the org (cascading everything) on cleanup.
func (e *wenv) seed(url string) (delID, msgID, orgID, epID, appID uuid.UUID) {
	e.t.Helper()
	secret, _ := GenerateSecret()
	delID, msgID, orgID, epID, appID = seedDeliveryFull(e.t, e.q, url, secret)
	e.t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, store.UUID(orgID))
	})
	return
}

func (e *wenv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

// burn spends the whole retry budget. The SQL shortcut skips only the retry
// ladder; the failure counter and the disable still come from real Process calls.
// (Next failure dead-letters.)
func (e *wenv) burn(delID uuid.UUID) {
	e.t.Helper()
	e.exec(`UPDATE message_deliveries SET attempt_count = $2 WHERE id = $1`, store.UUID(delID), len(retrySchedule))
}

// lease enqueues the delivery and leases exactly it. Other tasks that may sit
// in this test's private queue (operational events published by a dead-letter)
// are acked away; the loop is bounded.
func (e *wenv) lease(delID, orgID uuid.UUID) (dqueue.Payload, string) {
	e.t.Helper()
	enqueueDelivery(e.t, e.dq, delID, orgID)
	return e.leaseNext(delID)
}

func (e *wenv) leaseNext(delID uuid.UUID) (dqueue.Payload, string) {
	e.t.Helper()
	for i := 0; i < 20; i++ {
		raw, p, ok, err := e.dq.FairPick(context.Background(), 60000)
		if err != nil || !ok {
			e.t.Fatalf("fairpick: ok=%v err=%v", ok, err)
		}
		if id, _ := deliveryID(p); id == delID {
			return p, raw
		}
		_ = e.dq.Ack(context.Background(), raw)
	}
	e.t.Fatal("delivery never leased")
	return dqueue.Payload{}, ""
}

func (e *wenv) process(delID, orgID uuid.UUID) error {
	e.t.Helper()
	p, raw := e.lease(delID, orgID)
	return e.h.Process(context.Background(), p, raw, e.dq)
}

func (e *wenv) delStatus(delID uuid.UUID) (string, int) {
	e.t.Helper()
	var s string
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT status, attempt_count FROM message_deliveries WHERE id = $1`, store.UUID(delID)).Scan(&s, &n); err != nil {
		e.t.Fatalf("delivery row: %v", err)
	}
	return s, n
}

type attemptRow struct {
	Num    int
	Status *int32
	Err    string
}

func (e *wenv) attempts(delID uuid.UUID) []attemptRow {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT attempt_num, response_status, coalesce(error_message,'') FROM message_delivery_attempts WHERE delivery_id = $1 ORDER BY attempt_num`, store.UUID(delID))
	if err != nil {
		e.t.Fatalf("attempts: %v", err)
	}
	defer rows.Close()
	var out []attemptRow
	for rows.Next() {
		var a attemptRow
		if err := rows.Scan(&a.Num, &a.Status, &a.Err); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// lanes reports the task's whereabouts in this test's private queue.
func (e *wenv) lanes() (scheduled, processing, dead int64) {
	e.t.Helper()
	ctx := context.Background()
	scheduled, _ = e.rdb.ZCard(ctx, e.prefix+":scheduled").Result()
	processing, _ = e.rdb.ZCard(ctx, e.prefix+":processing").Result()
	dead, _ = e.rdb.LLen(ctx, e.prefix+":dead").Result()
	return
}

func (e *wenv) endpoint(epID, appID uuid.UUID) store.Endpoint {
	e.t.Helper()
	ep, err := e.q.GetEndpointForApp(context.Background(), store.GetEndpointForAppParams{ID: store.UUID(epID), AppID: store.UUID(appID)})
	if err != nil {
		e.t.Fatal(err)
	}
	return ep
}

func (e *wenv) wantDead(delID uuid.UUID, wantAttemptErr string) {
	e.t.Helper()
	if st, _ := e.delStatus(delID); st != "dead" {
		e.t.Errorf("delivery status = %q, want dead", st)
	}
	if _, proc, dead := e.lanes(); dead != 1 || proc != 0 {
		e.t.Errorf("lanes: dead=%d processing=%d, want 1/0", dead, proc)
	}
	if wantAttemptErr != "" {
		at := e.attempts(delID)
		if len(at) != 1 || at[0].Status != nil || !strings.Contains(at[0].Err, wantAttemptErr) {
			e.t.Errorf("attempts = %+v, want one status-less attempt containing %q", at, wantAttemptErr)
		}
	}
	if e.hits.Load() != 0 {
		e.t.Errorf("a deterministic failure must not reach the wire; hits=%d", e.hits.Load())
	}
}

// --- task decoding and terminal states ---

func TestProcess_UndecodableTaskIsDeadLettered(t *testing.T) {
	e := newWenv(t)
	if err := e.dq.Enqueue(context.Background(), dqueue.Payload{Kind: "message", OrgID: uuid.New(), EnqueuedAt: time.Now().UnixMilli(), Data: []byte("not json")}); err != nil {
		t.Fatal(err)
	}
	raw, p, ok, err := e.dq.FairPick(context.Background(), 60000)
	if !ok || err != nil {
		t.Fatalf("fairpick: %v %v", ok, err)
	}
	if err := e.h.Process(context.Background(), p, raw, e.dq); err != nil {
		t.Fatal(err)
	}
	if _, proc, dead := e.lanes(); dead != 1 || proc != 0 {
		t.Fatalf("dead=%d processing=%d, want 1/0", dead, proc)
	}
}

func TestProcess_TerminalAndDisabledDeliveriesAreAckedWithoutSending(t *testing.T) {
	for _, st := range []string{"delivered", "dead", "disabled", "filtered"} {
		t.Run(st, func(t *testing.T) {
			e := newWenv(t)
			delID, _, orgID, _, _ := e.seed(e.srv.URL)
			e.exec(`UPDATE message_deliveries SET status = $2 WHERE id = $1`, store.UUID(delID), st)
			if err := e.process(delID, orgID); err != nil {
				t.Fatal(err)
			}
			if got, _ := e.delStatus(delID); got != st {
				t.Errorf("status = %q, want it left as %q", got, st)
			}
			if sch, proc, dead := e.lanes(); sch+proc+dead != 0 {
				t.Errorf("lanes: scheduled=%d processing=%d dead=%d, want all 0 (acked)", sch, proc, dead)
			}
			if e.hits.Load() != 0 || len(e.attempts(delID)) != 0 {
				t.Errorf("hits=%d attempts=%v, want none", e.hits.Load(), e.attempts(delID))
			}
		})
	}
}

func TestProcess_EndpointDisabledAfterEnqueueMarksDisabled(t *testing.T) {
	e := newWenv(t)
	delID, _, orgID, epID, _ := e.seed(e.srv.URL)
	e.exec(`UPDATE endpoints SET disabled = true WHERE id = $1`, store.UUID(epID))
	if err := e.process(delID, orgID); err != nil {
		t.Fatal(err)
	}
	if st, n := e.delStatus(delID); st != "disabled" || n != 0 {
		t.Errorf("status=%q attempts=%d, want disabled/0", st, n)
	}
	if sch, proc, dead := e.lanes(); sch+proc+dead != 0 || e.hits.Load() != 0 || len(e.attempts(delID)) != 0 {
		t.Errorf("must be acked with no send and no attempt: lanes=%d/%d/%d hits=%d", sch, proc, dead, e.hits.Load())
	}
}

// --- the per-org in-flight gate, below the cap ---

func TestProcess_InflightUnderCapSendsAndReleasesSlot(t *testing.T) {
	e := newWenv(t)
	delID, _, orgID, _, _ := e.seed(e.srv.URL)
	key := "inflight:org:" + orgID.String()
	t.Cleanup(func() { e.rdb.Del(context.Background(), key) })
	e.h.Redis, e.h.PerOrgMaxInflight = e.rdb, 3
	if err := e.process(delID, orgID); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.delStatus(delID); st != "delivered" || e.hits.Load() != 1 {
		t.Fatalf("status=%q hits=%d, want delivered/1", st, e.hits.Load())
	}
	if v, err := e.rdb.Get(context.Background(), key).Int(); err != nil || v != 0 {
		t.Fatalf("in-flight counter after send = %d (%v), want slot released to 0", v, err)
	}
}

func TestProcess_DeferralScheduleFailureLeavesTaskLeased(t *testing.T) {
	e := newWenv(t)
	delID, _, orgID, _, _ := e.seed(e.srv.URL)
	key := "inflight:org:" + orgID.String()
	t.Cleanup(func() { e.rdb.Del(context.Background(), key) })
	e.rdb.Set(context.Background(), key, 1, time.Minute)
	e.h.Redis, e.h.PerOrgMaxInflight = e.rdb, 1
	p, raw := e.lease(delID, orgID)
	if err := e.h.Process(context.Background(), p, raw, deadQueue(t)); err == nil {
		t.Fatal("want the queue error surfaced so the recoverer re-leases the task")
	}
	if _, proc, _ := e.lanes(); proc != 1 {
		t.Errorf("processing = %d, want the task still leased", proc)
	}
	if e.hits.Load() != 0 {
		t.Error("deferred delivery must not send")
	}
}

// --- filter, transform ---

func TestProcess_FilterEvalErrorFailsOpenAndDelivers(t *testing.T) {
	e := newWenv(t)
	lb := &logCapture{}
	e.h.Log = lb.logger()
	delID, _, orgID, epID, _ := e.seed(e.srv.URL)
	e.exec(`UPDATE endpoints SET filter_expr = 'payload.missing == 1' WHERE id = $1`, store.UUID(epID))
	if err := e.process(delID, orgID); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.delStatus(delID); st != "delivered" || e.hits.Load() != 1 {
		t.Fatalf("status=%q hits=%d, want fail-open delivery", st, e.hits.Load())
	}
	if !strings.Contains(lb.String(), "filter eval error; failing open") {
		t.Errorf("fail-open not logged: %s", lb)
	}
}

func TestProcess_TransformFailuresAreTerminal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		js      string
		timeout time.Duration
		maxOut  int
		want    string
	}{
		{"throws", `function transform(p, h) { throw new Error("boom"); }`, 0, 0, "transform:"},
		{"configured timeout", `function transform(p, h) { while (true) {} }`, 50 * time.Millisecond, 0, "transform:"},
		{"configured output cap", `function transform(p, h) { p.pad = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"; return p; }`, 0, 16, "exceeds cap 16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newWenv(t)
			e.h.TransformTimeout, e.h.TransformMaxOutput = tc.timeout, tc.maxOut
			delID, _, orgID, epID, _ := e.seed(e.srv.URL)
			e.exec(`UPDATE endpoints SET transform_js = $2 WHERE id = $1`, store.UUID(epID), tc.js)
			if err := e.process(delID, orgID); err != nil {
				t.Fatal(err)
			}
			e.wantDead(delID, tc.want)
		})
	}
}

func TestHandler_TransformDefaults(t *testing.T) {
	var h Handler
	if h.effTransformTimeout() != time.Second || h.effTransformMaxOutput() != 5<<20 {
		t.Fatalf("zero-value defaults = %v / %d, want 1s / 5MiB", h.effTransformTimeout(), h.effTransformMaxOutput())
	}
	h = Handler{TransformTimeout: 3 * time.Second, TransformMaxOutput: 7}
	if h.effTransformTimeout() != 3*time.Second || h.effTransformMaxOutput() != 7 {
		t.Fatalf("configured values ignored: %v / %d", h.effTransformTimeout(), h.effTransformMaxOutput())
	}
}

// --- deterministic failures never reach the wire ---

func TestProcess_InvalidURLIsTerminal(t *testing.T) {
	e := newWenv(t)
	delID, _, orgID, _, _ := e.seed("ftp://example.test/hook")
	if err := e.process(delID, orgID); err != nil {
		t.Fatal(err)
	}
	e.wantDead(delID, "invalid url:")
}

func TestProcess_MalformedSecretIsTerminal(t *testing.T) {
	e := newWenv(t)
	delID, _, orgID, epID, _ := e.seed(e.srv.URL)
	e.exec(`UPDATE endpoints SET secret = '%%%not-base64%%%' WHERE id = $1`, store.UUID(epID))
	if err := e.process(delID, orgID); err != nil {
		t.Fatal(err)
	}
	e.wantDead(delID, "sign:")
}

// Documents CURRENT behaviour, not contract: this is a DEFECT. A padded URL
// passes ValidateDestinationURL (which trims) but cannot be built into a
// request (Process sends it untrimmed), so it burns the full 8-rung retry
// ladder and writes no attempt row: the operator sees deliveries vanish with no
// history. Flip this test when it is fixed.
func TestProcess_PaddedURLPassesValidationButCannotBeRequested(t *testing.T) {
	e := newWenv(t)
	delID, _, orgID, _, _ := e.seed(" " + e.srv.URL)
	if err := e.process(delID, orgID); err != nil {
		t.Fatal(err)
	}
	if sch, proc, dead := e.lanes(); sch != 1 || proc != 0 || dead != 0 {
		t.Errorf("lanes scheduled=%d processing=%d dead=%d, want 1/0/0 (retry)", sch, proc, dead)
	}
	if st, n := e.delStatus(delID); st != "queued" || n != 1 {
		t.Errorf("status=%q attempt_count=%d, want a retry with 1 attempt counted", st, n)
	}
	if e.hits.Load() != 0 || len(e.attempts(delID)) != 0 {
		t.Errorf("hits=%d attempts=%v, want none", e.hits.Load(), e.attempts(delID))
	}
}

// --- transport failure: recorded, retried ---

func TestProcess_ConnectionRefusedRecordsAttemptAndSchedulesRetry(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedURL := "http://" + ln.Addr().String() + "/hook"
	_ = ln.Close() // nothing listens any more
	e := newWenv(t)
	delID, _, orgID, _, _ := e.seed(closedURL)
	if err := e.process(delID, orgID); err != nil {
		t.Fatal(err)
	}
	at := e.attempts(delID)
	if len(at) != 1 || at[0].Num != 1 || at[0].Status != nil || !strings.Contains(at[0].Err, "refused") {
		t.Fatalf("attempts = %+v, want one attempt #1 with no status and a connection-refused error", at)
	}
	if sch, proc, dead := e.lanes(); sch != 1 || proc != 0 || dead != 0 {
		t.Errorf("lanes scheduled=%d processing=%d dead=%d, want 1/0/0", sch, proc, dead)
	}
	var nextRetry *time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT next_retry_at FROM message_deliveries WHERE id = $1`, store.UUID(delID)).Scan(&nextRetry); err != nil || nextRetry == nil || time.Until(*nextRetry) < 2*time.Second {
		t.Errorf("next_retry_at = %v err=%v, want ~5s ahead (first rung)", nextRetry, err)
	}
}

func TestProcess_EndpointHeadersThatAreNotAMapAreIgnored(t *testing.T) {
	e := newWenv(t)
	delID, _, orgID, epID, _ := e.seed(e.srv.URL)
	e.exec(`UPDATE endpoints SET headers = '[1,2]'::jsonb WHERE id = $1`, store.UUID(epID))
	if err := e.process(delID, orgID); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.delStatus(delID); st != "delivered" || e.hits.Load() != 1 {
		t.Fatalf("status=%q hits=%d, want delivered/1", st, e.hits.Load())
	}
}

func TestUnmarshalHeaderMap(t *testing.T) {
	if m, err := unmarshalHeaderMap(nil); m != nil || err != nil {
		t.Errorf("empty = %v, %v, want nil, nil", m, err)
	}
	if _, err := unmarshalHeaderMap([]byte(`[1]`)); err == nil {
		t.Error("a JSON array is not a header map")
	}
	if m, err := unmarshalHeaderMap([]byte(`{"X-A":"b"}`)); err != nil || m["X-A"] != "b" {
		t.Errorf("map = %v, %v", m, err)
	}
}

// --- consecutive failures: auto-disable and reset ---

// deadOnce drives one more delivery of the endpoint to the dead lane.
func (e *wenv) deadOnce(orgID, appID, epID uuid.UUID) uuid.UUID {
	e.t.Helper()
	e.status.Store(http.StatusInternalServerError)
	id := seedExtraDelivery(e.t, e.q, orgID, appID, epID)
	e.burn(id)
	if err := e.process(id, orgID); err != nil {
		e.t.Fatal(err)
	}
	if st, _ := e.delStatus(id); st != "dead" {
		e.t.Fatalf("delivery status = %q, want dead", st)
	}
	at := e.attempts(id)
	if len(at) != 1 || at[0].Num != len(retrySchedule)+1 || at[0].Status == nil || *at[0].Status != 500 {
		e.t.Fatalf("attempts = %+v, want one attempt #%d with status 500", at, len(retrySchedule)+1)
	}
	return id
}

func TestAutoDisable_EndpointDisablesExactlyAtThreshold(t *testing.T) {
	e := newWenv(t)
	e.h.MaxConsecutiveFailures = 3
	_, _, orgID, epID, appID := e.seed(e.srv.URL)
	opApp, err := opevents.SeedOperationalApp(context.Background(), e.q, orgID)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := GenerateSecret()
	if _, err := e.q.CreateEndpoint(context.Background(), store.CreateEndpointParams{
		AppID: store.UUID(opApp), OrgID: store.UUID(orgID), Url: "https://example.test/ops", Secret: secret, Headers: []byte("{}"),
	}); err != nil {
		t.Fatal(err)
	}

	for want := int32(1); want <= 2; want++ {
		e.deadOnce(orgID, appID, epID)
		ep := e.endpoint(epID, appID)
		if ep.Disabled || ep.ConsecutiveFailures != want || ep.DisabledAt.Valid {
			t.Fatalf("after %d dead: disabled=%v failures=%d disabled_at=%v, want enabled with %d", want, ep.Disabled, ep.ConsecutiveFailures, ep.DisabledAt.Valid, want)
		}
		if n := opMessageCounts(t, e.q, opApp)["endpoint.disabled"]; n != 0 {
			t.Fatalf("endpoint.disabled emitted below the threshold (%d)", n)
		}
	}
	e.deadOnce(orgID, appID, epID)
	ep := e.endpoint(epID, appID)
	if !ep.Disabled || ep.ConsecutiveFailures != 3 || !ep.DisabledAt.Valid {
		t.Fatalf("at threshold: disabled=%v failures=%d disabled_at=%v, want disabled/3/stamped", ep.Disabled, ep.ConsecutiveFailures, ep.DisabledAt.Valid)
	}
	if n := opMessageCounts(t, e.q, opApp)["endpoint.disabled"]; n != 1 {
		t.Fatalf("endpoint.disabled op messages = %d, want exactly 1", n)
	}

	// Once disabled, a further delivery is skipped: marked disabled, no send.
	hits := e.hits.Load()
	next := seedExtraDelivery(t, e.q, orgID, appID, epID)
	if err := e.process(next, orgID); err != nil {
		t.Fatal(err)
	}
	if st, n := e.delStatus(next); st != "disabled" || n != 0 || e.hits.Load() != hits || len(e.attempts(next)) != 0 {
		t.Fatalf("delivery to disabled endpoint: status=%q attempts=%d hits %d->%d", st, n, hits, e.hits.Load())
	}
}

func TestAutoDisable_SuccessResetsTheRun(t *testing.T) {
	e := newWenv(t)
	e.h.MaxConsecutiveFailures = 3
	_, _, orgID, epID, appID := e.seed(e.srv.URL)
	e.deadOnce(orgID, appID, epID)
	e.deadOnce(orgID, appID, epID)
	if got := e.endpoint(epID, appID).ConsecutiveFailures; got != 2 {
		t.Fatalf("failures before success = %d, want 2", got)
	}

	e.status.Store(http.StatusOK)
	okID := seedExtraDelivery(t, e.q, orgID, appID, epID)
	if err := e.process(okID, orgID); err != nil {
		t.Fatal(err)
	}
	if ep := e.endpoint(epID, appID); ep.ConsecutiveFailures != 0 || ep.Disabled {
		t.Fatalf("after success: failures=%d disabled=%v, want 0/false", ep.ConsecutiveFailures, ep.Disabled)
	}
	at := e.attempts(okID)
	if st, _ := e.delStatus(okID); st != "delivered" || len(at) != 1 || at[0].Status == nil || *at[0].Status != 200 {
		t.Fatalf("success delivery: status=%q attempts=%+v", st, at)
	}

	// The run restarts from zero: two more failures are one short of the limit.
	e.deadOnce(orgID, appID, epID)
	e.deadOnce(orgID, appID, epID)
	if ep := e.endpoint(epID, appID); ep.Disabled || ep.ConsecutiveFailures != 2 {
		t.Fatalf("after reset + 2 dead: disabled=%v failures=%d, want enabled/2", ep.Disabled, ep.ConsecutiveFailures)
	}
}

func TestAutoDisable_OffAtZeroAndRetriesDoNotCount(t *testing.T) {
	e := newWenv(t)
	e.h.MaxConsecutiveFailures = 0
	_, _, orgID, epID, appID := e.seed(e.srv.URL)
	for i := 0; i < 3; i++ {
		e.deadOnce(orgID, appID, epID)
	}
	if ep := e.endpoint(epID, appID); ep.Disabled || ep.ConsecutiveFailures != 0 {
		t.Fatalf("threshold 0 must not count: disabled=%v failures=%d", ep.Disabled, ep.ConsecutiveFailures)
	}

	// A failure that still has retry budget is a retry, not a dead delivery.
	e.h.MaxConsecutiveFailures = 1
	e.status.Store(http.StatusInternalServerError)
	retry := seedExtraDelivery(t, e.q, orgID, appID, epID)
	if err := e.process(retry, orgID); err != nil {
		t.Fatal(err)
	}
	if ep := e.endpoint(epID, appID); ep.Disabled || ep.ConsecutiveFailures != 0 {
		t.Fatalf("a retried failure must not count: disabled=%v failures=%d", ep.Disabled, ep.ConsecutiveFailures)
	}
	if sch, _, _ := e.lanes(); sch != 1 {
		t.Errorf("scheduled = %d, want the retry queued", sch)
	}
}

// --- store and queue failures leave the task leased for the recoverer ---

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

func (e *wenv) failOn(name string) {
	e.h.Queries = store.New(faultDB{DBTX: e.pool, match: "-- name: " + name + " "})
}

func deadQueue(t *testing.T) *dqueue.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, PoolSize: 1})
	t.Cleanup(func() { _ = c.Close() })
	return dqueue.NewClient(c)
}

func TestProcess_StoreFailuresSurfaceAndLeaveTaskLeased(t *testing.T) {
	cases := []struct {
		query string
		burn  bool
		code  int
		hits  int32
	}{
		{"GetMessageDeliveryForSend", false, 200, 0},
		{"MarkDeliveryInFlight", false, 200, 0},
		{"MarkDeliveryDelivered", false, 200, 1},
		{"MarkDeliveryForRetry", false, 500, 1},
		{"MarkDeliveryDead", true, 500, 1},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			e := newWenv(t)
			e.status.Store(int32(tc.code))
			delID, _, orgID, _, _ := e.seed(e.srv.URL)
			if tc.burn {
				e.burn(delID)
			}
			p, raw := e.lease(delID, orgID)
			e.failOn(tc.query)
			err := e.h.Process(context.Background(), p, raw, e.dq)
			if !errors.Is(err, errInjected) {
				t.Fatalf("err = %v, want the injected store error", err)
			}
			if _, proc, dead := e.lanes(); proc != 1 || dead != 0 {
				t.Errorf("processing=%d dead=%d, want the task left leased", proc, dead)
			}
			if e.hits.Load() != tc.hits {
				t.Errorf("hits = %d, want %d", e.hits.Load(), tc.hits)
			}
		})
	}
}

func TestProcess_RetryScheduleFailureSurfaces(t *testing.T) {
	e := newWenv(t)
	e.status.Store(http.StatusInternalServerError)
	delID, _, orgID, _, _ := e.seed(e.srv.URL)
	p, raw := e.lease(delID, orgID)
	if err := e.h.Process(context.Background(), p, raw, deadQueue(t)); err == nil {
		t.Fatal("want the Schedule error surfaced")
	}
	if _, proc, _ := e.lanes(); proc != 1 {
		t.Errorf("processing = %d, want the task left leased", proc)
	}
	if n := len(e.attempts(delID)); n != 1 {
		t.Errorf("attempts = %d, want the attempt recorded before the queue failure", n)
	}
}

func TestProcess_AttemptRecordFailureIsLoggedAndDeliveryStillCompletes(t *testing.T) {
	e := newWenv(t)
	lb := &logCapture{}
	e.h.Log = lb.logger()
	delID, _, orgID, _, _ := e.seed(e.srv.URL)
	p, raw := e.lease(delID, orgID)
	e.failOn("CreateMessageDeliveryAttempt")
	if err := e.h.Process(context.Background(), p, raw, e.dq); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.delStatus(delID); st != "delivered" {
		t.Errorf("status = %q, want delivered", st)
	}
	if !strings.Contains(lb.String(), "outbound: record attempt") || len(e.attempts(delID)) != 0 {
		t.Errorf("want a logged failure and no attempt row; log=%s attempts=%v", lb, e.attempts(delID))
	}
}

// --- the reaper ---

func TestReaper_RequeuesStuckDeliveriesAndLogsClaimFailures(t *testing.T) {
	e := newWenv(t)
	lb := &logCapture{}
	e.h.Log = lb.logger()
	delID, _, orgID, _, _ := e.seed(e.srv.URL)
	// 'queued', never retried, untouched for an hour: an Enqueue that failed
	// after the row was written.
	e.exec(`UPDATE message_deliveries SET updated_at = now() - interval '1 hour' WHERE id = $1`, store.UUID(delID))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.h.runReaper(ctx, e.dq, 10*time.Millisecond); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	var items []dqueue.Item
	for {
		items, _, _ = e.dq.Items(context.Background(), "pending", orgID.String(), 10)
		if len(items) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if len(items) != 1 {
		t.Fatalf("pending items for the org = %d, want the stuck delivery re-enqueued", len(items))
	}
	var task dqueue.Payload
	if err := json.Unmarshal([]byte(items[0].Raw), &task); err != nil {
		t.Fatal(err)
	}
	if id, err := deliveryID(task); err != nil || id != delID || task.Kind != "message" {
		t.Fatalf("re-enqueued task = %+v (%v), want a message task for %s", task, err, delID)
	}

	// A failing claim is logged and the loop keeps running.
	e.failOn("ClaimStuckMessageDeliveries")
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { e.h.runReaper(ctx2, e.dq, 10*time.Millisecond); close(done2) }()
	for !strings.Contains(lb.String(), "outbound reaper") {
		if time.Now().After(deadline.Add(10 * time.Second)) {
			t.Fatalf("claim failure never logged: %s", lb)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel2()
	<-done2
}

// --- signing ---

func TestSign_RejectsUnusableSecrets(t *testing.T) {
	for _, s := range []string{"whsec_%%%", "whsec_", ""} {
		if sig, err := Sign(s, "m", 1, []byte("x")); err == nil || sig != "" {
			t.Errorf("Sign(%q) = %q, %v, want an error and no signature", s, sig, err)
		}
		if ValidSecret(s) {
			t.Errorf("ValidSecret(%q) = true", s)
		}
	}
	if _, err := signHeader("whsec_%%%", nil, pgtypeNoTime(), "m", 1, nil); err == nil || !strings.Contains(err.Error(), "decode secret") {
		t.Errorf("signHeader with a bad current secret err = %v", err)
	}
	g, _ := GenerateSecret()
	if !strings.HasPrefix(g, "whsec_") || !ValidSecret(g) {
		t.Errorf("generated secret %q not usable", g)
	}
}

// logCapture is a goroutine-safe slog sink.
type logCapture struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *logCapture) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }
func (l *logCapture) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, nil))
}

func pgtypeNoTime() pgtype.Timestamptz { return pgtype.Timestamptz{} }

func TestProcess_DeliveryDeletedAfterEnqueueIsAcked(t *testing.T) {
	e := newWenv(t)
	delID, _, orgID, _, _ := e.seed(e.srv.URL)
	p, raw := e.lease(delID, orgID)
	e.exec(`DELETE FROM message_deliveries WHERE id = $1`, store.UUID(delID))
	if err := e.h.Process(context.Background(), p, raw, e.dq); err != nil {
		t.Fatal(err)
	}
	if sch, proc, dead := e.lanes(); sch+proc+dead != 0 || e.hits.Load() != 0 {
		t.Errorf("lanes=%d/%d/%d hits=%d, want a plain ack and no send", sch, proc, dead, e.hits.Load())
	}
}

func TestRunReaper_ReturnsWhenContextIsDone(t *testing.T) {
	e := newWenv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { e.h.RunReaper(ctx, e.dq); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunReaper did not return after its context was cancelled")
	}
}
