package outbound

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/filter"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/transform"
)

const selfHost = "dstream-self.example.test"

func withSelfHost(h *Handlers) { h.SelfHosts = []string{selfHost} }

func compileErrs(t *testing.T) (filterMsg, jsMsg string) {
	t.Helper()
	_, err := filter.Compile("payload.x ==", true)
	if err == nil {
		t.Fatal("expected filter compile error")
	}
	_, jerr := transform.Compile("function(")
	if jerr == nil {
		t.Fatal("expected transform compile error")
	}
	return err.Error(), jerr.Error()
}

func TestEndpoint_CreateStoresEveryField(t *testing.T) {
	f := newFx(t)
	uid := uniq("ep")
	rec := f.own(http.MethodPost, appPath(f.app)+"/endpoints", map[string]any{
		"uid": uid, "url": "https://ex.test/all", "description": "d",
		"filter_event_types": []string{"a.b", "c.d"}, "headers": map[string]string{"X-Team": "core"},
		"rate_limit": 7, "channels": []string{"acme"}, "filter_expr": `event_type == "a.b"`, "transform_js": "function transform(p,h){ return p }",
	})
	want(t, rec, http.StatusCreated, "")
	id := obj(t, rec)["id"].(string)
	row := f.epRow(f.app, store.UUID(uuid.MustParse(id)))
	if row.Url != "https://ex.test/all" || row.Description != "d" || *row.Uid != uid ||
		!reflect.DeepEqual(row.FilterEventTypes, []string{"a.b", "c.d"}) || string(row.Headers) != `{"X-Team": "core"}` ||
		row.RateLimit == nil || *row.RateLimit != 7 || !reflect.DeepEqual(row.Channels, []string{"acme"}) ||
		row.FilterExpr == nil || row.TransformJs == nil || row.Disabled || row.ConsecutiveFailures != 0 {
		t.Fatalf("stored endpoint: %+v", row)
	}
	v := obj(t, f.own(http.MethodGet, appPath(f.app)+"/endpoints/"+id, nil))
	if v["uid"] != uid || v["rate_limit"] != float64(7) || v["disabled"] != false || v["disabled_at"] != nil {
		t.Fatalf("view: %v", v)
	}
	// Same uid in the same app is a 409; the same uid in the sibling app is fine.
	want(t, f.own(http.MethodPost, appPath(f.app)+"/endpoints", map[string]any{"uid": uid, "url": "https://ex.test/dup"}), http.StatusConflict, "endpoint uid already in use")
	if n := f.epCount(f.app); n != 1 {
		t.Fatalf("duplicate uid created a row, have %d", n)
	}
	want(t, f.own(http.MethodPost, appPath(f.app2)+"/endpoints", map[string]any{"uid": uid, "url": "https://ex.test/dup"}), http.StatusCreated, "")
}

func TestEndpoint_CreateRefusals(t *testing.T) {
	filterMsg, jsMsg := compileErrs(t)
	f := newFx(t, withHandlers(withSelfHost))
	base := appPath(f.app) + "/endpoints"
	tooMany := map[string]string{}
	for i := 0; i < maxHeaders+1; i++ {
		tooMany["X-H-"+string(rune('a'+i))] = "v"
	}
	for _, c := range []struct {
		name string
		body any
		msg  string
	}{
		{"no body", nil, "invalid json"},
		{"scheme", map[string]any{"url": "ftp://ex.test/x"}, `invalid url: url scheme must be http or https, got "ftp"`},
		{"no host", map[string]any{"url": "https:///x"}, "invalid url: url must have a host"},
		{"loop guard", map[string]any{"url": "https://" + selfHost + "/hook"}, "url must not point at dstream itself (loop guard)"},
		{"reserved header", map[string]any{"url": "https://ex.test/x", "headers": map[string]string{"Webhook-Signature": "x"}}, `header "Webhook-Signature" is reserved`},
		{"empty header name", map[string]any{"url": "https://ex.test/x", "headers": map[string]string{"": "x"}}, `header name "" is not a valid token`},
		{"separator in header name", map[string]any{"url": "https://ex.test/x", "headers": map[string]string{"X:Bad": "x"}}, `header name "X:Bad" is not a valid token`},
		{"bad header name", map[string]any{"url": "https://ex.test/x", "headers": map[string]string{"bad name": "x"}}, `header name "bad name" is not a valid token`},
		{"too many headers", map[string]any{"url": "https://ex.test/x", "headers": tooMany}, "too many headers: 21 (max 20)"},
		{"rate negative", map[string]any{"url": "https://ex.test/x", "rate_limit": -1}, "rate_limit must be between 0 and 1000000"},
		{"rate huge", map[string]any{"url": "https://ex.test/x", "rate_limit": maxRateLimit + 1}, "rate_limit must be between 0 and 1000000"},
		{"channel charset", map[string]any{"url": "https://ex.test/x", "channels": []string{"no spaces"}}, `invalid channel "no spaces"`},
		{"too many channels", map[string]any{"url": "https://ex.test/x", "channels": make([]string, maxChannels+1)}, "too many channels: 11 (max 10)"},
		{"filter", map[string]any{"url": "https://ex.test/x", "filter_expr": "payload.x =="}, filterMsg},
		{"transform", map[string]any{"url": "https://ex.test/x", "transform_js": "function("}, jsMsg},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			want(t, f.own(http.MethodPost, base, c.body), http.StatusBadRequest, c.msg)
			if n := f.epCount(f.app); n != 0 {
				t.Fatalf("refused create stored %d endpoints", n)
			}
		})
	}
	want(t, f.own(http.MethodPost, "/api/applications/not-a-uuid/endpoints", map[string]any{"url": "https://ex.test/x"}), http.StatusBadRequest, "invalid app id")
	want(t, f.own(http.MethodPost, "/api/applications/"+uuid.NewString()+"/endpoints", map[string]any{"url": "https://ex.test/x"}), http.StatusNotFound, "application not found")
}

