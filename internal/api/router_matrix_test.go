package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// TestRoleMatrix exercises the role × method × path matrix to lock in
// the spec's authorization contract. Each case seeds a fresh org with the
// caller installed at the named role, then issues the request and asserts
// the expected HTTP status.
//
// Some cases (createInvite) need a real Redis to drive the rate limiter;
// those are tagged needsRedis and skipped cleanly if DSTREAM_TEST_REDIS_ADDR
// isn't set. The remaining DB-only cases run from a single shared pool.
//
// DB seeding lives inline (no separate matrix-specific helpers) so the
// failure path for any case is a single test function the developer can
// re-run in isolation.

type matrixEnv struct {
	OrgID  uuid.UUID
	UserID uuid.UUID
	Signer *auth.SessionSigner
	Router http.Handler
}

// matrixCase models one row of the role-permission matrix.
type matrixCase struct {
	name       string
	role       string // "owner" | "admin" | "member"
	method     string
	pathFn     func(e *matrixEnv) string
	bodyFn     func(e *matrixEnv) any // nil for no body
	wantStatus int
	needsRedis bool // createInvite-style cases that touch the rate limiter
	apiKey     bool // when true, authenticate via API key (no session)
}

// setupMatrixEnv seeds an org and a member at the named role. The signer +
// router returned are usable directly with httptest. When needsRedis is set
// the router is built with a real *redis.Client; otherwise Redis is nil.
func setupMatrixEnv(t *testing.T, q *store.Queries, role string, needsRedis bool) *matrixEnv {
	t.Helper()
	ctx := context.Background()

	// Fresh user + org per case so the suite can run in parallel against a
	// shared DB without conflicting on slugs or memberships.
	u, err := q.CreateUser(ctx, store.CreateUserParams{
		Email: "matrix+" + uuid.NewString() + "@example.test",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: "matrix-" + role,
		Slug: "matrix-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
		OrgID:  o.ID,
		UserID: u.ID,
		Role:   role,
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	// Some matrix cases that target POST /api/orgs/{id}/api-keys for a
	// privileged caller need an alternate owner — but for the listed cases
	// we always pick a member whose own role matches; no extra seeding
	// needed.

	signer := &auth.SessionSigner{Secret: []byte("test-secret-do-not-use-in-prod")}
	deps := Deps{
		Queries:       q,
		Signer:        signer,
		PublicBaseURL: "http://test.local",
	}
	if needsRedis {
		addr := os.Getenv("DSTREAM_TEST_REDIS_ADDR")
		if addr == "" {
			t.Skip("DSTREAM_TEST_REDIS_ADDR not set; case needs redis")
		}
		rdb := redis.NewClient(&redis.Options{Addr: addr})
		t.Cleanup(func() { _ = rdb.Close() })
		// Flush any stale rate-limit counters so back-to-back runs don't
		// trip 429 from a previous test's identity.
		_ = rdb.FlushDB(ctx).Err()
		deps.Redis = rdb
	}

	router := chi.NewRouter()
	Mount(router, deps)
	return &matrixEnv{
		OrgID:  store.GoUUID(o.ID),
		UserID: store.GoUUID(u.ID),
		Signer: signer,
		Router: router,
	}
}

// matrix is the table of (role, method, path) → expected-status cases.
// Coverage focuses on the highest-signal authorization boundaries from the
// spec: invite create, api-key create, org delete, audit access, plus a
// read-path baseline that all roles can hit.
var matrix = []matrixCase{
	// Read paths — any member can list.
	{
		name:       "member GET /api/sources → 200",
		role:       "member",
		method:     http.MethodGet,
		pathFn:     func(e *matrixEnv) string { return "/api/sources" },
		wantStatus: http.StatusOK,
	},
	{
		name:       "member GET /api/orgs/{id}/members → 200",
		role:       "member",
		method:     http.MethodGet,
		pathFn:     func(e *matrixEnv) string { return "/api/orgs/" + e.OrgID.String() + "/members" },
		wantStatus: http.StatusOK,
	},
	{
		name:       "member GET /api/audit → 200",
		role:       "member",
		method:     http.MethodGet,
		pathFn:     func(e *matrixEnv) string { return "/api/audit" },
		wantStatus: http.StatusOK,
	},

	// Mutations on resources — any role can per spec.
	{
		name:   "member POST /api/sources → 201",
		role:   "member",
		method: http.MethodPost,
		pathFn: func(e *matrixEnv) string { return "/api/sources" },
		bodyFn: func(e *matrixEnv) any {
			return map[string]any{"name": "m-src-" + uuid.NewString()[:6], "type": "generic"}
		},
		wantStatus: http.StatusCreated,
	},

	// Invites — admin+ only.
	{
		name:   "member POST /api/orgs/{id}/invites → 403",
		role:   "member",
		method: http.MethodPost,
		pathFn: func(e *matrixEnv) string { return "/api/orgs/" + e.OrgID.String() + "/invites" },
		bodyFn: func(e *matrixEnv) any {
			return map[string]any{"email": "x+" + uuid.NewString()[:6] + "@example.test", "role": "member"}
		},
		wantStatus: http.StatusForbidden,
		// member is rejected before the rate limiter ever runs, so no redis needed.
	},
	{
		name:   "admin POST /api/orgs/{id}/invites → 202",
		role:   "admin",
		method: http.MethodPost,
		pathFn: func(e *matrixEnv) string { return "/api/orgs/" + e.OrgID.String() + "/invites" },
		bodyFn: func(e *matrixEnv) any {
			return map[string]any{"email": "x+" + uuid.NewString()[:6] + "@example.test", "role": "member"}
		},
		wantStatus: http.StatusAccepted,
		needsRedis: true,
	},

	// API keys — admin+ only.
	{
		name:   "member POST /api/orgs/{id}/api-keys → 403",
		role:   "member",
		method: http.MethodPost,
		pathFn: func(e *matrixEnv) string { return "/api/orgs/" + e.OrgID.String() + "/api-keys" },
		bodyFn: func(e *matrixEnv) any {
			return map[string]any{"name": "k-" + uuid.NewString()[:6]}
		},
		wantStatus: http.StatusForbidden,
	},
	{
		name:   "admin POST /api/orgs/{id}/api-keys → 201",
		role:   "admin",
		method: http.MethodPost,
		pathFn: func(e *matrixEnv) string { return "/api/orgs/" + e.OrgID.String() + "/api-keys" },
		bodyFn: func(e *matrixEnv) any {
			return map[string]any{"name": "k-" + uuid.NewString()[:6]}
		},
		wantStatus: http.StatusCreated,
	},

	// Org delete — owner only.
	{
		name:       "admin DELETE /api/orgs/{id} → 403",
		role:       "admin",
		method:     http.MethodDelete,
		pathFn:     func(e *matrixEnv) string { return "/api/orgs/" + e.OrgID.String() },
		wantStatus: http.StatusForbidden,
	},
	{
		name:       "owner DELETE /api/orgs/{id} → 204",
		role:       "owner",
		method:     http.MethodDelete,
		pathFn:     func(e *matrixEnv) string { return "/api/orgs/" + e.OrgID.String() },
		wantStatus: http.StatusNoContent,
	},

	// API key principal cannot list members (session-only endpoint).
	{
		name:       "api_key GET /api/orgs/{id}/members → 403",
		role:       "owner", // role on the org is irrelevant — auth is by key
		method:     http.MethodGet,
		pathFn:     func(e *matrixEnv) string { return "/api/orgs/" + e.OrgID.String() + "/members" },
		wantStatus: http.StatusForbidden,
		apiKey:     true,
	},
}

func TestRoleMatrix(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)

	for _, tc := range matrix {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			env := setupMatrixEnv(t, q, tc.role, tc.needsRedis)

			var bodyReader io.Reader
			if tc.bodyFn != nil {
				buf := new(bytes.Buffer)
				if err := json.NewEncoder(buf).Encode(tc.bodyFn(env)); err != nil {
					t.Fatalf("encode body: %v", err)
				}
				bodyReader = buf
			}

			req := httptest.NewRequest(tc.method, tc.pathFn(env), bodyReader)
			if bodyReader != nil {
				req.Header.Set("Content-Type", "application/json")
			}

			switch {
			case tc.apiKey:
				// Seed a real API key on this org and authenticate with it.
				full, prefix, hash, err := auth.NewAPIKey()
				if err != nil {
					t.Fatalf("new api key: %v", err)
				}
				if _, err := q.CreateAPIKey(context.Background(), store.CreateAPIKeyParams{
					OrgID:   store.UUID(env.OrgID),
					Name:    "matrix",
					Prefix:  prefix,
					KeyHash: hash,
				}); err != nil {
					t.Fatalf("persist api key: %v", err)
				}
				req.Header.Set("Authorization", "Bearer "+full)
			default:
				attachSession(t, req, env.Signer, env.UserID, env.OrgID)
			}

			rec := httptest.NewRecorder()
			env.Router.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d want %d; body=%s",
					rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// attachSession signs a session cookie for (userID, orgID) and attaches it
// to req. Uses the same flow as the existing requestWithSession helper but
// works against an already-built request, which the matrix needs.
func attachSession(t *testing.T, req *http.Request, s *auth.SessionSigner, userID, orgID uuid.UUID) {
	t.Helper()
	w := httptest.NewRecorder()
	s.Issue(w, userID, orgID, 0)
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			req.AddCookie(c)
		}
	}
}
