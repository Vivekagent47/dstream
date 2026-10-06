package outbound

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Vivekagent47/dstream/internal/store"
)

// isoSnapshot is everything a cross-tenant request could damage.
type isoSnapshot struct {
	App, App2           store.Application
	Ep                  store.Endpoint
	Msg                 store.Message
	Deliveries          []store.ListDeliveriesForMessageRow
	EpCount, EpCount2   int
	MsgCount, MsgCount2 int
	ET                  store.EventType
}

func (f *fx) snapshot(ep store.Endpoint, msg store.Message, etName string) isoSnapshot {
	f.t.Helper()
	ctx := context.Background()
	get := func(a store.Application) store.Application {
		r, err := f.q.GetApplicationForOrg(ctx, store.GetApplicationForOrgParams{ID: a.ID, OrgID: a.OrgID})
		if err != nil {
			f.t.Fatalf("snapshot app: %v", err)
		}
		return r
	}
	m, err := f.q.GetMessageForApp(ctx, store.GetMessageForAppParams{ID: msg.ID, AppID: f.app.ID})
	if err != nil {
		f.t.Fatalf("snapshot msg: %v", err)
	}
	et, err := f.q.GetEventTypeForOrg(ctx, store.GetEventTypeForOrgParams{OrgID: store.UUID(f.oid), Name: etName})
	if err != nil {
		f.t.Fatalf("snapshot event type: %v", err)
	}
	return isoSnapshot{
		App: get(f.app), App2: get(f.app2), Ep: f.epRow(f.app, ep.ID), Msg: m,
		Deliveries: f.deliveries(msg),
		EpCount:    f.epCount(f.app), EpCount2: f.epCount(f.app2),
		MsgCount: f.msgCount(f.app), MsgCount2: f.msgCount(f.app2), ET: et,
	}
}

type isoCase struct {
	name, method, path string // path uses {app} {ep} {msg}
	body               any
	orgMsg             string // error from another org's session; "" = skip
	appMsg             string // error when the path uses the sibling app; "" = skip
}

