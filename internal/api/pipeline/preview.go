package pipeline

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/filter"
	"github.com/Vivekagent47/dstream/internal/transform"
)

// previewMaxPayload caps preview request payloads. These endpoints are authed
// dev aids, but the payload is still untrusted input — reject an oversized body
// rather than compile/eval against it.
const previewMaxPayload = 262144 // 256 KiB

type filterPreviewReq struct {
	Expr      string            `json:"expr"`
	Payload   json.RawMessage   `json:"payload"`
	Headers   map[string]string `json:"headers,omitempty"`
	Outbound  bool              `json:"outbound,omitempty"`
	EventType string            `json:"event_type,omitempty"`
	Channels  []string          `json:"channels,omitempty"`
}

// FilterPreview compiles + evaluates a CEL filter against a sample payload and
// reports whether it matches. Stateless (no receiver): registered verbatim
// under both the session and portal authed route groups. Unlike delivery
// (fail-open), a preview compile/eval error is reported as 400 so the author
// sees it.
func FilterPreview(w http.ResponseWriter, r *http.Request) {
	var body filterPreviewReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if len(body.Payload) > previewMaxPayload {
		httpx.Err(w, http.StatusBadRequest, "payload too large")
		return
	}
	payload := body.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	match, err := filter.Match(body.Expr, body.Outbound, payload, body.Headers, filter.Meta{
		EventType: body.EventType, Channels: body.Channels,
	})
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"match": match})
}

type transformPreviewReq struct {
	JS      string            `json:"js"`
	Payload json.RawMessage   `json:"payload"`
	Headers map[string]string `json:"headers,omitempty"`
}

// TransformPreview runs a transform script against a sample payload and returns
// the transformed JSON. Uses the delivery defaults (1s / 5 MiB) — hardcoded, as
// this is a dev-time aid, not the delivery path.
func TransformPreview(w http.ResponseWriter, r *http.Request) {
	var body transformPreviewReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if len(body.Payload) > previewMaxPayload {
		httpx.Err(w, http.StatusBadRequest, "payload too large")
		return
	}
	payload := body.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	out, err := transform.Apply(body.JS, payload, body.Headers, time.Second, 5<<20)
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, err.Error())
		return
	}
	// json.RawMessage so the transformed JSON is embedded raw, not re-encoded.
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"result": json.RawMessage(out)})
}
