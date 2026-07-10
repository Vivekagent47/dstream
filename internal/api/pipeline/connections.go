package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Vivekagent47/dstream/internal/api/httpx"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/queue"
	"github.com/Vivekagent47/dstream/internal/store"
)

type createConnectionReq struct {
	SourceID      uuid.UUID `json:"source_id"`
	DestinationID uuid.UUID `json:"destination_id"`
	Enabled       *bool     `json:"enabled,omitempty"`
	Name          *string   `json:"name,omitempty"`
}

func (d Handlers) CreateConnection(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	var body createConnectionReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.SourceID == uuid.Nil || body.DestinationID == uuid.Nil {
		httpx.Err(w, http.StatusBadRequest, "source_id and destination_id required")
		return
	}
	// Verify both source and destination belong to the caller's org.
	if _, err := d.Queries.GetSourceForOrg(r.Context(), store.GetSourceForOrgParams{
		ID:    store.UUID(body.SourceID),
		OrgID: store.UUID(p.OrgID),
	}); err != nil {
		httpx.Err(w, http.StatusBadRequest, "source not found in this org")
		return
	}
	if _, err := d.Queries.GetDestinationForOrg(r.Context(), store.GetDestinationForOrgParams{
		ID:    store.UUID(body.DestinationID),
		OrgID: store.UUID(p.OrgID),
	}); err != nil {
		httpx.Err(w, http.StatusBadRequest, "destination not found in this org")
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	row, err := d.Queries.CreateConnection(r.Context(), store.CreateConnectionParams{
		SourceID:      store.UUID(body.SourceID),
		DestinationID: store.UUID(body.DestinationID),
		Enabled:       enabled,
		Name:          body.Name,
	})
	if err != nil {
		d.Log.Error("create connection", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "connection.create",
		TargetType: "connection",
		TargetID:   audit.PtrUUID(store.GoUUID(row.ID)),
		Metadata: map[string]any{
			"source_id":      store.GoUUID(row.SourceID).String(),
			"destination_id": store.GoUUID(row.DestinationID).String(),
		},
	})
	httpx.WriteJSON(w, http.StatusCreated, connectionView(row))
}

func (d Handlers) ListConnections(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	rows, err := d.Queries.ListConnectionsByOrg(r.Context(), store.UUID(p.OrgID))
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "list")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, connectionView(row))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (d Handlers) GetConnection(w http.ResponseWriter, r *http.Request) {
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
	row, err := d.Queries.GetConnectionForOrg(r.Context(), store.GetConnectionForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, connectionView(row))
}

// ConnectionStats returns per-status delivery counts for a connection over the
// last 24h, excluding test events. Feeds the detail-page overview cards.
func (d Handlers) ConnectionStats(w http.ResponseWriter, r *http.Request) {
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
	// Ownership check: 404 if the connection isn't in the caller's org.
	if _, err := d.Queries.GetConnectionForOrg(r.Context(), store.GetConnectionForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	}); err != nil {
		httpx.Err(w, http.StatusNotFound, "not found")
		return
	}
	rows, err := d.Queries.CountEventsByConnectionSince(r.Context(), store.CountEventsByConnectionSinceParams{
		ConnectionID: store.UUID(id),
		OrgID:        store.UUID(p.OrgID),
		Since:        pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true},
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "stats")
		return
	}
	var delivered, failed, pending, total int64
	for _, row := range rows {
		total += row.Count
		switch row.Status {
		case "delivered":
			delivered += row.Count
		case "failed", "dead":
			failed += row.Count
		case "queued", "in_flight":
			pending += row.Count
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"delivered": delivered,
		"failed":    failed,
		"pending":   pending,
		"total":     total,
		"window":    "24h",
	})
}

