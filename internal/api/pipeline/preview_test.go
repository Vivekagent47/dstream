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
