package pipeline

import (
	"encoding/base64"
	"encoding/json"
	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

const defaultEventsPageSize = 50

func (d Handlers) ListEvents(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	limit := defaultEventsPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	params := store.ListEventsParams{
		OrgID:     store.UUID(p.OrgID),
		PageLimit: int32(limit),
	}

	connID, status, after, ok := parseEventFilters(w, r)
	if !ok {
		return
	}
	params.ConnectionID = connID
	params.Status = status
	params.After = after

	if cur := r.URL.Query().Get("cursor"); cur != "" {
		ts, id, ok := decodeEventCursor(cur)
		if !ok {
			httpx.Err(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		params.BeforeCreatedAt = pgtype.Timestamptz{Time: ts, Valid: true}
		params.BeforeID = store.UUID(id)
	}

	rows, err := d.Queries.ListEvents(r.Context(), params)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "list")
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
	httpx.WriteJSON(w, http.StatusOK, resp)
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

// parseEventFilters reads the connection_id / status / after query params
// shared by the list and histogram endpoints. On a bad value it writes the
// error response and returns ok=false. All three are optional; absent params
// come back zero-valued (Valid:false / nil), which the queries treat as "no
// filter".
func parseEventFilters(w http.ResponseWriter, r *http.Request) (connID pgtype.UUID, status *string, after pgtype.Timestamptz, ok bool) {
	// A present-but-zero UUID still filters (matches nothing). pgtype.UUID
	// directly (not store.UUID) so a zero UUID keeps Valid:true — store.UUID
	// collapses uuid.Nil to Valid:false, which the narg guard reads as "no filter".
	if v := r.URL.Query().Get("connection_id"); v != "" {
		id, perr := uuid.Parse(v)
		if perr != nil {
			httpx.Err(w, http.StatusBadRequest, "invalid connection_id")
			return
		}
		connID = pgtype.UUID{Bytes: id, Valid: true}
	}
	if v := r.URL.Query().Get("status"); v != "" {
		switch v {
		case "queued", "in_flight", "delivered", "failed", "paused", "dead", "discarded":
			s := v
			status = &s
		default:
			httpx.Err(w, http.StatusBadRequest, "invalid status")
			return
		}
	}
	if v := r.URL.Query().Get("after"); v != "" {
		ts, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			httpx.Err(w, http.StatusBadRequest, "invalid after")
			return
		}
		after = pgtype.Timestamptz{Time: ts, Valid: true}
	}
	ok = true
	return
}

// EventsHistogram returns event counts bucketed over time for the events-page
// timeline graph. Same filters as ListEvents; @after defaults to the last 24h
// when absent (the graph is always over a finite window).
func (d Handlers) EventsHistogram(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	connID, status, after, ok := parseEventFilters(w, r)
	if !ok {
		return
	}
	if !after.Valid {
		after = pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true}
	}
	bucket := r.URL.Query().Get("bucket")
	switch bucket {
	case "minute", "hour", "day", "week":
	default:
		bucket = "hour"
	}
	// Cap the bucket count so a hand-crafted far-past `after` can't make
	// generate_series emit millions of rows (2026-07-21 audit #2).
	after = clampAfter(bucket, after)

	rows, err := d.Queries.EventsHistogram(r.Context(), store.EventsHistogramParams{
		Bucket:       bucket,
		OrgID:        store.UUID(p.OrgID),
		ConnectionID: connID,
		Status:       status,
		After:        after,
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "histogram")
		return
	}

	type bucketOut struct {
		Ts     string           `json:"ts"`
		Counts map[string]int64 `json:"counts"`
		Total  int64            `json:"total"`
	}
	// The query gap-fills the series, so every bucket in the window is present;
	// an empty bucket arrives as one row with a NULL status (count 0). Create the
	// bucket regardless, but only tally rows that carry a status.
	order := make([]string, 0)
	idx := make(map[string]*bucketOut)
	for _, row := range rows {
		key := row.Bucket.Time.UTC().Format(time.RFC3339)
		b := idx[key]
		if b == nil {
			b = &bucketOut{Ts: key, Counts: map[string]int64{}}
			idx[key] = b
			order = append(order, key)
		}
		if row.Status != nil {
			b.Counts[*row.Status] += row.Count
			b.Total += row.Count
		}
	}
	buckets := make([]*bucketOut, 0, len(order))
	for _, k := range order {
		buckets = append(buckets, idx[k])
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"bucket": bucket, "buckets": buckets})
}

func (d Handlers) GetEvent(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid id")
		return
	}
	row, err := d.Queries.GetEventDetailForOrg(r.Context(), store.GetEventDetailForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "not found")
		return
	}
	attempts, _ := d.Queries.ListAttemptsByEvent(r.Context(), row.ID)

	// Body is the raw stored payload. Best-effort — a missing/unreadable body
	// shouldn't 500 the whole detail view, so on error we just omit it.
	var body string
	if b, berr := d.BodyStore.Get(r.Context(), row.BodyRef); berr == nil {
		body = string(b)
	}

	out := map[string]any{
		"id":              store.GoUUID(row.ID).String(),
		"request_id":      store.GoUUID(row.RequestID).String(),
		"connection_id":   store.GoUUID(row.ConnectionID).String(),
		"source_id":       store.GoUUID(row.SourceID).String(),
		"destination_id":  store.GoUUID(row.DestinationID).String(),
		"status":          row.Status,
		"attempt_count":   row.AttemptCount,
		"last_attempt_at": tsValue(row.LastAttemptAt.Time, row.LastAttemptAt.Valid),
		"next_retry_at":   tsValue(row.NextRetryAt.Time, row.NextRetryAt.Valid),
		"created_at":      row.CreatedAt.Time,
		"updated_at":      row.UpdatedAt.Time,
		"is_test":         row.IsTest,
		"attempts":        attemptViews(attempts),
		"destination": map[string]any{
			"type": row.DestinationType,
			"url":  row.DestinationUrl,
		},
		"request": map[string]any{
			"method":       row.HTTPMethod,
			"path":         row.HTTPPath,
			"headers":      json.RawMessage(row.RequestHeaders),
			"body":         body,
			"body_size":    row.BodySize,
			"content_type": row.ContentType,
		},
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (d Handlers) RetryEvent(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid id")
		return
	}
	ev, err := d.Queries.GetEventForOrg(r.Context(), store.GetEventForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "not found")
		return
	}
	if err := d.Queries.ResetEventForManualRetry(r.Context(), ev.ID); err != nil {
		d.Log.Error("retry event: reset", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "retry failed")
		return
	}
	conn, err := d.Queries.GetConnectionByID(r.Context(), ev.ConnectionID)
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "load connection")
		return
	}
	if err := d.Queue.Enqueue(r.Context(), dqueue.Payload{
		EventID:             store.GoUUID(ev.ID),
		OrgID:               p.OrgID,
		Attempt:             0,
		EnqueuedAt:          time.Now().UnixMilli(),
		Manual:              true,
		RetryStrategy:       conn.RetryStrategy,
		RetryBaseMs:         conn.RetryBaseMs,
		RetryCapMs:          conn.RetryCapMs,
		RetryJitterPct:      conn.RetryJitterPct,
		CustomRetrySchedule: conn.CustomRetrySchedule,
	}); err != nil {
		d.Log.Error("retry event: enqueue", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "retry failed")
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
		"is_test":         e.IsTest,
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
