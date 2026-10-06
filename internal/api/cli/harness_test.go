package cli

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/metrics"
	"github.com/Vivekagent47/dstream/internal/store"
)

const waitFor = 5 * time.Second

// syncBuf is a goroutine-safe log sink so tests can assert on handler logs.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	pool, err := store.NewPool(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// poll retries cond every 10ms until it holds or the deadline fails the test.
func poll(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// env is one test's world: real Postgres + Redis, a real HTTP server running
// the real handlers, and a captured log.
type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	q    *store.Queries
	rdb  *redis.Client
	h    Handlers
	srv  *httptest.Server
	log  *syncBuf
}

// X-Org carries the test principal's org: absent means no principal (what the
// auth middleware leaves for an unauthenticated request).
func withPrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("X-Org"); o != "" {
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{OrgID: uuid.MustParse(o)})))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func newEnv(t *testing.T, tweak func(*Handlers)) *env {
	t.Helper()
	rdb := testRedis(t)
	t.Cleanup(func() { rdb.Close() })
	pool := testPool(t)
	q := store.New(pool)
	lb := &syncBuf{}
	h := Handlers{Log: slog.New(slog.NewTextHandler(lb, nil)), Queries: q, Redis: rdb}
	if tweak != nil {
		tweak(&h)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/connect", h.Connect)
	mux.HandleFunc("/sources", h.ListSources)
	srv := httptest.NewServer(withPrincipal(mux))
	t.Cleanup(srv.Close)
	return &env{t: t, pool: pool, q: q, rdb: rdb, h: h, srv: srv, log: lb}
}

// seedOrg creates an org (cascade-deleted at cleanup) and returns its id.
func (e *env) seedOrg() uuid.UUID {
	e.t.Helper()
	o, err := e.q.CreateOrganization(context.Background(), store.CreateOrganizationParams{Name: "T", Slug: "cli-" + uuid.NewString()})
	if err != nil {
		e.t.Fatalf("org: %v", err)
	}
	e.t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), `DELETE FROM organizations WHERE id=$1`, o.ID) })
	return store.GoUUID(o.ID)
}

func (e *env) seedSource(org uuid.UUID, name string) uuid.UUID {
	e.t.Helper()
	s, err := e.q.CreateSource(context.Background(), store.CreateSourceParams{
		OrgID: store.UUID(org), Name: name, Type: "generic", IngestToken: "tok-" + uuid.NewString(), SigningConfig: []byte("{}"),
	})
	if err != nil {
		e.t.Fatalf("source: %v", err)
	}
	sid := store.GoUUID(s.ID)
	e.t.Cleanup(func() { e.rdb.Del(context.Background(), SessionKey(sid), DispatchKey(sid)) })
	return sid
}

// seedEvent makes a cli destination + connection + request + queued event on
// source and returns the event id. bodyRef "" means the real stored body ref.
func (e *env) seedEvent(org, source uuid.UUID, body []byte, bodyRef string) uuid.UUID {
	e.t.Helper()
	ctx := context.Background()
	dst, err := e.q.CreateDestination(ctx, store.CreateDestinationParams{
		OrgID: store.UUID(org), Name: "d-" + uuid.NewString(), Type: "cli", AuthConfig: []byte("{}"),
	})
	if err != nil {
		e.t.Fatalf("destination: %v", err)
	}
	conn, err := e.q.CreateConnection(ctx, store.CreateConnectionParams{SourceID: store.UUID(source), DestinationID: dst.ID, Enabled: true})
	if err != nil {
		e.t.Fatalf("connection: %v", err)
	}
	reqID := uuid.Must(uuid.NewV7())
	if bodyRef == "" {
		bodyRef = "pg:" + reqID.String()
	}
	ct := "application/json"
	req, err := e.q.CreateRequest(ctx, store.CreateRequestParams{
		ID: store.UUID(reqID), SourceID: store.UUID(source), HTTPMethod: "POST", HTTPPath: "/e/x",
		Headers: []byte(`{"X-In":["1"]}`), BodyHash: "h", BodyRef: bodyRef, BodySize: int32(len(body)), ContentType: &ct,
	})
	if err != nil {
		e.t.Fatalf("request: %v", err)
	}
	if _, err := ingest.NewPostgresBodyStore(e.q).Put(ctx, reqID, body); err != nil {
		e.t.Fatalf("body: %v", err)
	}
	evs, err := e.q.CreateEventsBatch(ctx, store.CreateEventsBatchParams{
		RequestID: req.ID, OrgID: store.UUID(org), ConnectionIds: []pgtype.UUID{conn.ID},
	})
	if err != nil || len(evs) != 1 {
		e.t.Fatalf("event: %v (%d)", err, len(evs))
	}
	return store.GoUUID(evs[0].ID)
}

