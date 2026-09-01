package outbound

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

const listLimit = 100

const pageSize = 50

var maxCursorTime = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
var maxCursorID = uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")

// pageCursor reads ?cursor (opaque "RFC3339Nano|uuid"); absent/invalid → first-page sentinel.
func pageCursor(r *http.Request) (time.Time, uuid.UUID) {
	c := r.URL.Query().Get("cursor")
	if c == "" {
		return maxCursorTime, maxCursorID
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return maxCursorTime, maxCursorID
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return maxCursorTime, maxCursorID
	}
	ts, e1 := time.Parse(time.RFC3339Nano, parts[0])
	id, e2 := uuid.Parse(parts[1])
	if e1 != nil || e2 != nil {
		return maxCursorTime, maxCursorID
	}
	return ts, id
}

func encodeCursor(ts time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(ts.Format(time.RFC3339Nano) + "|" + id.String()))
}

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
	ts, cid := pageCursor(r)
	rows, err := d.Queries.ListMessagesByApp(r.Context(), store.ListMessagesByAppParams{
		AppID: app.ID, CursorTs: pgtype.Timestamptz{Time: ts, Valid: true}, CursorID: store.UUID(cid), Lim: pageSize,
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
	var next any = nil
	if len(rows) == pageSize {
		last := rows[len(rows)-1]
		next = encodeCursor(last.CreatedAt.Time, store.GoUUID(last.ID))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": out, "next_cursor": next})
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
	rows, err := d.Queries.ListAttemptsByMessage(r.Context(), store.ListAttemptsByMessageParams{
		MessageID: store.UUID(id), Lim: listLimit,
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

func (d Handlers) ListMessageDeliveries(w http.ResponseWriter, r *http.Request) {
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
	// ownership: message belongs to this app
	if _, err := d.Queries.GetMessageForApp(r.Context(), store.GetMessageForAppParams{ID: store.UUID(id), AppID: app.ID}); err != nil {
		httpx.Err(w, http.StatusNotFound, "message not found")
		return
	}
	rows, err := d.Queries.ListDeliveriesForMessage(r.Context(), store.UUID(id))
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "list deliveries")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, dl := range rows {
		out = append(out, map[string]any{
			"delivery_id":     store.GoUUID(dl.DeliveryID).String(),
			"endpoint_id":     store.GoUUID(dl.EndpointID).String(),
			"endpoint_url":    dl.EndpointUrl,
			"status":          dl.Status,
			"attempt_count":   dl.AttemptCount,
			"next_retry_at":   nullTime(dl.NextRetryAt),
			"last_attempt_at": nullTime(dl.LastAttemptAt),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// nullTime returns the time or nil for a NULL timestamptz.
func nullTime(t pgtype.Timestamptz) any {
	if !t.Valid {
		return nil
	}
	return t.Time
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
