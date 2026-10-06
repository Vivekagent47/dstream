package pipeline

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
)

func mkBookmark(t *testing.T, q *store.Queries, oid, reqID uuid.UUID, name string, tags ...string) store.Bookmark {
	t.Helper()
	if tags == nil {
		tags = []string{}
	}
	bm, err := q.CreateBookmark(context.Background(), store.CreateBookmarkParams{
		OrgID: store.UUID(oid), RequestID: store.UUID(reqID), Name: name, Description: "d-" + name, Tags: tags,
	})
	if err != nil {
		t.Fatalf("seed bookmark: %v", err)
	}
	return bm
}

func bmCount(t *testing.T, pool *pgxpool.Pool, oid uuid.UUID) int {
	t.Helper()
	return rowCount(t, pool, `SELECT count(*) FROM bookmarks WHERE org_id=$1`, oid)
}

func bmRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (name, desc string, tags []string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT name, description, tags FROM bookmarks WHERE id=$1`, id).Scan(&name, &desc, &tags); err != nil {
		t.Fatalf("read bookmark: %v", err)
	}
	return
}

func eventCount(t *testing.T, pool *pgxpool.Pool, reqID uuid.UUID) int {
	t.Helper()
	return rowCount(t, pool, `SELECT count(*) FROM events WHERE request_id=$1`, reqID)
}

func TestCreateBookmarkValidationAndStorage(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	uid, oid := seedOrg(t, q)
	_, oidB := seedOrg(t, q)
	reqID, _ := seedRequest(t, q, oid)
	foreign, _ := seedRequest(t, q, oidB)
	post := func(body any) *httptest.ResponseRecorder {
		return do(t, r, http.MethodPost, "/api/bookmarks", uid, oid, body)
	}

	wantErr(t, post(map[string]any{"request_id": reqID.String()}), 400, "name required")
	wantErr(t, post(map[string]any{"name": "n", "request_id": "nope"}), 400, "invalid request_id")
	wantErr(t, post(map[string]any{"name": "n", "request_id": uuid.NewString()}), 404, "request not found")
	wantErr(t, post(map[string]any{"name": "n", "request_id": foreign.String()}), 404, "request not found")
	req := sessionReq(t, http.MethodPost, "/api/bookmarks", uid, oid, nil)
	req.Body = http.NoBody
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	wantErr(t, rec, 400, "invalid json")
	mustEq(t, "nothing stored by the refusals", bmCount(t, pool, oid)+bmCount(t, pool, oidB), 0)

	m := decodeMap(t, post(map[string]any{"name": "keep", "request_id": reqID.String(), "description": "why"}))
	id := uuid.MustParse(m["id"].(string))
	mustEq(t, "tags default to []", len(m["tags"].([]any)), 0)
	name, desc, tags := bmRow(t, pool, id)
	mustEq(t, "stored name", name, "keep")
	mustEq(t, "stored description", desc, "why")
	mustEq(t, "stored tags", len(tags), 0)
	wantErr(t, post(map[string]any{"name": "keep", "request_id": reqID.String()}), 409, "bookmark name already in use")
	mustEq(t, "still one", bmCount(t, pool, oid), 1)
}

func TestListBookmarksFilters(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	uid, oid := seedOrg(t, q)
	uidB, oidB := seedOrg(t, q)
	req1, src1 := seedRequest(t, q, oid)
	req2, _ := seedRequestOn(t, q, store.Source{ID: store.UUID(src1)})
	req3, src3 := seedRequest(t, q, oid)
	b1 := mkBookmark(t, q, oid, req1, "l1", "x", "common")
	b2 := mkBookmark(t, q, oid, req2, "l2", "y", "common")
	b3 := mkBookmark(t, q, oid, req3, "l3", "x")
	reqB, _ := seedRequest(t, q, oidB)
	mkBookmark(t, q, oidB, reqB, "lB", "x")
	list := func(query string) []map[string]any {
		t.Helper()
		return decodeList(t, do(t, r, http.MethodGet, "/api/bookmarks"+query, uid, oid, nil))
	}
	ids := func(l []map[string]any, want ...store.Bookmark) {
		t.Helper()
		mustEq(t, "count", len(l), len(want))
		for _, w := range want {
			if !hasID(l, store.GoUUID(w.ID)) {
				t.Fatalf("missing %s in %v", store.GoUUID(w.ID), l)
			}
		}
	}
	ids(list(""), b1, b2, b3)
	ids(list("?source_id="+src1.String()), b1, b2)
	ids(list("?source_id="+src3.String()), b3)
	ids(list("?tag=x"), b1, b3)
	ids(list("?tag=common&source_id="+src1.String()), b1, b2)
	ids(list("?tag=x&source_id="+src1.String()), b1)
	ids(list("?source_id=" + uuid.NewString()))
	row := list("?tag=y")[0]
	mustEq(t, "http_method", row["http_method"], "POST")
	mustEq(t, "http_path", row["http_path"], "/e/x")
	mustEq(t, "source_id", row["source_id"], src1.String())
	mustEq(t, "request_id", row["request_id"], req2.String())
	wantErr(t, do(t, r, http.MethodGet, "/api/bookmarks?source_id=nope", uid, oid, nil), 400, "invalid source_id")
	otherOrg := decodeList(t, do(t, r, http.MethodGet, "/api/bookmarks", uidB, oidB, nil))
	mustEq(t, "org B sees only its own", len(otherOrg), 1)
}

func TestPatchBookmarkBehaviours(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	uid, oid := seedOrg(t, q)
	req1, _ := seedRequest(t, q, oid)
	req2, _ := seedRequest(t, q, oid)
	bm := mkBookmark(t, q, oid, req1, "pb", "a", "b")
	mkBookmark(t, q, oid, req2, "taken")
	id := store.GoUUID(bm.ID)
	patch := func(body any) *httptest.ResponseRecorder {
		return do(t, r, http.MethodPatch, "/api/bookmarks/"+id.String(), uid, oid, body)
	}

	wantStatus(t, patch(map[string]any{"description": "new"}), 200)
	name, desc, tags := bmRow(t, pool, id)
	mustEq(t, "name kept", name, "pb")
	mustEq(t, "description", desc, "new")
	mustEq(t, "tags kept", len(tags), 2)

	wantStatus(t, patch(map[string]any{"tags": []string{"z"}}), 200)
	_, _, tags = bmRow(t, pool, id)
	mustEq(t, "tags replaced", len(tags), 1)
	mustEq(t, "tag", tags[0], "z")

	wantStatus(t, patch(map[string]any{"tags": []string{}}), 200)
	_, _, tags = bmRow(t, pool, id)
	mustEq(t, "tags cleared", len(tags), 0)

	wantStatus(t, patch(map[string]any{"name": "  spaced  "}), 200)
	name, _, _ = bmRow(t, pool, id)
	mustEq(t, "name trimmed", name, "spaced")

	wantErr(t, patch(map[string]any{"name": " "}), 400, "name cannot be empty")
	wantErr(t, patch(map[string]any{"name": "taken"}), 409, "bookmark name already in use")
	name, _, _ = bmRow(t, pool, id)
	mustEq(t, "name after refusals", name, "spaced")

	req := sessionReq(t, http.MethodPatch, "/api/bookmarks/"+id.String(), uid, oid, nil)
	req.Body = http.NoBody
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	wantErr(t, rec, 400, "invalid json")
}

func TestBookmarkHandlersRefuseAnotherOrgsRow(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	_, oid := seedOrg(t, q)
	uidB, oidB := seedOrg(t, q)
	bmID := bmWithBody(t, q, oid, []byte(`secret`))
	var reqID uuid.UUID
	var srcID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT b.request_id, r.source_id FROM bookmarks b JOIN requests r ON r.id=b.request_id WHERE b.id=$1`, bmID).Scan(&reqID, &srcID); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	t.Cleanup(sink.Close)
	id := bmID.String()
	asB := func(method, path string, body any) *httptest.ResponseRecorder {
		return do(t, r, method, path, uidB, oidB, body)
	}

	wantErr(t, asB(http.MethodGet, "/api/bookmarks/"+id, nil), 404, "bookmark not found")
	wantErr(t, asB(http.MethodPatch, "/api/bookmarks/"+id, map[string]any{"name": "mine now", "description": "x", "tags": []string{"x"}}), 404, "bookmark not found")
	wantErr(t, asB(http.MethodDelete, "/api/bookmarks/"+id, nil), 404, "bookmark not found")
	wantErr(t, asB(http.MethodPost, "/api/bookmarks/"+id+"/replay", nil), 404, "bookmark not found")
	wantErr(t, asB(http.MethodPost, "/api/bookmarks/"+id+"/replay-to", map[string]any{"url": sink.URL}), 404, "bookmark not found")
	rec := asB(http.MethodGet, "/api/bookmarks/"+id+"/export", nil)
	wantErr(t, rec, 404, "bookmark not found")
	wantErr(t, asB(http.MethodPost, "/api/bookmarks", map[string]any{"name": "steal", "request_id": reqID.String()}), 404, "request not found")
	wantErr(t, asB(http.MethodPost, "/api/bookmarks/import", map[string]any{"name": "steal", "source_id": srcID.String(), "method": "POST"}), 404, "source not found")

	name, desc, tags := bmRow(t, pool, bmID)
	mustEq(t, "name", name[:3], "bm-")
	mustEq(t, "description", desc, "")
	mustEq(t, "tags", len(tags), 0)
	mustEq(t, "org A bookmarks", bmCount(t, pool, oid), 1)
	mustEq(t, "org B bookmarks", bmCount(t, pool, oidB), 0)
	mustEq(t, "requests on A's source", rowCount(t, pool, `SELECT count(*) FROM requests WHERE source_id=$1`, srcID), 1)
	mustEq(t, "events", eventCount(t, pool, reqID), 0)
	mustEq(t, "sink hits", hits.Load(), int32(0))
}

