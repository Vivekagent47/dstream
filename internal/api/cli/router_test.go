package cli_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/api"
	apicli "github.com/Vivekagent47/dstream/internal/api/cli"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/metrics"
	"github.com/Vivekagent47/dstream/internal/store"
)

// Through api.Mount with the real auth middleware and a real API key: covers
// the /api/cli/* route wiring, which the in-package harness bypasses.
func TestConnectThroughRouterWithAPIKey(t *testing.T) {
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	addr := os.Getenv("DSTREAM_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { rdb.Close() })
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skip("no redis at " + addr)
	}
	pool, err := store.NewPool(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)

	mkOrg := func() (pgtype.UUID, string) {
		o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{Name: "T", Slug: "clir-" + uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM organizations WHERE id=$1`, o.ID) })
		full, prefix, hash, err := auth.NewAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{OrgID: o.ID, Name: "k", Prefix: prefix, KeyHash: hash, Role: string(auth.RoleMember)}); err != nil {
			t.Fatal(err)
		}
		return o.ID, full
	}
	orgA, keyA := mkOrg()
	_, keyB := mkOrg()
	srcRow, err := q.CreateSource(ctx, store.CreateSourceParams{OrgID: orgA, Name: "mine", Type: "generic", IngestToken: "t-" + uuid.NewString(), SigningConfig: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	src := store.GoUUID(srcRow.ID)
	t.Cleanup(func() { rdb.Del(ctx, apicli.SessionKey(src), apicli.DispatchKey(src)) })

	r := chi.NewRouter()
	api.Mount(r, api.Deps{Log: nil, Queries: q, Redis: rdb, Signer: &auth.SessionSigner{Secret: []byte("test-secret-do-not-use-in-prod")}})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	get := func(path, key string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	connect := "/api/cli/connect?source_id=" + src.String()

	if code, _ := get(connect, ""); code != 401 {
		t.Fatalf("no key: got %d, want 401", code)
	}
	if code, body := get(connect, keyB); code != 404 || body != `{"error":"source not found"}` {
		t.Fatalf("other org's key: got %d %s, want 404 source not found", code, body)
	}
	if code, body := get("/api/cli/sources", keyA); code != 200 || body != `[{"id":"`+src.String()+`","name":"mine","type":"generic"}]` {
		t.Fatalf("sources: got %d %s", code, body)
	}

	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(dctx, srv.URL+connect, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + keyA}}})
	if err != nil {
		t.Fatalf("dial with valid key: %v", err)
	}
	defer c.CloseNow()
	_, b, err := c.Read(dctx)
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(b)), `"source_id":"`+src.String()+`","type":"hello"}`) {
		t.Fatalf("hello = %s (%v)", b, err)
	}
	if rdb.Exists(ctx, apicli.SessionKey(src)).Val() != 1 {
		t.Fatal("session key not registered")
	}
	base := activeSessions(t) - 1 // this tunnel is the one open session we caused
	c.Close(websocket.StatusNormalClosure, "")
	// The handler's dispatch loop sits in a 5s BLPOP; push a skipped payload to
	// wake it so teardown completes, and wait for the gauge so no later test
	// (-shuffle, -count) inherits a leaked session.
	deadline := time.Now().Add(5 * time.Second)
	for activeSessions(t) != base {
		if time.Now().After(deadline) {
			t.Fatal("tunnel did not tear down")
		}
		rdb.RPush(ctx, apicli.DispatchKey(src), "wake")
		time.Sleep(10 * time.Millisecond)
	}
}

func activeSessions(t *testing.T) float64 {
	t.Helper()
	fams, err := metrics.Reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() == "dstream_cli_sessions_active" {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatal("gauge not found")
	return 0
}
