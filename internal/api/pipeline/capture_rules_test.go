package pipeline

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

func captureRuleRoutes(r chi.Router, h Handlers) {
	r.Route("/capture-rules", func(r chi.Router) {
		r.Get("/", h.ListCaptureRules)
		r.Post("/", h.CreateCaptureRule)
		r.Get("/{id}", h.GetCaptureRule)
		r.Patch("/{id}", h.PatchCaptureRule)
		r.Delete("/{id}", h.DeleteCaptureRule)
	})
}

// seedSource creates a bare source in orgID (no request needed, unlike
// seedRequest) for capture-rule tests.
func seedSource(t *testing.T, q *store.Queries, orgID uuid.UUID) uuid.UUID {
	t.Helper()
	src, err := q.CreateSource(context.Background(), store.CreateSourceParams{
		OrgID: store.UUID(orgID), Name: "src-" + uuid.NewString(), Type: "generic",
		IngestToken: "tok-" + uuid.NewString(), SigningConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	return store.GoUUID(src.ID)
}

func TestCreateCaptureRule(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	srcID := seedSource(t, q, oid)
	r := newRouter(q, captureRuleRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/capture-rules", uid, oid,
		map[string]any{"source_id": srcID.String(), "name": "rule-1", "filter_expr": "true", "cap": 100}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["name"] != "rule-1" || got["source_id"] != srcID.String() || got["enabled"] != true {
		t.Fatalf("body: %v", got)
	}
	if got["cap"].(float64) != 100 {
		t.Fatalf("cap: %v", got["cap"])
	}
}

func TestCreateCaptureRuleDefaultsCap(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	srcID := seedSource(t, q, oid)
	r := newRouter(q, captureRuleRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/capture-rules", uid, oid,
		map[string]any{"source_id": srcID.String(), "name": "rule-default"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["cap"].(float64) != 50 {
		t.Fatalf("default cap: %v", got["cap"])
	}
	if got["filter_expr"] != nil {
		t.Fatalf("filter_expr should be null: %v", got["filter_expr"])
	}
}

func TestCreateCaptureRuleBadFilter(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	srcID := seedSource(t, q, oid)
	r := newRouter(q, captureRuleRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/capture-rules", uid, oid,
		map[string]any{"source_id": srcID.String(), "name": "bad-filter", "filter_expr": "not valid cel {{"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad filter: got %d want 400 body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateCaptureRuleForeignSource(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	_, otherOrg := seedOrg(t, q)
	foreignSrc := seedSource(t, q, otherOrg)
	r := newRouter(q, captureRuleRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/capture-rules", uid, oid,
		map[string]any{"source_id": foreignSrc.String(), "name": "x"}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign source: got %d want 404 body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateCaptureRuleDuplicateName(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	srcID := seedSource(t, q, oid)
	r := newRouter(q, captureRuleRoutes)
	body := map[string]any{"source_id": srcID.String(), "name": "dup-rule"}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/capture-rules", uid, oid, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first: got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/capture-rules", uid, oid, body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("dup name: got %d want 409", rec.Code)
	}
}

func TestCreateCaptureRuleCapOutOfRange(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	srcID := seedSource(t, q, oid)
	r := newRouter(q, captureRuleRoutes)

	for _, cap := range []int{0, 1001} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/capture-rules", uid, oid,
			map[string]any{"source_id": srcID.String(), "name": "cap-" + uuid.NewString(), "cap": cap}))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("cap=%d: got %d want 400 body=%s", cap, rec.Code, rec.Body.String())
		}
	}
}

func TestListGetCaptureRule(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	srcID := seedSource(t, q, oid)
	otherSrcID := seedSource(t, q, oid)
	r := newRouter(q, captureRuleRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/capture-rules", uid, oid,
		map[string]any{"source_id": srcID.String(), "name": "list-1"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/capture-rules", uid, oid,
		map[string]any{"source_id": otherSrcID.String(), "name": "list-2"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create 2: %d", rec.Code)
	}

	// list all → 2
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/capture-rules", uid, oid, nil))
	var all []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &all)
	if len(all) != 2 {
		t.Fatalf("list all: got %d want 2: %v", len(all), all)
	}

	// list filtered by source_id → 1
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/capture-rules?source_id="+srcID.String(), uid, oid, nil))
	var filtered []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &filtered)
	if len(filtered) != 1 || filtered[0]["id"] != id {
		t.Fatalf("list filtered: %v", filtered)
	}

	// get
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/capture-rules/"+id, uid, oid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}

	// get from another org (a member of THAT org, not this rule's org) → 404
	otherUID, otherOrg := seedOrg(t, q)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/capture-rules/"+id, otherUID, otherOrg, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get foreign org: got %d want 404", rec.Code)
	}
}

func TestPatchCaptureRule(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	srcID := seedSource(t, q, oid)
	r := newRouter(q, captureRuleRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/capture-rules", uid, oid,
		map[string]any{"source_id": srcID.String(), "name": "patch-me", "cap": 10}))
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	// disable
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPatch, "/api/capture-rules/"+id, uid, oid,
		map[string]any{"enabled": false}))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch disable: got %d body=%s", rec.Code, rec.Body.String())
	}
	var patched map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if patched["enabled"] != false {
		t.Fatalf("enabled not reflected: %v", patched)
	}
	// untouched fields survive the merge
	if patched["name"] != "patch-me" || patched["cap"].(float64) != 10 {
		t.Fatalf("merge lost fields: %v", patched)
	}

	// bad filter → 400, and the rule is unchanged (still disabled)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPatch, "/api/capture-rules/"+id, uid, oid,
		map[string]any{"filter_expr": "not valid cel {{"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch bad filter: got %d want 400 body=%s", rec.Code, rec.Body.String())
	}

	// cap out of range → 400
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPatch, "/api/capture-rules/"+id, uid, oid,
		map[string]any{"cap": 5000}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch bad cap: got %d want 400 body=%s", rec.Code, rec.Body.String())
	}

	// patch from another org (a member of THAT org, not this rule's org) → 404
	otherUID, otherOrg := seedOrg(t, q)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPatch, "/api/capture-rules/"+id, otherUID, otherOrg,
		map[string]any{"enabled": true}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch foreign org: got %d want 404", rec.Code)
	}
}

func TestDeleteCaptureRule(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	srcID := seedSource(t, q, oid)
	r := newRouter(q, captureRuleRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/capture-rules", uid, oid,
		map[string]any{"source_id": srcID.String(), "name": "delete-me"}))
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodDelete, "/api/capture-rules/"+id, uid, oid, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/capture-rules/"+id, uid, oid, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: got %d want 404", rec.Code)
	}

	// delete again (already gone) → 404
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodDelete, "/api/capture-rules/"+id, uid, oid, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete again: got %d want 404", rec.Code)
	}
}
