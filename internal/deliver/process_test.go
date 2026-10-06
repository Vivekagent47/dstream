package deliver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
)

// ---- fixtures ---------------------------------------------------------------

type seedOpt struct {
	destType    string // default "http"
	url         *string
	maxRetries  int32 // default 3 (set maxRetriesSet to use 0)
	zeroRetries bool
	strategy    string // default exponential
	baseMs      int32  // default 1000
	capMs       int32  // default 3600000
	custom      []byte
	jitter      int32
	rps, burst  *int32
	maxInflight *int32
	filter      *string
	transform   *string
	headers     string // JSON; default content-type only
	body        []byte // default {"amount":1}
	bodyRef     string // override requests.body_ref
}

type fx struct {
	t                        *testing.T
	q                        *store.Queries
	pool                     *pgxpool.Pool
	dq                       *dqueue.Client
	rdb                      *redis.Client
	h                        *Handler
	log                      *syncBuf
	org, src, dest, conn, ev uuid.UUID
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

var (
	sharedOnce sync.Once
	sharedQ    *store.Queries
	sharedPool *pgxpool.Pool
	sharedErr  error
)

// sharedDB is one pool for the whole test binary: connecting per test costs
// more than the tests themselves. It lives until the process exits.
func sharedDB(t *testing.T) (*store.Queries, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	sharedOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sharedPool, sharedErr = store.NewPool(ctx, dsn, 8)
		if sharedErr == nil {
			sharedQ = store.New(sharedPool)
		}
	})
	if sharedErr != nil {
		t.Fatalf("connect: %v", sharedErr)
	}
	return sharedQ, sharedPool
}

func i32(n int32) *int32 { return &n }

