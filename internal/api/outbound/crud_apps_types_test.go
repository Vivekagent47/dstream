package outbound

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
)

func (f *fx) appRow(id string) (store.Application, error) {
	return f.q.GetApplicationForOrg(context.Background(), store.GetApplicationForOrgParams{ID: store.UUID(uuid.MustParse(id)), OrgID: store.UUID(f.oid)})
}

func TestApplication_CreateValidatesAndStores(t *testing.T) {
	f := newFx(t)
	want(t, f.own(http.MethodPost, "/api/applications", nil), http.StatusBadRequest, "invalid json")
	want(t, f.own(http.MethodPost, "/api/applications", map[string]any{"name": ""}), http.StatusBadRequest, "name required")

	rec := f.own(http.MethodPost, "/api/applications", map[string]any{"name": "Meta", "metadata": map[string]any{"tier": "gold"}})
	want(t, rec, http.StatusCreated, "")
	row, err := f.appRow(obj(t, rec)["id"].(string))
	if err != nil || row.Name != "Meta" || string(row.Metadata) != `{"tier": "gold"}` {
		t.Fatalf("stored app: err=%v row=%+v", err, row)
	}
	rec = f.own(http.MethodPost, "/api/applications", map[string]any{"name": "Plain"})
	want(t, rec, http.StatusCreated, "")
	if row, _ := f.appRow(obj(t, rec)["id"].(string)); string(row.Metadata) != `{}` {
		t.Fatalf("absent metadata must store {}, got %s", row.Metadata)
	}
}

func TestApplication_CreateDatabaseFailureIs500(t *testing.T) {
	f := newFx(t, withQueries(tracedQueries(t, failAll("insert into applications"))))
	want(t, f.own(http.MethodPost, "/api/applications", map[string]any{"name": "Nope"}), http.StatusInternalServerError, "create application")
	if !strings.Contains(f.logs.String(), "create application") {
		t.Fatalf("failure must be logged")
	}
}

func TestApplication_GetAndList(t *testing.T) {
	f := newFx(t)
	want(t, f.own(http.MethodGet, "/api/applications/not-a-uuid", nil), http.StatusBadRequest, "invalid app id")
	want(t, f.own(http.MethodGet, "/api/applications/"+uuid.NewString(), nil), http.StatusNotFound, "application not found")
	rec := f.own(http.MethodGet, appPath(f.app), nil)
	want(t, rec, http.StatusOK, "")
	if b := obj(t, rec); b["id"] != uid36(f.app.ID) || b["org_id"] != f.oid.String() || b["name"] != f.app.Name || b["is_operational"] != false {
		t.Fatalf("get app view: %v", b)
	}
	// The operational app is not part of the customer-facing list.
	opRec := f.own(http.MethodGet, "/api/operational-app", nil)
	want(t, opRec, http.StatusOK, "")
	opID := obj(t, opRec)["id"].(string)
	rec = f.own(http.MethodGet, "/api/applications", nil)
	want(t, rec, http.StatusOK, "")
	if strings.Contains(rec.Body.String(), opID) || !strings.Contains(rec.Body.String(), uid36(f.app.ID)) {
		t.Fatalf("list must show customer apps and hide the operational one: %s", rec.Body.String())
	}
}

