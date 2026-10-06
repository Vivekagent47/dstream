package pipeline

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
)

func sourceRoutes(r chi.Router, h Handlers) {
	r.Route("/sources", func(r chi.Router) {
		r.Get("/", h.ListSources)
		r.Post("/", h.CreateSource)
		r.Get("/{id}", h.GetSource)
		r.Patch("/{id}", h.PatchSource)
		r.Delete("/{id}", h.DeleteSource)
	})
}

func TestGenerateIngestToken(t *testing.T) {
	shape := regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`) // 24 random bytes, raw base64url
	seen := map[string]bool{}
	for range 5 {
		tok, err := generateIngestToken()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !shape.MatchString(tok) {
			t.Fatalf("token %q does not match %s", tok, shape)
		}
		if seen[tok] {
			t.Fatalf("token %q repeated", tok)
		}
		seen[tok] = true
	}
}

func TestSourceCRUD(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	var evicted []string
	r := newRouter(q, func(r chi.Router, h Handlers) {
		h.PublicBaseURL = "https://api.example.test/" // trailing slash must be trimmed
		h.EvictSourceCache = func(tok string) { evicted = append(evicted, tok) }
		sourceRoutes(r, h)
	})
	ctx := context.Background()

	// An org with no sources lists as [] (not null).
	if l := decodeList(t, do(t, r, http.MethodGet, "/api/sources", uid, oid, nil)); len(l) != 0 {
		t.Fatalf("fresh org lists %d sources", len(l))
	}

	// Create with defaults: type falls back to generic, signing_config to {}.
	rec := do(t, r, http.MethodPost, "/api/sources", uid, oid, map[string]any{"name": "stripe", "description": "d"})
	wantStatus(t, rec, http.StatusCreated)
	created := decodeMap(t, rec)
	id := uuid.MustParse(created["id"].(string))
	row, err := q.GetSourceForOrg(ctx, store.GetSourceForOrgParams{ID: store.UUID(id), OrgID: store.UUID(oid)})
	if err != nil {
		t.Fatalf("stored source: %v", err)
	}
	mustEq(t, "stored name", row.Name, "stripe")
	mustEq(t, "stored type", row.Type, "generic")
	mustEq(t, "stored description", row.Description, "d")
	mustEq(t, "stored enabled", row.Enabled, true)
	mustEq(t, "stored signing_config", string(row.SigningConfig), "{}")
	mustEq(t, "body type", created["type"], "generic")
	mustEq(t, "body ingest_token", created["ingest_token"], row.IngestToken)
	mustEq(t, "body ingest_url", created["ingest_url"], "https://api.example.test/e/"+row.IngestToken)
	mustEq(t, "body org_id", created["org_id"], oid.String())
	if n := auditCount(t, pool, oid, "source.create", id); n != 1 {
		t.Fatalf("source.create audit rows = %d, want 1", n)
	}

	// Create with explicit type + signing_config is stored verbatim.
	rec = do(t, r, http.MethodPost, "/api/sources", uid, oid, map[string]any{
		"name": "gh", "type": "github", "signing_config": map[string]any{"header": "x-sig"},
	})
	wantStatus(t, rec, http.StatusCreated)
	gid := uuid.MustParse(decodeMap(t, rec)["id"].(string))
	grow, _ := q.GetSourceForOrg(ctx, store.GetSourceForOrgParams{ID: store.UUID(gid), OrgID: store.UUID(oid)})
	mustEq(t, "github type", grow.Type, "github")
	mustEq(t, "github signing", string(grow.SigningConfig), `{"header": "x-sig"}`)

	// List shows both, Get returns the same view.
	l := decodeList(t, do(t, r, http.MethodGet, "/api/sources", uid, oid, nil))
	if !hasID(l, id) || !hasID(l, gid) {
		t.Fatalf("list missing created sources: %v", l)
	}
	got := decodeMap(t, do(t, r, http.MethodGet, "/api/sources/"+id.String(), uid, oid, nil))
	mustEq(t, "get id", got["id"], id.String())
	mustEq(t, "get ingest_url", got["ingest_url"], created["ingest_url"])

	// Patch every field; methods are normalised to upper case.
	rec = do(t, r, http.MethodPatch, "/api/sources/"+id.String(), uid, oid, map[string]any{
		"name": "stripe2", "description": "d2", "allowed_methods": []string{"post", " put "}, "enabled": false,
	})
	wantStatus(t, rec, http.StatusOK)
	body := decodeMap(t, rec)
	mustEq(t, "patched name", body["name"], "stripe2")
	mustEq(t, "patched enabled", body["enabled"], false)
	row, _ = q.GetSourceForOrg(ctx, store.GetSourceForOrgParams{ID: store.UUID(id), OrgID: store.UUID(oid)})
	mustEq(t, "stored name", row.Name, "stripe2")
	mustEq(t, "stored description", row.Description, "d2")
	mustEq(t, "stored enabled", row.Enabled, false)
	if !reflect.DeepEqual(row.AllowedMethods, []string{"POST", "PUT"}) {
		t.Fatalf("stored methods = %v", row.AllowedMethods)
	}
	if !reflect.DeepEqual(evicted, []string{row.IngestToken}) {
		t.Fatalf("evicted = %v, want the patched source's token", evicted)
	}
	if n := auditCount(t, pool, oid, "source.update", id); n != 1 {
		t.Fatalf("source.update audit rows = %d, want 1", n)
	}

	// A partial patch leaves untouched fields alone.
	wantStatus(t, do(t, r, http.MethodPatch, "/api/sources/"+id.String(), uid, oid, map[string]any{"enabled": true}), http.StatusOK)
	row, _ = q.GetSourceForOrg(ctx, store.GetSourceForOrgParams{ID: store.UUID(id), OrgID: store.UUID(oid)})
	mustEq(t, "enabled", row.Enabled, true)
	mustEq(t, "name kept", row.Name, "stripe2")
	if !reflect.DeepEqual(row.AllowedMethods, []string{"POST", "PUT"}) {
		t.Fatalf("methods changed by partial patch: %v", row.AllowedMethods)
	}

	// Delete: 204, row gone, cache evicted for its token, then 404.
	evicted = nil
	wantStatus(t, do(t, r, http.MethodDelete, "/api/sources/"+id.String(), uid, oid, nil), http.StatusNoContent)
	if _, err := q.GetSourceForOrg(ctx, store.GetSourceForOrgParams{ID: store.UUID(id), OrgID: store.UUID(oid)}); err == nil {
		t.Fatal("source still stored after delete")
	}
	if !reflect.DeepEqual(evicted, []string{row.IngestToken}) {
		t.Fatalf("delete evicted = %v", evicted)
	}
	if n := auditCount(t, pool, oid, "source.delete", id); n != 1 {
		t.Fatalf("source.delete audit rows = %d, want 1", n)
	}
	wantErr(t, do(t, r, http.MethodGet, "/api/sources/"+id.String(), uid, oid, nil), http.StatusNotFound, "not found")
	wantErr(t, do(t, r, http.MethodDelete, "/api/sources/"+id.String(), uid, oid, nil), http.StatusNotFound, "not found")
	wantErr(t, do(t, r, http.MethodPatch, "/api/sources/"+id.String(), uid, oid, map[string]any{"enabled": true}), http.StatusNotFound, "not found")
	// The other source is unaffected.
	if _, err := q.GetSourceForOrg(ctx, store.GetSourceForOrgParams{ID: store.UUID(gid), OrgID: store.UUID(oid)}); err != nil {
		t.Fatalf("sibling source lost: %v", err)
	}
}

func TestSourceValidation(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	r := newRouter(q, sourceRoutes)
	src := newSource(t, q, oid)
	other := newSource(t, q, oid)
	sid := store.GoUUID(src.ID).String()

	rawBody := func(method, path, body string) { // malformed JSON can't go through sessionReq
		req := sessionReq(t, method, path, uid, oid, map[string]any{})
		req.Body = io.NopCloser(strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		wantErr(t, rec, http.StatusBadRequest, "invalid json")
	}
	rawBody(http.MethodPost, "/api/sources", "{nope")
	rawBody(http.MethodPatch, "/api/sources/"+sid, "{nope")

	wantErr(t, do(t, r, http.MethodPost, "/api/sources", uid, oid, map[string]any{"type": "x"}), http.StatusBadRequest, "name required")
	wantErr(t, do(t, r, http.MethodGet, "/api/sources/not-a-uuid", uid, oid, nil), http.StatusBadRequest, "invalid id")
	wantErr(t, do(t, r, http.MethodPatch, "/api/sources/not-a-uuid", uid, oid, map[string]any{"enabled": true}), http.StatusBadRequest, "invalid id")
	wantErr(t, do(t, r, http.MethodDelete, "/api/sources/not-a-uuid", uid, oid, nil), http.StatusBadRequest, "invalid id")
	wantErr(t, do(t, r, http.MethodGet, "/api/sources/"+uuid.NewString(), uid, oid, nil), http.StatusNotFound, "not found")

	wantErr(t, do(t, r, http.MethodPatch, "/api/sources/"+sid, uid, oid, map[string]any{}), http.StatusBadRequest, "nothing to update")
	wantErr(t, do(t, r, http.MethodPatch, "/api/sources/"+sid, uid, oid, map[string]any{"allowed_methods": []string{}}), http.StatusBadRequest, "at least one method required")
	wantErr(t, do(t, r, http.MethodPatch, "/api/sources/"+sid, uid, oid, map[string]any{"allowed_methods": []string{"POST", "GET"}}), http.StatusBadRequest, `unsupported method: "GET"`)

	// Documents CURRENT behaviour, not contract: duplicate names read as 500, not 409.
	wantErr(t, do(t, r, http.MethodPost, "/api/sources", uid, oid, map[string]any{"name": src.Name}), http.StatusInternalServerError, "create source")
	wantErr(t, do(t, r, http.MethodPatch, "/api/sources/"+sid, uid, oid, map[string]any{"name": other.Name}), http.StatusInternalServerError, "update source")

	// None of the rejected writes changed the row.
	row, err := q.GetSourceForOrg(context.Background(), store.GetSourceForOrgParams{ID: src.ID, OrgID: store.UUID(oid)})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	mustEq(t, "name", row.Name, src.Name)
	if !reflect.DeepEqual(row.AllowedMethods, src.AllowedMethods) {
		t.Fatalf("methods changed: %v", row.AllowedMethods)
	}
	mustEq(t, "sources named like src", rowCount(t, pool, `SELECT count(*) FROM sources WHERE org_id=$1 AND name=$2`, oid, src.Name), 1)
}

func TestSourceCrossOrgIsolation(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uidA, oidA := seedOrg(t, q)
	uidB, oidB := seedOrg(t, q)
	var evicted int
	r := newRouter(q, func(r chi.Router, h Handlers) {
		h.EvictSourceCache = func(string) { evicted++ }
		sourceRoutes(r, h)
	})
	src := newSource(t, q, oidA)
	sid := store.GoUUID(src.ID).String()

	wantErr(t, do(t, r, http.MethodGet, "/api/sources/"+sid, uidB, oidB, nil), http.StatusNotFound, "not found")
	wantErr(t, do(t, r, http.MethodPatch, "/api/sources/"+sid, uidB, oidB, map[string]any{"name": "hijack", "enabled": false}), http.StatusNotFound, "not found")
	wantErr(t, do(t, r, http.MethodDelete, "/api/sources/"+sid, uidB, oidB, nil), http.StatusNotFound, "not found")
	if l := decodeList(t, do(t, r, http.MethodGet, "/api/sources", uidB, oidB, nil)); hasID(l, store.GoUUID(src.ID)) || len(l) != 0 {
		t.Fatalf("org B sees org A's source: %v", l)
	}
	if evicted != 0 {
		t.Fatalf("cache evicted %d times for refused writes", evicted)
	}

	row, err := q.GetSourceForOrg(context.Background(), store.GetSourceForOrgParams{ID: src.ID, OrgID: store.UUID(oidA)})
	if err != nil {
		t.Fatalf("org A's source gone: %v", err)
	}
	mustEq(t, "name", row.Name, src.Name)
	mustEq(t, "enabled", row.Enabled, true)

	// And org A still sees it.
	if l := decodeList(t, do(t, r, http.MethodGet, "/api/sources", uidA, oidA, nil)); !hasID(l, store.GoUUID(src.ID)) {
		t.Fatalf("org A lost its source: %v", l)
	}
}

// Deleting a source takes its connections, requests and events with it, but
// not the destination those connections pointed at.
func TestDeleteSourceCascades(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	dropEvents(t, pool, oid)
	r := newRouter(q, sourceRoutes)
	reqID, srcID := seedRequest(t, q, oid)
	dst := seedDest(t, q, oid, "https://sink.example.test/h")
	conn, err := q.CreateConnection(context.Background(), store.CreateConnectionParams{
		SourceID: store.UUID(srcID), DestinationID: dst.ID, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	seedEvent(t, q, oid, reqID, conn, false)
	mustEq(t, "events before", rowCount(t, pool, `SELECT count(*) FROM events WHERE connection_id=$1`, conn.ID), 1)

	wantStatus(t, do(t, r, http.MethodDelete, "/api/sources/"+srcID.String(), uid, oid, nil), http.StatusNoContent)

	mustEq(t, "connections", rowCount(t, pool, `SELECT count(*) FROM connections WHERE id=$1`, conn.ID), 0)
	mustEq(t, "requests", rowCount(t, pool, `SELECT count(*) FROM requests WHERE id=$1`, reqID), 0)
	mustEq(t, "events", rowCount(t, pool, `SELECT count(*) FROM events WHERE connection_id=$1`, conn.ID), 0)
	mustEq(t, "destination kept", rowCount(t, pool, `SELECT count(*) FROM destinations WHERE id=$1`, dst.ID), 1)
}
