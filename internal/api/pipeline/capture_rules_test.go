package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/auth"
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

func seedRule(t *testing.T, q *store.Queries, oid uuid.UUID, src store.Source, name string) store.CaptureRule {
	t.Helper()
	cr, err := q.CreateCaptureRule(context.Background(), store.CreateCaptureRuleParams{
		OrgID: store.UUID(oid), SourceID: src.ID, Name: name, FilterExpr: pstr("true"), Cap: 10, Enabled: true,
	})
	if err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	return cr
}

func ruleRow(t *testing.T, q *store.Queries, oid uuid.UUID, id pgtype.UUID) store.CaptureRule {
	t.Helper()
	cr, err := q.GetCaptureRuleForOrg(context.Background(), store.GetCaptureRuleForOrgParams{ID: id, OrgID: store.UUID(oid)})
	if err != nil {
		t.Fatalf("load rule: %v", err)
	}
	return cr
}

func TestCaptureRuleViewShape(t *testing.T) {
	q := store.New(testPool(t))
	_, oid := seedOrg(t, q)
	src := newSource(t, q, oid)
	cr := seedRule(t, q, oid, src, "shape")
	// Update so updated_at differs from created_at; a swapped field then shows.
	if _, err := q.UpdateCaptureRule(context.Background(), store.UpdateCaptureRuleParams{
		ID: cr.ID, OrgID: cr.OrgID, Name: cr.Name, FilterExpr: cr.FilterExpr, Cap: cr.Cap, Enabled: cr.Enabled,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	cr = ruleRow(t, q, oid, cr.ID)
	if !cr.UpdatedAt.Time.After(cr.CreatedAt.Time) {
		t.Fatalf("updated_at %v did not move past created_at %v", cr.UpdatedAt.Time, cr.CreatedAt.Time)
	}
	v := captureRuleView(cr)
	mustEq(t, "id", v["id"], store.GoUUID(cr.ID).String())
	mustEq(t, "source_id", v["source_id"], store.GoUUID(src.ID).String())
	mustEq(t, "name", v["name"], "shape")
	mustEq(t, "cap", v["cap"], int32(10))
	mustEq(t, "enabled", v["enabled"], true)
	if *v["filter_expr"].(*string) != "true" || !v["created_at"].(time.Time).Equal(cr.CreatedAt.Time) || !v["updated_at"].(time.Time).Equal(cr.UpdatedAt.Time) {
		t.Fatalf("view: %v", v)
	}
}

func TestValidCaptureRuleCap(t *testing.T) {
	for cap, want := range map[int32]bool{-1: false, 0: false, 1: true, 50: true, 1000: true, 1001: false} {
		if got := validCaptureRuleCap(cap); got != want {
			t.Fatalf("cap %d: got %v want %v", cap, got, want)
		}
	}
}

func TestCaptureRuleUnauthenticated(t *testing.T) {
	h := Handlers{Log: discardLog(), Queries: store.New(testPool(t))}
	for name, fn := range map[string]http.HandlerFunc{
		"Create": h.CreateCaptureRule, "List": h.ListCaptureRules, "Get": h.GetCaptureRule,
		"Patch": h.PatchCaptureRule, "Delete": h.DeleteCaptureRule,
	} {
		for who, p := range map[string]*auth.Principal{"no principal": nil, "no active org": {UserID: uuid.New()}} {
			rec := direct(context.Background(), fn, http.MethodGet, p, uuid.NewString(), `{}`)
			if rec.Code != http.StatusUnauthorized || decodeMap(t, rec)["error"] != "active org required" {
				t.Fatalf("%s (%s): %d %s", name, who, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestCaptureRuleCreateValidation(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	src := newSource(t, q, oid)
	sid := store.GoUUID(src.ID).String()
	var evicted int
	r := newRouter(q, func(r chi.Router, h Handlers) {
		h.EvictSourceCache = func(string) { evicted++ }
		captureRuleRoutes(r, h)
	})
	post := func(body map[string]any) *httptest.ResponseRecorder {
		return do(t, r, http.MethodPost, "/api/capture-rules", uid, oid, body)
	}

	// Malformed JSON.
	h := Handlers{Log: discardLog(), Queries: q}
	rec := direct(context.Background(), h.CreateCaptureRule, http.MethodPost, &auth.Principal{UserID: uid, OrgID: oid}, "", "{nope")
	wantErr(t, rec, http.StatusBadRequest, "invalid json")

	wantErr(t, post(map[string]any{"source_id": sid}), http.StatusBadRequest, "name and source_id are required")
	wantErr(t, post(map[string]any{"name": "n"}), http.StatusBadRequest, "name and source_id are required")
	wantErr(t, post(map[string]any{"name": "n", "source_id": "nope"}), http.StatusBadRequest, "invalid source_id")
	wantErr(t, post(map[string]any{"name": "n", "source_id": uuid.NewString()}), http.StatusNotFound, "source not found")

	rec = post(map[string]any{"name": "n", "source_id": sid, "filter_expr": "not valid cel {{"})
	wantStatus(t, rec, http.StatusBadRequest)
	if msg, _ := decodeMap(t, rec)["error"].(string); !strings.HasPrefix(msg, "invalid filter_expr: ") {
		t.Fatalf("filter error: %q", msg)
	}
	wantErr(t, post(map[string]any{"name": "n", "source_id": sid, "cap": 0}), http.StatusBadRequest, "cap must be between 1 and 1000")
	wantErr(t, post(map[string]any{"name": "n", "source_id": sid, "cap": 1001}), http.StatusBadRequest, "cap must be between 1 and 1000")

	// Nothing was stored by any refusal.
	if n := rowCount(t, pool, `SELECT count(*) FROM capture_rules WHERE org_id=$1`, oid); n != 0 {
		t.Fatalf("%d rules stored by refused creates", n)
	}
	mustEq(t, "evictions after refusals", evicted, 0)

	// Both ends of the cap range are accepted; an empty filter is stored as-is.
	for name, cap := range map[string]int{"lo": 1, "hi": 1000} {
		rec := post(map[string]any{"name": name, "source_id": sid, "cap": cap, "filter_expr": ""})
		wantStatus(t, rec, http.StatusCreated)
		m := decodeMap(t, rec)
		mustEq(t, name+" cap", m["cap"], float64(cap))
		mustEq(t, name+" filter", m["filter_expr"], "")
	}
	mustEq(t, "evictions after accepted creates", evicted, 2)
}

func TestCaptureRuleHandlersCrossOrgIsolation(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oidA := seedOrg(t, q)
	uidB, oidB := seedOrg(t, q)
	var evicted int
	r := newRouter(q, func(r chi.Router, h Handlers) {
		h.EvictSourceCache = func(string) { evicted++ }
		captureRuleRoutes(r, h)
	})
	src := newSource(t, q, oidA)
	cr := seedRule(t, q, oidA, src, "mine")
	id := store.GoUUID(cr.ID).String()

	wantErr(t, do(t, r, http.MethodGet, "/api/capture-rules/"+id, uidB, oidB, nil), http.StatusNotFound, "capture rule not found")
	wantErr(t, do(t, r, http.MethodPatch, "/api/capture-rules/"+id, uidB, oidB, map[string]any{"name": "hijack", "enabled": false, "cap": 999}), http.StatusNotFound, "capture rule not found")
	wantErr(t, do(t, r, http.MethodDelete, "/api/capture-rules/"+id, uidB, oidB, nil), http.StatusNotFound, "capture rule not found")
	wantErr(t, do(t, r, http.MethodPost, "/api/capture-rules", uidB, oidB, map[string]any{"name": "x", "source_id": store.GoUUID(src.ID).String()}), http.StatusNotFound, "source not found")
	if l := decodeList(t, do(t, r, http.MethodGet, "/api/capture-rules?source_id="+store.GoUUID(src.ID).String(), uidB, oidB, nil)); len(l) != 0 {
		t.Fatalf("org B lists org A's rules: %v", l)
	}
	if evicted != 0 {
		t.Fatalf("cache evicted %d times for refused requests", evicted)
	}
	got := ruleRow(t, q, oidA, cr.ID)
	mustEq(t, "name", got.Name, "mine")
	mustEq(t, "enabled", got.Enabled, true)
	mustEq(t, "cap", got.Cap, int32(10))
	mustEq(t, "rules in A", rowCount(t, pool, `SELECT count(*) FROM capture_rules WHERE org_id=$1`, oidA), 1)
}

func TestCaptureRuleCacheEviction(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	src := newSource(t, q, oid)
	var evicted []string
	r := newRouter(q, func(r chi.Router, h Handlers) {
		h.EvictSourceCache = func(tok string) { evicted = append(evicted, tok) }
		captureRuleRoutes(r, h)
	})
	want := func(n int) {
		t.Helper()
		if len(evicted) != n {
			t.Fatalf("evictions = %v, want %d", evicted, n)
		}
		for _, tok := range evicted {
			if tok != src.IngestToken {
				t.Fatalf("evicted %q, want the source's ingest token %q", tok, src.IngestToken)
			}
		}
	}

	rec := do(t, r, http.MethodPost, "/api/capture-rules", uid, oid, map[string]any{"name": "ev", "source_id": store.GoUUID(src.ID).String()})
	wantStatus(t, rec, http.StatusCreated)
	id := decodeMap(t, rec)["id"].(string)
	want(1)

	wantStatus(t, do(t, r, http.MethodPatch, "/api/capture-rules/"+id, uid, oid, map[string]any{"enabled": false}), http.StatusOK)
	want(2)

	// A refused patch (bad filter) must not evict.
	wantStatus(t, do(t, r, http.MethodPatch, "/api/capture-rules/"+id, uid, oid, map[string]any{"filter_expr": "{{"}), http.StatusBadRequest)
	want(2)

	wantStatus(t, do(t, r, http.MethodDelete, "/api/capture-rules/"+id, uid, oid, nil), http.StatusNoContent)
	want(3)
}

func TestCaptureRulePatchMerge(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	src := newSource(t, q, oid)
	other := seedRule(t, q, oid, src, "taken")
	cr := seedRule(t, q, oid, src, "orig")
	pid := cr.ID
	id := store.GoUUID(cr.ID).String()
	var evicted int
	r := newRouter(q, func(r chi.Router, h Handlers) {
		h.EvictSourceCache = func(string) { evicted++ }
		captureRuleRoutes(r, h)
	})
	patch := func(body map[string]any) *httptest.ResponseRecorder {
		return do(t, r, http.MethodPatch, "/api/capture-rules/"+id, uid, oid, body)
	}

	// An empty patch changes nothing.
	wantStatus(t, patch(map[string]any{}), http.StatusOK)
	got := ruleRow(t, q, oid, pid)
	mustEq(t, "name", got.Name, "orig")
	mustEq(t, "cap", got.Cap, int32(10))
	mustEq(t, "enabled", got.Enabled, true)
	mustEq(t, "filter", *got.FilterExpr, "true")

	// Each field alone, the rest preserved.
	rec := patch(map[string]any{"name": "renamed"})
	wantStatus(t, rec, http.StatusOK)
	mustEq(t, "resp name", decodeMap(t, rec)["name"], "renamed")
	rec = patch(map[string]any{"filter_expr": "payload.a == 1", "cap": 7})
	wantStatus(t, rec, http.StatusOK)
	got = ruleRow(t, q, oid, pid)
	mustEq(t, "name", got.Name, "renamed")
	mustEq(t, "cap", got.Cap, int32(7))
	mustEq(t, "filter", *got.FilterExpr, "payload.a == 1")
	mustEq(t, "enabled", got.Enabled, true)

	// Validation: nothing changes on refusal.
	wantErr(t, patch(map[string]any{"cap": 0}), http.StatusBadRequest, "cap must be between 1 and 1000")
	wantErr(t, patch(map[string]any{"cap": 1001}), http.StatusBadRequest, "cap must be between 1 and 1000")
	wantErr(t, patch(map[string]any{"name": "taken"}), http.StatusConflict, "capture rule name already in use")
	// A malformed body is rejected after the row is found.
	h := Handlers{Log: discardLog(), Queries: q}
	rec = direct(context.Background(), h.PatchCaptureRule, http.MethodPatch, &auth.Principal{UserID: uid, OrgID: oid}, id, "{nope")
	wantErr(t, rec, http.StatusBadRequest, "invalid json")
	wantErr(t, do(t, r, http.MethodPatch, "/api/capture-rules/not-a-uuid", uid, oid, map[string]any{}), http.StatusBadRequest, "invalid capture rule id")

	got = ruleRow(t, q, oid, pid)
	mustEq(t, "name after refusals", got.Name, "renamed")
	mustEq(t, "cap after refusals", got.Cap, int32(7))
	mustEq(t, "other untouched", ruleRow(t, q, oid, other.ID).Name, "taken")
	mustEq(t, "evictions after refusals", evicted, 3)

	// Clearing the filter with "" is allowed and stored.
	wantStatus(t, patch(map[string]any{"filter_expr": ""}), http.StatusOK)
	mustEq(t, "cleared filter", *ruleRow(t, q, oid, pid).FilterExpr, "")
	mustEq(t, "evictions after accepted patches", evicted, 4)
}

func TestCaptureRuleSwallowsCacheLookupFailure(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	src := newSource(t, q, oid)
	cr := seedRule(t, q, oid, src, "swallow")
	var evicted int
	h := Handlers{Log: discardLog(), Queries: failingQueries(t, "name: GetSourceForOrg"), EvictSourceCache: func(string) { evicted++ }}
	p := &auth.Principal{UserID: uid, OrgID: oid}

	rec := direct(context.Background(), h.PatchCaptureRule, http.MethodPatch, p, store.GoUUID(cr.ID).String(), `{"enabled":false}`)
	wantStatus(t, rec, http.StatusOK)
	mustEq(t, "enabled stored", ruleRow(t, q, oid, cr.ID).Enabled, false)
	rec = direct(context.Background(), h.DeleteCaptureRule, http.MethodDelete, p, store.GoUUID(cr.ID).String(), "")
	wantStatus(t, rec, http.StatusNoContent)
	mustEq(t, "rules left", rowCount(t, pool, `SELECT count(*) FROM capture_rules WHERE id=$1`, cr.ID), 0)
	mustEq(t, "evictions", evicted, 0)
}

func TestCaptureRuleDatabaseFailures(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	src := newSource(t, q, oid)
	cr := seedRule(t, q, oid, src, "dbfail")
	id := store.GoUUID(cr.ID).String()
	sid := store.GoUUID(src.ID).String()
	p := &auth.Principal{UserID: uid, OrgID: oid}
	var evicted int
	hf := func(marker string) Handlers {
		return Handlers{Log: discardLog(), Queries: failingQueries(t, marker), EvictSourceCache: func(string) { evicted++ }}
	}
	ctx := context.Background()

	h := hf("name: GetSourceForOrg")
	wantErr(t, direct(ctx, h.CreateCaptureRule, http.MethodPost, p, "", `{"name":"x","source_id":"`+sid+`"}`), http.StatusInternalServerError, "lookup source")
	h = hf("name: CreateCaptureRule")
	wantErr(t, direct(ctx, h.CreateCaptureRule, http.MethodPost, p, "", `{"name":"x","source_id":"`+sid+`"}`), http.StatusInternalServerError, "create capture rule")
	h = hf("name: ListCaptureRulesForOrg")
	wantErr(t, direct(ctx, h.ListCaptureRules, http.MethodGet, p, "", ""), http.StatusInternalServerError, "list capture rules")
	h = hf("name: GetCaptureRuleForOrg")
	wantErr(t, direct(ctx, h.GetCaptureRule, http.MethodGet, p, id, ""), http.StatusInternalServerError, "get capture rule")
	wantErr(t, direct(ctx, h.PatchCaptureRule, http.MethodPatch, p, id, `{"name":"x"}`), http.StatusInternalServerError, "lookup capture rule")
	wantErr(t, direct(ctx, h.DeleteCaptureRule, http.MethodDelete, p, id, ""), http.StatusInternalServerError, "lookup capture rule")
	h = hf("name: UpdateCaptureRule")
	wantErr(t, direct(ctx, h.PatchCaptureRule, http.MethodPatch, p, id, `{"name":"x"}`), http.StatusInternalServerError, "patch capture rule")
	h = hf("name: DeleteCaptureRuleForOrg")
	wantErr(t, direct(ctx, h.DeleteCaptureRule, http.MethodDelete, p, id, ""), http.StatusInternalServerError, "delete capture rule")

	// Every failure left the world as it was.
	got := ruleRow(t, q, oid, cr.ID)
	mustEq(t, "name", got.Name, "dbfail")
	mustEq(t, "rules", rowCount(t, pool, `SELECT count(*) FROM capture_rules WHERE org_id=$1`, oid), 1)
	mustEq(t, "evictions", evicted, 0)
}

func TestCaptureRuleListAndIdValidation(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, captureRuleRoutes)
	wantErr(t, do(t, r, http.MethodGet, "/api/capture-rules?source_id=nope", uid, oid, nil), http.StatusBadRequest, "invalid source_id")
	wantErr(t, do(t, r, http.MethodGet, "/api/capture-rules/nope", uid, oid, nil), http.StatusBadRequest, "invalid capture rule id")
	wantErr(t, do(t, r, http.MethodDelete, "/api/capture-rules/nope", uid, oid, nil), http.StatusBadRequest, "invalid capture rule id")
	wantErr(t, do(t, r, http.MethodGet, "/api/capture-rules/"+uuid.NewString(), uid, oid, nil), http.StatusNotFound, "capture rule not found")
	wantErr(t, do(t, r, http.MethodPatch, "/api/capture-rules/"+uuid.NewString(), uid, oid, map[string]any{}), http.StatusNotFound, "capture rule not found")
	wantErr(t, do(t, r, http.MethodDelete, "/api/capture-rules/"+uuid.NewString(), uid, oid, nil), http.StatusNotFound, "capture rule not found")
	if l := decodeList(t, do(t, r, http.MethodGet, "/api/capture-rules", uid, oid, nil)); len(l) != 0 {
		t.Fatalf("fresh org lists %v", l)
	}
}