func TestBookmarkIDsAreValidated(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	uid, oid := seedOrg(t, q)
	for _, c := range []struct{ method, suffix string }{
		{http.MethodGet, ""}, {http.MethodPatch, ""}, {http.MethodDelete, ""},
		{http.MethodPost, "/replay"}, {http.MethodPost, "/replay-to"}, {http.MethodGet, "/export"},
	} {
		wantErr(t, do(t, r, c.method, "/api/bookmarks/nope"+c.suffix, uid, oid, map[string]any{"url": "http://sink.example.test"}), 400, "invalid bookmark id")
	}
	// unknown (well-formed) ids
	wantErr(t, do(t, r, http.MethodDelete, "/api/bookmarks/"+uuid.NewString(), uid, oid, nil), 404, "bookmark not found")
	wantErr(t, do(t, r, http.MethodPost, "/api/bookmarks/"+uuid.NewString()+"/replay", uid, oid, nil), 404, "bookmark not found")
	wantErr(t, do(t, r, http.MethodGet, "/api/bookmarks/"+uuid.NewString()+"/export", uid, oid, nil), 404, "bookmark not found")
	wantErr(t, do(t, r, http.MethodPost, "/api/bookmarks/"+uuid.NewString()+"/replay-to", uid, oid, map[string]any{"url": "http://sink.example.test"}), 404, "bookmark not found")
}