// AllConnectionStats returns 24h per-connection delivery counts for the whole
// org in one call, keyed by connection id. Feeds the connections-list stat
// column without an N+1. Test events excluded.
func (d Handlers) AllConnectionStats(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	rows, err := d.Queries.CountEventsByOrgGroupedByConnection(r.Context(), store.CountEventsByOrgGroupedByConnectionParams{
		OrgID: store.UUID(p.OrgID),
		Since: pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true},
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "stats")
		return
	}
	type bucket struct {
		Delivered int64 `json:"delivered"`
		Failed    int64 `json:"failed"`
		Pending   int64 `json:"pending"`
		Total     int64 `json:"total"`
	}
	out := map[string]*bucket{}
	for _, row := range rows {
		cid := store.GoUUID(row.ConnectionID).String()
		b := out[cid]
		if b == nil {
			b = &bucket{}
			out[cid] = b
		}
		b.Total += row.Count
		switch row.Status {
		case "delivered":
			b.Delivered += row.Count
		case "failed", "dead":
			b.Failed += row.Count
		case "queued", "in_flight":
			b.Pending += row.Count
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

type patchConnectionReq struct {
	Enabled             *bool           `json:"enabled,omitempty"`
	Name                *string         `json:"name,omitempty"`
	MaxRetries          *int32          `json:"max_retries,omitempty"`
	RetryStrategy       *string         `json:"retry_strategy,omitempty"`
	RetryBaseMs         *int32          `json:"retry_base_ms,omitempty"`
	RetryCapMs          *int32          `json:"retry_cap_ms,omitempty"`
	RetryJitterPct      *int32          `json:"retry_jitter_pct,omitempty"`
	CustomRetrySchedule json.RawMessage `json:"custom_retry_schedule,omitempty"`
}

func (d Handlers) PatchConnection(w http.ResponseWriter, r *http.Request) {
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
	var body patchConnectionReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.RetryStrategy != nil {
		s := *body.RetryStrategy
		if s != "exponential" && s != "linear" && s != "fixed" && s != "custom" {
			httpx.Err(w, http.StatusBadRequest, "invalid retry_strategy")
			return
		}
	}
	old, err := d.Queries.GetConnectionForOrg(r.Context(), store.GetConnectionForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "not found")
		return
	}
	params := store.PatchConnectionForOrgParams{
		ID:             store.UUID(id),
		OrgID:          store.UUID(p.OrgID),
		Enabled:        body.Enabled,
		Name:           body.Name,
		MaxRetries:     body.MaxRetries,
		RetryStrategy:  body.RetryStrategy,
		RetryBaseMs:    body.RetryBaseMs,
		RetryCapMs:     body.RetryCapMs,
		RetryJitterPct: body.RetryJitterPct,
	}
	if len(body.CustomRetrySchedule) > 0 {
		params.CustomRetrySchedule = body.CustomRetrySchedule
	}
	row, err := d.Queries.PatchConnectionForOrg(r.Context(), params)
	if err != nil {
		d.Log.Error("patch connection", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "update")
		return
	}
	if changed := diffConnection(old, row); len(changed) > 0 {
		audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
			Action:     "connection.update",
			TargetType: "connection",
			TargetID:   audit.PtrUUID(store.GoUUID(row.ID)),
			Metadata:   map[string]any{"changed": changed},
		})
	}
	httpx.WriteJSON(w, http.StatusOK, connectionView(row))
}

func (d Handlers) DeleteConnection(w http.ResponseWriter, r *http.Request) {
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
	if err := d.Queries.DeleteConnectionForOrg(r.Context(), store.DeleteConnectionForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	}); err != nil {
		d.Log.Error("delete connection", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "delete")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "connection.delete",
		TargetType: "connection",
		TargetID:   audit.PtrUUID(id),
		Metadata:   map[string]any{},
	})
	w.WriteHeader(http.StatusNoContent)
}