func TestApplication_ListPaginatesWithACursor(t *testing.T) {
	f := newFx(t)
	bUID, bOrg := seedOrg(t, f.q) // a fresh org so the page counts are exact
	for i := 0; i < pageSize+3; i++ {
		f.mkApp(bOrg, "P")
	}
	type page struct {
		Data []map[string]any `json:"data"`
		Next *string          `json:"next_cursor"`
	}
	get := func(q string) page {
		rec := f.as(bUID, bOrg, http.MethodGet, "/api/applications"+q, nil)
		want(t, rec, http.StatusOK, "")
		var p page
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p1 := get("")
	if len(p1.Data) != pageSize || p1.Next == nil {
		t.Fatalf("page 1: len=%d next=%v", len(p1.Data), p1.Next)
	}
	p2 := get("?cursor=" + *p1.Next)
	if len(p2.Data) != 3 || p2.Next != nil {
		t.Fatalf("page 2: len=%d next=%v", len(p2.Data), p2.Next)
	}
	seen := map[any]bool{}
	for _, a := range p1.Data {
		seen[a["id"]] = true
	}
	for _, a := range p2.Data {
		if seen[a["id"]] {
			t.Fatalf("page 2 repeats %v", a["id"])
		}
	}
	// A cursor that does not decode is the first page, not an error.
	for _, bad := range []string{"%%%", "bm90LWEtY3Vyc29y" /* no "|" */, "YmFkLXRpbWV8YmFkLXV1aWQ" /* bad ts+uuid */} {
		if p := get("?cursor=" + bad); len(p.Data) != pageSize {
			t.Fatalf("cursor %q: want the first page (%d), got %d", bad, pageSize, len(p.Data))
		}
	}
}

func TestApplication_ListDatabaseFailureIs500(t *testing.T) {
	f := newFx(t, withQueries(tracedQueries(t, failAll("from applications", "(created_at, id) <"))))
	want(t, f.own(http.MethodGet, "/api/applications", nil), http.StatusInternalServerError, "list applications")
}

func TestApplication_Patch(t *testing.T) {
	f := newFx(t)
	other := f.mkApp(f.oid, "other")
	uidB := uniq("uid")
	if _, err := f.pool.Exec(context.Background(), `UPDATE applications SET uid=$1 WHERE id=$2`, uidB, other.ID); err != nil {
		t.Fatal(err)
	}
	want(t, f.own(http.MethodPatch, "/api/applications/not-a-uuid", map[string]any{"name": "x"}), http.StatusBadRequest, "invalid app id")
	want(t, f.own(http.MethodPatch, appPath(f.app), nil), http.StatusBadRequest, "invalid json")
	want(t, f.own(http.MethodPatch, "/api/applications/"+uuid.NewString(), map[string]any{"name": "x"}), http.StatusNotFound, "not found")

	rec := f.own(http.MethodPatch, appPath(f.app), map[string]any{"name": "Renamed", "metadata": map[string]any{"a": 1}})
	want(t, rec, http.StatusOK, "")
	row, _ := f.appRow(uid36(f.app.ID))
	if row.Name != "Renamed" || string(row.Metadata) != `{"a": 1}` {
		t.Fatalf("patch must persist name+metadata: %+v", row)
	}
	// Absent fields stay.
	want(t, f.own(http.MethodPatch, appPath(f.app), map[string]any{"name": "Again"}), http.StatusOK, "")
	row, _ = f.appRow(uid36(f.app.ID))
	if row.Name != "Again" || string(row.Metadata) != `{"a": 1}` {
		t.Fatalf("an absent metadata must be left alone: %+v", row)
	}
	// Taking another app's uid is a 409 and changes nothing.
	before, _ := f.appRow(uid36(f.app.ID))
	want(t, f.own(http.MethodPatch, appPath(f.app), map[string]any{"uid": uidB}), http.StatusConflict, "application uid already in use")
	if after, _ := f.appRow(uid36(f.app.ID)); !reflect.DeepEqual(before, after) {
		t.Fatalf("conflicting patch changed the row")
	}
}

func TestApplication_PatchAndDeleteDatabaseFailureAre500(t *testing.T) {
	f := newFx(t, withQueries(tracedQueries(t, failAll("update applications"))))
	before, _ := f.appRow(uid36(f.app.ID))
	want(t, f.own(http.MethodPatch, appPath(f.app), map[string]any{"name": "x"}), http.StatusInternalServerError, "update application")
	if after, _ := f.appRow(uid36(f.app.ID)); !reflect.DeepEqual(before, after) {
		t.Fatalf("failed patch changed the row")
	}
	g := newFx(t, withQueries(tracedQueries(t, failAll("delete from applications"))))
	want(t, g.own(http.MethodDelete, appPath(g.app), nil), http.StatusInternalServerError, "delete application")
	if _, err := g.appRow(uid36(g.app.ID)); err != nil {
		t.Fatalf("failed delete removed the app: %v", err)
	}
}

func TestApplication_DeleteRemovesTheRowAndItsChildren(t *testing.T) {
	f := newFx(t)
	want(t, f.own(http.MethodDelete, "/api/applications/not-a-uuid", nil), http.StatusBadRequest, "invalid app id")
	want(t, f.own(http.MethodDelete, "/api/applications/"+uuid.NewString(), nil), http.StatusNotFound, "not found")
	ep := f.mkEp(f.app, "https://ex.test/del")
	msg := f.mkMsg(f.app, "x")
	want(t, f.own(http.MethodDelete, appPath(f.app), nil), http.StatusNoContent, "")
	if _, err := f.appRow(uid36(f.app.ID)); err == nil {
		t.Fatalf("application still exists")
	}
	var n int
	_ = f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM endpoints WHERE id=$1) + (SELECT count(*) FROM messages WHERE id=$2)`, ep.ID, msg.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("children of a deleted application survived (%d rows)", n)
	}
	want(t, f.own(http.MethodGet, appPath(f.app), nil), http.StatusNotFound, "application not found")
}

func TestOperationalApp_GetOrCreate(t *testing.T) {
	f := newFx(t)
	rec := f.own(http.MethodGet, "/api/operational-app", nil)
	want(t, rec, http.StatusOK, "")
	b := obj(t, rec)
	row, err := f.appRow(b["id"].(string))
	if err != nil || !row.IsOperational || b["is_operational"] != true {
		t.Fatalf("operational app: err=%v row=%+v view=%v", err, row, b)
	}
	if again := obj(t, f.own(http.MethodGet, "/api/operational-app", nil)); again["id"] != b["id"] {
		t.Fatalf("second call must return the same app, got %v vs %v", again["id"], b["id"])
	}
}

func TestOperationalApp_DatabaseFailuresAre500(t *testing.T) {
	f := newFx(t, withQueries(tracedQueries(t, failAll("insert into applications"))))
	want(t, f.own(http.MethodGet, "/api/operational-app", nil), http.StatusInternalServerError, "operational app")

	g := newFx(t, withQueries(tracedQueries(t, failAll("from applications where id = $1 and org_id = $2"))))
	want(t, g.own(http.MethodGet, "/api/operational-app", nil), http.StatusInternalServerError, "load operational app")
}

// ---- event types ----

func TestEventType_CreateValidation(t *testing.T) {
	f := newFx(t)
	want(t, f.own(http.MethodPost, "/api/event-types", nil), http.StatusBadRequest, "invalid json")
	want(t, f.own(http.MethodPost, "/api/event-types", map[string]any{"name": ""}), http.StatusBadRequest, "name required")
	name := uniq("bad.schema")
	rec := f.own(http.MethodPost, "/api/event-types", map[string]any{"name": name, "schema": map[string]any{"type": "not-a-type"}})
	want(t, rec, http.StatusBadRequest, "")
	// Documents CURRENT behaviour, not contract: only the prefix is pinned. The full
	// message carries the server's working directory (validate.go registers the schema
	// under the bare name "schema.json", so the compiler resolves it to a file:/// URL),
	// a known information leak; do not tighten this onto the leaking part.
	if msg := obj(t, rec)["error"].(string); !strings.HasPrefix(msg, "schema is not a valid JSON Schema: ") {
		t.Fatalf("schema error message: %q", msg)
	}
	if _, err := f.q.GetEventTypeForOrg(context.Background(), store.GetEventTypeForOrgParams{OrgID: store.UUID(f.oid), Name: name}); err == nil {
		t.Fatalf("a refused create stored the event type")
	}
	// Schemas the compiler cannot register or resolve are refused the same way.
	for prefix, schema := range map[string]map[string]any{
		`duplicate anchor "a"`: {"$anchor": "a", "$defs": map[string]any{"x": map[string]any{"$anchor": "a"}}},
		"error in parsing id":  {"$id": ":bad"},
	} {
		rec = f.own(http.MethodPost, "/api/event-types", map[string]any{"name": uniq("bad.reg"), "schema": schema})
		want(t, rec, http.StatusBadRequest, "")
		// Documents CURRENT behaviour, not contract: only the prefix is pinned. The full
		// message carries the server's working directory (validate.go registers the schema
		// under the bare name "schema.json", so the compiler resolves it to a file:/// URL),
		// a known information leak; do not tighten this onto the leaking part.
		if msg := obj(t, rec)["error"].(string); !strings.HasPrefix(msg, "schema is not a valid JSON Schema: "+prefix) {
			t.Fatalf("want a %q refusal, got %q", prefix, msg)
		}
	}
	// JSON null means "no schema" and is stored as SQL NULL.
	nul := uniq("null.schema")
	body := strings.NewReader(`{"name":"` + nul + `","schema":null}`)
	rec = httpDo(f, http.MethodPost, "/api/event-types", body)
	want(t, rec, http.StatusCreated, "")
	got, _ := f.q.GetEventTypeForOrg(context.Background(), store.GetEventTypeForOrgParams{OrgID: store.UUID(f.oid), Name: nul})
	if got.Schema != nil {
		t.Fatalf("null schema must store NULL, got %s", got.Schema)
	}
}

func TestEventType_CreateDatabaseFailureIs500(t *testing.T) {
	f := newFx(t, withQueries(tracedQueries(t, failAll("insert into event_types"))))
	name := uniq("fail")
	want(t, f.own(http.MethodPost, "/api/event-types", map[string]any{"name": name}), http.StatusInternalServerError, "create event type")
	if _, err := f.q.GetEventTypeForOrg(context.Background(), store.GetEventTypeForOrgParams{OrgID: store.UUID(f.oid), Name: name}); err == nil {
		t.Fatalf("a failed create left a row")
	}
}

func TestEventType_ListGetPatchDelete(t *testing.T) {
	f := newFx(t)
	a, b := uniq("zz.crud"), uniq("aa.crud")
	f.mkEventType(f.oid, a, nil)
	f.mkEventType(f.oid, b, []byte(`{"type":"object"}`))

	rec := f.own(http.MethodGet, "/api/event-types", nil)
	want(t, rec, http.StatusOK, "")
	var order []string
	for _, e := range list(t, rec) {
		if n := e["name"].(string); n == a || n == b {
			order = append(order, n)
		}
	}
	if len(order) != 2 || order[0] != b || order[1] != a {
		t.Fatalf("list must be name-ordered [%s %s], got %v", b, a, order)
	}

	want(t, f.own(http.MethodGet, "/api/event-types/"+uniq("missing"), nil), http.StatusNotFound, "not found")
	got := obj(t, f.own(http.MethodGet, "/api/event-types/"+b, nil))
	if got["name"] != b || got["archived"] != false || got["schema"] == nil {
		t.Fatalf("get event type: %v", got)
	}
	if obj(t, f.own(http.MethodGet, "/api/event-types/"+a, nil))["schema"] != nil {
		t.Fatalf("a type without a schema must render schema:null")
	}

	// Patch: description + archived persist; schema omitted stays, null clears,
	// an object replaces.
	want(t, f.own(http.MethodPatch, "/api/event-types/"+b, nil), http.StatusBadRequest, "invalid json")
	want(t, f.own(http.MethodPatch, "/api/event-types/"+uniq("missing"), map[string]any{"description": "x"}), http.StatusNotFound, "not found")
	rec = f.own(http.MethodPatch, "/api/event-types/"+b, map[string]any{"description": "d", "archived": true})
	want(t, rec, http.StatusOK, "")
	row, _ := f.q.GetEventTypeForOrg(context.Background(), store.GetEventTypeForOrgParams{OrgID: store.UUID(f.oid), Name: b})
	if row.Description != "d" || !row.Archived || string(row.Schema) != `{"type": "object"}` {
		t.Fatalf("patch result: %+v", row)
	}
	rec = f.own(http.MethodPatch, "/api/event-types/"+b, map[string]any{"schema": map[string]any{"type": "string"}})
	want(t, rec, http.StatusOK, "")
	row, _ = f.q.GetEventTypeForOrg(context.Background(), store.GetEventTypeForOrgParams{OrgID: store.UUID(f.oid), Name: b})
	if string(row.Schema) != `{"type": "string"}` || row.Description != "d" {
		t.Fatalf("schema replace: %+v", row)
	}
	before := row
	rec = f.own(http.MethodPatch, "/api/event-types/"+b, map[string]any{"schema": map[string]any{"type": "bogus"}})
	want(t, rec, http.StatusBadRequest, "")
	// Documents CURRENT behaviour, not contract: only the prefix is pinned. The full
	// message carries the server's working directory (validate.go registers the schema
	// under the bare name "schema.json", so the compiler resolves it to a file:/// URL),
	// a known information leak; do not tighten this onto the leaking part.
	if !strings.HasPrefix(obj(t, rec)["error"].(string), "schema is not a valid JSON Schema: ") {
		t.Fatalf("bad schema message: %s", rec.Body.String())
	}
	if after, _ := f.q.GetEventTypeForOrg(context.Background(), store.GetEventTypeForOrgParams{OrgID: store.UUID(f.oid), Name: b}); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused patch changed the row")
	}

	want(t, f.own(http.MethodDelete, "/api/event-types/"+uniq("missing"), nil), http.StatusNotFound, "not found")
	want(t, f.own(http.MethodDelete, "/api/event-types/"+b, nil), http.StatusNoContent, "")
	if _, err := f.q.GetEventTypeForOrg(context.Background(), store.GetEventTypeForOrgParams{OrgID: store.UUID(f.oid), Name: b}); err == nil {
		t.Fatalf("event type survived delete")
	}
	if _, err := f.q.GetEventTypeForOrg(context.Background(), store.GetEventTypeForOrgParams{OrgID: store.UUID(f.oid), Name: a}); err != nil {
		t.Fatalf("delete removed a sibling: %v", err)
	}
}

func TestEventType_DatabaseFailuresAre500(t *testing.T) {
	for _, c := range []struct {
		name, marker, method, path string
		body                       any
		msg                        string
	}{
		{"list", "from event_types where org_id = $1 order by", "GET", "/api/event-types", nil, "list event types"},
		{"patch", "update event_types", "PATCH", "/api/event-types/%s", map[string]any{"description": "x"}, "update event type"},
		{"delete", "delete from event_types", "DELETE", "/api/event-types/%s", nil, "delete event type"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t, withQueries(tracedQueries(t, failAll(c.marker))))
			name := uniq("fail")
			f.mkEventType(f.oid, name, nil)
			path := c.path
			if strings.Contains(path, "%s") {
				path = strings.Replace(path, "%s", name, 1)
			}
			want(t, f.own(c.method, path, c.body), http.StatusInternalServerError, c.msg)
			got, err := f.q.GetEventTypeForOrg(context.Background(), store.GetEventTypeForOrgParams{OrgID: store.UUID(f.oid), Name: name})
			if err != nil || got.Description != "" || got.Archived {
				t.Fatalf("failed %s changed the event type: err=%v %+v", c.name, err, got)
			}
		})
	}
}
