package outbound

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/Vivekagent47/dstream/internal/store"
)

func etRoutes(r chi.Router, h Handlers) {
	r.Route("/event-types", func(r chi.Router) {
		r.Get("/", h.ListEventTypes)
		r.Post("/", h.CreateEventType)
		r.Get("/{name}", h.GetEventType)
		r.Patch("/{name}", h.PatchEventType)
		r.Delete("/{name}", h.DeleteEventType)
	})
}

func TestCreateAndGetEventType(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, nil, sign(t), etRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/event-types", uid, oid,
		map[string]any{"name": "invoice.paid", "description": "an invoice was paid"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet, "/api/event-types/invoice.paid", uid, oid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet, "/api/event-types/nope", uid, oid, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get unknown: got %d want 404", rec.Code)
	}
}

func TestCreateEventTypeDuplicate(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, nil, sign(t), etRoutes)

	body := map[string]any{"name": "user.created", "description": "a user was created"}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/event-types", uid, oid, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/event-types", uid, oid, body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: got %d want 409 body=%s", rec.Code, rec.Body.String())
	}
}
