package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/streamingo/dstream/internal/store"
)

type createConnectionReq struct {
	SourceID      uuid.UUID `json:"source_id"`
	DestinationID uuid.UUID `json:"destination_id"`
	Enabled       *bool     `json:"enabled,omitempty"`
}

func (d Deps) createConnection(w http.ResponseWriter, r *http.Request) {
	var body createConnectionReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.SourceID == uuid.Nil || body.DestinationID == uuid.Nil {
		httpErr(w, http.StatusBadRequest, "source_id and destination_id required")
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
		httpErr(w, http.StatusInternalServerError, "create: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, connectionView(row))
}

func (d Deps) listConnections(w http.ResponseWriter, r *http.Request) {
	sourceIDStr := r.URL.Query().Get("source_id")
	if sourceIDStr == "" {
		httpErr(w, http.StatusBadRequest, "source_id query param required")
		return
	}
	sid, err := uuid.Parse(sourceIDStr)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid source_id")
		return
	}
	rows, err := d.Queries.ListConnectionsBySource(r.Context(), store.UUID(sid))
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
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	row, err := d.Queries.GetConnectionByID(r.Context(), store.UUID(id))
	if err != nil {
		httpErr(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, connectionView(row))
}

type patchConnectionReq struct {
	Enabled               *bool           `json:"enabled,omitempty"`
	MaxRetries            *int32          `json:"max_retries,omitempty"`
	RetryStrategy         *string         `json:"retry_strategy,omitempty"`
	RetryBaseMs           *int32          `json:"retry_base_ms,omitempty"`
	RetryCapMs            *int32          `json:"retry_cap_ms,omitempty"`
	RetryJitterPct        *int32          `json:"retry_jitter_pct,omitempty"`
	CustomRetrySchedule   json.RawMessage `json:"custom_retry_schedule,omitempty"`
}

func (d Deps) patchConnection(w http.ResponseWriter, r *http.Request) {
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
	params := store.UpdateConnectionParams{ID: store.UUID(id)}
	if body.Enabled != nil {
		params.Enabled = body.Enabled
	}
	if body.MaxRetries != nil {
		params.MaxRetries = body.MaxRetries
	}
	if body.RetryStrategy != nil {
		params.RetryStrategy = body.RetryStrategy
	}
	if body.RetryBaseMs != nil {
		params.RetryBaseMs = body.RetryBaseMs
	}
	if body.RetryCapMs != nil {
		params.RetryCapMs = body.RetryCapMs
	}
	if body.RetryJitterPct != nil {
		params.RetryJitterPct = body.RetryJitterPct
	}
	if len(body.CustomRetrySchedule) > 0 {
		params.CustomRetrySchedule = body.CustomRetrySchedule
	}
	row, err := d.Queries.UpdateConnection(r.Context(), params)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "update: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, connectionView(row))
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