func (e *env) eventStatus(id uuid.UUID) string {
	e.t.Helper()
	ev, err := e.q.GetEventByID(context.Background(), store.UUID(id))
	if err != nil {
		e.t.Fatalf("event: %v", err)
	}
	return ev.Status
}

func (e *env) attempts(id uuid.UUID) []store.Attempt {
	e.t.Helper()
	as, err := e.q.ListAttemptsByEvent(context.Background(), store.UUID(id))
	if err != nil {
		e.t.Fatalf("attempts: %v", err)
	}
	return as
}

// waitAttempt polls until the event has an attempt row and returns it.
func (e *env) waitAttempt(id uuid.UUID) store.Attempt {
	e.t.Helper()
	var as []store.Attempt
	poll(e.t, "attempt row", func() bool { as = e.attempts(id); return len(as) > 0 })
	if len(as) != 1 {
		e.t.Fatalf("want exactly 1 attempt, got %d", len(as))
	}
	return as[0]
}

// dial opens the tunnel as org; dialHdr takes raw headers for the rejection cases.
func (e *env) dial(org, source uuid.UUID) *websocket.Conn {
	e.t.Helper()
	c, _, err := e.dialHdr(source.String(), http.Header{"X-Org": {org.String()}})
	if err != nil {
		e.t.Fatalf("dial: %v", err)
	}
	e.t.Cleanup(func() { c.CloseNow() })
	return c
}

func (e *env) dialHdr(sourceID string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	return websocket.Dial(ctx, e.srv.URL+"/connect?source_id="+sourceID, &websocket.DialOptions{HTTPHeader: hdr})
}

// read returns the next raw text frame, failing the test on timeout.
func read(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	// wsjson frames end with the json.Encoder newline; that is not part of the payload.
	return strings.TrimSuffix(string(b), "\n")
}

func send(t *testing.T, c *websocket.Conn, frame string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("send frame: %v", err)
	}
}

func (e *env) push(source uuid.UUID, payload string) {
	e.t.Helper()
	if err := e.rdb.RPush(context.Background(), DispatchKey(source), payload).Err(); err != nil {
		e.t.Fatalf("push: %v", err)
	}
}

func (e *env) pushEvent(source, event uuid.UUID) { e.push(source, `{"event_id":"`+event.String()+`"}`) }

// helloFor consumes the hello frame, asserting its literal shape.
func helloFor(t *testing.T, c *websocket.Conn, source uuid.UUID) {
	t.Helper()
	f := read(t, c)
	if !strings.HasPrefix(f, `{"now":"`) || !strings.HasSuffix(f, `","source_id":"`+source.String()+`","type":"hello"}`) {
		t.Fatalf("hello frame = %s", f)
	}
}

// sessionsActive / disconnects read the real Prometheus registry.
func sessionsActive(t *testing.T) float64 { return metricSum(t, "dstream_cli_sessions_active", "") }

func disconnects(t *testing.T, reason string) float64 {
	return metricSum(t, "dstream_cli_disconnects_total", reason)
}

func metricSum(t *testing.T, name, reason string) float64 {
	t.Helper()
	fams, err := metrics.Reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sum float64
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if reason != "" {
				ok := false
				for _, l := range m.GetLabel() {
					ok = ok || (l.GetName() == "reason" && l.GetValue() == reason)
				}
				if !ok {
					continue
				}
			}
			sum += m.GetGauge().GetValue() + m.GetCounter().GetValue()
		}
	}
	return sum
}

// waitSessions waits for the live-tunnel gauge to reach n. The handler's
// dispatch loop sits in a 5s BLPOP, so a garbage payload is pushed on each
// poll to wake it; garbage is skipped by design.
func (e *env) waitSessions(source uuid.UUID, n float64) {
	e.t.Helper()
	poll(e.t, "tunnel teardown", func() bool {
		e.rdb.RPush(context.Background(), DispatchKey(source), "wake")
		return sessionsActive(e.t) == n
	})
}
