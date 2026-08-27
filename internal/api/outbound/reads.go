package outbound

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

const listLimit = 100

func attemptView(a store.MessageDeliveryAttempt) map[string]any {
	var headers any
	if len(a.ResponseHeaders) > 0 {
		headers = json.RawMessage(a.ResponseHeaders)
	}
	return map[string]any{
		"id":               store.GoUUID(a.ID).String(),
		"delivery_id":      store.GoUUID(a.DeliveryID).String(),
		"attempt_num":      a.AttemptNum,
		"response_status":  httpx.DerefInt32(a.ResponseStatus),
		"response_headers": headers,
		"response_body":    string(a.ResponseBody),
		"duration_ms":      httpx.DerefInt32(a.DurationMs),
		"error_message":    httpx.DerefString(a.ErrorMessage),
		"attempted_at":     a.AttemptedAt.Time,
	}
}

func (d Handlers) ListMessages(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	rows, err := d.Queries.ListMessagesByApp(r.Context(), store.ListMessagesByAppParams{
		AppID: app.ID, Limit: listLimit,
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "list messages")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		out = append(out, map[string]any{
			"id":           store.GoUUID(m.ID).String(),
			"app_id":       store.GoUUID(m.AppID).String(),
			"event_type":   m.EventType,
			"payload_hash": m.PayloadHash,
			"event_id":     httpx.DerefString(m.EventID),
			"created_at":   m.CreatedAt.Time,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (d Handlers) GetMessage(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid message id")
		return
	}
	m, err := d.Queries.GetMessageForApp(r.Context(), store.GetMessageForAppParams{
		ID: store.UUID(id), AppID: app.ID,
	})
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "message not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id":           store.GoUUID(m.ID).String(),
		"app_id":       store.GoUUID(m.AppID).String(),
		"event_type":   m.EventType,
		"payload":      json.RawMessage(m.Payload),
		"payload_hash": m.PayloadHash,
		"event_id":     httpx.DerefString(m.EventID),
		"created_at":   m.CreatedAt.Time,
	})
}

func (d Handlers) ListMessageAttempts(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid message id")
		return
	}
	// Ownership: confirm the message belongs to the caller's app.
	if _, err := d.Queries.GetMessageForApp(r.Context(), store.GetMessageForAppParams{
		ID: store.UUID(id), AppID: app.ID,
	}); err != nil {
		httpx.Err(w, http.StatusNotFound, "message not found")
		return
	}
	rows, err := d.Queries.ListAttemptsByMessage(r.Context(), store.UUID(id))
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "list attempts")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, attemptView(a))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (d Handlers) ListEndpointAttempts(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	ep, ok := d.endpointForApp(w, r, store.GoUUID(app.ID))
	if !ok {
		return
	}
	rows, err := d.Queries.ListAttemptsByEndpoint(r.Context(), store.ListAttemptsByEndpointParams{
		EndpointID: ep.ID, Limit: listLimit,
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "list attempts")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, attemptView(a))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
