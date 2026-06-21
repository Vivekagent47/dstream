package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// listMyOrgs serves GET /api/orgs — returns every org the calling user is a
// member of. Session-only (API keys are scoped to a single org and have no
// notion of "my orgs").
func (d Deps) listMyOrgs(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpErr(w, http.StatusForbidden, "session required")
		return
	}
	p, _ := auth.FromContext(r.Context())
	orgs, err := d.Queries.ListOrgsForUser(r.Context(), store.UUID(p.UserID))
	if err != nil {
		d.Log.Error("list orgs", "err", err)
		httpErr(w, http.StatusInternalServerError, "list orgs")
		return
	}
	writeJSON(w, http.StatusOK, orgs)
}

// createOrg serves POST /api/orgs — creates a new org and adds the caller as
// owner. Session-only. Slug is derived from the name (immutable in v1 per
// spec non-goals).
func (d Deps) createOrg(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpErr(w, http.StatusForbidden, "session required")
		return
	}
	var body struct {
		Name string `json:"name"`
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
	slug := slugifyName(body.Name)
	org, err := d.Queries.CreateOrganization(r.Context(), store.CreateOrganizationParams{
		Name: body.Name,
		Slug: slug,
	})
	if err != nil {
		d.Log.Error("create org", "err", err)
		httpErr(w, http.StatusBadRequest, "create org")
		return
	}
	p, _ := auth.FromContext(r.Context())
	if err := d.Queries.AddOrgMember(r.Context(), store.AddOrgMemberParams{
		OrgID:  org.ID,
		UserID: store.UUID(p.UserID),
		Role:   string(auth.RoleOwner),
	}); err != nil {
		d.Log.Error("add owner", "err", err)
		httpErr(w, http.StatusInternalServerError, "add owner")
		return
	}
	orgUUID := store.GoUUID(org.ID)
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "org.create",
		TargetType: "org",
		TargetID:   &orgUUID,
		OrgID:      orgUUID,
		Metadata:   map[string]any{"name": org.Name, "slug": org.Slug},
	})
	writeJSON(w, http.StatusCreated, org)
}

// selectOrg serves POST /api/orgs/select — re-issues the session cookie with
// a new active_org_id. The caller must already be a member of that org.
func (d Deps) selectOrg(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpErr(w, http.StatusForbidden, "session required")
		return
	}
	var body struct {
		OrgID string `json:"org_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	orgID, err := uuid.Parse(body.OrgID)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid org_id")
		return
	}
	if orgID == uuid.Nil {
		// Defense-in-depth: store.UUID now maps uuid.Nil → NULL, so the
		// downstream GetOrgMember would short-circuit on NULL. Reject
		// outright so a malicious client can't force a Nil-org session
		// even temporarily.
		httpErr(w, http.StatusBadRequest, "invalid org_id")
		return
	}
	p, _ := auth.FromContext(r.Context())
	if _, err := d.Queries.GetOrgMember(r.Context(), store.GetOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: store.UUID(p.UserID),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpErr(w, http.StatusForbidden, "not a member of that org")
			return
		}
		d.Log.Error("select org: membership lookup", "err", err)
		httpErr(w, http.StatusInternalServerError, "membership lookup")
		return
	}
	d.Signer.Issue(w, p.UserID, orgID)
	writeJSON(w, http.StatusOK, map[string]any{"active_org_id": orgID.String()})
}

// updateOrg serves PATCH /api/orgs/{org_id} — renames the org. Admin+ only.
// Slug is intentionally not editable in v1 (spec non-goal); callers can rename
// the display name freely without invalidating URLs.
func (d Deps) updateOrg(w http.ResponseWriter, r *http.Request) {
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
		Name *string `json:"name,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Name == nil {
		httpErr(w, http.StatusBadRequest, "nothing to update")
		return
	}
	newName := strings.TrimSpace(*body.Name)
	if newName == "" {
		httpErr(w, http.StatusBadRequest, "name required")
		return
	}
	old, err := d.Queries.GetOrganizationByID(r.Context(), store.UUID(orgID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpErr(w, http.StatusNotFound, "org not found")
			return
		}
		d.Log.Error("get org", "err", err)
		httpErr(w, http.StatusInternalServerError, "get org")
		return
	}
	if old.Name == newName {
		// No-op: skip the write and the audit row.
		writeJSON(w, http.StatusOK, old)
		return
	}
	updated, err := d.Queries.UpdateOrgName(r.Context(), store.UpdateOrgNameParams{
		ID:   store.UUID(orgID),
		Name: newName,
	})
	if err != nil {
		d.Log.Error("update org", "err", err)
		httpErr(w, http.StatusInternalServerError, "update org")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "org.update",
		TargetType: "org",
		TargetID:   audit.PtrUUID(orgID),
		OrgID:      orgID,
		Metadata: map[string]any{
			"changed": map[string]any{
				"name": map[string]any{"from": old.Name, "to": updated.Name},
			},
		},
	})
	writeJSON(w, http.StatusOK, updated)
}

