package outbound

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

func TestCreateAndRevokePortalAccess(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	ps := &auth.PortalSigner{Secret: []byte("test-secret-do-not-use-in-prod!!"), TTL: time.Hour}
	h := Handlers{Log: discardLog(), Queries: q, Portal: ps, AppBaseURL: "https://app.example.test"}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, sign(t)))
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireOrg(q))
			r.Route("/applications", func(r chi.Router) {
				r.Post("/", h.CreateApplication)
				r.Post("/{app_id}/portal-access", h.CreatePortalAccess)
				r.Post("/{app_id}/portal-access/revoke", h.RevokePortalAccess)
			})
		})
	})

	// create app
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications", uid, oid, map[string]any{"name": "A"}))
	var app map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &app)
	appID := app["id"].(string)

	// mint
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications/"+appID+"/portal-access", uid, oid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var mint map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &mint)
	tok, _ := mint["token"].(string)
	url, _ := mint["url"].(string)
	if tok == "" || url == "" {
		t.Fatalf("empty mint: %s", rec.Body.String())
	}
	// url must be AppBaseURL + /portal#token=<tok>
	if url != "https://app.example.test/portal#token="+tok {
		t.Fatalf("bad url: %s", url)
	}
	appUUID, _ := uuid.Parse(appID)
	gotApp, _, epoch, err := ps.Parse(tok)
	if err != nil || gotApp != appUUID || epoch != 0 {
		t.Fatalf("token bad: app=%v epoch=%d err=%v", gotApp, epoch, err)
	}

	// revoke
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications/"+appID+"/portal-access/revoke", uid, oid, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	row, _ := q.GetApplicationForOrg(context.Background(), store.GetApplicationForOrgParams{ID: store.UUID(appUUID), OrgID: store.UUID(oid)})
	if row.PortalEpoch != 1 {
		t.Fatalf("epoch after revoke = %d want 1", row.PortalEpoch)
	}
}
