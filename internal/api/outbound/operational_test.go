package outbound

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/Vivekagent47/dstream/internal/store"
)

// opRoutes mounts just the routes under test: get-or-create the op app and the
// normal application list (which must exclude it).
func opRoutes(r chi.Router, h Handlers) {
	r.Get("/operational-app", h.GetOperationalApp)
	r.Get("/applications", h.ListApplications)
}

func TestGetOperationalApp(t *testing.T) {
	q := store.New(testPool(t))
	r := newRouter(q, nil, sign(t), opRoutes)
	user, org := seedOrg(t, q)

	// first call: 200 + is_operational:true, get-or-create.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet, "/api/operational-app", user, org, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 got %d: %s", rec.Code, rec.Body.String())
	}
	var first map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first["is_operational"] != true {
		t.Fatalf("is_operational: want true got %v", first["is_operational"])
	}
	id1, _ := first["id"].(string)
	if id1 == "" {
		t.Fatal("missing id")
	}

	// second call: same id (idempotent get-or-create).
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, sessionReq(t, sign(t), http.MethodGet, "/api/operational-app", user, org, nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second call: want 200 got %d", rec2.Code)
	}
	var second map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if second["id"] != id1 {
		t.Fatalf("second id: want %s got %v", id1, second["id"])
	}

	// the op app must NOT appear in GET /api/applications.
	recL := httptest.NewRecorder()
	r.ServeHTTP(recL, sessionReq(t, sign(t), http.MethodGet, "/api/applications", user, org, nil))
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recL.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, a := range list.Data {
		if a["id"] == id1 {
			t.Fatal("operational app must not appear in GET /api/applications")
		}
	}
}
