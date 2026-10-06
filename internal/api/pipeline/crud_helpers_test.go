package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// do serves one session-authenticated request and returns the recorder.
func do(t *testing.T, r http.Handler, method, path string, uid, oid uuid.UUID, body any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, method, path, uid, oid, body))
	return rec
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode object: %v body=%s", err, rec.Body.String())
	}
	return m
}

func decodeList(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var l []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatalf("decode list: %v body=%s", err, rec.Body.String())
	}
	if l == nil {
		t.Fatalf("list must be [] not null: %s", rec.Body.String())
	}
	return l
}

// wantErr asserts the exact status and {"error": msg} envelope.
func wantErr(t *testing.T, rec *httptest.ResponseRecorder, status int, msg string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, status, rec.Body.String())
	}
	if got := decodeMap(t, rec)["error"]; got != msg {
		t.Fatalf("error = %v, want %q", got, msg)
	}
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, status, rec.Body.String())
	}
}

// hasID reports whether a list response contains the row with this id.
func hasID(list []map[string]any, id uuid.UUID) bool {
	for _, m := range list {
		if m["id"] == id.String() {
			return true
		}
	}
	return false
}

func mustEq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func pstr(s string) *string { return &s }

func newSource(t *testing.T, q *store.Queries, orgID uuid.UUID) store.Source {
	t.Helper()
	s, err := q.CreateSource(context.Background(), store.CreateSourceParams{
		OrgID: store.UUID(orgID), Name: "src-" + uuid.NewString(), Type: "generic",
		IngestToken: "tok-" + uuid.NewString(), SigningConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("seed source: %v", err)
	}
	return s
}

func seedDest(t *testing.T, q *store.Queries, orgID uuid.UUID, url string) store.Destination {
	t.Helper()
	d, err := q.CreateDestination(context.Background(), store.CreateDestinationParams{
		OrgID: store.UUID(orgID), Name: "dst-" + uuid.NewString(), Type: "http",
		Url: &url, AuthConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	return d
}

func seedConn(t *testing.T, q *store.Queries, src store.Source, dst store.Destination) store.Connection {
	t.Helper()
	c, err := q.CreateConnection(context.Background(), store.CreateConnectionParams{
		SourceID: src.ID, DestinationID: dst.ID, Enabled: true,
	})
	if err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	return c
}

func auditCount(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, action string, target uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE org_id=$1 AND action=$2 AND target_id=$3`,
		orgID, action, target).Scan(&n)
	if err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

func rowCount(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// direct invokes a handler without the router: principal (optional) is put in
// the context, id becomes the {id} URL param, and ctx may be pre-cancelled to
// provoke a real database error from the real pool.
func direct(ctx context.Context, h http.HandlerFunc, method string, p *auth.Principal, id string, body string) *httptest.ResponseRecorder {
	if p != nil {
		ctx = auth.WithPrincipal(ctx, *p)
	}
	rc := chi.NewRouteContext()
	if id != "" {
		rc.URLParams.Add("id", id)
	}
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rc)
	r := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// seedEvent fans one request out to one connection and returns the event.
func seedEvent(t *testing.T, q *store.Queries, orgID, reqID uuid.UUID, conn store.Connection, isTest bool) store.Event {
	t.Helper()
	evs, err := q.CreateEventsBatch(context.Background(), store.CreateEventsBatchParams{
		RequestID: store.UUID(reqID), OrgID: store.UUID(orgID), ConnectionIds: []pgtype.UUID{conn.ID}, IsTest: isTest,
	})
	if err != nil || len(evs) != 1 {
		t.Fatalf("seed event: %v (%d rows)", err, len(evs))
	}
	return evs[0]
}

// seedRequestOn records one inbound request on an existing source.
func seedRequestOn(t *testing.T, q *store.Queries, src store.Source) (uuid.UUID, uuid.UUID) {
	t.Helper()
	reqID := uuid.New()
	ct := "application/json"
	if _, err := q.CreateRequest(context.Background(), store.CreateRequestParams{
		ID: store.UUID(reqID), SourceID: src.ID, HTTPMethod: "POST", HTTPPath: "/e/x",
		Headers: []byte("{}"), BodyHash: "h-" + uuid.NewString(), BodyRef: "pg:" + reqID.String(),
		ContentType: &ct,
	}); err != nil {
		t.Fatalf("seed request: %v", err)
	}
	return reqID, store.GoUUID(src.ID)
}

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

// failingQueries is a real pool on which statements containing marker fail.
func failingQueries(t *testing.T, marker string) *store.Queries {
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

func redisAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("DSTREAM_REDIS_ADDR")
	if addr == "" {
		t.Skip("DSTREAM_REDIS_ADDR not set")
	}
	return addr
}

// dropEvents removes the org's events when the test ends, so queued/in_flight
// rows never leak into tests (e.g. the delivery reaper's) that scan globally.
func dropEvents(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM events WHERE org_id=$1`, orgID) })
}