func TestEndpoint_DatabaseFailuresAre500(t *testing.T) {
	f := newFx(t, withQueries(tracedQueries(t, failAll("insert into endpoints"))))
	want(t, f.own(http.MethodPost, appPath(f.app)+"/endpoints", map[string]any{"url": "https://ex.test/x"}), http.StatusInternalServerError, "create endpoint")
	if f.epCount(f.app) != 0 {
		t.Fatalf("failed create left a row")
	}
	for _, c := range []struct {
		name, marker, method, suffix string
		body                         any
		msg                          string
	}{
		{"list", "from endpoints where app_id = $1 order by", "GET", "", nil, "list endpoints"},
		{"patch", "update endpoints", "PATCH", "/%s", map[string]any{"description": "x"}, "update endpoint"},
		{"delete", "delete from endpoints", "DELETE", "/%s", nil, "delete endpoint"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			g := newFx(t, withQueries(tracedQueries(t, failAll(c.marker))))
			ep := g.mkEp(g.app, "https://ex.test/keep")
			before := g.epRow(g.app, ep.ID)
			path := appPath(g.app) + "/endpoints" + strings.Replace(c.suffix, "%s", uid36(ep.ID), 1)
			want(t, g.own(c.method, path, c.body), http.StatusInternalServerError, c.msg)
			if after := g.epRow(g.app, ep.ID); !reflect.DeepEqual(before, after) {
				t.Fatalf("failed %s changed the row", c.name)
			}
		})
	}
}

func TestEndpoint_ListAndGet(t *testing.T) {
	f := newFx(t)
	a := f.mkEp(f.app, "https://ex.test/a")
	b := f.mkEp(f.app, "https://ex.test/b")
	f.mkEp(f.app2, "https://ex.test/sibling")
	rec := f.own(http.MethodGet, appPath(f.app)+"/endpoints", nil)
	want(t, rec, http.StatusOK, "")
	got := list(t, rec)
	if len(got) != 2 || got[0]["id"] != uid36(b.ID) || got[1]["id"] != uid36(a.ID) {
		t.Fatalf("list must be this app's endpoints newest first, got %v", got)
	}
	want(t, f.own(http.MethodGet, "/api/applications/not-a-uuid/endpoints", nil), http.StatusBadRequest, "invalid app id")
	want(t, f.own(http.MethodGet, appPath(f.app)+"/endpoints/not-a-uuid", nil), http.StatusBadRequest, "invalid endpoint id")
	want(t, f.own(http.MethodGet, appPath(f.app)+"/endpoints/"+uuid.NewString(), nil), http.StatusNotFound, "endpoint not found")
	one := obj(t, f.own(http.MethodGet, epPath(f.app, a), nil))
	if one["id"] != uid36(a.ID) || one["app_id"] != uid36(f.app.ID) || one["url"] != "https://ex.test/a" {
		t.Fatalf("get view: %v", one)
	}
}

func TestEndpoint_PatchPersistsEachFieldAndClearsOptionals(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/old", func(p *store.CreateEndpointParams) {
		fe, tj := `event_type == "x"`, "function transform(p,h){ return p }"
		p.FilterExpr, p.TransformJs = &fe, &tj
		p.FilterEventTypes = []string{"keep.me"}
		p.Channels = []string{"keep"}
	})
	path := epPath(f.app, ep)

	rec := f.own(http.MethodPatch, path, map[string]any{
		"url": "https://ex.test/new", "description": "dd", "disabled": true,
		"filter_event_types": []string{"n.1"}, "headers": map[string]string{"X-A": "b"},
		"rate_limit": 9, "channels": []string{"c1"}, "filter_expr": `event_type == "n.1"`, "transform_js": "function transform(p,h){ return p }",
	})
	want(t, rec, http.StatusOK, "")
	row := f.epRow(f.app, ep.ID)
	if row.Url != "https://ex.test/new" || row.Description != "dd" || !row.Disabled ||
		!reflect.DeepEqual(row.FilterEventTypes, []string{"n.1"}) || string(row.Headers) != `{"X-A": "b"}` ||
		*row.RateLimit != 9 || !reflect.DeepEqual(row.Channels, []string{"c1"}) || *row.FilterExpr != `event_type == "n.1"` {
		t.Fatalf("patched row: %+v", row)
	}
	// Explicit empties clear; absent fields stay (filter/channels/expr/js).
	want(t, f.own(http.MethodPatch, path, map[string]any{"filter_event_types": []string{}, "channels": []string{}, "filter_expr": "", "transform_js": ""}), http.StatusOK, "")
	row = f.epRow(f.app, ep.ID)
	if len(row.FilterEventTypes) != 0 || len(row.Channels) != 0 || row.FilterExpr != nil || row.TransformJs != nil {
		t.Fatalf("empties must clear: %+v", row)
	}
	if row.Url != "https://ex.test/new" || row.Description != "dd" || string(row.Headers) != `{"X-A": "b"}` {
		t.Fatalf("a PATCH without those fields must leave them: %+v", row)
	}
}