// diffConnection produces the changed-fields map for an audit entry. Returns
// an empty map if nothing changed.
func diffConnection(old, new store.Connection) map[string]map[string]any {
	out := map[string]map[string]any{}
	if old.Enabled != new.Enabled {
		out["enabled"] = map[string]any{"from": old.Enabled, "to": new.Enabled}
	}
	if old.MaxRetries != new.MaxRetries {
		out["max_retries"] = map[string]any{"from": old.MaxRetries, "to": new.MaxRetries}
	}
	if old.RetryStrategy != new.RetryStrategy {
		out["retry_strategy"] = map[string]any{"from": old.RetryStrategy, "to": new.RetryStrategy}
	}
	if old.RetryBaseMs != new.RetryBaseMs {
		out["retry_base_ms"] = map[string]any{"from": old.RetryBaseMs, "to": new.RetryBaseMs}
	}
	if old.RetryCapMs != new.RetryCapMs {
		out["retry_cap_ms"] = map[string]any{"from": old.RetryCapMs, "to": new.RetryCapMs}
	}
	if old.RetryJitterPct != new.RetryJitterPct {
		out["retry_jitter_pct"] = map[string]any{"from": old.RetryJitterPct, "to": new.RetryJitterPct}
	}
	if !bytes.Equal(old.CustomRetrySchedule, new.CustomRetrySchedule) {
		out["custom_retry_schedule"] = map[string]any{"changed": true}
	}
	return out
}

// TestConnection sends a synthetic event through the real delivery pipeline
// for one connection, so a user can confirm the destination works before real
// traffic. Reuses the ingest path: create request + body + one is_test event,
// then enqueue delivery. The event (and its attempt) show up in the Events tab.
func (d Handlers) TestConnection(w http.ResponseWriter, r *http.Request) {
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
	conn, err := d.Queries.GetConnectionForOrg(r.Context(), store.GetConnectionForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "not found")
		return
	}

	reqID := uuid.Must(uuid.NewV7())
	body := []byte(fmt.Sprintf(`{"dstream":"test.ping","connection_id":%q,"sent_at":%q}`,
		id.String(), time.Now().UTC().Format(time.RFC3339)))
	sum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(sum[:])

	req, err := d.Queries.CreateRequest(r.Context(), store.CreateRequestParams{
		ID:          store.UUID(reqID),
		SourceID:    conn.SourceID,
		HTTPMethod:  http.MethodPost,
		HTTPPath:    "/__dstream_test",
		Headers:     []byte(`{"Content-Type":["application/json"],"Dstream-Test":["true"]}`),
		BodyHash:    bodyHash,
		BodyRef:     "pg:" + reqID.String(),
		BodySize:    int32(len(body)),
		ContentType: optStr("application/json"),
		SigVerified: false,
		IngestIP:    nil,
	})
	if err != nil {
		d.Log.Error("test connection: create request", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create request")
		return
	}
	if _, err := d.BodyStore.Put(r.Context(), store.GoUUID(req.ID), body); err != nil {
		d.Log.Error("test connection: store body", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "store body")
		return
	}

	events, err := d.Queries.CreateEventsBatch(r.Context(), store.CreateEventsBatchParams{
		RequestID:     req.ID,
		OrgID:         store.UUID(p.OrgID),
		ConnectionIds: []pgtype.UUID{conn.ID},
		IsTest:        true,
	})
	if err != nil || len(events) != 1 {
		d.Log.Error("test connection: create event", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create event")
		return
	}
	ev := events[0]

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
		httpx.Err(w, http.StatusInternalServerError, "enqueue")
		return
	}

	evID := store.GoUUID(ev.ID)
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "connection.test",
		TargetType: "connection",
		TargetID:   audit.PtrUUID(id),
		Metadata:   map[string]any{"event_id": evID.String()},
	})
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"event_id": evID.String()})
}

// optStr returns a pointer to s (for optional string columns).
func optStr(s string) *string { return &s }

func connectionView(c store.Connection) map[string]any {
	return map[string]any{
		"id":                    store.GoUUID(c.ID).String(),
		"source_id":             store.GoUUID(c.SourceID).String(),
		"destination_id":        store.GoUUID(c.DestinationID).String(),
		"enabled":               c.Enabled,
		"name":                  c.Name,
		"max_retries":           c.MaxRetries,
		"retry_strategy":        c.RetryStrategy,
		"retry_base_ms":         c.RetryBaseMs,
		"retry_cap_ms":          c.RetryCapMs,
		"retry_jitter_pct":      c.RetryJitterPct,
		"custom_retry_schedule": json.RawMessage(c.CustomRetrySchedule),
		"created_at":            c.CreatedAt.Time,
		"updated_at":            c.UpdatedAt.Time,
	}
}
