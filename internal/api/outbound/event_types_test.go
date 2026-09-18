package outbound

import (
	"encoding/json"
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

// A blank schema box in the UI sends {"schema":null}. That must not 400 (it
// previously compiled the literal "null" and failed), and on a typed event type
// it clears the stored schema.
func TestPatchEventTypeNullSchema(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, nil, sign(t), etRoutes)

	// schema-less event type: editing description + archiving with schema:null must succeed.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/event-types", uid, oid,
		map[string]any{"name": "no.schema", "description": "d"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPatch, "/api/event-types/no.schema", uid, oid,
		map[string]any{"description": "d2", "schema": nil, "archived": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch null schema: got %d want 200 body=%s", rec.Code, rec.Body.String())
	}

	// typed event type: schema:null clears it.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/event-types", uid, oid,
		map[string]any{"name": "has.schema", "schema": map[string]any{"type": "object"}}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create typed: got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPatch, "/api/event-types/has.schema", uid, oid,
		map[string]any{"schema": nil}))
	if rec.Code != http.StatusOK {
		t.Fatalf("clear schema: got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet, "/api/event-types/has.schema", uid, oid, nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["schema"] != nil {
		t.Errorf("schema not cleared: got %v", got["schema"])
	}
}
