package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/store"
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
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
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
	if body.Type == "http" {
		if err := deliver.ValidateDestinationURL(*body.URL); err != nil {
			httpErr(w, http.StatusBadRequest, "invalid destination url: "+err.Error())
			return
		}
	}
	authCfg := body.AuthConfig
	if len(authCfg) == 0 {
		authCfg = json.RawMessage(`{}`)
	}
	row, err := d.Queries.CreateDestination(r.Context(), store.CreateDestinationParams{
		OrgID:          store.UUID(p.OrgID),
		Name:           body.Name,
		Type:           body.Type,
		Url:            body.URL,
		AuthConfig:     authCfg,
		RateLimitRps:   body.RateLimitRPS,
		RateLimitBurst: body.RateLimitBurst,
		MaxInflight:    body.MaxInflight,
	})
	if err != nil {
		d.Log.Error("create destination", "err", err)
		httpErr(w, http.StatusInternalServerError, "create destination")
		return
	}
	createMeta := map[string]any{
		"name": row.Name,
		"type": row.Type,
	}
	if row.Url != nil {
		createMeta["url"] = *row.Url
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "destination.create",
		TargetType: "destination",
		TargetID:   audit.PtrUUID(store.GoUUID(row.ID)),
		Metadata:   createMeta,
	})
	writeJSON(w, http.StatusCreated, destinationView(row))
}

func (d Deps) listDestinations(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	rows, err := d.Queries.ListDestinationsByOrg(r.Context(), store.UUID(p.OrgID))
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
	row, err := d.Queries.GetDestinationForOrg(r.Context(), store.GetDestinationForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		httpErr(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, destinationView(row))
}

type patchDestinationReq struct {
	Name           *string         `json:"name,omitempty"`
	Type           *string         `json:"type,omitempty"`
	URL            *string         `json:"url,omitempty"`
	AuthConfig     json.RawMessage `json:"auth_config,omitempty"`
	RateLimitRPS   *int32          `json:"rate_limit_rps,omitempty"`
	RateLimitBurst *int32          `json:"rate_limit_burst,omitempty"`
	MaxInflight    *int32          `json:"max_inflight,omitempty"`
}

func (d Deps) patchDestination(w http.ResponseWriter, r *http.Request) {
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
	var body patchDestinationReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Type != nil && *body.Type != "http" && *body.Type != "cli" {
		httpErr(w, http.StatusBadRequest, "type must be 'http' or 'cli'")
		return
	}
	old, err := d.Queries.GetDestinationForOrg(r.Context(), store.GetDestinationForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		httpErr(w, http.StatusNotFound, "not found")
		return
	}
	// Post-merge type/url consistency. Compute what each field will be
	// AFTER the COALESCE-style patch and reject combinations that would
	// permanently break delivery (http destination with NULL url, etc).
	effectiveType := old.Type
	if body.Type != nil {
		effectiveType = *body.Type
	}
	effectiveURL := old.Url
	if body.URL != nil {
		effectiveURL = body.URL
	}
	if effectiveType == "http" && (effectiveURL == nil || *effectiveURL == "") {
		httpErr(w, http.StatusBadRequest, "url required for http destination")
		return
	}
	if effectiveType == "http" {
		if err := deliver.ValidateDestinationURL(*effectiveURL); err != nil {
			httpErr(w, http.StatusBadRequest, "invalid destination url: "+err.Error())
			return
		}
	}
	params := store.PatchDestinationForOrgParams{
		ID:             store.UUID(id),
		OrgID:          store.UUID(p.OrgID),
		Name:           body.Name,
		Type:           body.Type,
		Url:            body.URL,
		RateLimitRps:   body.RateLimitRPS,
		RateLimitBurst: body.RateLimitBurst,
		MaxInflight:    body.MaxInflight,
	}
	if len(body.AuthConfig) > 0 {
		params.AuthConfig = body.AuthConfig
	}
	row, err := d.Queries.PatchDestinationForOrg(r.Context(), params)
	if err != nil {
		d.Log.Error("patch destination", "err", err)
		httpErr(w, http.StatusInternalServerError, "update")
		return
	}
	if changed := diffDestination(old, row); len(changed) > 0 {
		audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
			Action:     "destination.update",
			TargetType: "destination",
			TargetID:   audit.PtrUUID(store.GoUUID(row.ID)),
			Metadata:   map[string]any{"changed": changed},
		})
	}
	writeJSON(w, http.StatusOK, destinationView(row))
}

