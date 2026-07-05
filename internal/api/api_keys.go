package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// listAPIKeys serves GET /api/orgs/{org_id}/api-keys — any member can list.
// We never expose key_hash in the response; clients see only the public
// metadata + the prefix.
func (d Deps) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpErr(w, http.StatusForbidden, "session required")
		return
	}
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid org_id")
		return
	}
	p, _ := auth.FromContext(r.Context())
	if _, err := d.Queries.GetOrgMember(r.Context(), store.GetOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: store.UUID(p.UserID),
	}); err != nil {
		httpErr(w, http.StatusForbidden, "not a member")
		return
	}
	rows, err := d.Queries.ListAPIKeysByOrg(r.Context(), store.UUID(orgID))
	if err != nil {
		d.Log.Error("list api keys", "err", err)
		httpErr(w, http.StatusInternalServerError, "list keys")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, k := range rows {
		out = append(out, map[string]any{
			"id":           store.GoUUID(k.ID).String(),
			"name":         k.Name,
			"prefix":       k.Prefix,
			"last_used_at": k.LastUsedAt,
			"expires_at":   k.ExpiresAt,
			"created_at":   k.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// createAPIKey serves POST /api/orgs/{org_id}/api-keys — admin+ only.
// Returns the full plaintext key exactly once; subsequent fetches via
// listAPIKeys expose only the prefix.
func (d Deps) createAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpErr(w, http.StatusForbidden, "session required")
		return
	}
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid org_id")
		return
	}
	p, _ := auth.FromContext(r.Context())
	caller, err := d.Queries.GetOrgMember(r.Context(), store.GetOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: store.UUID(p.UserID),
	})
	if err != nil {
		httpErr(w, http.StatusForbidden, "not a member")
		return
	}
	if auth.Role(caller.Role).LessThan(auth.RoleAdmin) {
		httpErr(w, http.StatusForbidden, "admin required")
		return
	}
	var body struct {
		Name string `json:"name"`
		// Optional: key auto-expires after this many days. Omit / <=0 for a
		// non-expiring key (NULL). Bounds a leaked key's exposure window.
		ExpiresInDays *int `json:"expires_in_days,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		httpErr(w, http.StatusBadRequest, "name required")
		return
	}

	full, prefix, hash, err := auth.NewAPIKey()
	if err != nil {
		d.Log.Error("generate api key", "err", err)
		httpErr(w, http.StatusInternalServerError, "gen key")
		return
	}
	params := store.CreateAPIKeyParams{
		OrgID:   store.UUID(orgID),
		Name:    body.Name,
		Prefix:  prefix,
		KeyHash: hash,
	}
	if body.ExpiresInDays != nil && *body.ExpiresInDays > 0 {
		params.ExpiresAt = pgtype.Timestamptz{
			Time:  time.Now().Add(time.Duration(*body.ExpiresInDays) * 24 * time.Hour),
			Valid: true,
		}
	}
	row, err := d.Queries.CreateAPIKey(r.Context(), params)
	if err != nil {
		d.Log.Error("create api key", "err", err)
		httpErr(w, http.StatusInternalServerError, "create key")
		return
	}
	rid := store.GoUUID(row.ID)
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "api_key.create",
		TargetType: "api_key",
		TargetID:   &rid,
		OrgID:      orgID,
		Metadata: map[string]any{
			"name":   body.Name,
			"prefix": prefix,
		},
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":     rid.String(),
		"name":   body.Name,
		"key":    full, // only time the plaintext secret is returned
		"prefix": prefix,
	})
}

// revokeAPIKey serves DELETE /api/orgs/{org_id}/api-keys/{id} — admin+.
// Soft-revoke (UPDATE revoked_at) so later audit lookups can resolve the
// prefix to its name. The store query is org-scoped so a key from a sibling
// org never matches by ID alone.
func (d Deps) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpErr(w, http.StatusForbidden, "session required")
		return
	}
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid org_id")
		return
	}
	keyID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid key id")
		return
	}
	p, _ := auth.FromContext(r.Context())
	caller, err := d.Queries.GetOrgMember(r.Context(), store.GetOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: store.UUID(p.UserID),
	})
	if err != nil {
		httpErr(w, http.StatusForbidden, "not a member")
		return
	}
	if auth.Role(caller.Role).LessThan(auth.RoleAdmin) {
		httpErr(w, http.StatusForbidden, "admin required")
		return
	}
	if err := d.Queries.RevokeAPIKeyForOrg(r.Context(), store.RevokeAPIKeyForOrgParams{
		ID:    store.UUID(keyID),
		OrgID: store.UUID(orgID),
	}); err != nil {
		d.Log.Error("revoke api key", "err", err)
		httpErr(w, http.StatusInternalServerError, "revoke key")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "api_key.revoke",
		TargetType: "api_key",
		TargetID:   &keyID,
		OrgID:      orgID,
	})
	w.WriteHeader(http.StatusNoContent)
}
