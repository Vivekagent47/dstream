package outbound

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/webhook"
)

// A full outbound router mounting the read + get routes, for cross-org checks.
func isoRoutes(r chi.Router, h Handlers) {
	r.Route("/applications", func(r chi.Router) {
		r.Get("/{app_id}", h.GetApplication)
		r.Route("/{app_id}/endpoints", func(r chi.Router) { r.Get("/{id}", h.GetEndpoint) })
		r.Route("/{app_id}/messages", func(r chi.Router) { r.Get("/{id}", h.GetMessage) })
	})
}

func TestCrossOrgReadsAre404(t *testing.T) {
	q := store.New(testPool(t))
	h := Handlers{Log: discardLog(), Queries: q}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, sign(t)))
		r.Group(func(r chi.Router) { r.Use(auth.RequireOrg(q)); isoRoutes(r, h) })
	})
	ctx := context.Background()

	// org A owns the resources
	_, aOrg := seedOrg(t, q)
	appA, _ := q.CreateApplication(ctx, store.CreateApplicationParams{OrgID: store.UUID(aOrg), Name: "A", Metadata: []byte(`{}`)})
	sec, _ := webhook.GenerateSecret()
	epA, _ := q.CreateEndpoint(ctx, store.CreateEndpointParams{AppID: appA.ID, OrgID: store.UUID(aOrg), Url: "https://ex.test/a", Secret: sec})
	msgA, _ := q.CreateMessage(ctx, store.CreateMessageParams{AppID: appA.ID, OrgID: store.UUID(aOrg), EventType: "x", Payload: []byte(`{}`), PayloadHash: "h"})

	// org B's session (different user+org)
	bUser, bOrg := seedOrg(t, q)

	appStr := store.GoUUID(appA.ID).String()
	for _, path := range []string{
		"/api/applications/" + appStr,
		"/api/applications/" + appStr + "/endpoints/" + store.GoUUID(epA.ID).String(),
		"/api/applications/" + appStr + "/messages/" + store.GoUUID(msgA.ID).String(),
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet, path, bUser, bOrg, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("cross-org %s: want 404 got %d", path, rec.Code)
		}
	}
}