func (d Deps) deleteDestination(w http.ResponseWriter, r *http.Request) {
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
	if err := d.Queries.DeleteDestinationForOrg(r.Context(), store.DeleteDestinationForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	}); err != nil {
		d.Log.Error("delete destination", "err", err)
		httpErr(w, http.StatusInternalServerError, "delete")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "destination.delete",
		TargetType: "destination",
		TargetID:   audit.PtrUUID(id),
		Metadata:   map[string]any{},
	})
	w.WriteHeader(http.StatusNoContent)
}

// diffDestination returns a map of changed fields between old and new in the
// shape {<field>: {"from": <old>, "to": <new>}}. The auth_config field is
// never emitted in cleartext — if it changed, the value is recorded as
// "redacted" so audit logs don't leak HMAC secrets / bearer tokens.
func diffDestination(old, new store.Destination) map[string]map[string]any {
	out := map[string]map[string]any{}
	if old.Name != new.Name {
		out["name"] = map[string]any{"from": old.Name, "to": new.Name}
	}
	if old.Type != new.Type {
		out["type"] = map[string]any{"from": old.Type, "to": new.Type}
	}
	if !strPtrEq(old.Url, new.Url) {
		out["url"] = map[string]any{"from": derefString(old.Url), "to": derefString(new.Url)}
	}
	if !int32PtrEq(old.RateLimitRps, new.RateLimitRps) {
		out["rate_limit_rps"] = map[string]any{"from": derefInt32(old.RateLimitRps), "to": derefInt32(new.RateLimitRps)}
	}
	if !int32PtrEq(old.RateLimitBurst, new.RateLimitBurst) {
		out["rate_limit_burst"] = map[string]any{"from": derefInt32(old.RateLimitBurst), "to": derefInt32(new.RateLimitBurst)}
	}
	if !int32PtrEq(old.MaxInflight, new.MaxInflight) {
		out["max_inflight"] = map[string]any{"from": derefInt32(old.MaxInflight), "to": derefInt32(new.MaxInflight)}
	}
	if !bytesEq(old.AuthConfig, new.AuthConfig) {
		// Never leak secret material into audit metadata.
		out["auth_config"] = map[string]any{"from": "redacted", "to": "redacted"}
	}
	return out
}

// destinationView shapes the JSON returned to API callers. Importantly it
// OMITS auth_config — that JSONB blob may hold HMAC signing secrets, bearer
// tokens, basic-auth creds, etc. Leaking it to every org member (read access
// is granted to the `member` role) would broadcast destination credentials.
// We surface a non-sensitive boolean instead so the UI can render
// "configured / not configured" without seeing the value. Mutations
// (create/patch) accept auth_config in the request body via a separate path.
func destinationView(d store.Destination) map[string]any {
	authConfigured := false
	// Treat both NULL and empty-JSON-object as "not configured" — the
	// create handler defaults to `{}` when the caller omits the field.
	if len(d.AuthConfig) > 0 && string(d.AuthConfig) != "{}" {
		authConfigured = true
	}
	return map[string]any{
		"id":               store.GoUUID(d.ID).String(),
		"org_id":           store.GoUUID(d.OrgID).String(),
		"name":             d.Name,
		"type":             d.Type,
		"url":              d.Url,
		"auth_configured":  authConfigured,
		"rate_limit_rps":   d.RateLimitRps,
		"rate_limit_burst": d.RateLimitBurst,
		"max_inflight":     d.MaxInflight,
		"created_at":       d.CreatedAt.Time,
		"updated_at":       d.UpdatedAt.Time,
	}
}
