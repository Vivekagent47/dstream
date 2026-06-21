package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

type createConnectionReq struct {
	SourceID      uuid.UUID `json:"source_id"`
	DestinationID uuid.UUID `json:"destination_id"`
	Enabled       *bool     `json:"enabled,omitempty"`
}

func (d Deps) createConnection(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	var body createConnectionReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.SourceID == uuid.Nil || body.DestinationID == uuid.Nil {
		httpErr(w, http.StatusBadRequest, "source_id and destination_id required")
		return
	}
	// Verify both source and destination belong to the caller's org.
	if _, err := d.Queries.GetSourceForOrg(r.Context(), store.GetSourceForOrgParams{
		ID:    store.UUID(body.SourceID),
		OrgID: store.UUID(p.OrgID),
	}); err != nil {
		httpErr(w, http.StatusBadRequest, "source not found in this org")
		return
	}
	if _, err := d.Queries.GetDestinationForOrg(r.Context(), store.GetDestinationForOrgParams{
		ID:    store.UUID(body.DestinationID),
		OrgID: store.UUID(p.OrgID),
	}); err != nil {
		httpErr(w, http.StatusBadRequest, "destination not found in this org")
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
	})
	if err != nil {
		d.Log.Error("create connection", "err", err)
		httpErr(w, http.StatusInternalServerError, "create")
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
	writeJSON(w, http.StatusCreated, connectionView(row))
}

func (d Deps) listConnections(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	rows, err := d.Queries.ListConnectionsByOrg(r.Context(), store.UUID(p.OrgID))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "list")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, connectionView(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) getConnection(w http.ResponseWriter, r *http.Request) {
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
	row, err := d.Queries.GetConnectionForOrg(r.Context(), store.GetConnectionForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		httpErr(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, connectionView(row))
}

type patchConnectionReq struct {
	Enabled             *bool           `json:"enabled,omitempty"`
	MaxRetries          *int32          `json:"max_retries,omitempty"`
	RetryStrategy       *string         `json:"retry_strategy,omitempty"`
	RetryBaseMs         *int32          `json:"retry_base_ms,omitempty"`
	RetryCapMs          *int32          `json:"retry_cap_ms,omitempty"`
	RetryJitterPct      *int32          `json:"retry_jitter_pct,omitempty"`
	CustomRetrySchedule json.RawMessage `json:"custom_retry_schedule,omitempty"`
}

func (d Deps) patchConnection(w http.ResponseWriter, r *http.Request) {
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
	var body patchConnectionReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.RetryStrategy != nil {
		s := *body.RetryStrategy
		if s != "exponential" && s != "linear" && s != "fixed" && s != "custom" {
			httpErr(w, http.StatusBadRequest, "invalid retry_strategy")
			return
		}
	}
	old, err := d.Queries.GetConnectionForOrg(r.Context(), store.GetConnectionForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		httpErr(w, http.StatusNotFound, "not found")
		return
	}
	params := store.PatchConnectionForOrgParams{
		ID:             store.UUID(id),
		OrgID:          store.UUID(p.OrgID),
		Enabled:        body.Enabled,
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
		httpErr(w, http.StatusInternalServerError, "update")
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
	writeJSON(w, http.StatusOK, connectionView(row))
}

func (d Deps) deleteConnection(w http.ResponseWriter, r *http.Request) {
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
	if err := d.Queries.DeleteConnectionForOrg(r.Context(), store.DeleteConnectionForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	}); err != nil {
		d.Log.Error("delete connection", "err", err)
		httpErr(w, http.StatusInternalServerError, "delete")
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
	if !bytesEq(old.CustomRetrySchedule, new.CustomRetrySchedule) {
		out["custom_retry_schedule"] = map[string]any{"changed": true}
	}
	return out
}

func connectionView(c store.Connection) map[string]any {
	return map[string]any{
		"id":                    store.GoUUID(c.ID).String(),
		"source_id":             store.GoUUID(c.SourceID).String(),
		"destination_id":        store.GoUUID(c.DestinationID).String(),
		"enabled":               c.Enabled,
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