func TestBookmarkHandlersMidFlowDatabaseFailure(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	dropEvents(t, pool, oid)
	bmID := bmWithBody(t, q, oid, []byte(`body`))
	var reqID, srcID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT b.request_id, r.source_id FROM bookmarks b JOIN requests r ON r.id=b.request_id WHERE b.id=$1`, bmID).Scan(&reqID, &srcID); err != nil {
		t.Fatal(err)
	}
	seedEnabledConnection(t, q, oid, srcID)
	id := bmID.String()
	const (
		getBM   = "from bookmarks where id = $1 and org_id = $2"
		reqJoin = "join sources s on s.id = r.source_id"
	)
	route := func(marker string) http.Handler { return fullRouter(failingQueries(t, marker), pool, nil) }
	importBody := func(src uuid.UUID, name string) map[string]any {
		return map[string]any{"source_id": src.String(), "name": name, "method": "POST", "body_base64": "aGk="}
	}

	for _, c := range []struct {
		what, marker, method, path string
		body                       any
		msg                        string
	}{
		{"create: request lookup", reqJoin, "POST", "/api/bookmarks", map[string]any{"name": "f1", "request_id": reqID.String()}, "lookup request"},
		{"create: insert", "insert into bookmarks", "POST", "/api/bookmarks", map[string]any{"name": "f2", "request_id": reqID.String()}, "create bookmark"},
		{"patch: lookup", getBM, "PATCH", "/api/bookmarks/" + id, map[string]any{"name": "f3"}, "update bookmark"},
		{"patch: update", "update bookmarks set", "PATCH", "/api/bookmarks/" + id, map[string]any{"name": "f4"}, "update bookmark"},
		{"list", "from bookmarks b", "GET", "/api/bookmarks", nil, "list bookmarks"},
		{"get", getBM, "GET", "/api/bookmarks/" + id, nil, "get bookmark"},
		{"delete", "delete from bookmarks where id", "DELETE", "/api/bookmarks/" + id, nil, "delete bookmark"},
		{"replay: bookmark lookup", getBM, "POST", "/api/bookmarks/" + id + "/replay", nil, "replay"},
		{"replay: request lookup", reqJoin, "POST", "/api/bookmarks/" + id + "/replay", nil, "replay"},
		{"replay: connections", "source_id = $1 and enabled = true", "POST", "/api/bookmarks/" + id + "/replay", nil, "replay"},
		{"replay: events", "insert into events", "POST", "/api/bookmarks/" + id + "/replay", nil, "replay"},
		{"replay-to: lookup", getBM, "POST", "/api/bookmarks/" + id + "/replay-to", map[string]any{"url": "http://sink.example.test"}, "replay-to"},
		{"export: lookup", getBM, "GET", "/api/bookmarks/" + id + "/export", nil, "export"},
		{"import: source lookup", "from sources where id = $1 and org_id = $2", "POST", "/api/bookmarks/import", importBody(srcID, "f5"), "import"},
		{"import: request insert", "insert into requests", "POST", "/api/bookmarks/import", importBody(srcID, "f6"), "import"},
		// Documents CURRENT behaviour, not contract: the next two failures leave the
		// already-inserted request row behind (an orphan, with no body or no bookmark).
		{"import: body insert", "insert into request_bodies", "POST", "/api/bookmarks/import", importBody(srcID, "f7"), "import"},
		{"import: bookmark insert", "insert into bookmarks", "POST", "/api/bookmarks/import", importBody(srcID, "f8"), "import"},
	} {
		t.Run(c.what, func(t *testing.T) {
			before := rowCount(t, pool, `SELECT count(*) FROM requests WHERE source_id=$1`, srcID)
			wantErr(t, do(t, route(c.marker), c.method, c.path, uid, oid, c.body), http.StatusInternalServerError, c.msg)
			if c.what == "import: source lookup" || c.what == "import: request insert" {
				mustEq(t, "requests", rowCount(t, pool, `SELECT count(*) FROM requests WHERE source_id=$1`, srcID), before)
			}
		})
	}

	name, desc, _ := bmRow(t, pool, bmID)
	mustEq(t, "name untouched", name[:3], "bm-")
	mustEq(t, "description untouched", desc, "")
	mustEq(t, "bookmarks", bmCount(t, pool, oid), 1)
	mustEq(t, "events created by failed replays", eventCount(t, pool, reqID), 0)
}

func TestReplayBookmarkVersusReplayTo(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	dq := testQueue(t)
	r := fullRouter(q, pool, dq)
	uid, oid := seedOrg(t, q)
	_, oidB := seedOrg(t, q)
	dropEvents(t, pool, oid)
	var hits atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	t.Cleanup(sink.Close)
	replay := func(id uuid.UUID) *httptest.ResponseRecorder {
		return do(t, r, http.MethodPost, "/api/bookmarks/"+id.String()+"/replay", uid, oid, nil)
	}
	replayTo := func(id uuid.UUID) *httptest.ResponseRecorder {
		return do(t, r, http.MethodPost, "/api/bookmarks/"+id.String()+"/replay-to", uid, oid, map[string]any{"url": sink.URL})
	}

	t.Run("fans out to enabled connections only, as queued test events", func(t *testing.T) {
		reqID, srcID := seedRequestWithBody(t, q, oid, []byte(`{}`))
		src := store.Source{ID: store.UUID(srcID)}
		on := seedConn(t, q, src, seedDest(t, q, oid, "https://sink.example.test/on"))
		off := seedConn(t, q, src, seedDest(t, q, oid, "https://sink.example.test/off"))
		if _, err := pool.Exec(context.Background(), `UPDATE connections SET enabled=false WHERE id=$1`, off.ID); err != nil {
			t.Fatal(err)
		}
		bm := mkBookmark(t, q, oid, reqID, "fan")
		m := decodeMap(t, replay(store.GoUUID(bm.ID)))
		evs := m["event_ids"].([]any)
		mustEq(t, "event ids", len(evs), 1)
		evID := uuid.MustParse(evs[0].(string))
		var conn uuid.UUID
		var status string
		var isTest bool
		if err := pool.QueryRow(context.Background(), `SELECT connection_id, status, is_test FROM events WHERE id=$1`, evID).Scan(&conn, &status, &isTest); err != nil {
			t.Fatal(err)
		}
		mustEq(t, "connection", conn, store.GoUUID(on.ID))
		mustEq(t, "status", status, "queued")
		mustEq(t, "is_test", isTest, true)
		mustEq(t, "events for the request", eventCount(t, pool, reqID), 1)
		if att, ok := pending(t, dq, oid)[evID]; !ok || att != 0 {
			t.Fatalf("queue = %v, want %s at attempt 0", pending(t, dq, oid), evID)
		}
	})

	t.Run("no enabled connection means an empty, successful replay", func(t *testing.T) {
		reqID, _ := seedRequestWithBody(t, q, oid, []byte(`{}`))
		bm := mkBookmark(t, q, oid, reqID, "lonely")
		rec := replay(store.GoUUID(bm.ID))
		wantStatus(t, rec, 200)
		ids, ok := decodeMap(t, rec)["event_ids"].([]any)
		if !ok || len(ids) != 0 {
			t.Fatalf("event_ids = %v, want []", decodeMap(t, rec)["event_ids"])
		}
		mustEq(t, "events", eventCount(t, pool, reqID), 0)
	})

	t.Run("replay works without the stored body; replay-to needs it", func(t *testing.T) {
		reqID, srcID := seedRequest(t, q, oid) // no body row
		seedConn(t, q, store.Source{ID: store.UUID(srcID)}, seedDest(t, q, oid, "https://sink.example.test/nb"))
		bm := mkBookmark(t, q, oid, reqID, "expunged")
		bid := store.GoUUID(bm.ID)
		mustEq(t, "replay event ids", len(decodeMap(t, replay(bid))["event_ids"].([]any)), 1)
		wantErr(t, replayTo(bid), http.StatusGone, "captured payload no longer stored")
		mustEq(t, "sink hits", hits.Load(), int32(0))
	})

	t.Run("a request that left the org is gone for replay, replay-to and export", func(t *testing.T) {
		reqB, srcB := seedRequest(t, q, oidB)
		seedConn(t, q, store.Source{ID: store.UUID(srcB)}, seedDest(t, q, oidB, "https://sink.example.test/stray"))
		bm := mkBookmark(t, q, oid, reqB, "stray")
		bid := store.GoUUID(bm.ID)
		wantErr(t, replay(bid), 404, "captured request no longer exists")
		wantErr(t, replayTo(bid), 404, "captured request no longer exists")
		wantErr(t, do(t, r, http.MethodGet, "/api/bookmarks/"+bid.String()+"/export", uid, oid, nil), 404, "captured request no longer exists")
		mustEq(t, "events", eventCount(t, pool, reqB), 0)
		mustEq(t, "sink hits", hits.Load(), int32(0))
	})

	// Documents CURRENT behaviour, not contract: when the queue is down the replay
	// answers 200 with an empty event_ids list although events were created; they stay
	// 'queued' in Postgres for the reaper, and the caller is not told.
	t.Run("a queue that is down still leaves the events queued for the reaper", func(t *testing.T) {
		reqID, srcID := seedRequestWithBody(t, q, oid, []byte(`{}`))
		seedConn(t, q, store.Source{ID: store.UUID(srcID)}, seedDest(t, q, oid, "https://sink.example.test/down"))
		bm := mkBookmark(t, q, oid, reqID, "queue-down")
		dead := redis.NewClient(&redis.Options{Addr: redisAddr(t)})
		_ = dead.Close()
		rd := fullRouter(q, pool, dqueue.NewClient(dead).WithPrefix("cov8-dead"))
		rec := do(t, rd, http.MethodPost, "/api/bookmarks/"+store.GoUUID(bm.ID).String()+"/replay", uid, oid, nil)
		wantStatus(t, rec, 200)
		mustEq(t, "event ids", len(decodeMap(t, rec)["event_ids"].([]any)), 0)
		mustEq(t, "events kept", eventCount(t, pool, reqID), 1)
		mustEq(t, "status", rowCount(t, pool, `SELECT count(*) FROM events WHERE request_id=$1 AND status='queued' AND is_test`, reqID), 1)
	})
}

func TestReplayBookmarkToPassesTargetAnswerThrough(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	uid, oid := seedOrg(t, q)
	bmID := bmWithBody(t, q, oid, []byte(`ping`))
	gotCh := make(chan string, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotCh <- string(b)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	t.Cleanup(sink.Close)
	rec := do(t, r, http.MethodPost, "/api/bookmarks/"+bmID.String()+"/replay-to", uid, oid, map[string]any{"url": sink.URL})
	wantStatus(t, rec, 200) // the target's 500 is data, not this API's failure
	m := decodeMap(t, rec)
	mustEq(t, "status", m["status"], float64(500))
	mustEq(t, "response_body", m["response_body"], "boom")
	select {
	case got := <-gotCh:
		mustEq(t, "target got", got, "ping")
	case <-time.After(5 * time.Second):
		t.Fatal("sink never received the replayed body")
	}

	req := sessionReq(t, http.MethodPost, "/api/bookmarks/"+bmID.String()+"/replay-to", uid, oid, nil)
	req.Body = http.NoBody
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	wantErr(t, rr, 400, "invalid json")
	rec = do(t, r, http.MethodPost, "/api/bookmarks/"+bmID.String()+"/replay-to", uid, oid, map[string]any{"url": "ftp://x"})
	wantStatus(t, rec, 400)
	if msg, _ := decodeMap(t, rec)["error"].(string); len(msg) < 13 || msg[:13] != "invalid url: " {
		t.Fatalf("message = %q", msg)
	}
}

func TestExportBookmarkReadsHeadersAndContentType(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	uid, oid := seedOrg(t, q)
	src := newSource(t, q, oid)
	mk := func(name string, headers string) uuid.UUID {
		reqID := uuid.New()
		if _, err := q.CreateRequest(context.Background(), store.CreateRequestParams{
			ID: store.UUID(reqID), SourceID: src.ID, HTTPMethod: "PUT", HTTPPath: "/e/y",
			Headers: []byte(headers), BodyHash: "h-" + uuid.NewString(), BodyRef: "pg:" + reqID.String(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := ingestStore(q).Put(context.Background(), reqID, []byte("raw")); err != nil {
			t.Fatal(err)
		}
		return store.GoUUID(mkBookmark(t, q, oid, reqID, name).ID)
	}
	export := func(id uuid.UUID) (*httptest.ResponseRecorder, struct {
		Method      string              `json:"method"`
		Path        string              `json:"path"`
		Headers     map[string][]string `json:"headers"`
		ContentType string              `json:"content_type"`
		Body        string              `json:"body_base64"`
	}) {
		rec := do(t, r, http.MethodGet, "/api/bookmarks/"+id.String()+"/export", uid, oid, nil)
		wantStatus(t, rec, 200)
		var out struct {
			Method      string              `json:"method"`
			Path        string              `json:"path"`
			Headers     map[string][]string `json:"headers"`
			ContentType string              `json:"content_type"`
			Body        string              `json:"body_base64"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return rec, out
	}

	rec, out := export(mk("..", `{"X-Trace":["a","b"]}`))
	mustEq(t, "disposition", rec.Header().Get("Content-Disposition"), `attachment; filename="fixture.json"`)
	mustEq(t, "content-type", rec.Header().Get("Content-Type"), "application/json")
	mustEq(t, "method", out.Method, "PUT")
	mustEq(t, "path", out.Path, "/e/y")
	mustEq(t, "no content type recorded", out.ContentType, "")
	mustEq(t, "trace header", len(out.Headers["X-Trace"]), 2)
	mustEq(t, "trace[1]", out.Headers["X-Trace"][1], "b")
	mustEq(t, "body", out.Body, base64.StdEncoding.EncodeToString([]byte("raw")))

	// headers that are not a header map are dropped, not fatal
	_, out = export(mk("odd-headers", `[1,2]`))
	mustEq(t, "headers", len(out.Headers), 0)
	mustEq(t, "body still exported", out.Body, base64.StdEncoding.EncodeToString([]byte("raw")))
}

