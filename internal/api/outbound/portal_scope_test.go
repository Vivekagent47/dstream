package outbound

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

func portalReq(method, path, token string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// TestPortalScopeIsolation mounts the admin app subtree (to create data over
// HTTP) alongside a /portal subtree mirroring router.go, and asserts a portal
// token for app A cannot see or reach app B, that publish is absent, and that a
// bad token is rejected.
func TestPortalScopeIsolation(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	ps := &auth.PortalSigner{Secret: []byte("test-secret-do-not-use-in-prod!!"), TTL: time.Hour}
	h := Handlers{Log: discardLog(), Queries: q, Portal: ps}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		// admin subtree — create apps + endpoints over HTTP
		r.Group(func(r chi.Router) {
			r.Use(auth.Authenticate(q, sign(t)))
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireOrg(q))
				r.Route("/applications", func(r chi.Router) {
					r.Post("/", h.CreateApplication)
					r.Route("/{app_id}/endpoints", func(r chi.Router) { r.Post("/", h.CreateEndpoint) })
				})
			})
		})
		// portal subtree — MIRROR router.go (RequirePortal + same routes;
		// publish POST /messages ABSENT by construction).
		r.Route("/portal", func(r chi.Router) {
			r.Use(auth.RequirePortal(q, ps))
			r.Get("/app", h.GetApplication)
			r.Route("/endpoints", func(r chi.Router) {
				r.Get("/", h.ListEndpoints)
				r.Get("/{id}", h.GetEndpoint)
			})
			r.Route("/messages", func(r chi.Router) {
				r.Get("/", h.ListMessages)
			})
		})
	})

	mkApp := func(name string) string {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications", uid, oid, map[string]any{"name": name}))
		var a map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &a)
		return a["id"].(string)
	}
	mkEp := func(appID, url string) string {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications/"+appID+"/endpoints", uid, oid, map[string]any{"url": url}))
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("mkEp %s: %d %s", appID, rec.Code, rec.Body.String())
		}
		var e map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return e["id"].(string)
	}
	appA := mkApp("A")
	appB := mkApp("B")
	epA := mkEp(appA, "https://ex.test/a")
	epB := mkEp(appB, "https://ex.test/b")

	appAUUID, _ := uuid.Parse(appA)
	tokA, _ := ps.Mint(appAUUID, oid, 0)

	// 1) portal endpoints list with A's token → only A's endpoint (not B's)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, portalReq(http.MethodGet, "/api/portal/endpoints", tokA))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, epB) {
		t.Fatalf("portal A leaked app B's endpoint: %s", body)
	}
	if !strings.Contains(body, epA) {
		t.Fatalf("portal A missing its own endpoint: %s", body)
	}

	// 2) cross-app GET B's endpoint via A's token → 404
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, portalReq(http.MethodGet, "/api/portal/endpoints/"+epB, tokA))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-app endpoint: %d want 404", rec.Code)
	}

	// 3) publish route absent on portal → 404 or 405
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, portalReq(http.MethodPost, "/api/portal/messages", tokA))
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("publish should be absent: %d", rec.Code)
	}

	// 4) bad token → 401
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, portalReq(http.MethodGet, "/api/portal/endpoints", "garbage"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d want 401", rec.Code)
	}
}
