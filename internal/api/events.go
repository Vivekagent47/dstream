package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/streamingo/dstream/internal/auth"
	"github.com/streamingo/dstream/internal/queue"
	"github.com/streamingo/dstream/internal/store"
)

const defaultEventsPageSize = 50

func (d Deps) listEvents(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.ProjectID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "api key required")
		return
	}
	limit := defaultEventsPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	rows, err := d.Queries.ListEventsByProject(r.Context(), store.ListEventsByProjectParams{
		ProjectID: store.UUID(p.ProjectID),
		Limit:     int32(limit),
		Offset:    int32(offset),
	})
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "list")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, e := range rows {
		out = append(out, eventView(e))
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) getEvent(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	ev, err := d.Queries.GetEventByID(r.Context(), store.UUID(id))
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
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	ev, err := d.Queries.GetEventByID(r.Context(), store.UUID(id))
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
		EventID:    store.GoUUID(ev.ID),
		Attempt:    0,
		EnqueuedAt: time.Now().UnixMilli(),
	}, int(conn.MaxRetries)); err != nil {
		httpErr(w, http.StatusInternalServerError, "enqueue: "+err.Error())
		return
	}
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