// TestIsolation_EveryIDRouteRefusesForeignOrgAndSiblingApp drives every handler
// that takes an id with the FIRST application's real ids, from a second org and
// from a second application in the same org, and then proves nothing moved.
func TestIsolation_EveryIDRouteRefusesForeignOrgAndSiblingApp(t *testing.T) {
	f := newFx(t)
	etName := uniq("iso")
	f.mkEventType(f.oid, etName, nil)
	ep := f.mkEp(f.app, "https://ex.test/iso")
	ep2 := f.mkEp(f.app2, "https://ex.test/iso2")
	msg := f.mkMsg(f.app, etName)
	del := f.mkDel(msg, ep)
	if err := f.q.MarkDeliveryDead(context.Background(), del); err != nil {
		t.Fatal(err)
	}
	bUID, bOrg := seedOrg(t, f.q)

	cases := []isoCase{
		{"get app", "GET", "/api/applications/{app}", nil, "application not found", ""},
		{"patch app", "PATCH", "/api/applications/{app}", map[string]any{"name": "hijacked"}, "not found", ""},
		{"delete app", "DELETE", "/api/applications/{app}", nil, "not found", ""},
		{"portal mint", "POST", "/api/applications/{app}/portal-access", nil, "application not found", ""},
		{"portal revoke", "POST", "/api/applications/{app}/portal-access/revoke", nil, "application not found", ""},
		{"create endpoint", "POST", "/api/applications/{app}/endpoints", map[string]any{"url": "https://ex.test/injected"}, "application not found", ""},
		{"list endpoints", "GET", "/api/applications/{app}/endpoints", nil, "application not found", ""},
		{"get endpoint", "GET", "/api/applications/{app}/endpoints/{ep}", nil, "application not found", "endpoint not found"},
		{"get secret", "GET", "/api/applications/{app}/endpoints/{ep}/secret", nil, "application not found", "endpoint not found"},
		{"rotate secret", "POST", "/api/applications/{app}/endpoints/{ep}/rotate-secret", nil, "application not found", "endpoint not found"},
		{"patch endpoint", "PATCH", "/api/applications/{app}/endpoints/{ep}", map[string]any{"description": "hijacked", "disabled": true}, "application not found", "not found"},
		{"delete endpoint", "DELETE", "/api/applications/{app}/endpoints/{ep}", nil, "application not found", "not found"},
		{"recover endpoint", "POST", "/api/applications/{app}/endpoints/{ep}/recover", map[string]any{"since": "2000-01-01T00:00:00Z"}, "application not found", "endpoint not found"},
		{"test endpoint", "POST", "/api/applications/{app}/endpoints/{ep}/test", map[string]any{"event_type": "x"}, "application not found", "endpoint not found"},
		{"endpoint attempts", "GET", "/api/applications/{app}/endpoints/{ep}/attempts", nil, "application not found", "endpoint not found"},
		{"publish", "POST", "/api/applications/{app}/messages", map[string]any{"event_type": etName, "payload": map[string]any{"x": 1}}, "application not found", ""},
		{"list messages", "GET", "/api/applications/{app}/messages", nil, "application not found", ""},
		{"get message", "GET", "/api/applications/{app}/messages/{msg}", nil, "application not found", "message not found"},
		{"message attempts", "GET", "/api/applications/{app}/messages/{msg}/attempts", nil, "application not found", "message not found"},
		{"message deliveries", "GET", "/api/applications/{app}/messages/{msg}/deliveries", nil, "application not found", "message not found"},
		{"replay", "POST", "/api/applications/{app}/messages/{msg}/endpoints/{ep}/replay", nil, "application not found", "message not found"},
	}
	before := f.snapshot(ep, msg, etName)

	fill := func(p string, app store.Application) string {
		return strings.NewReplacer("{app}", uid36(app.ID), "{ep}", uid36(ep.ID), "{msg}", uid36(msg.ID)).Replace(p)
	}
	for _, c := range cases {
		c := c
		t.Run("org/"+c.name, func(t *testing.T) {
			rec := f.as(bUID, bOrg, c.method, fill(c.path, f.app), c.body)
			want(t, rec, http.StatusNotFound, c.orgMsg)
		})
		if c.appMsg != "" {
			t.Run("app/"+c.name, func(t *testing.T) {
				rec := f.own(c.method, fill(c.path, f.app2), c.body)
				want(t, rec, http.StatusNotFound, c.appMsg)
			})
		}
	}
	// Sibling-app list routes are legal reads of the sibling's own (empty) data;
	// they must not leak the first app's rows.
	for _, p := range []string{"/endpoints", "/messages"} {
		rec := f.own(http.MethodGet, appPath(f.app2)+p, nil)
		want(t, rec, http.StatusOK, "")
		for _, id := range []string{uid36(ep.ID), uid36(msg.ID)} {
			if strings.Contains(rec.Body.String(), id) {
				t.Fatalf("sibling app %s leaked %s: %s", p, id, rec.Body.String())
			}
		}
	}
	// Replay of the first app's message to an endpoint of the sibling app, from
	// inside the first app: the endpoint is foreign, so no delivery may appear.
	rec := f.own(http.MethodPost, appPath(f.app)+"/messages/"+uid36(msg.ID)+"/endpoints/"+uid36(ep2.ID)+"/replay", nil)
	want(t, rec, http.StatusNotFound, "endpoint not found")
	// Same, from the other org: its own app, but this org's message.
	bApp := f.mkApp(bOrg, "B")
	rec = f.as(bUID, bOrg, http.MethodPost, appPath(bApp)+"/messages/"+uid36(msg.ID)+"/endpoints/"+uid36(ep.ID)+"/replay", nil)
	want(t, rec, http.StatusNotFound, "message not found")

	if after := f.snapshot(ep, msg, etName); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused request changed state:\nbefore=%+v\nafter =%+v", before, after)
	}
	if n := f.msgCount(bApp); n != 0 {
		t.Fatalf("foreign org gained %d messages", n)
	}
	if n := drainMessageTasks(t, f.dq); n != 0 {
		t.Fatalf("refused requests enqueued %d tasks", n)
	}
	if got := f.deliveries(msg); len(got) != 1 || got[0].Status != "dead" {
		t.Fatalf("delivery must stay dead and single, got %+v", got)
	}
}

