package pipeline

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
)

func bookmarkRoutes(r chi.Router, h Handlers) {
	r.Route("/bookmarks", func(r chi.Router) {
		r.Get("/", h.ListBookmarks)
		r.Post("/", h.CreateBookmark)
		r.Get("/{id}", h.GetBookmark)
		r.Delete("/{id}", h.DeleteBookmark)
	})
}

// seedRequest creates a source + a captured request in orgID; returns the
// request id (the thing a bookmark points at) and the source id.
func seedRequest(t *testing.T, q *store.Queries, orgID uuid.UUID) (requestID, sourceID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	src, err := q.CreateSource(ctx, store.CreateSourceParams{
		OrgID: store.UUID(orgID), Name: "src-" + uuid.NewString(), Type: "generic",
		IngestToken: "tok-" + uuid.NewString(), SigningConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	reqID := uuid.New()
	ct := "application/json"
	if _, err := q.CreateRequest(ctx, store.CreateRequestParams{
		ID: store.UUID(reqID), SourceID: src.ID, HTTPMethod: "POST", HTTPPath: "/e/x",
		Headers: []byte("{}"), BodyHash: "h-" + uuid.NewString(), BodyRef: "pg:" + reqID.String(),
		BodySize: 0, ContentType: &ct, SigVerified: false, IngestIP: nil,
	}); err != nil {
		t.Fatalf("create request: %v", err)
	}
	return reqID, store.GoUUID(src.ID)
}

func TestCreateBookmark(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	reqID, _ := seedRequest(t, q, oid)
	r := newRouter(q, bookmarkRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/bookmarks", uid, oid,
		map[string]any{"request_id": reqID.String(), "name": "stripe-paid", "tags": []string{"stripe"}}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["name"] != "stripe-paid" || got["request_id"] != reqID.String() {
		t.Fatalf("body: %v", got)
	}
}

func TestCreateBookmarkForeignRequest(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	// a request in a DIFFERENT org
	_, otherOrg := seedOrg(t, q)
	foreignReq, _ := seedRequest(t, q, otherOrg)
	r := newRouter(q, bookmarkRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/bookmarks", uid, oid,
		map[string]any{"request_id": foreignReq.String(), "name": "x"}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign request: got %d want 404 body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateBookmarkDuplicateName(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	reqID, _ := seedRequest(t, q, oid)
	r := newRouter(q, bookmarkRoutes)
	body := map[string]any{"request_id": reqID.String(), "name": "dup"}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/bookmarks", uid, oid, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first: got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/bookmarks", uid, oid, body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("dup name: got %d want 409", rec.Code)
	}
}

func TestListGetDeleteBookmark(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	reqID, srcID := seedRequest(t, q, oid)
	r := newRouter(q, bookmarkRoutes)

	// create one with a tag
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/bookmarks", uid, oid,
		map[string]any{"request_id": reqID.String(), "name": "b1", "tags": []string{"t1"}}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	// list (all)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/bookmarks", uid, oid, nil))
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0]["source_id"] != srcID.String() {
		t.Fatalf("list: %v", list)
	}
	// list filtered by tag=t1 (1) and tag=nope (0)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/bookmarks?tag=nope", uid, oid, nil))
	var none []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &none)
	if len(none) != 0 {
		t.Fatalf("tag filter should be empty, got %v", none)
	}

	// get
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/bookmarks/"+id, uid, oid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}

	// delete → 204, then get → 404
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodDelete, "/api/bookmarks/"+id, uid, oid, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/bookmarks/"+id, uid, oid, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d want 404", rec.Code)
	}
}

// newReplayRouter mounts /api with a Handlers carrying a real Queue (the base
// newRouter helper omits Queue). Skips the test if Redis is unreachable.
func newReplayRouter(t *testing.T, q *store.Queries) *chi.Mux {
	t.Helper()
	addr := os.Getenv("DSTREAM_REDIS_ADDR")
	if addr == "" {
		t.Skip("DSTREAM_REDIS_ADDR not set")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
	dq := dqueue.NewClient(rdb).WithPrefix("bmreplaytest-" + uuid.NewString())
	h := Handlers{Log: discardLog(), Queries: q, Queue: dq}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, testSigner))
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireOrg(q))
			bookmarkRoutes(r, h)
			r.Post("/bookmarks/{id}/replay", h.ReplayBookmark)
		})
	})
	return r
}