func TestImportBookmarkStoresTheFixtureAndReplaysIt(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	uid, oid := seedOrg(t, q)
	src := newSource(t, q, oid)
	srcID := store.GoUUID(src.ID)
	raw := []byte(`{"imported":true}`)

	m := decodeMap(t, do(t, r, http.MethodPost, "/api/bookmarks/import", uid, oid, map[string]any{
		"source_id": srcID.String(), "name": "imp", "description": "from file", "tags": []string{"fx"},
		"method": "PATCH", "path": "/hook", "headers": map[string][]string{"X-A": {"1"}},
		"content_type": "text/x-test", "body_base64": base64.StdEncoding.EncodeToString(raw),
	}))
	mustEq(t, "name", m["name"], "imp")
	var method, path, ct, hdrs string
	var size int
	var sig bool
	reqID := uuid.MustParse(m["request_id"].(string))
	if err := pool.QueryRow(context.Background(),
		`SELECT http_method, http_path, content_type, headers::text, body_size, sig_verified FROM requests WHERE id=$1 AND source_id=$2`,
		reqID, src.ID).Scan(&method, &path, &ct, &hdrs, &size, &sig); err != nil {
		t.Fatalf("imported request row: %v", err)
	}
	mustEq(t, "method", method, "PATCH")
	mustEq(t, "path", path, "/hook")
	mustEq(t, "content type", ct, "text/x-test")
	mustEq(t, "size", size, len(raw))
	mustEq(t, "sig_verified", sig, false)
	var h map[string][]string
	_ = json.Unmarshal([]byte(hdrs), &h)
	mustEq(t, "header", h["X-A"][0], "1")
	stored, err := ingestStore(q).Get(context.Background(), "pg:"+reqID.String())
	if err != nil || string(stored) != string(raw) {
		t.Fatalf("stored body = %q, %v", stored, err)
	}
	_, _, tags := bmRow(t, pool, uuid.MustParse(m["id"].(string)))
	mustEq(t, "tags", tags[0], "fx")

	// the fixture replays to a target with its body and content type
	type seen struct{ body, ct string }
	seenCh := make(chan seen, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenCh <- seen{string(b), r.Header.Get("Content-Type")}
	}))
	t.Cleanup(sink.Close)
	wantStatus(t, do(t, r, http.MethodPost, "/api/bookmarks/"+m["id"].(string)+"/replay-to", uid, oid, map[string]any{"url": sink.URL}), 200)
	var got seen
	select {
	case got = <-seenCh:
	case <-time.After(5 * time.Second):
		t.Fatal("sink never received the imported fixture replay")
	}
	mustEq(t, "replayed body", got.body, string(raw))
	mustEq(t, "replayed content type", got.ct, "text/x-test")

	// minimal fixture: no headers, no content type, no tags
	m2 := decodeMap(t, do(t, r, http.MethodPost, "/api/bookmarks/import", uid, oid, map[string]any{
		"source_id": srcID.String(), "name": "imp-min", "method": "GET",
	}))
	if err := pool.QueryRow(context.Background(), `SELECT content_type IS NULL, headers::text, body_size FROM requests WHERE id=$1`,
		uuid.MustParse(m2["request_id"].(string))).Scan(&sig, &hdrs, &size); err != nil {
		t.Fatal(err)
	}
	mustEq(t, "content type null", sig, true)
	mustEq(t, "headers", hdrs, "{}")
	mustEq(t, "size", size, 0)
	mustEq(t, "tags", len(m2["tags"].([]any)), 0)
}

