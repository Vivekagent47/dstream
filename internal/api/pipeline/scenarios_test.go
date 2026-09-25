package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
)

// newScenarioRouter mounts /api/scenarios with a Handlers carrying a Pool
// (needed for the transactional step-replace), mirroring newRouter.
func newScenarioRouter(t *testing.T, pool *pgxpool.Pool) (*chi.Mux, *store.Queries) {
	t.Helper()
	q := store.New(pool)
	h := Handlers{Log: discardLog(), Queries: q, Pool: pool}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, testSigner))
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireOrg(q))
			r.Route("/scenarios", func(r chi.Router) {
				r.Get("/", h.ListScenarios)
				r.Post("/", h.CreateScenario)
				r.Get("/{id}", h.GetScenario)
				r.Patch("/{id}", h.PatchScenario)
				r.Delete("/{id}", h.DeleteScenario)
			})
		})
	})
	return r, q
}

// newScenarioReplayRouter mounts scenario CRUD (create, needed to seed a
// scenario via the API) + replay-to, with a Handlers carrying Pool +
// BodyStore + Replayer — mirrors newReplayToRouter (bookmarks_test.go).
func newScenarioReplayRouter(t *testing.T, pool *pgxpool.Pool, allowPrivate bool) (*chi.Mux, *store.Queries) {
	t.Helper()
	q := store.New(pool)
	h := Handlers{
		Log: discardLog(), Queries: q, Pool: pool,
		BodyStore: ingest.NewPostgresBodyStore(q),
		Replayer:  deliver.NewSafeHTTPClient(10*time.Second, allowPrivate),
	}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, testSigner))
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireOrg(q))
			r.Route("/scenarios", func(r chi.Router) {
				r.Post("/", h.CreateScenario)
				r.Post("/{id}/replay-to", h.ReplayScenarioTo)
			})
		})
	})
	return r, q
}

func TestReplayScenarioTo(t *testing.T) {
	pool := testPool(t)
	r, q := newScenarioReplayRouter(t, pool, true)
	uid, oid := seedOrg(t, q)
	b1 := bmWithBody(t, q, oid, []byte(`{"step":1}`))
	b2 := bmWithBody(t, q, oid, []byte(`{"step":2}`))

	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits = append(hits, string(body))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios", uid, oid, map[string]any{
		"name": "replay-flow",
		"steps": []map[string]any{
			{"bookmark_id": b1.String(), "delay_ms": 0},
			{"bookmark_id": b2.String(), "delay_ms": 0},
		},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios/"+id+"/replay-to", uid, oid, map[string]any{"url": srv.URL}))
	if rec.Code != http.StatusOK {
		t.Fatalf("replay-to: got %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Results []struct {
			Position int    `json:"position"`
			Status   int    `json:"status"`
			Error    string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Results) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(got.Results), got.Results)
	}
	for i, res := range got.Results {
		if res.Status != http.StatusOK || res.Error != "" {
			t.Fatalf("result[%d]: %+v", i, res)
		}
	}
	if len(hits) != 2 || hits[0] != `{"step":1}` || hits[1] != `{"step":2}` {
		t.Fatalf("sink hits (order/content): %+v", hits)
	}
}