func TestEndpoint_PatchRefusalsChangeNothing(t *testing.T) {
	filterMsg, jsMsg := compileErrs(t)
	f := newFx(t, withHandlers(withSelfHost))
	ep := f.mkEp(f.app, "https://ex.test/keep")
	before := f.epRow(f.app, ep.ID)
	path := epPath(f.app, ep)
	for _, c := range []struct {
		name string
		body any
		msg  string
	}{
		{"no body", nil, "invalid json"},
		{"scheme", map[string]any{"url": "gopher://ex.test"}, `invalid url: url scheme must be http or https, got "gopher"`},
		{"loop guard", map[string]any{"url": "https://" + selfHost + "/x"}, "url must not point at dstream itself (loop guard)"},
		{"reserved header", map[string]any{"headers": map[string]string{"dstream-x": "1"}}, `header "dstream-x" is reserved`},
		{"rate", map[string]any{"rate_limit": maxRateLimit + 1}, "rate_limit must be between 0 and 1000000"},
		{"channel", map[string]any{"channels": []string{"bad channel"}}, `invalid channel "bad channel"`},
		{"filter", map[string]any{"filter_expr": "payload.x =="}, filterMsg},
		{"transform", map[string]any{"transform_js": "function("}, jsMsg},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			want(t, f.own(http.MethodPatch, path, c.body), http.StatusBadRequest, c.msg)
			if after := f.epRow(f.app, ep.ID); !reflect.DeepEqual(before, after) {
				t.Fatalf("refused patch changed the row")
			}
		})
	}
	want(t, f.own(http.MethodPatch, appPath(f.app)+"/endpoints/not-a-uuid", map[string]any{"description": "x"}), http.StatusBadRequest, "invalid endpoint id")
	want(t, f.own(http.MethodPatch, appPath(f.app)+"/endpoints/"+uuid.NewString(), map[string]any{"description": "x"}), http.StatusNotFound, "not found")
	want(t, f.own(http.MethodPatch, appPath(f.app)+"/endpoints/"+uuid.NewString(), map[string]any{"url": "https://ex.test/y"}), http.StatusNotFound, "not found")
}

func TestEndpoint_Delete(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/gone")
	keep := f.mkEp(f.app, "https://ex.test/stay")
	msg := f.mkMsg(f.app, "x")
	f.mkDel(msg, ep)
	want(t, f.own(http.MethodDelete, appPath(f.app)+"/endpoints/not-a-uuid", nil), http.StatusBadRequest, "invalid endpoint id")
	want(t, f.own(http.MethodDelete, appPath(f.app)+"/endpoints/"+uuid.NewString(), nil), http.StatusNotFound, "not found")
	want(t, f.own(http.MethodDelete, epPath(f.app, ep), nil), http.StatusNoContent, "")
	if _, err := f.q.GetEndpointForApp(context.Background(), store.GetEndpointForAppParams{ID: ep.ID, AppID: f.app.ID}); err == nil {
		t.Fatalf("endpoint survived delete")
	}
	if len(f.deliveries(msg)) != 0 {
		t.Fatalf("deliveries of a deleted endpoint survived")
	}
	f.epRow(f.app, keep.ID) // sibling endpoint untouched
}

// A row whose headers column is not an object (written by something else) is
// shown as {} rather than failing the whole read.
func TestEndpoint_MalformedStoredHeadersRenderEmpty(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/h")
	if _, err := f.pool.Exec(context.Background(), `UPDATE endpoints SET headers='[1,2]'::jsonb WHERE id=$1`, ep.ID); err != nil {
		t.Fatal(err)
	}
	rec := f.own(http.MethodGet, epPath(f.app, ep), nil)
	want(t, rec, http.StatusOK, "")
	if h, ok := obj(t, rec)["headers"].(map[string]any); !ok || len(h) != 0 {
		t.Fatalf("malformed headers must render as {}, got %v", obj(t, rec)["headers"])
	}
}

func TestHeadersMap_NilIsEmptyNotNil(t *testing.T) {
	if m := headersMap(nil); m == nil || len(m) != 0 {
		t.Fatalf("headersMap(nil) must be an empty non-nil map, got %#v", m)
	}
}
