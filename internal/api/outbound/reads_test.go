package outbound

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/webhook"
)

func readRoutes(r chi.Router, h Handlers) {
	r.Route("/applications", func(r chi.Router) {
		r.Post("/", h.CreateApplication)
		r.Route("/{app_id}/messages", func(r chi.Router) {
			r.Get("/", h.ListMessages)
			r.Get("/{id}", h.GetMessage)
		})
	})
}

func TestListMessages(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, nil, sign(t), readRoutes)
	ctx := context.Background()

	// app + one message inserted directly via store
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications", uid, oid, map[string]any{"name": "A"}))
	var app map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &app)
	appUUID, _ := uuid.Parse(app["id"].(string))
	if _, err := q.CreateMessage(ctx, store.CreateMessageParams{
		AppID: store.UUID(appUUID), OrgID: store.UUID(oid),
		EventType: "invoice.paid", Payload: []byte(`{"x":1}`), PayloadHash: "h",
	}); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet, "/api/applications/"+app["id"].(string)+"/messages", uid, oid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: got %d", rec.Code)
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	out := body.Data
	if len(out) != 1 || out[0]["event_type"] != "invoice.paid" {
		t.Fatalf("unexpected list: %v", out)
	}
}

func TestListMessageDeliveries(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	h := Handlers{Log: discardLog(), Queries: q}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, sign(t)))
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireOrg(q))
			r.Route("/applications/{app_id}/messages", func(r chi.Router) {
				r.Get("/{id}/deliveries", h.ListMessageDeliveries)
			})
		})
	})
	ctx := context.Background()
	app, _ := q.CreateApplication(ctx, store.CreateApplicationParams{OrgID: store.UUID(oid), Name: "A", Metadata: []byte(`{}`)})
	sec, _ := webhook.GenerateSecret()
	ep1, _ := q.CreateEndpoint(ctx, store.CreateEndpointParams{AppID: app.ID, OrgID: store.UUID(oid), Url: "https://ex.test/1", Secret: sec})
	ep2, _ := q.CreateEndpoint(ctx, store.CreateEndpointParams{AppID: app.ID, OrgID: store.UUID(oid), Url: "https://ex.test/2", Secret: sec})
	msg, _ := q.CreateMessage(ctx, store.CreateMessageParams{AppID: app.ID, OrgID: store.UUID(oid), EventType: "x", Payload: []byte(`{}`), PayloadHash: "h"})
	_, _ = q.CreateMessageDeliveriesBatch(ctx, store.CreateMessageDeliveriesBatchParams{MessageID: msg.ID, OrgID: store.UUID(oid), EndpointIds: []pgtype.UUID{ep1.ID, ep2.ID}})

	path := "/api/applications/" + store.GoUUID(app.ID).String() + "/messages/" + store.GoUUID(msg.ID).String() + "/deliveries"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet, path, uid, oid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	var out []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out) != 2 {
		t.Fatalf("want 2 deliveries, got %d", len(out))
	}
	if out[0]["endpoint_url"] == nil || out[0]["status"] == nil {
		t.Fatalf("delivery view missing endpoint_url/status: %v", out[0])
	}
}