// deleteOrg serves DELETE /api/orgs/{org_id} — owner-only destructive op.
// All org-owned rows cascade via FK ON DELETE CASCADE. We write the audit
// row BEFORE the delete; the audit row will itself be cascade-deleted as
// part of the same op, which is acceptable for v1 (the action is
// destructive and a future "trash" feature can opt out of cascade).
func (d Deps) deleteOrg(w http.ResponseWriter, r *http.Request) {
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
	if caller.Role != string(auth.RoleOwner) {
		httpErr(w, http.StatusForbidden, "owner required")
		return
	}
	// Snapshot name + slug into the audit row BEFORE the cascade — the FK
	// is ON DELETE SET NULL so the audit row survives, but the
	// organizations row is gone and we want forensic readability.
	org, gerr := d.Queries.GetOrganizationByID(r.Context(), store.UUID(orgID))
	meta := map[string]any{}
	if gerr == nil {
		meta["name"] = org.Name
		meta["slug"] = org.Slug
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "org.delete",
		TargetType: "org",
		TargetID:   audit.PtrUUID(orgID),
		OrgID:      orgID,
		Metadata:   meta,
	})
	if err := d.Queries.DeleteOrganization(r.Context(), store.UUID(orgID)); err != nil {
		d.Log.Error("delete org", "err", err)
		httpErr(w, http.StatusInternalServerError, "delete org")
		return
	}
	// If the deleted org was the caller's active session-org, rotate the
	// cookie to point at whatever org they still have (or uuid.Nil if
	// they're now org-less). Without this, the next request 409s on
	// RequireOrg with a stale active_org_id.
	if p.OrgID == orgID {
		next, nerr := d.Queries.GetFirstOrgForUser(r.Context(), store.UUID(p.UserID))
		var nextOrg uuid.UUID
		if nerr == nil {
			nextOrg = store.GoUUID(next)
		} else if !errors.Is(nerr, pgx.ErrNoRows) {
			d.Log.Warn("delete org: lookup next org", "err", nerr)
		}
		d.Signer.Issue(w, p.UserID, nextOrg)
	}
	w.WriteHeader(http.StatusNoContent)
}

// transferOwnership serves POST /api/orgs/{org_id}/transfer — owner-only.
// Atomically promotes the target user to owner and demotes the current owner
// (the caller) to admin. Target must already be a member of the org.
//
// Concurrency: the heavy lifting is done by a single UPDATE that self-guards
// on (a) caller still being owner and (b) target still being a member. If
// either fact is invalidated by a concurrent operation between request entry
// and the UPDATE, the statement matches zero rows and we return 403/400 —
// no TOCTOU window between SELECT and UPDATE.
func (d Deps) transferOwnership(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpErr(w, http.StatusForbidden, "session required")
		return
	}
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid org_id")
		return
	}
	var body struct {
		ToUserID string `json:"to_user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	toID, err := uuid.Parse(body.ToUserID)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid to_user_id")
		return
	}
	p, _ := auth.FromContext(r.Context())
	if toID == p.UserID {
		httpErr(w, http.StatusBadRequest, "cannot transfer to self")
		return
	}
	rows, err := d.Queries.TransferOrgOwnership(r.Context(), store.TransferOrgOwnershipParams{
		OrgID:        store.UUID(orgID),
		ToUserID:     store.UUID(toID),
		CallerUserID: store.UUID(p.UserID),
	})
	if err != nil {
		d.Log.Error("transfer ownership", "err", err)
		httpErr(w, http.StatusInternalServerError, "transfer ownership")
		return
	}
	if rows == 0 {
		// Either the caller isn't (still) owner, or the target isn't a
		// member. Both are user-correctable; respond with a generic 403
		// to avoid leaking which guard tripped (matches the existing
		// "owner required" envelope returned by other handlers).
		httpErr(w, http.StatusForbidden, "transfer not permitted")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "org.transfer",
		TargetType: "org",
		TargetID:   audit.PtrUUID(orgID),
		OrgID:      orgID,
		Metadata: map[string]any{
			"to_user_id":   toID.String(),
			"from_user_id": p.UserID.String(),
		},
	})
	w.WriteHeader(http.StatusNoContent)
}

// slugifyName derives a URL-safe slug from a free-form org name. Lowercases,
// keeps [a-z0-9], collapses anything else into single dashes, trims leading/
// trailing dashes, falls back to "org" for empty input, and appends a 6-byte
// (48-bit) random hex suffix so two orgs with identical names can't collide
// AND so the slug is unguessable for enumeration attacks (3-byte suffix was
// online-feasible to brute-force).
func slugifyName(name string) string {
	b := make([]byte, 0, len(name))
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b = append(b, byte(r))
			prevDash = false
		default:
			if !prevDash && len(b) > 0 {
				b = append(b, '-')
				prevDash = true
			}
		}
	}
	// Trim trailing dash from the collapse step.
	for len(b) > 0 && b[len(b)-1] == '-' {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		b = []byte("org")
	}
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	return string(b) + "-" + hex.EncodeToString(suffix[:])
}