func TestImportBookmarkRejections(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	r := fullRouter(q, pool, nil)
	uid, oid := seedOrg(t, q)
	src := newSource(t, q, oid)
	s := store.GoUUID(src.ID).String()
	big := base64.StdEncoding.EncodeToString(make([]byte, maxImportBody+1))
	post := func(body any) *httptest.ResponseRecorder {
		return do(t, r, http.MethodPost, "/api/bookmarks/import", uid, oid, body)
	}

	wantErr(t, post(map[string]any{"source_id": "nope", "name": "n", "method": "POST"}), 400, "invalid source_id")
	wantErr(t, post(map[string]any{"source_id": uuid.NewString(), "name": "n", "method": "POST"}), 404, "source not found")
	wantErr(t, post(map[string]any{"source_id": s, "name": "n", "method": "POST", "body_base64": big}), 400, "body exceeds 5MiB")
	req := sessionReq(t, http.MethodPost, "/api/bookmarks/import", uid, oid, nil)
	req.Body = http.NoBody
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	wantErr(t, rec, 400, "invalid or oversized json")
	mustEq(t, "bookmarks", bmCount(t, pool, oid), 0)
	mustEq(t, "requests", rowCount(t, pool, `SELECT count(*) FROM requests WHERE source_id=$1`, src.ID), 0)
}

func ingestStore(q *store.Queries) ingest.BodyStore { return ingest.NewPostgresBodyStore(q) }
