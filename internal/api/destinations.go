package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/streamingo/dstream/internal/auth"
	"github.com/streamingo/dstream/internal/store"
)

type createDestinationReq struct {
	Name           string          `json:"name"`
	Type           string          `json:"type"` // "http" | "cli"
	URL            *string         `json:"url,omitempty"`
	AuthConfig     json.RawMessage `json:"auth_config,omitempty"`
	RateLimitRPS   *int32          `json:"rate_limit_rps,omitempty"`
	RateLimitBurst *int32          `json:"rate_limit_burst,omitempty"`
	MaxInflight    *int32          `json:"max_inflight,omitempty"`
}

func (d Deps) createDestination(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.ProjectID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "api key required")
		return
	}
	var body createDestinationReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Name == "" {
		httpErr(w, http.StatusBadRequest, "name required")
		return
	}
	if body.Type != "http" && body.Type != "cli" {
		httpErr(w, http.StatusBadRequest, "type must be 'http' or 'cli'")
		return
	}
	if body.Type == "http" && (body.URL == nil || *body.URL == "") {
		httpErr(w, http.StatusBadRequest, "url required for http destination")
		return
	}
	authCfg := body.AuthConfig
	if len(authCfg) == 0 {
		authCfg = json.RawMessage(`{}`)
	}
	row, err := d.Queries.CreateDestination(r.Context(), store.CreateDestinationParams{
		ProjectID:      store.UUID(p.ProjectID),
		Name:           body.Name,
		Type:           body.Type,
		Url:            body.URL,
		AuthConfig:     authCfg,
		RateLimitRps:   body.RateLimitRPS,
		RateLimitBurst: body.RateLimitBurst,
		MaxInflight:    body.MaxInflight,
	})
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "create: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, destinationView(row))
}

func (d Deps) listDestinations(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.ProjectID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "api key required")
		return
	}
	rows, err := d.Queries.ListDestinationsByProject(r.Context(), store.UUID(p.ProjectID))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "list")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, destinationView(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) getDestination(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	row, err := d.Queries.GetDestinationByID(r.Context(), store.UUID(id))
	if err != nil {
		httpErr(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, destinationView(row))
}

type patchDestinationReq struct {
	Name           *string         `json:"name,omitempty"`
	URL            *string         `json:"url,omitempty"`
	AuthConfig     json.RawMessage `json:"auth_config,omitempty"`
	RateLimitRPS   *int32          `json:"rate_limit_rps,omitempty"`
	RateLimitBurst *int32          `json:"rate_limit_burst,omitempty"`
	MaxInflight    *int32          `json:"max_inflight,omitempty"`
}

func (d Deps) patchDestination(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body patchDestinationReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	params := store.UpdateDestinationParams{ID: store.UUID(id)}
	if body.Name != nil {
		params.Name = body.Name
	}
	if body.URL != nil {
		params.Url = body.URL
	}
	if len(body.AuthConfig) > 0 {
		params.AuthConfig = body.AuthConfig
	}
	if body.RateLimitRPS != nil {
		params.RateLimitRps = body.RateLimitRPS
	}
	if body.RateLimitBurst != nil {
		params.RateLimitBurst = body.RateLimitBurst
	}
	if body.MaxInflight != nil {
		params.MaxInflight = body.MaxInflight
	}
	row, err := d.Queries.UpdateDestination(r.Context(), params)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "update: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, destinationView(row))
}

func destinationView(d store.Destination) map[string]any {
	return map[string]any{
		"id":               store.GoUUID(d.ID).String(),
		"project_id":       store.GoUUID(d.ProjectID).String(),
		"name":             d.Name,
		"type":             d.Type,
		"url":              d.Url,
		"auth_config":      json.RawMessage(d.AuthConfig),
		"rate_limit_rps":   d.RateLimitRps,
		"rate_limit_burst": d.RateLimitBurst,
		"max_inflight":     d.MaxInflight,
		"created_at":       d.CreatedAt.Time,
		"updated_at":       d.UpdatedAt.Time,
	}
}