// Event types are org-scoped by name: a second org can neither read, edit nor
// delete the first org's, and cannot see it in its own list.
func TestIsolation_EventTypesAreOrgScoped(t *testing.T) {
	f := newFx(t)
	name := uniq("iso.et")
	orig := f.mkEventType(f.oid, name, []byte(`{"type":"object"}`))
	bUID, bOrg := seedOrg(t, f.q)

	path := "/api/event-types/" + name
	want(t, f.as(bUID, bOrg, http.MethodGet, path, nil), http.StatusNotFound, "not found")
	want(t, f.as(bUID, bOrg, http.MethodPatch, path, map[string]any{"description": "hijack", "archived": true}), http.StatusNotFound, "not found")
	want(t, f.as(bUID, bOrg, http.MethodDelete, path, nil), http.StatusNotFound, "not found")
	rec := f.as(bUID, bOrg, http.MethodGet, "/api/event-types", nil)
	want(t, rec, http.StatusOK, "")
	if strings.Contains(rec.Body.String(), name) {
		t.Fatalf("org B's list leaked %s: %s", name, rec.Body.String())
	}
	got, err := f.q.GetEventTypeForOrg(context.Background(), store.GetEventTypeForOrgParams{OrgID: store.UUID(f.oid), Name: name})
	if err != nil || !reflect.DeepEqual(got, orig) {
		t.Fatalf("event type changed by a foreign org: err=%v got=%+v want=%+v", err, got, orig)
	}
}

// With no principal in the context (a mis-mounted route) every handler refuses
// with 401 instead of touching the database.
func TestEveryHandlerRefusesWithoutAPrincipal(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/p")
	msg := f.mkMsg(f.app, "x")
	a, e, m := uid36(f.app.ID), uid36(ep.ID), uid36(msg.ID)
	for _, c := range []struct{ method, path string }{
		{"GET", "/bare/operational-app"},
		{"GET", "/bare/applications"}, {"POST", "/bare/applications"},
		{"GET", "/bare/applications/" + a}, {"PATCH", "/bare/applications/" + a}, {"DELETE", "/bare/applications/" + a},
		{"POST", "/bare/applications/" + a + "/portal-access"}, {"POST", "/bare/applications/" + a + "/portal-access/revoke"},
		{"GET", "/bare/applications/" + a + "/endpoints"}, {"POST", "/bare/applications/" + a + "/endpoints"},
		{"GET", "/bare/applications/" + a + "/endpoints/" + e}, {"PATCH", "/bare/applications/" + a + "/endpoints/" + e},
		{"DELETE", "/bare/applications/" + a + "/endpoints/" + e},
		{"GET", "/bare/applications/" + a + "/endpoints/" + e + "/secret"},
		{"POST", "/bare/applications/" + a + "/endpoints/" + e + "/rotate-secret"},
		{"POST", "/bare/applications/" + a + "/endpoints/" + e + "/recover"},
		{"POST", "/bare/applications/" + a + "/endpoints/" + e + "/test"},
		{"GET", "/bare/applications/" + a + "/endpoints/" + e + "/attempts"},
		{"GET", "/bare/applications/" + a + "/messages"}, {"POST", "/bare/applications/" + a + "/messages"},
		{"GET", "/bare/applications/" + a + "/messages/" + m},
		{"GET", "/bare/applications/" + a + "/messages/" + m + "/attempts"},
		{"GET", "/bare/applications/" + a + "/messages/" + m + "/deliveries"},
		{"POST", "/bare/applications/" + a + "/messages/" + m + "/endpoints/" + e + "/replay"},
		{"GET", "/bare/event-types"}, {"POST", "/bare/event-types"},
		{"GET", "/bare/event-types/x"}, {"PATCH", "/bare/event-types/x"}, {"DELETE", "/bare/event-types/x"},
	} {
		want(t, f.bare(c.method, c.path), http.StatusUnauthorized, "active org required")
	}
	if f.epCount(f.app) != 1 || f.msgCount(f.app) != 1 {
		t.Fatalf("unauthenticated calls changed state")
	}
}
