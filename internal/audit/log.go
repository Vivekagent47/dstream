// Package audit writes one audit_logs row per successful mutation. Writes are
// best-effort: failures are logged but never propagated, so audit must never
// block user-facing work.
package audit

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// Entry describes a single audit event.
type Entry struct {
	Action     string         // e.g. "source.create", "destination.update"
	TargetType string         // e.g. "source", "destination", "member"
	TargetID   *uuid.UUID     // nil for org-level actions
	Metadata   map[string]any // serialized to JSONB; changed-fields diffs preferred
	// OrgID overrides which org the audit row belongs to. Set this when the
	// mutation targets an org other than the caller's active session-org
	// (e.g. /api/orgs/{org_id}/members/... — the URL path identifies the
	// owning org, not the caller's cookie). When zero/nil, Log falls back to
	// principal.OrgID. This split matters: a user who is admin of OrgA but
	// has OrgB as their session-active org calling DELETE /orgs/{A}/members
	// must file the audit row under OrgA, not OrgB.
	OrgID uuid.UUID
}

// Log writes one audit row. Best-effort: errors are logged via slog and
// swallowed. Must NOT be called inside a request transaction that might
// rollback — audit is an out-of-band record.
//
// Resolves the actor from the Principal in ctx:
//   - SourceSession → actor_user_id + actor_email_snapshot (denormalized email)
//   - SourceAPIKey  → actor_api_key_id
//
// When no Principal is in ctx (e.g. CLI bootstrap) the call is a no-op with a
// warning; out-of-band privileged actions should not flow through here.
func Log(ctx context.Context, q *store.Queries, log *slog.Logger, e Entry) {
	if log == nil {
		log = slog.Default()
	}
	p, err := auth.FromContext(ctx)
	if err != nil {
		log.Warn("audit: no principal in ctx", "action", e.Action)
		return
	}

	var (
		actorUser pgtype.UUID
		actorKey  pgtype.UUID
		emailSnap *string
	)
	switch p.Source {
	case auth.SourceSession:
		actorUser = store.UUID(p.UserID)
		// Email was captured by Authenticate when the session was resolved.
		// Stamping it onto the audit row keeps the trail readable after a
		// future DELETE on users. No fresh GetUserByID per mutation.
		if p.UserEmail != "" {
			s := p.UserEmail
			emailSnap = &s
		}
	case auth.SourceAPIKey:
		actorKey = store.UUID(p.APIKeyID)
	default:
		log.Warn("audit: unknown principal source", "source", p.Source, "action", e.Action)
		return
	}

	var tid pgtype.UUID
	if e.TargetID != nil {
		tid = store.UUID(*e.TargetID)
	}

	// Resolve the owning org. Prefer Entry.OrgID when the caller knows the
	// target's home (URL path scoped, super-admin acting cross-tenant);
	// fall back to the caller's active session-org. We refuse to insert
	// with no usable org id rather than FK-violate against organizations.
	orgID := e.OrgID
	if orgID == uuid.Nil {
		orgID = p.OrgID
	}
	if orgID == uuid.Nil {
		log.Warn("audit: no org id resolved", "action", e.Action)
		return
	}

	meta := []byte("{}")
	if e.Metadata != nil {
		if b, jerr := json.Marshal(e.Metadata); jerr == nil {
			meta = b
		} else {
			log.Warn("audit: metadata marshal failed", "err", jerr, "action", e.Action)
		}
	}

	if err := q.InsertAuditLog(ctx, store.InsertAuditLogParams{
		OrgID:              store.UUID(orgID),
		ActorUserID:        actorUser,
		ActorApiKeyID:      actorKey,
		ActorEmailSnapshot: emailSnap,
		Action:             e.Action,
		TargetType:         e.TargetType,
		TargetID:           tid,
		Metadata:           meta,
	}); err != nil {
		log.Warn("audit: insert failed", "err", err, "action", e.Action, "org_id", orgID)
	}
}

// PtrUUID is a small convenience so call sites can pass an addressable
// *uuid.UUID to Entry.TargetID without ceremony.
func PtrUUID(u uuid.UUID) *uuid.UUID { return &u }
