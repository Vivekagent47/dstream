package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
)

func destRoutes(r chi.Router, h Handlers) {
	h.SelfHosts = []string{"dstream.example.test"}
	r.Route("/destinations", func(r chi.Router) {
		r.Get("/", h.ListDestinations)
		r.Post("/", h.CreateDestination)
		r.Get("/{id}", h.GetDestination)
		r.Patch("/{id}", h.PatchDestination)
		r.Delete("/{id}", h.DeleteDestination)
	})
}

func getDest(t *testing.T, q *store.Queries, oid uuid.UUID, id string) store.Destination {
	t.Helper()
	d, err := q.GetDestinationForOrg(context.Background(), store.GetDestinationForOrgParams{
		ID: store.UUID(uuid.MustParse(id)), OrgID: store.UUID(oid)})
	if err != nil {
		t.Fatalf("get destination: %v", err)
	}
	return d
}

func TestDestinationCRUD(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	r := newRouter(q, destRoutes)

	if l := decodeList(t, do(t, r, http.MethodGet, "/api/destinations", uid, oid, nil)); len(l) != 0 {
		t.Fatalf("fresh org lists %d destinations", len(l))
	}

	// Create http with every optional field. auth_config is stored but never echoed.
	rec := do(t, r, http.MethodPost, "/api/destinations", uid, oid, map[string]any{
		"name": "api", "type": "http", "description": "prod", "url": "https://hooks.example.test/in",
		"auth_config": map[string]any{"secret": "s3cr3t"}, "rate_limit_rps": 5, "rate_limit_burst": 10, "max_inflight": 2,
	})
	wantStatus(t, rec, http.StatusCreated)
	if strings.Contains(rec.Body.String(), "s3cr3t") {
		t.Fatalf("response leaks auth_config: %s", rec.Body.String())
	}
	created := decodeMap(t, rec)
	id := created["id"].(string)
	row := getDest(t, q, oid, id)
	mustEq(t, "name", row.Name, "api")
	mustEq(t, "type", row.Type, "http")
	mustEq(t, "description", row.Description, "prod")
	mustEq(t, "url", *row.Url, "https://hooks.example.test/in")
	mustEq(t, "rps", *row.RateLimitRps, int32(5))
	mustEq(t, "burst", *row.RateLimitBurst, int32(10))
	mustEq(t, "inflight", *row.MaxInflight, int32(2))
	mustEq(t, "stored auth", strings.ReplaceAll(string(row.AuthConfig), " ", ""), `{"secret":"s3cr3t"}`)
	mustEq(t, "auth_configured", created["auth_configured"], true)
	mustEq(t, "body url", created["url"], "https://hooks.example.test/in")
	mustEq(t, "body rps", created["rate_limit_rps"], float64(5))
	if n := auditCount(t, pool, oid, "destination.create", uuid.MustParse(id)); n != 1 {
		t.Fatalf("destination.create audit rows = %d", n)
	}

	// A cli destination needs no url; omitted auth_config defaults to {} → not configured.
	rec = do(t, r, http.MethodPost, "/api/destinations", uid, oid, map[string]any{"name": "laptop", "type": "cli"})
	wantStatus(t, rec, http.StatusCreated)
	cli := decodeMap(t, rec)
	cliID := cli["id"].(string)
	crow := getDest(t, q, oid, cliID)
	if crow.Url != nil {
		t.Fatalf("cli destination stored url %q", *crow.Url)
	}
	mustEq(t, "cli auth", string(crow.AuthConfig), "{}")
	mustEq(t, "cli auth_configured", cli["auth_configured"], false)
	if cli["url"] != nil {
		t.Fatalf("cli url = %v, want null", cli["url"])
	}

	l := decodeList(t, do(t, r, http.MethodGet, "/api/destinations", uid, oid, nil))
	if !hasID(l, uuid.MustParse(id)) || !hasID(l, uuid.MustParse(cliID)) {
		t.Fatalf("list missing created destinations: %v", l)
	}
	got := decodeMap(t, do(t, r, http.MethodGet, "/api/destinations/"+id, uid, oid, nil))
	mustEq(t, "get name", got["name"], "api")
	mustEq(t, "get org_id", got["org_id"], oid.String())

	// Patch several fields at once, including the URL (owner may repoint).
	rec = do(t, r, http.MethodPatch, "/api/destinations/"+id, uid, oid, map[string]any{
		"name": "api2", "description": "staging", "url": "https://other.example.test/in",
		"rate_limit_rps": 7, "auth_config": map[string]any{"secret": "rotated"},
	})
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "rotated") {
		t.Fatalf("patch response leaks auth_config: %s", rec.Body.String())
	}
	row = getDest(t, q, oid, id)
	mustEq(t, "name", row.Name, "api2")
	mustEq(t, "description", row.Description, "staging")
	mustEq(t, "url", *row.Url, "https://other.example.test/in")
	mustEq(t, "rps", *row.RateLimitRps, int32(7))
	mustEq(t, "burst kept", *row.RateLimitBurst, int32(10))
	mustEq(t, "stored auth", strings.ReplaceAll(string(row.AuthConfig), " ", ""), `{"secret":"rotated"}`)
	mustEq(t, "body name", decodeMap(t, rec)["name"], "api2")

	// The audit entry lists what changed and redacts auth_config.
	var meta []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT metadata FROM audit_logs WHERE org_id=$1 AND action='destination.update' AND target_id=$2`,
		oid, uuid.MustParse(id)).Scan(&meta); err != nil {
		t.Fatalf("update audit row: %v", err)
	}
	var m struct {
		Changed map[string]map[string]any `json:"changed"`
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		t.Fatal(err)
	}
	mustEq(t, "audit auth_config", m.Changed["auth_config"]["to"], "redacted")
	mustEq(t, "audit name from", m.Changed["name"]["from"], "api")
	mustEq(t, "audit name to", m.Changed["name"]["to"], "api2")
	if strings.Contains(string(meta), "rotated") {
		t.Fatalf("audit metadata leaks secret: %s", meta)
	}

	// A patch that changes nothing writes no update audit entry.
	before := auditCount(t, pool, oid, "destination.update", uuid.MustParse(id))
	wantStatus(t, do(t, r, http.MethodPatch, "/api/destinations/"+id, uid, oid, map[string]any{"name": "api2"}), http.StatusOK)
	mustEq(t, "no-op audit rows", auditCount(t, pool, oid, "destination.update", uuid.MustParse(id)), before)

	// http → cli keeps working without a url check.
	wantStatus(t, do(t, r, http.MethodPatch, "/api/destinations/"+id, uid, oid, map[string]any{"type": "cli"}), http.StatusOK)
	mustEq(t, "type after patch", getDest(t, q, oid, id).Type, "cli")
	// cli → http with a url in the same patch is valid.
	wantStatus(t, do(t, r, http.MethodPatch, "/api/destinations/"+cliID, uid, oid, map[string]any{"type": "http", "url": "https://now-http.example.test"}), http.StatusOK)
	crow = getDest(t, q, oid, cliID)
	mustEq(t, "cli→http type", crow.Type, "http")
	mustEq(t, "cli→http url", *crow.Url, "https://now-http.example.test")

	// Delete, then it is gone.
	wantStatus(t, do(t, r, http.MethodDelete, "/api/destinations/"+id, uid, oid, nil), http.StatusNoContent)
	mustEq(t, "rows after delete", rowCount(t, pool, `SELECT count(*) FROM destinations WHERE id=$1`, uuid.MustParse(id)), 0)
	mustEq(t, "delete audit", auditCount(t, pool, oid, "destination.delete", uuid.MustParse(id)), 1)
	wantErr(t, do(t, r, http.MethodGet, "/api/destinations/"+id, uid, oid, nil), http.StatusNotFound, "not found")
	wantErr(t, do(t, r, http.MethodPatch, "/api/destinations/"+id, uid, oid, map[string]any{"name": "x"}), http.StatusNotFound, "not found")
}

func TestDestinationGuardsAndValidation(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	r := newRouter(q, destRoutes)
	ctx := context.Background()
	const orig = "https://orig.example.test/h"
	dst := seedDest(t, q, oid, orig)
	cli, err := q.CreateDestination(ctx, store.CreateDestinationParams{
		OrgID: store.UUID(oid), Name: "cli-" + uuid.NewString(), Type: "cli", AuthConfig: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	did, cid := store.GoUUID(dst.ID).String(), store.GoUUID(cli.ID).String()
	orgCount := func() int { return rowCount(t, pool, `SELECT count(*) FROM destinations WHERE org_id=$1`, oid) }
	startCount := orgCount()

	create := func(body map[string]any, status int, msg string) {
		t.Helper()
		wantErr(t, do(t, r, http.MethodPost, "/api/destinations", uid, oid, body), status, msg)
	}
	create(map[string]any{"type": "http", "url": orig}, 400, "name required")
	create(map[string]any{"name": "n", "type": "ws"}, 400, "type must be 'http' or 'cli'")
	create(map[string]any{"name": "n"}, 400, "type must be 'http' or 'cli'")
	create(map[string]any{"name": "n", "type": "http"}, 400, "url required for http destination")
	create(map[string]any{"name": "n", "type": "http", "url": ""}, 400, "url required for http destination")
	create(map[string]any{"name": "n", "type": "http", "url": "ftp://files.example.test/x"}, 400, `invalid destination url: url scheme must be http or https, got "ftp"`)
	create(map[string]any{"name": "n", "type": "http", "url": "https://"}, 400, "invalid destination url: url must have a host")
	create(map[string]any{"name": "n", "type": "http", "url": "javascript:alert(1)"}, 400, `invalid destination url: url scheme must be http or https, got "javascript"`)
	// Loop guard: dstream's own host is refused, case-insensitively, with or without port/path.
	for _, u := range []string{"https://dstream.example.test/e/abc", "http://DSTREAM.example.test:8080/x"} {
		create(map[string]any{"name": "n", "type": "http", "url": u}, 400, "url must not point at dstream itself (loop guard)")
	}
	create(map[string]any{"name": "n", "type": "http", "url": "http://[::1"}, 400, `invalid destination url: invalid url: parse "http://[::1": missing ']' in host`)
	// Documents CURRENT behaviour, not contract: a duplicate name is a user conflict but reads as 500, not 409.
	create(map[string]any{"name": dst.Name, "type": "cli"}, 500, "create destination")
	rec := httptest.NewRecorder()
	req := sessionReq(t, http.MethodPost, "/api/destinations", uid, oid, map[string]any{})
	req.Body = io.NopCloser(strings.NewReader("{nope"))
	r.ServeHTTP(rec, req)
	wantErr(t, rec, 400, "invalid json")
	mustEq(t, "destinations after rejected creates", orgCount(), startCount)

	patch := func(id string, body map[string]any, status int, msg string) {
		t.Helper()
		wantErr(t, do(t, r, http.MethodPatch, "/api/destinations/"+id, uid, oid, body), status, msg)
	}
	patch(did, map[string]any{"type": "ws"}, 400, "type must be 'http' or 'cli'")
	patch(did, map[string]any{"url": ""}, 400, "url required for http destination")
	patch(cid, map[string]any{"type": "http"}, 400, "url required for http destination") // cli has no url to inherit
	patch(did, map[string]any{"url": "ftp://x.example.test"}, 400, `invalid destination url: url scheme must be http or https, got "ftp"`)
	patch(did, map[string]any{"url": "https://dstream.example.test/loop"}, 400, "url must not point at dstream itself (loop guard)")
	patch(cid, map[string]any{"type": "http", "url": "https://dstream.example.test/loop"}, 400, "url must not point at dstream itself (loop guard)")
	patch(did, map[string]any{"url": "http://[::1"}, 400, `invalid destination url: invalid url: parse "http://[::1": missing ']' in host`)
	// Documents CURRENT behaviour, not contract: duplicate name reads as 500, not 409.
	patch(did, map[string]any{"name": cli.Name}, 500, "update")
	patch(uuid.NewString(), map[string]any{"name": "x"}, 404, "not found")
	patch("not-a-uuid", map[string]any{"name": "x"}, 400, "invalid id")
	rec = httptest.NewRecorder()
	req = sessionReq(t, http.MethodPatch, "/api/destinations/"+did, uid, oid, map[string]any{})
	req.Body = io.NopCloser(strings.NewReader("{nope"))
	r.ServeHTTP(rec, req)
	wantErr(t, rec, 400, "invalid json")
	wantErr(t, do(t, r, http.MethodGet, "/api/destinations/not-a-uuid", uid, oid, nil), 400, "invalid id")
	wantErr(t, do(t, r, http.MethodDelete, "/api/destinations/not-a-uuid", uid, oid, nil), 400, "invalid id")
	wantErr(t, do(t, r, http.MethodGet, "/api/destinations/"+uuid.NewString(), uid, oid, nil), 404, "not found")

	// Every rejected write left both rows exactly as seeded.
	after := getDest(t, q, oid, did)
	mustEq(t, "http name", after.Name, dst.Name)
	mustEq(t, "http url", *after.Url, orig)
	mustEq(t, "http type", after.Type, "http")
	acli := getDest(t, q, oid, cid)
	mustEq(t, "cli type", acli.Type, "cli")
	if acli.Url != nil {
		t.Fatalf("cli url became %q", *acli.Url)
	}
	mustEq(t, "destinations after rejected patches", orgCount(), startCount)
}

func TestDestinationCrossOrgIsolation(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uidA, oidA := seedOrg(t, q)
	uidB, oidB := seedOrg(t, q)
	r := newRouter(q, destRoutes)
	dst := seedDest(t, q, oidA, "https://a.example.test/h")
	id := store.GoUUID(dst.ID).String()

	wantErr(t, do(t, r, http.MethodGet, "/api/destinations/"+id, uidB, oidB, nil), 404, "not found")
	wantErr(t, do(t, r, http.MethodPatch, "/api/destinations/"+id, uidB, oidB, map[string]any{"name": "hijack", "url": "https://evil.example.test"}), 404, "not found")
	// Documents CURRENT behaviour, not contract: a foreign DELETE answers 204 (and
	// audits a delete in the caller's org) although nothing was deleted; sources 404.
	wantStatus(t, do(t, r, http.MethodDelete, "/api/destinations/"+id, uidB, oidB, nil), http.StatusNoContent)
	if l := decodeList(t, do(t, r, http.MethodGet, "/api/destinations", uidB, oidB, nil)); len(l) != 0 {
		t.Fatalf("org B sees org A's destinations: %v", l)
	}

	row := getDest(t, q, oidA, id)
	mustEq(t, "name", row.Name, dst.Name)
	mustEq(t, "url", *row.Url, "https://a.example.test/h")
	if l := decodeList(t, do(t, r, http.MethodGet, "/api/destinations", uidA, oidA, nil)); !hasID(l, uuid.MustParse(id)) {
		t.Fatalf("org A lost its destination: %v", l)
	}
}

// Deleting a destination removes its connections (and their events) but
// leaves the source and its requests alone.
func TestDeleteDestinationCascades(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	dropEvents(t, pool, oid)
	r := newRouter(q, destRoutes)
	reqID, srcID := seedRequest(t, q, oid)
	dst := seedDest(t, q, oid, "https://sink.example.test/h")
	conn, err := q.CreateConnection(context.Background(), store.CreateConnectionParams{
		SourceID: store.UUID(srcID), DestinationID: dst.ID, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	seedEvent(t, q, oid, reqID, conn, false)

	wantStatus(t, do(t, r, http.MethodDelete, "/api/destinations/"+store.GoUUID(dst.ID).String(), uid, oid, nil), http.StatusNoContent)

	mustEq(t, "connection", rowCount(t, pool, `SELECT count(*) FROM connections WHERE id=$1`, conn.ID), 0)
	mustEq(t, "events", rowCount(t, pool, `SELECT count(*) FROM events WHERE connection_id=$1`, conn.ID), 0)
	mustEq(t, "source kept", rowCount(t, pool, `SELECT count(*) FROM sources WHERE id=$1`, srcID), 1)
	mustEq(t, "request kept", rowCount(t, pool, `SELECT count(*) FROM requests WHERE id=$1`, reqID), 1)
}

func TestDiffDestination(t *testing.T) {
	i := func(v int32) *int32 { return &v }
	old := store.Destination{
		Name: "a", Type: "http", Description: "d", Url: pstr("https://x.example.test"),
		RateLimitRps: i(5), RateLimitBurst: i(9), MaxInflight: nil, AuthConfig: []byte(`{"k":"v"}`),
	}

	if d := diffDestination(old, old); len(d) != 0 {
		t.Fatalf("identical rows diff = %v", d)
	}

	nw := old
	nw.Name, nw.Type, nw.Description = "b", "cli", "e"
	nw.Url = nil            // set to nil
	nw.RateLimitRps = i(6)  // changed
	nw.RateLimitBurst = nil // set to nil
	nw.MaxInflight = i(3)   // nil → value
	nw.AuthConfig = []byte(`{"k":"other"}`)
	want := map[string]map[string]any{
		"name":             {"from": "a", "to": "b"},
		"type":             {"from": "http", "to": "cli"},
		"description":      {"from": "d", "to": "e"},
		"url":              {"from": "https://x.example.test", "to": nil},
		"rate_limit_rps":   {"from": int32(5), "to": int32(6)},
		"rate_limit_burst": {"from": int32(9), "to": nil},
		"max_inflight":     {"from": nil, "to": int32(3)},
		"auth_config":      {"from": "redacted", "to": "redacted"},
	}
	if got := diffDestination(old, nw); !reflect.DeepEqual(got, want) {
		t.Fatalf("diff = %#v\nwant   %#v", got, want)
	}

	// Only the changed field appears.
	one := old
	one.Name = "z"
	if got := diffDestination(old, one); len(got) != 1 || got["name"]["to"] != "z" {
		t.Fatalf("single-field diff = %v", got)
	}
}