// seedEnabledConnection adds a destination + an enabled connection on sourceID.
func seedEnabledConnection(t *testing.T, q *store.Queries, orgID, sourceID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	url := "http://sink.example.test"
	dst, err := q.CreateDestination(ctx, store.CreateDestinationParams{
		OrgID: store.UUID(orgID), Name: "dst-" + uuid.NewString(), Type: "http",
		Url: &url, AuthConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}
	if _, err := q.CreateConnection(ctx, store.CreateConnectionParams{
		SourceID: store.UUID(sourceID), DestinationID: dst.ID, Enabled: true,
	}); err != nil {
		t.Fatalf("create connection: %v", err)
	}
}

func TestReplayBookmarkReinjects(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	reqID, srcID := seedRequest(t, q, oid)
	seedEnabledConnection(t, q, oid, srcID)
	seedEnabledConnection(t, q, oid, srcID) // two enabled connections
	// bookmark the request directly via the store
	bm, err := q.CreateBookmark(context.Background(), store.CreateBookmarkParams{
		OrgID: store.UUID(oid), RequestID: store.UUID(reqID), Name: "replayme", Tags: []string{},
	})
	if err != nil {
		t.Fatalf("create bookmark: %v", err)
	}
	r := newReplayRouter(t, q) // skips if no Redis

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost,
		"/api/bookmarks/"+store.GoUUID(bm.ID).String()+"/replay", uid, oid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("replay: got %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		EventIDs []string `json:"event_ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.EventIDs) != 2 {
		t.Fatalf("want 2 event ids, got %d: %v", len(got.EventIDs), got.EventIDs)
	}
	// confirm 2 is_test events exist for this request
	var n int
	err = testPool(t).QueryRow(context.Background(),
		"SELECT count(*) FROM events WHERE request_id=$1 AND is_test=true", store.UUID(reqID)).Scan(&n)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 is_test events, got %d", n)
	}

	// a bookmark id from another org → 404
	_, otherOrg := seedOrg(t, q)
	otherReq, _ := seedRequest(t, q, otherOrg)
	obm, _ := q.CreateBookmark(context.Background(), store.CreateBookmarkParams{
		OrgID: store.UUID(otherOrg), RequestID: store.UUID(otherReq), Name: "other", Tags: []string{},
	})
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost,
		"/api/bookmarks/"+store.GoUUID(obm.ID).String()+"/replay", uid, oid, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign bookmark replay: got %d want 404", rec.Code)
	}
}

// newReplayToRouter builds a router whose Handlers carry a BodyStore + a
// SafeHTTPClient. allowPrivate=true lets it reach the 127.0.0.1 httptest sink;
// false exercises the SSRF block.
func newReplayToRouter(q *store.Queries, allowPrivate bool) *chi.Mux {
	h := Handlers{
		Log: discardLog(), Queries: q,
		BodyStore: ingest.NewPostgresBodyStore(q),
		Replayer:  deliver.NewSafeHTTPClient(10*time.Second, allowPrivate),
	}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, testSigner))
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireOrg(q))
			r.Post("/bookmarks/{id}/replay-to", h.ReplayBookmarkTo)
			r.Get("/bookmarks/{id}/export", h.ExportBookmark)
		})
	})
	return r
}

// seedRequestWithBody = seedRequest + a stored body row, so replay-to/export
// can read the payload.
func seedRequestWithBody(t *testing.T, q *store.Queries, orgID uuid.UUID, body []byte) (requestID, sourceID uuid.UUID) {
	t.Helper()
	reqID, srcID := seedRequest(t, q, orgID)
	bs := ingest.NewPostgresBodyStore(q)
	if _, err := bs.Put(context.Background(), reqID, body); err != nil {
		t.Fatalf("put body: %v", err)
	}
	return reqID, srcID
}

