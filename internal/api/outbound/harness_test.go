package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// uuidNewShort is a short unique prefix so parallel runs don't collide on Redis.
func uuidNewShort() string { return uuid.NewString()[:8] }

// sign returns a process-wide signer with a fixed secret so the router's
// Authenticate middleware and the request cookie agree.
var testSigner = &auth.SessionSigner{Secret: []byte("test-secret-do-not-use-in-prod-000")}

func sign(_ *testing.T) *auth.SessionSigner { return testSigner }

func testPool(t *testing.T) *pgxpool.Pool {
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
	return pool
}

func testRedis(t *testing.T) *redis.Client {
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
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// seedOrg makes a user + org + owner membership with unique slug/email and
// returns their Go UUIDs (mirrors internal/api seedUserAndOrg).
func seedOrg(t *testing.T, q *store.Queries) (userID, orgID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	u, err := q.CreateUser(ctx, store.CreateUserParams{Email: "t+" + uuid.NewString() + "@example.test"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{Name: "T", Slug: "t-" + uuid.NewString()})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{OrgID: o.ID, UserID: u.ID, Role: "owner"}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return store.GoUUID(u.ID), store.GoUUID(o.ID)
}

// newRouter mounts /api with the real Authenticate + RequireOrg middleware and
// lets the caller register just the routes under test via `register`.
func newRouter(q *store.Queries, dq *dqueue.Client, s *auth.SessionSigner, register func(chi.Router, Handlers)) *chi.Mux {
	h := Handlers{Log: discardLog(), Queries: q, Queue: dq}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, s))
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireOrg(q))
			register(r, h)
		})
	})
	return r
}

// sessionReq builds a request carrying a valid session cookie for (userID, orgID).
func sessionReq(t *testing.T, s *auth.SessionSigner, method, path string, userID, orgID uuid.UUID, body any) *http.Request {
	t.Helper()
	w := httptest.NewRecorder()
	s.Issue(w, userID, orgID, 0)
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			r.AddCookie(c)
		}
	}
	return r
}
