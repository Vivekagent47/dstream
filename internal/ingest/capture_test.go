package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

// captureEnv is a real DB+Redis handler wired for one org/source, with NO
// enabled connections — capture must fire on the no-connections success path,
// not only after a fan-out.
type captureEnv struct {
	h   *Handler
	q   *store.Queries
	org store.Organization
	src store.Source
	mux *chi.Mux
}

func newCaptureEnv(t *testing.T) *captureEnv {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	ctx := context.Background()

	pool, err := store.NewPool(ctx, dsn, 2)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)

	addr := os.Getenv("DSTREAM_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skip("no redis at " + addr)
	}
	prefix := "capturetest-" + uuid.NewString()[:8]
	dq := dqueue.NewClient(rdb).WithPrefix(prefix)
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+":*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
		_ = rdb.Close()
	})

	org, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{Name: "T", Slug: "t-" + uuid.NewString()})
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	token := "tok-" + uuid.NewString()
	src, err := q.CreateSource(ctx, store.CreateSourceParams{
		OrgID: org.ID, Name: "s-" + uuid.NewString(), Type: "generic", Description: "", IngestToken: token, SigningConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("source: %v", err)
	}

	h := &Handler{
		Log:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Queries:   q,
		Redis:     rdb,
		Queue:     dq,
		BodyStore: NewPostgresBodyStore(q),
	}
	mux := chi.NewRouter()
	h.Mount(mux)
	return &captureEnv{h: h, q: q, org: org, src: src, mux: mux}
}

func (e *captureEnv) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/e/"+e.src.IngestToken, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	e.mux.ServeHTTP(rec, req)
	return rec
}

// bookmarksForRule counts this org's bookmarks whose capture_rule_id matches.
func (e *captureEnv) bookmarksForRule(t *testing.T, ruleID pgtype.UUID) int {
	t.Helper()
	rows, err := e.q.ListBookmarksForOrg(context.Background(), store.ListBookmarksForOrgParams{OrgID: e.org.ID})
	if err != nil {
		t.Fatalf("list bookmarks: %v", err)
	}
	n := 0
	for _, b := range rows {
		if b.CaptureRuleID.Valid && b.CaptureRuleID == ruleID {
			n++
		}
	}
	return n
}

func (e *captureEnv) totalBookmarks(t *testing.T) int {
	t.Helper()
	rows, err := e.q.ListBookmarksForOrg(context.Background(), store.ListBookmarksForOrgParams{OrgID: e.org.ID})
	if err != nil {
		t.Fatalf("list bookmarks: %v", err)
	}
	return len(rows)
}

// TestCaptureMatchAllRule: a rule with no filter_expr captures every request.
func TestCaptureMatchAllRule(t *testing.T) {
	e := newCaptureEnv(t)
	rule, err := e.q.CreateCaptureRule(context.Background(), store.CreateCaptureRuleParams{
		OrgID: e.org.ID, SourceID: e.src.ID, Name: "r-" + uuid.NewString(), FilterExpr: nil, Cap: 50, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	rec := e.post(t, `{"hello":"world"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ingest status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if n := e.bookmarksForRule(t, rule.ID); n != 1 {
		t.Fatalf("bookmarks for rule = %d, want 1", n)
	}
}

// TestCaptureFilterNoMatch: a non-matching filter_expr captures nothing, but
// ingest still succeeds.
func TestCaptureFilterNoMatch(t *testing.T) {
	e := newCaptureEnv(t)
	expr := "payload.amount > 1000"
	rule, err := e.q.CreateCaptureRule(context.Background(), store.CreateCaptureRuleParams{
		OrgID: e.org.ID, SourceID: e.src.ID, Name: "r-" + uuid.NewString(), FilterExpr: &expr, Cap: 50, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	rec := e.post(t, `{"amount":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ingest status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if n := e.bookmarksForRule(t, rule.ID); n != 0 {
		t.Fatalf("bookmarks for rule = %d, want 0 (filter didn't match)", n)
	}
}

// TestCaptureEviction: a rule with cap=2 keeps only the newest 2 bookmarks
// after 4 matching requests.
func TestCaptureEviction(t *testing.T) {
	e := newCaptureEnv(t)
	rule, err := e.q.CreateCaptureRule(context.Background(), store.CreateCaptureRuleParams{
		OrgID: e.org.ID, SourceID: e.src.ID, Name: "r-" + uuid.NewString(), FilterExpr: nil, Cap: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	for i := 0; i < 4; i++ {
		rec := e.post(t, fmt.Sprintf(`{"seq":%d}`, i))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("post #%d status = %d, want 202; body=%s", i, rec.Code, rec.Body.String())
		}
		time.Sleep(2 * time.Millisecond) // keep created_at strictly increasing
	}
	if n := e.bookmarksForRule(t, rule.ID); n != 2 {
		t.Fatalf("bookmarks for rule = %d, want 2 (cap enforced)", n)
	}
}

// TestCaptureNoRules: a source with no capture rules never creates a
// bookmark, and ingest is unaffected.
func TestCaptureNoRules(t *testing.T) {
	e := newCaptureEnv(t)

	rec := e.post(t, `{"hello":"world"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ingest status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if n := e.totalBookmarks(t); n != 0 {
		t.Fatalf("bookmarks = %d, want 0 (no rules)", n)
	}
}

// TestCaptureFilterEvalError: a filter that errors at eval time (references a
// field absent from THIS payload) fails closed — no capture — rather than
// capturing spuriously.
func TestCaptureFilterEvalError(t *testing.T) {
	e := newCaptureEnv(t)
	expr := "payload.missing == 1"
	rule, err := e.q.CreateCaptureRule(context.Background(), store.CreateCaptureRuleParams{
		OrgID: e.org.ID, SourceID: e.src.ID, Name: "r-" + uuid.NewString(), FilterExpr: &expr, Cap: 50, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	rec := e.post(t, `{"amount":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ingest status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if n := e.bookmarksForRule(t, rule.ID); n != 0 {
		t.Fatalf("bookmarks for rule = %d, want 0 (fail-closed on eval error)", n)
	}
}
