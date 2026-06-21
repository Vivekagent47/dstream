package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// validMemberRole returns true if s names a known org role. We accept exactly
// the same set the CHECK constraint enforces; rejecting it here gives a clean
// 400 instead of letting the DB explode the request.
func validMemberRole(s string) bool {
	switch auth.Role(s) {
	case auth.RoleOwner, auth.RoleAdmin, auth.RoleMember:
		return true
	}
	return false
}

// fetchCallerAndTarget runs the caller-membership and target-membership
// lookups concurrently. They're independent reads; serializing them costs
// ~1ms on every member PATCH / DELETE under pg latency. Returns the two
// rows plus their respective errors so callers can branch on which side
// failed (caller missing = 403, target missing = 404).
func fetchCallerAndTarget(
	ctx context.Context, q *store.Queries,
	orgID, callerUserID, targetUserID uuid.UUID,
) (caller, target store.OrgMember, cErr, tErr error) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		caller, cErr = q.GetOrgMember(ctx, store.GetOrgMemberParams{
			OrgID:  store.UUID(orgID),
			UserID: store.UUID(callerUserID),
		})
	}()
	go func() {
		defer wg.Done()
		target, tErr = q.GetOrgMember(ctx, store.GetOrgMemberParams{
			OrgID:  store.UUID(orgID),
			UserID: store.UUID(targetUserID),
		})
	}()
	wg.Wait()
	return
}

// listMembers serves GET /api/orgs/{org_id}/members — any member of the org
// can list. Session-only.
func (d Deps) listMembers(w http.ResponseWriter, r *http.Request) {
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
	rows, err := d.Queries.ListOrgMembersByOrg(r.Context(), store.UUID(orgID))
	if err != nil {
		d.Log.Error("list members", "err", err)
		httpErr(w, http.StatusInternalServerError, "list members")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// patchMember serves PATCH /api/orgs/{org_id}/members/{user_id} — admin+.
// Enforces the last-owner guard: demoting the only owner returns 409.
func (d Deps) patchMember(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpErr(w, http.StatusForbidden, "session required")
		return
	}
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid org_id")
		return
	}
	userID, err := uuid.Parse(chi.URLParam(r, "user_id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid user_id")
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !validMemberRole(body.Role) {
		httpErr(w, http.StatusBadRequest, "invalid role")
		return
	}
	p, _ := auth.FromContext(r.Context())
	// Fetch caller + target membership rows concurrently — they're
	// independent lookups and serializing them adds ~1ms to every PATCH.
	caller, target, cErr, tErr := fetchCallerAndTarget(r.Context(), d.Queries, orgID, p.UserID, userID)
	if cErr != nil {
		httpErr(w, http.StatusForbidden, "not a member")
		return
	}
	if auth.Role(caller.Role).LessThan(auth.RoleAdmin) {
		httpErr(w, http.StatusForbidden, "admin required")
		return
	}
	if tErr != nil {
		if errors.Is(tErr, pgx.ErrNoRows) {
			httpErr(w, http.StatusNotFound, "member not found")
			return
		}
		d.Log.Error("get member", "err", tErr)
		httpErr(w, http.StatusInternalServerError, "get member")
		return
	}
	// Spec: cannot promote ANYONE to owner via PATCH — use the transfer
	// flow. This handles two attack patterns at once:
	//   - admin promoting a third party to owner (silent privilege grant)
	//   - any caller self-promoting to owner
	// Owner grants always go through POST /transfer with its own guards.
	if body.Role == string(auth.RoleOwner) && target.Role != string(auth.RoleOwner) {
		httpErr(w, http.StatusBadRequest, "use POST /transfer to promote to owner")
		return
	}
	// Only an owner may mutate another owner's role (demotion to admin /
	// member). Admins cannot demote owners — that would let an admin
	// quietly strip ownership from above.
	if target.Role == string(auth.RoleOwner) && body.Role != string(auth.RoleOwner) {
		if auth.Role(caller.Role) != auth.RoleOwner {
			httpErr(w, http.StatusForbidden, "owner required to demote an owner")
			return
		}
		// Atomic last-owner guard. DemoteOrgOwnerIfNotLast updates the row
		// only when at least one other owner remains; if 0 rows are
		// affected, we know the target is the last owner. Race-free vs
		// the count-then-update split this replaces.
		rows, err := d.Queries.DemoteOrgOwnerIfNotLast(r.Context(), store.DemoteOrgOwnerIfNotLastParams{
			OrgID:  store.UUID(orgID),
			UserID: store.UUID(userID),
			Role:   body.Role,
		})
		if err != nil {
			d.Log.Error("demote owner", "err", err)
			httpErr(w, http.StatusInternalServerError, "demote owner")
			return
		}
		if rows == 0 {
			httpErr(w, http.StatusConflict, "cannot demote last owner")
			return
		}
		audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
			Action:     "member.role_change",
			TargetType: "member",
			TargetID:   audit.PtrUUID(userID),
			OrgID:      orgID,
			Metadata: map[string]any{
				"user_id": userID.String(),
				"from":    target.Role,
				"to":      body.Role,
			},
		})
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if target.Role == body.Role {
		// No-op write: skip the audit row but still return success so the
		// client can be idempotent.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := d.Queries.UpdateOrgMemberRole(r.Context(), store.UpdateOrgMemberRoleParams{
		OrgID:  store.UUID(orgID),
		UserID: store.UUID(userID),
		Role:   body.Role,
	}); err != nil {
		d.Log.Error("update member role", "err", err)
		httpErr(w, http.StatusInternalServerError, "update member role")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "member.role_change",
		TargetType: "member",
		TargetID:   audit.PtrUUID(userID),
		OrgID:      orgID,
		Metadata: map[string]any{
			"user_id": userID.String(),
			"from":    target.Role,
			"to":      body.Role,
		},
	})
	w.WriteHeader(http.StatusNoContent)
}

