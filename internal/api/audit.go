package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// listAudit serves GET /api/audit — the audit log for the caller's active
// org (resolved from the session cookie). Session-only: API-key principals
// are rejected with 403 so the human audit trail isn't visible to machine
// credentials.
func (d Deps) listAudit(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpErr(w, http.StatusForbidden, "session required")
		return
	}
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	d.serveAudit(w, r, p.OrgID)
}

// listAuditForOrg serves GET /api/orgs/{org_id}/audit — the audit log for a
// specific org. The caller must be a member of that org; we verify with a
// fresh GetOrgMember lookup so the URL path can't grant access the session
// principal doesn't already have.
func (d Deps) listAuditForOrg(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpErr(w, http.StatusForbidden, "session required")
		return
	}
	p, err := auth.FromContext(r.Context())
	if err != nil {
		httpErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid org_id")
		return
	}
	if _, err := d.Queries.GetOrgMember(r.Context(), store.GetOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: store.UUID(p.UserID),
	}); err != nil {
		httpErr(w, http.StatusForbidden, "not a member of this org")
		return
	}
	d.serveAudit(w, r, orgID)
}

// serveAudit is the shared body of the two endpoints. It parses query
// parameters, runs ListAuditLogsByOrg, and writes a JSON envelope with an
// optional pagination cursor.
//
// Pagination uses keyset on audit_logs.id (BIGSERIAL, monotonic): the
// response includes next_before_id when the result page is full so the
// client can request older rows via ?before_id=<n>.
func (d Deps) serveAudit(w http.ResponseWriter, r *http.Request, orgID uuid.UUID) {
	q := r.URL.Query()

	limit := int32(50)
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = int32(n)
		}
	}

	var beforeID *int64
	if v := q.Get("before_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			httpErr(w, http.StatusBadRequest, "invalid before_id")
			return
		}
		beforeID = &n
	}

	// Bad actor_user_id was previously swallowed → unfiltered dump. Fail
	// loudly instead so callers don't believe they're filtering when they
	// aren't.
	var actorUser pgtype.UUID
	if v := q.Get("actor_user_id"); v != "" {
		u, err := uuid.Parse(v)
		if err != nil {
			httpErr(w, http.StatusBadRequest, "invalid actor_user_id")
			return
		}
		actorUser = store.UUID(u)
	}

	var targetType, action *string
	if v := q.Get("target_type"); v != "" {
		s := v
		targetType = &s
	}
	if v := q.Get("action"); v != "" {
		s := v
		action = &s
	}

	rows, err := d.Queries.ListAuditLogsByOrg(r.Context(), store.ListAuditLogsByOrgParams{
		OrgID:       store.UUID(orgID),
		Limit:       limit,
		BeforeID:    beforeID,
		ActorUserID: actorUser,
		TargetType:  targetType,
		Action:      action,
	})
	if err != nil {
		d.Log.Error("list audit", "err", err)
		httpErr(w, http.StatusInternalServerError, "list audit")
		return
	}

	out := hydrateAuditRows(rows)
	resp := map[string]any{"entries": out}
	if int32(len(rows)) == limit && len(rows) > 0 {
		// Tail of a full page → expose a cursor for the next page. The
		// caller passes this back as ?before_id=N to fetch the next page.
		resp["next_before_id"] = rows[len(rows)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// hydrateAuditRows shapes the sqlc rows into the spec JSON. The actor object
// distinguishes user vs api_key principals so the UI can render either
// "Alice <alice@x>" or "API key 'bootstrap'" without further lookups.
func hydrateAuditRows(rows []store.ListAuditLogsByOrgRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		actor := map[string]any{}
		switch {
		case r.ActorUserID.Valid:
			actor["type"] = "user"
			actor["user_id"] = store.GoUUID(r.ActorUserID).String()
			// Prefer the live email join; fall back to the snapshot if the
			// user row has been deleted (FK is ON DELETE SET NULL).
			if r.ActorUserEmailJoin != "" {
				actor["email"] = r.ActorUserEmailJoin
			} else if r.ActorEmailSnapshot != nil {
				actor["email"] = *r.ActorEmailSnapshot
			}
			if r.ActorUserNameJoin != "" {
				actor["name"] = r.ActorUserNameJoin
			}
		case r.ActorApiKeyID.Valid:
			actor["type"] = "api_key"
			actor["api_key_id"] = store.GoUUID(r.ActorApiKeyID).String()
			if r.ActorApiKeyNameJoin != "" {
				actor["name"] = r.ActorApiKeyNameJoin
			}
		default:
			actor["type"] = "system"
		}

		target := map[string]any{"type": r.TargetType}
		if r.TargetID.Valid {
			target["id"] = store.GoUUID(r.TargetID).String()
		}

		out = append(out, map[string]any{
			"id":         r.ID,
			"created_at": r.CreatedAt.Time,
			"actor":      actor,
			"action":     r.Action,
			"target":     target,
			"metadata":   rawJSONOrEmpty(r.Metadata),
		})
	}
	return out
}
