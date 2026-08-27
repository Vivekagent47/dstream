package outbound

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
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
	var out []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out) != 1 || out[0]["event_type"] != "invoice.paid" {
		t.Fatalf("unexpected list: %v", out)
	}
}