// seed builds org→source→destination→connection→request(+body)→event with the
// retry policy in o, wires a Handler (SSRF opt-out on, so httptest on loopback
// is reachable) and registers cleanup of every row and Redis key it created.
func seed(t *testing.T, o seedOpt) *fx {
	t.Helper()
	q, pool := sharedDB(t)
	dq, rdb := inboundQueue(t)
	ctx := context.Background()
	if o.destType == "" {
		o.destType = "http"
	}
	if o.strategy == "" {
		o.strategy = "exponential"
	}
	if o.baseMs == 0 {
		o.baseMs = 1000
	}
	if o.capMs == 0 {
		o.capMs = 3600000
	}
	if o.maxRetries == 0 && !o.zeroRetries {
		o.maxRetries = 3
	}
	if o.headers == "" {
		o.headers = `{"Content-Type":["application/json"]}`
	}
	if o.body == nil {
		o.body = []byte(`{"amount":1}`)
	}
	f := &fx{t: t, q: q, pool: pool, dq: dq, rdb: rdb, log: &syncBuf{}}

	org, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{Name: "T", Slug: "t-" + uuid.NewString()})
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	f.org = store.GoUUID(org.ID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id=$1`, org.ID)
		rdb.Del(context.Background(),
			"inflight:org:"+f.org.String(), "inflight:dest:"+f.dest.String(),
			"rate:rl:dest:"+f.dest.String(), "cli:source:"+f.src.String(), "cli:dispatch:"+f.src.String())
	})
	src, err := q.CreateSource(ctx, store.CreateSourceParams{
		OrgID: org.ID, Name: "s-" + uuid.NewString(), Type: "generic",
		IngestToken: uuid.NewString(), SigningConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	f.src = store.GoUUID(src.ID)
	dst, err := q.CreateDestination(ctx, store.CreateDestinationParams{
		OrgID: org.ID, Name: "d-" + uuid.NewString(), Type: o.destType,
		Url: o.url, AuthConfig: []byte("{}"),
		RateLimitRps: o.rps, RateLimitBurst: o.burst, MaxInflight: o.maxInflight,
	})
	if err != nil {
		t.Fatalf("destination: %v", err)
	}
	f.dest = store.GoUUID(dst.ID)

	var connID pgtype.UUID
	var filter, tr any
	if o.filter != nil {
		filter = *o.filter
	}
	if o.transform != nil {
		tr = *o.transform
	}
	var custom any
	if o.custom != nil {
		custom = string(o.custom)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO connections
		(source_id, destination_id, enabled, max_retries, retry_strategy, retry_base_ms,
		 retry_cap_ms, retry_jitter_pct, custom_retry_schedule, filter_expr, transform_js)
		VALUES ($1,$2,true,$3,$4,$5,$6,$10,$7::jsonb,$8,$9) RETURNING id`,
		src.ID, dst.ID, o.maxRetries, o.strategy, o.baseMs, o.capMs, custom, filter, tr, o.jitter).Scan(&connID); err != nil {
		t.Fatalf("connection: %v", err)
	}
	f.conn = store.GoUUID(connID)

	reqID := uuid.Must(uuid.NewV7())
	req, err := q.CreateRequest(ctx, store.CreateRequestParams{
		ID: store.UUID(reqID), SourceID: src.ID, HTTPMethod: "POST", HTTPPath: "/e/x",
		Headers: []byte(o.headers), BodyHash: "h", BodyRef: "pg:" + reqID.String(),
		BodySize: int32(len(o.body)), ContentType: strp("application/json"),
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	bs := ingest.NewPostgresBodyStore(q)
	if _, err := bs.Put(ctx, reqID, o.body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if o.bodyRef != "" {
		if _, err := pool.Exec(ctx, `UPDATE requests SET body_ref=$1 WHERE id=$2`, o.bodyRef, req.ID); err != nil {
			t.Fatalf("body_ref: %v", err)
		}
	}
	evs, err := q.CreateEventsBatch(ctx, store.CreateEventsBatchParams{
		RequestID: req.ID, OrgID: org.ID, ConnectionIds: []pgtype.UUID{connID},
	})
	if err != nil || len(evs) != 1 {
		t.Fatalf("event: %v (%d)", err, len(evs))
	}
	f.ev = store.GoUUID(evs[0].ID)

	f.h = New(slogTo(f.log), q, rdb, bs, dq, true)
	return f
}

// pick enqueues a payload for the event and leases it, returning what the
// worker would hand to Process.
func (f *fx) pick(attempt int, manual bool, enqueuedAgo time.Duration) (dqueue.Payload, string) {
	f.t.Helper()
	ctx := context.Background()
	if err := f.dq.Enqueue(ctx, dqueue.Payload{
		EventID: f.ev, OrgID: f.org, Attempt: attempt, Manual: manual,
		EnqueuedAt: time.Now().Add(-enqueuedAgo).UnixMilli(),
	}); err != nil {
		f.t.Fatalf("enqueue: %v", err)
	}
	raw, p, ok, err := f.dq.FairPick(ctx, 60000)
	if err != nil || !ok {
		f.t.Fatalf("fairpick ok=%v err=%v", ok, err)
	}
	return p, raw
}

func (f *fx) process(attempt int) error {
	f.t.Helper()
	p, raw := f.pick(attempt, false, 0)
	return f.h.Process(context.Background(), p, raw)
}

func (f *fx) lane(name string) []dqueue.Item {
	f.t.Helper()
	org := ""
	if name == "pending" {
		org = f.org.String()
	}
	items, _, err := f.dq.Items(context.Background(), name, org, 50)
	if err != nil {
		f.t.Fatalf("items %s: %v", name, err)
	}
	return items
}

// settled asserts the leased member was released and returns the single item in
// the named lane (or fails if the lane does not hold exactly one).
func (f *fx) settled(name string) dqueue.Item {
	f.t.Helper()
	if n := len(f.lane("processing")); n != 0 {
		f.t.Fatalf("processing lane holds %d members; the lease must be released", n)
	}
	items := f.lane(name)
	if len(items) != 1 {
		f.t.Fatalf("%s lane has %d items, want 1", name, len(items))
	}
	if items[0].EventID != f.ev {
		f.t.Fatalf("%s lane holds event %v, want %v", name, items[0].EventID, f.ev)
	}
	return items[0]
}

func (f *fx) state() (status string, attemptCount int32, nextRetry *time.Time) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, attempt_count, next_retry_at FROM events WHERE id=$1`, f.ev).
		Scan(&status, &attemptCount, &nextRetry); err != nil {
		f.t.Fatalf("event state: %v", err)
	}
	return
}

func (f *fx) attempts() []store.Attempt {
	f.t.Helper()
	as, err := f.q.ListAttemptsByEvent(context.Background(), store.UUID(f.ev))
	if err != nil {
		f.t.Fatalf("attempts: %v", err)
	}
	return as
}

func (f *fx) wantState(status string, count int32) {
	f.t.Helper()
	if s, c, _ := f.state(); s != status || c != count {
		f.t.Fatalf("event = (%s, attempts %d), want (%s, %d)", s, c, status, count)
	}
}

// within asserts a ZSET score landed at [before+d, after+d] (ms).
func within(t *testing.T, name string, score int64, before, after time.Time, d time.Duration) {
	t.Helper()
	lo, hi := before.Add(d).UnixMilli(), after.Add(d).UnixMilli()
	if score < lo || score > hi {
		t.Fatalf("%s scheduled for %d, want within [%d, %d] (now+%v)", name, score, lo, hi, d)
	}
}

// ---- destination double ------------------------------------------------------

type seen struct {
	hdr    http.Header
	body   []byte
	method string
}

type dest struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []seen
}

func (d *dest) calls() []seen {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]seen(nil), d.reqs...)
}

// newDest answers the n-th call with codes[n] (the last code repeats) and body.
func newDest(t *testing.T, body string, codes ...int) *dest {
	t.Helper()
	d := &dest{}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.reqs = append(d.reqs, seen{r.Header.Clone(), b, r.Method})
		n := len(d.reqs) - 1
		d.mu.Unlock()
		if n >= len(codes) {
			n = len(codes) - 1
		}
		w.Header().Set("X-Reply", "yes")
		w.WriteHeader(codes[n])
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *dest) url() *string { u := d.URL; return &u }

// ---- fault injection over the REAL pool --------------------------------------

type faultDB struct {
	store.DBTX
	match string // substring of the sqlc statement text ("-- name: X")
	err   error
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

func (f faultDB) Exec(ctx context.Context, sql string, a ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, f.match) {
		return pgconn.CommandTag{}, f.err
	}
	return f.DBTX.Exec(ctx, sql, a...)
}
func (f faultDB) Query(ctx context.Context, sql string, a ...any) (pgx.Rows, error) {
	if strings.Contains(sql, f.match) {
		return nil, f.err
	}
	return f.DBTX.Query(ctx, sql, a...)
}
func (f faultDB) QueryRow(ctx context.Context, sql string, a ...any) pgx.Row {
	if strings.Contains(sql, f.match) {
		return errRow{f.err}
	}
	return f.DBTX.QueryRow(ctx, sql, a...)
}

var errInjected = errors.New("injected db failure")

func (f *fx) failQuery(name string) {
	f.h.Queries = store.New(faultDB{DBTX: f.pool, match: "-- name: " + name + " ", err: errInjected})
}

// deadQueue is a queue whose Redis client is closed: every operation errors.
func deadQueue(t *testing.T) *dqueue.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = c.Close()
	return dqueue.NewClient(c)
}

func slogTo(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

func redisClosed(o *redis.Options) *redis.Client {
	c := redis.NewClient(o)
	_ = c.Close()
	return c
}