func TestReplayScenarioToSSRFBlocked(t *testing.T) {
	pool := testPool(t)
	r, q := newScenarioReplayRouter(t, pool, false)
	uid, oid := seedOrg(t, q)
	b1 := bmWithBody(t, q, oid, []byte(`{}`))
	b2 := bmWithBody(t, q, oid, []byte(`{}`))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios", uid, oid, map[string]any{
		"name": "replay-flow-ssrf",
		"steps": []map[string]any{
			{"bookmark_id": b1.String(), "delay_ms": 0},
			{"bookmark_id": b2.String(), "delay_ms": 0},
		},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios/"+id+"/replay-to", uid, oid, map[string]any{"url": srv.URL}))
	if rec.Code != http.StatusOK {
		t.Fatalf("replay-to: got %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Results []struct {
			Position int    `json:"position"`
			Error    string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Blocked at the first step → loop stops; only one result, carrying an error.
	if len(got.Results) != 1 || got.Results[0].Error == "" {
		t.Fatalf("want 1 result with error, got: %+v", got.Results)
	}
}

func TestReplayScenarioToExpungedBodyStops(t *testing.T) {
	pool := testPool(t)
	r, q := newScenarioReplayRouter(t, pool, true)
	uid, oid := seedOrg(t, q)
	// seedRequest (no body row) + bookmark it → step load hits the 410 path.
	reqID, _ := seedRequest(t, q, oid)
	bm, err := q.CreateBookmark(context.Background(), store.CreateBookmarkParams{
		OrgID: store.UUID(oid), RequestID: store.UUID(reqID), Name: "nobody-" + uuid.NewString(), Tags: []string{},
	})
	if err != nil {
		t.Fatalf("create bookmark: %v", err)
	}
	b2 := bmWithBody(t, q, oid, []byte(`{}`))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios", uid, oid, map[string]any{
		"name": "replay-flow-expunged",
		"steps": []map[string]any{
			{"bookmark_id": store.GoUUID(bm.ID).String(), "delay_ms": 0},
			{"bookmark_id": b2.String(), "delay_ms": 0},
		},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios/"+id+"/replay-to", uid, oid, map[string]any{"url": srv.URL}))
	if rec.Code != http.StatusOK {
		t.Fatalf("replay-to: got %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Results []struct {
			Position int    `json:"position"`
			Status   int    `json:"status"`
			Error    string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Results) != 1 || got.Results[0].Status != http.StatusGone || got.Results[0].Error == "" {
		t.Fatalf("want 1 result with 410, got: %+v", got.Results)
	}
}

func TestCreateScenarioOrderedSteps(t *testing.T) {
	pool := testPool(t)
	r, q := newScenarioRouter(t, pool)
	uid, oid := seedOrg(t, q)
	b1 := bmWithBody(t, q, oid, []byte(`{}`))
	b2 := bmWithBody(t, q, oid, []byte(`{}`))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios", uid, oid, map[string]any{
		"name": "flow-1",
		"steps": []map[string]any{
			{"bookmark_id": b1.String(), "delay_ms": 0},
			{"bookmark_id": b2.String(), "delay_ms": 500},
		},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/scenarios/"+id, uid, oid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	var got struct {
		Steps []struct {
			Position   float64 `json:"position"`
			BookmarkID string  `json:"bookmark_id"`
			DelayMs    float64 `json:"delay_ms"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d: %+v", len(got.Steps), got.Steps)
	}
	if got.Steps[0].Position != 0 || got.Steps[0].BookmarkID != b1.String() {
		t.Fatalf("step0: %+v", got.Steps[0])
	}
	if got.Steps[1].Position != 1 || got.Steps[1].BookmarkID != b2.String() || got.Steps[1].DelayMs != 500 {
		t.Fatalf("step1: %+v", got.Steps[1])
	}
}

func TestCreateScenarioForeignBookmark404(t *testing.T) {
	pool := testPool(t)
	r, q := newScenarioRouter(t, pool)
	uid, oid := seedOrg(t, q)
	_, otherOrg := seedOrg(t, q)
	foreign := bmWithBody(t, q, otherOrg, []byte(`{}`))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios", uid, oid, map[string]any{
		"name":  "flow-foreign",
		"steps": []map[string]any{{"bookmark_id": foreign.String(), "delay_ms": 0}},
	}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign bookmark: got %d want 404 body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateScenarioDelayOutOfRange400(t *testing.T) {
	pool := testPool(t)
	r, q := newScenarioRouter(t, pool)
	uid, oid := seedOrg(t, q)
	b1 := bmWithBody(t, q, oid, []byte(`{}`))

	for _, delay := range []int{60001, -1} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios", uid, oid, map[string]any{
			"name":  "flow-delay-" + uuid.NewString(),
			"steps": []map[string]any{{"bookmark_id": b1.String(), "delay_ms": delay}},
		}))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("delay_ms=%d: got %d want 400 body=%s", delay, rec.Code, rec.Body.String())
		}
	}
}

func TestCreateScenarioDuplicateName409(t *testing.T) {
	pool := testPool(t)
	r, q := newScenarioRouter(t, pool)
	uid, oid := seedOrg(t, q)
	body := map[string]any{"name": "dup-scenario"}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios", uid, oid, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first: got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios", uid, oid, body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("dup name: got %d want 409", rec.Code)
	}
}

func TestPatchScenarioReplacesSteps(t *testing.T) {
	pool := testPool(t)
	r, q := newScenarioRouter(t, pool)
	uid, oid := seedOrg(t, q)
	b1 := bmWithBody(t, q, oid, []byte(`{}`))
	b2 := bmWithBody(t, q, oid, []byte(`{}`))
	b3 := bmWithBody(t, q, oid, []byte(`{}`))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios", uid, oid, map[string]any{
		"name": "flow-patch",
		"steps": []map[string]any{
			{"bookmark_id": b1.String(), "delay_ms": 0},
			{"bookmark_id": b2.String(), "delay_ms": 0},
		},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d", rec.Code)
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPatch, "/api/scenarios/"+id, uid, oid, map[string]any{
		"steps": []map[string]any{{"bookmark_id": b3.String(), "delay_ms": 250}},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/scenarios/"+id, uid, oid, nil))
	var got struct {
		Steps []struct {
			Position   float64 `json:"position"`
			BookmarkID string  `json:"bookmark_id"`
		} `json:"steps"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Steps) != 1 || got.Steps[0].Position != 0 || got.Steps[0].BookmarkID != b3.String() {
		t.Fatalf("patched steps: %+v", got.Steps)
	}
}

func TestPatchScenarioNameOnlyPreservesSteps(t *testing.T) {
	pool := testPool(t)
	r, q := newScenarioRouter(t, pool)
	uid, oid := seedOrg(t, q)
	b1 := bmWithBody(t, q, oid, []byte(`{}`))
	b2 := bmWithBody(t, q, oid, []byte(`{}`))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios", uid, oid, map[string]any{
		"name": "flow-nameonly",
		"steps": []map[string]any{
			{"bookmark_id": b1.String(), "delay_ms": 0},
			{"bookmark_id": b2.String(), "delay_ms": 500},
		},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPatch, "/api/scenarios/"+id, uid, oid, map[string]any{"name": "renamed"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/scenarios/"+id, uid, oid, nil))
	var got struct {
		Name  string `json:"name"`
		Steps []struct {
			Position   float64 `json:"position"`
			BookmarkID string  `json:"bookmark_id"`
		} `json:"steps"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Name != "renamed" {
		t.Fatalf("name: got %q want renamed", got.Name)
	}
	if len(got.Steps) != 2 {
		t.Fatalf("want 2 steps preserved, got %d: %+v", len(got.Steps), got.Steps)
	}
	if got.Steps[0].Position != 0 || got.Steps[0].BookmarkID != b1.String() {
		t.Fatalf("step0: %+v", got.Steps[0])
	}
	if got.Steps[1].Position != 1 || got.Steps[1].BookmarkID != b2.String() {
		t.Fatalf("step1: %+v", got.Steps[1])
	}
}

func TestDeleteScenario(t *testing.T) {
	pool := testPool(t)
	r, q := newScenarioRouter(t, pool)
	uid, oid := seedOrg(t, q)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios", uid, oid, map[string]any{"name": "to-delete"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d", rec.Code)
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodDelete, "/api/scenarios/"+id, uid, oid, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/scenarios/"+id, uid, oid, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d want 404", rec.Code)
	}
}

// TestDeleteBookmarkCascadesScenarioStep confirms the FK cascade: deleting a
// bookmark that a scenario step points at removes that step (not the whole
// scenario), since scenario_steps.bookmark_id has ON DELETE CASCADE.
func TestDeleteBookmarkCascadesScenarioStep(t *testing.T) {
	pool := testPool(t)
	r, q := newScenarioRouter(t, pool)
	uid, oid := seedOrg(t, q)
	b1 := bmWithBody(t, q, oid, []byte(`{}`))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/scenarios", uid, oid, map[string]any{
		"name":  "flow-cascade",
		"steps": []map[string]any{{"bookmark_id": b1.String(), "delay_ms": 0}},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d", rec.Code)
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	if _, err := q.DeleteBookmarkForOrg(context.Background(), store.DeleteBookmarkForOrgParams{
		ID: store.UUID(b1), OrgID: store.UUID(oid),
	}); err != nil {
		t.Fatalf("delete bookmark: %v", err)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/scenarios/"+id, uid, oid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get after bookmark delete: %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Steps []any `json:"steps"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Steps) != 0 {
		t.Fatalf("want 0 steps after cascade, got %d: %+v", len(got.Steps), got.Steps)
	}
}
