// Package auth — invite issuance and consumption.
//
// Org invites are short-lived (default 7d), single-use bearer tokens. We
// store only the sha256 hash so a DB read can't be replayed against the
// public surface. The plaintext token is returned exactly once from
// IssueOrgInvite — callers embed it in the email link.
//
// Consumption is intentionally idempotent w.r.t. membership: if the user is
// already a member of the org (e.g. they were added out-of-band), we still
// mark the invite accepted so it doesn't linger. Other write failures abort.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/store"
)

// ErrInvalidOrgInvite is returned when an invite token doesn't match any
// active row (unknown, expired, or already accepted). Handlers map this to
// 404 to avoid leaking which path triggered the failure.
var ErrInvalidOrgInvite = errors.New("auth: invalid or expired org invite")

const orgInviteTokenBytes = 32

// IssueOrgInvite generates a fresh single-use token, stores its sha256 hash
// alongside the invite row, and returns the human-deliverable token. The
// caller is expected to embed the token in an email link of the form
// /invites/{token}.
//
// Caller is responsible for upstream permission + rate-limit checks.
func IssueOrgInvite(
	ctx context.Context, q *store.Queries,
	orgID, invitedBy uuid.UUID,
	email string, role Role, ttl time.Duration,
) (token string, err error) {
	b := make([]byte, orgInviteTokenBytes)
	if _, err = rand.Read(b); err != nil {
		return "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(token))

	if _, err = q.CreateOrgInvite(ctx, store.CreateOrgInviteParams{
		OrgID:     store.UUID(orgID),
		Email:     email,
		Role:      string(role),
		TokenHash: h[:],
		InvitedBy: store.UUID(invitedBy),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(ttl), Valid: true},
	}); err != nil {
		return "", err
	}
	return token, nil
}

// ConsumeOrgInvite validates a presented token, adds the user as a member at
// the invite's role, and marks the invite accepted. Returns the loaded
// invite row so the caller can use the org_id / role for cookie + UI hand-off.
//
// Already-a-member is treated as success: instead of stranding the invite,
// we upgrade the user's existing role IF the invite grants more privilege
// than they currently have. A re-issued admin invite for a `member` upgrades
// them to `admin`; a re-issued `member` invite for an existing `admin` does
// NOT demote. Never lateral-moves between owner and admin via this path
// (ownership changes go through the transfer flow).
func ConsumeOrgInvite(
	ctx context.Context, pool TxBeginner, q *store.Queries,
	token string, userID uuid.UUID,
) (store.GetActiveOrgInviteByTokenHashRow, error) {
	// One transaction so lookup → add-member → mark-accepted serialize against a
	// concurrent consumer of the same token. GetActiveOrgInviteByTokenHash is
	// `FOR UPDATE OF i`, so a second consumer blocks on the invite row and, after
	// this tx commits, re-evaluates `accepted_at IS NULL` to zero rows — enforcing
	// single-use atomically. Mirrors ConsumeMagicLink (audit #21).
	tx, err := pool.Begin(ctx)
	if err != nil {
		return store.GetActiveOrgInviteByTokenHashRow{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit (ErrTxClosed swallowed)
	qtx := q.WithTx(tx)

	h := sha256.Sum256([]byte(token))
	row, err := qtx.GetActiveOrgInviteByTokenHash(ctx, h[:])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.GetActiveOrgInviteByTokenHashRow{}, ErrInvalidOrgInvite
		}
		return store.GetActiveOrgInviteByTokenHashRow{}, err
	}
	if err := qtx.AddOrgMember(ctx, store.AddOrgMemberParams{
		OrgID:  row.OrgID,
		UserID: store.UUID(userID),
		Role:   row.Role,
	}); err != nil {
		if !isUniqueViolation(err) {
			return store.GetActiveOrgInviteByTokenHashRow{}, err
		}
		// Already a member. Upgrade existing role if the invite grants
		// more privilege. We do NOT downgrade — a re-issued lower-role
		// invite must not strip privilege the user already holds.
		if err := upgradeMemberRole(ctx, qtx, row.OrgID, store.UUID(userID), Role(row.Role)); err != nil {
			return store.GetActiveOrgInviteByTokenHashRow{}, err
		}
	}
	if err := qtx.MarkOrgInviteAccepted(ctx, row.ID); err != nil {
		return store.GetActiveOrgInviteByTokenHashRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.GetActiveOrgInviteByTokenHashRow{}, err
	}
	return row, nil
}

// upgradeMemberRole bumps an existing member to `target` IFF target is
// strictly higher than the existing role. We refuse to set "owner" through
// this path — ownership transitions must go through the dedicated transfer
// flow with its last-owner guard.
func upgradeMemberRole(
	ctx context.Context, q *store.Queries,
	orgID, userID pgtype.UUID, target Role,
) error {
	if target == RoleOwner {
		return nil
	}
	existing, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{
		OrgID:  orgID,
		UserID: userID,
	})
	if err != nil {
		return err
	}
	current := Role(existing.Role)
	if !current.LessThan(target) {
		return nil
	}
	return q.UpdateOrgMemberRole(ctx, store.UpdateOrgMemberRoleParams{
		OrgID:  orgID,
		UserID: userID,
		Role:   string(target),
	})
}
