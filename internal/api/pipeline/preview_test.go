package pipeline

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// preview handlers are stateless (no DB/redis); call them directly.
func doPreview(fn http.HandlerFunc, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	fn(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	return rec
}

func TestFilterPreview(t *testing.T) {
	var m struct {
		Match bool `json:"match"`
	}

	rec := doPreview(FilterPreview, `{"expr":"payload.amount > 100","payload":{"amount":150}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("match: got %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if !m.Match {
		t.Fatal("want match=true")
	}

	rec = doPreview(FilterPreview, `{"expr":"payload.amount > 1000","payload":{"amount":150}}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if rec.Code != http.StatusOK || m.Match {
		t.Fatalf("want match=false, got %d %s", rec.Code, rec.Body.String())
	}

	rec = doPreview(FilterPreview, `{"expr":"payload.x ==","payload":{}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad expr want 400, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestTransformPreview(t *testing.T) {
	rec := doPreview(TransformPreview, `{"js":"function transform(p,h){ p.added = true; return p; }","payload":{"a":1}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid: got %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Result map[string]any `json:"result"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Result["added"] != true {
		t.Fatalf("want mutated result added=true, got %v", out.Result)
	}

	rec = doPreview(TransformPreview, `{"js":"function(","payload":{}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad js want 400, got %d %s", rec.Code, rec.Body.String())
	}
}

func previewErr(t *testing.T, fn http.HandlerFunc, body string) string {
	t.Helper()
	rec := doPreview(fn, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v %s", err, rec.Body.String())
	}
	return m["error"]
}

// jsonStringOfLen is a valid JSON string whose raw length is exactly n bytes.
func jsonStringOfLen(n int) string {
	return `"` + strings.Repeat("a", n-2) + `"`
}

// bigPayload is valid JSON one byte over the preview cap.
func bigPayload() string { return jsonStringOfLen(previewMaxPayload + 1) }

func TestPreviewRejectsBadBodies(t *testing.T) {
	for name, fn := range map[string]http.HandlerFunc{"filter": FilterPreview, "transform": TransformPreview} {
		if got := previewErr(t, fn, `{not json`); got != "invalid json" {
			t.Fatalf("%s bad json: %q", name, got)
		}
		if got := previewErr(t, fn, `{"payload":`+bigPayload()+`}`); got != "payload too large" {
			t.Fatalf("%s big payload: %q", name, got)
		}
	}
}

func TestPreviewPayloadCapBoundary(t *testing.T) {
	atCap := jsonStringOfLen(previewMaxPayload)
	rec := doPreview(FilterPreview, `{"expr":"payload == payload","payload":`+atCap+`}`)
	wantStatus(t, rec, http.StatusOK)
	rec = doPreview(TransformPreview, `{"js":"function transform(p,h){ return {n: p.length}; }","payload":`+atCap+`}`)
	wantStatus(t, rec, http.StatusOK)
}

func TestFilterPreviewVariants(t *testing.T) {
	match := func(body string) bool {
		t.Helper()
		rec := doPreview(FilterPreview, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d %s", body, rec.Code, rec.Body.String())
		}
		var m struct {
			Match bool `json:"match"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m.Match
	}
	// Omitted payload defaults to {}: `payload` is an empty map, not null.
	if !match(`{"expr":"size(payload) == 0"}`) {
		t.Fatal("omitted payload should evaluate against {}")
	}
	if !match(`{"expr":"headers['x-k'] == 'v'","headers":{"x-k":"v"}}`) {
		t.Fatal("headers not bound")
	}
	// event_type / channels exist only on an outbound filter.
	out := `{"expr":"event_type == 'order.paid' && 'a' in channels","outbound":true,"event_type":"order.paid","channels":["a"]}`
	if !match(out) {
		t.Fatal("outbound meta not bound")
	}
	if got := previewErr(t, FilterPreview, strings.Replace(out, `"outbound":true,`, "", 1)); !strings.Contains(got, "undeclared reference to 'event_type'") {
		t.Fatalf("inbound filter may not see event_type: %q", got)
	}
}

func TestFilterPreviewReportsAuthorErrors(t *testing.T) {
	if got := previewErr(t, FilterPreview, `{"expr":"1 + 1"}`); got != "filter expression must evaluate to bool, got int" {
		t.Fatalf("non-bool: %q", got)
	}
	if got := previewErr(t, FilterPreview, `{"expr":"payload.a.b == 1","payload":{}}`); !strings.Contains(got, "no such key: a") {
		t.Fatalf("runtime error: %q", got)
	}
}

func TestTransformPreviewVariants(t *testing.T) {
	ok := func(body string) string {
		t.Helper()
		rec := doPreview(TransformPreview, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d %s", body, rec.Code, rec.Body.String())
		}
		var m struct {
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return string(m.Result)
	}
	// Omitted payload defaults to {}; headers reach the script; the result is
	// embedded raw (an array stays an array).
	if got := ok(`{"js":"function transform(p,h){ p.k = h['x-k']; return p; }","headers":{"x-k":"v"}}`); got != `{"k":"v"}` {
		t.Fatalf("defaulted payload + headers: %s", got)
	}
	if got := ok(`{"js":"function transform(p,h){ return [1,2]; }"}`); got != `[1,2]` {
		t.Fatalf("array result: %s", got)
	}

	if got := previewErr(t, TransformPreview, `{"js":"function transform(p,h){ throw new Error('boom'); }"}`); !strings.HasPrefix(got, "transform run:") || !strings.Contains(got, "boom") {
		t.Fatalf("throwing transform: %q", got)
	}
	if got := previewErr(t, TransformPreview, `{"js":"function transform(p,h){ return 5; }"}`); got != "transform must return an object or array, got int64" {
		t.Fatalf("non-object result: %q", got)
	}
	if got := previewErr(t, TransformPreview, `{"js":"var x = 1;"}`); got != "transform: no function named transform(payload, headers)" {
		t.Fatalf("missing function: %q", got)
	}
}
