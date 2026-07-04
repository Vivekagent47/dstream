package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/queue"
	"github.com/Vivekagent47/dstream/internal/store"
)

const defaultEventsPageSize = 50

func (d Deps) listEvents(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	limit := defaultEventsPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	params := store.ListEventsByOrgParams{
		OrgID:     store.UUID(p.OrgID),
		PageLimit: int32(limit),
	}
	// Keyset cursor: opaque token carrying the previous page's last
	// (created_at, id). Absent on the first page; the query treats a NULL
	// before_created_at as "no lower bound".
	if cur := r.URL.Query().Get("cursor"); cur != "" {
		ts, id, ok := decodeEventCursor(cur)
		if !ok {
			httpErr(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		params.BeforeCreatedAt = pgtype.Timestamptz{Time: ts, Valid: true}
		params.BeforeID = store.UUID(id)
	}

	rows, err := d.Queries.ListEventsByOrg(r.Context(), params)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "list")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, e := range rows {
		out = append(out, eventView(e))
	}
	resp := map[string]any{"events": out}
	// A full page means there may be more; hand back a cursor pointing at the
	// last row. A short page is the end of the stream.
	if len(rows) == limit && limit > 0 {
		last := rows[len(rows)-1]
		resp["next_cursor"] = encodeEventCursor(last.CreatedAt.Time, store.GoUUID(last.ID))
	}
	writeJSON(w, http.StatusOK, resp)
}

// Event cursors are an opaque base64 of "<rfc3339nano>|<uuid>" — the last row
// of the previous page. Opaque so the client treats it as a token; the format
// is purely internal.
func encodeEventCursor(ts time.Time, id uuid.UUID) string {
	raw := ts.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeEventCursor(s string) (time.Time, uuid.UUID, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, false
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, false
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, false
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, false
	}
	return ts, id, true
}

func (d Deps) getEvent(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	ev, err := d.Queries.GetEventForOrg(r.Context(), store.GetEventForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		httpErr(w, http.StatusNotFound, "not found")
		return
	}
	attempts, _ := d.Queries.ListAttemptsByEvent(r.Context(), ev.ID)
	out := eventView(ev)
	out["attempts"] = attemptViews(attempts)
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) retryEvent(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	ev, err := d.Queries.GetEventForOrg(r.Context(), store.GetEventForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		httpErr(w, http.StatusNotFound, "not found")
		return
	}
	if err := d.Queries.ResetEventForManualRetry(r.Context(), ev.ID); err != nil {
		httpErr(w, http.StatusInternalServerError, "reset: "+err.Error())
		return
	}
	conn, err := d.Queries.GetConnectionByID(r.Context(), ev.ConnectionID)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "load connection")
		return
	}
	if _, err := d.Queue.EnqueueDeliver(r.Context(), queue.DeliverPayload{
		EventID:             store.GoUUID(ev.ID),
		Attempt:             0,
		EnqueuedAt:          time.Now().UnixMilli(),
		RetryStrategy:       conn.RetryStrategy,
		RetryBaseMs:         conn.RetryBaseMs,
		RetryCapMs:          conn.RetryCapMs,
		RetryJitterPct:      conn.RetryJitterPct,
		CustomRetrySchedule: conn.CustomRetrySchedule,
	}, int(conn.MaxRetries)); err != nil {
		httpErr(w, http.StatusInternalServerError, "enqueue: "+err.Error())
		return
	}
	evID := store.GoUUID(ev.ID)
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "event.retry",
		TargetType: "event",
		TargetID:   audit.PtrUUID(evID),
		Metadata:   map[string]any{"event_id": evID.String()},
	})
	w.WriteHeader(http.StatusAccepted)
}

func eventView(e store.Event) map[string]any {
	return map[string]any{
		"id":              store.GoUUID(e.ID).String(),
		"request_id":      store.GoUUID(e.RequestID).String(),
		"connection_id":   store.GoUUID(e.ConnectionID).String(),
		"status":          e.Status,
		"attempt_count":   e.AttemptCount,
		"last_attempt_at": tsValue(e.LastAttemptAt.Time, e.LastAttemptAt.Valid),
		"next_retry_at":   tsValue(e.NextRetryAt.Time, e.NextRetryAt.Valid),
		"created_at":      e.CreatedAt.Time,
		"updated_at":      e.UpdatedAt.Time,
	}
}

func attemptViews(rows []store.Attempt) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, map[string]any{
			"id":               store.GoUUID(a.ID).String(),
			"attempt_num":      a.AttemptNum,
			"response_status":  a.ResponseStatus,
			"response_headers": json.RawMessage(a.ResponseHeaders),
			"response_body":    a.ResponseBody,
			"duration_ms":      a.DurationMs,
			"queued_in_ms":     a.QueuedInMs,
			"error_message":    a.ErrorMessage,
			"attempted_at":     a.AttemptedAt.Time,
		})
	}
	return out
}

func tsValue(t time.Time, valid bool) any {
	if !valid {
		return nil
	}
	return t
}