// removeMember serves DELETE /api/orgs/{org_id}/members/{user_id}.
//
// Auth rules per spec §Membership management:
//   - Caller must be admin+ to remove anyone else.
//   - Caller may remove themself regardless of role (leave-org flow).
//   - Last-owner guard always applies.
//
// On self-remove from the active org we re-issue the session cookie with
// the user's next available org (or uuid.Nil if they're now org-less).
func (d Deps) removeMember(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpErr(w, http.StatusForbidden, "session required")
		return
	}
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid org_id")
		return
	}
	userID, err := uuid.Parse(chi.URLParam(r, "user_id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid user_id")
		return
	}
	p, _ := auth.FromContext(r.Context())
	isSelf := userID == p.UserID

	caller, target, cErr, tErr := fetchCallerAndTarget(r.Context(), d.Queries, orgID, p.UserID, userID)
	if cErr != nil {
		httpErr(w, http.StatusForbidden, "not a member")
		return
	}
	if !isSelf && auth.Role(caller.Role).LessThan(auth.RoleAdmin) {
		httpErr(w, http.StatusForbidden, "admin required")
		return
	}
	if tErr != nil {
		if errors.Is(tErr, pgx.ErrNoRows) {
			httpErr(w, http.StatusNotFound, "member not found")
			return
		}
		d.Log.Error("get member", "err", tErr)
		httpErr(w, http.StatusInternalServerError, "get member")
		return
	}
	if target.Role == string(auth.RoleOwner) {
		// Only owners may remove other owners. Admins removing owners
		// would be an upward privilege strip. Self-removal is still
		// permitted (an owner leaving their own org), provided the
		// last-owner guard passes below.
		if !isSelf && auth.Role(caller.Role) != auth.RoleOwner {
			httpErr(w, http.StatusForbidden, "owner required to remove an owner")
			return
		}
		// Atomic last-owner guard: delete the owner row only if at least
		// one other owner remains. 0 rows → target is the last owner.
		rows, err := d.Queries.DeleteOrgOwnerIfNotLast(r.Context(), store.DeleteOrgOwnerIfNotLastParams{
			OrgID:  store.UUID(orgID),
			UserID: store.UUID(userID),
		})
		if err != nil {
			d.Log.Error("remove owner", "err", err)
			httpErr(w, http.StatusInternalServerError, "remove owner")
			return
		}
		if rows == 0 {
			httpErr(w, http.StatusConflict, "cannot remove last owner")
			return
		}
	} else if err := d.Queries.DeleteOrgMember(r.Context(), store.DeleteOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: store.UUID(userID),
	}); err != nil {
		d.Log.Error("delete member", "err", err)
		httpErr(w, http.StatusInternalServerError, "delete member")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "member.remove",
		TargetType: "member",
		TargetID:   audit.PtrUUID(userID),
		OrgID:      orgID,
		Metadata: map[string]any{
			"user_id":         userID.String(),
			"removed_by_self": isSelf,
		},
	})
	// Self-leave from the active org: rotate the session cookie so the next
	// request doesn't 403 on RequireOrg with a stale active_org_id.
	if isSelf && p.OrgID == orgID {
		next, err := d.Queries.GetFirstOrgForUser(r.Context(), store.UUID(p.UserID))
		var nextOrg uuid.UUID
		if err == nil {
			nextOrg = store.GoUUID(next)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			d.Log.Warn("self-leave: lookup next org", "err", err)
		}
		d.Signer.Issue(w, p.UserID, nextOrg)
	}
	w.WriteHeader(http.StatusNoContent)
}