func bmWithBody(t *testing.T, q *store.Queries, orgID uuid.UUID, body []byte) (bookmarkID uuid.UUID) {
	t.Helper()
	reqID, _ := seedRequestWithBody(t, q, orgID, body)
	bm, err := q.CreateBookmark(context.Background(), store.CreateBookmarkParams{
		OrgID: store.UUID(orgID), RequestID: store.UUID(reqID), Name: "bm-" + uuid.NewString(), Tags: []string{},
	})
	if err != nil {
		t.Fatalf("create bookmark: %v", err)
	}
	return store.GoUUID(bm.ID)
}

func TestReplayBookmarkToURL(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	bmID := bmWithBody(t, q, oid, []byte(`{"hello":"world"}`))

	var gotBody []byte
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(201)
		w.Write([]byte("received"))
	}))
	defer srv.Close()

	// allowPrivate=true so the 127.0.0.1 httptest sink is reachable
	r := newReplayToRouter(q, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/bookmarks/"+bmID.String()+"/replay-to", uid, oid,
		map[string]any{"url": srv.URL}))
	if rec.Code != http.StatusOK {
		t.Fatalf("replay-to: got %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Status       int    `json:"status"`
		ResponseBody string `json:"response_body"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Status != 201 || got.ResponseBody != "received" {
		t.Fatalf("resp: %+v", got)
	}
	if string(gotBody) != `{"hello":"world"}` || gotCT != "application/json" {
		t.Fatalf("target got body=%s ct=%s", gotBody, gotCT)
	}
}

func TestReplayBookmarkToBlockedBySSRF(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	bmID := bmWithBody(t, q, oid, []byte(`{}`))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()

	// allowPrivate=false → the 127.0.0.1 target is blocked at dial → 502
	r := newReplayToRouter(q, false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/bookmarks/"+bmID.String()+"/replay-to", uid, oid,
		map[string]any{"url": srv.URL}))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("ssrf-blocked replay-to: got %d want 502 body=%s", rec.Code, rec.Body.String())
	}
}

func TestReplayBookmarkToBadURL(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	bmID := bmWithBody(t, q, oid, []byte(`{}`))
	r := newReplayToRouter(q, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/bookmarks/"+bmID.String()+"/replay-to", uid, oid,
		map[string]any{"url": "ftp://nope"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad url: got %d want 400", rec.Code)
	}
}

func TestExportBookmark(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	bmID := bmWithBody(t, q, oid, []byte(`{"k":"v"}`))
	r := newReplayToRouter(q, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/bookmarks/"+bmID.String()+"/export", uid, oid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("export: got %d", rec.Code)
	}
	var got struct {
		Method string `json:"method"`
		Body   string `json:"body_base64"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("export not json: %v", err)
	}
	if got.Method != "POST" {
		t.Fatalf("export method: %s", got.Method)
	}
	dec, _ := base64.StdEncoding.DecodeString(got.Body)
	if string(dec) != `{"k":"v"}` {
		t.Fatalf("export body: %s", dec)
	}
}

func TestExportExpungedBody410(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	// seedRequest (NO body row) → BodyStore.Get → ErrNoRows → 410
	reqID, _ := seedRequest(t, q, oid)
	bm, _ := q.CreateBookmark(context.Background(), store.CreateBookmarkParams{
		OrgID: store.UUID(oid), RequestID: store.UUID(reqID), Name: "nobody-" + uuid.NewString(), Tags: []string{},
	})
	r := newReplayToRouter(q, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/bookmarks/"+store.GoUUID(bm.ID).String()+"/export", uid, oid, nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("expunged body export: got %d want 410", rec.Code)
	}
}

func TestExportBookmarkNameSanitizedInFilename(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	reqID, _ := seedRequestWithBody(t, q, oid, []byte(`{}`))
	bm, err := q.CreateBookmark(context.Background(), store.CreateBookmarkParams{
		OrgID: store.UUID(oid), RequestID: store.UUID(reqID), Name: `he"llo`, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("create bookmark: %v", err)
	}
	r := newReplayToRouter(q, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodGet, "/api/bookmarks/"+store.GoUUID(bm.ID).String()+"/export", uid, oid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("export: got %d body=%s", rec.Code, rec.Body.String())
	}
	got := rec.Header().Get("Content-Disposition")
	if got != `attachment; filename="he_llo.json"` {
		t.Fatalf("Content-Disposition not sanitized: %q", got)
	}
}
