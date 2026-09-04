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
// Already-a-member is treated as success (idempotent): we mark the invite
// accepted but do NOT change the user's existing role — a re-issued
// higher-role invite does not silently escalate privilege. Role changes go
// through member management.
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
	// Add the member. Already-a-member (unique violation) is fine — invite
	// acceptance is idempotent and does NOT change an existing member's role
	// (see package doc). Isolate the INSERT in a savepoint so the violation
	// doesn't poison the outer tx.
	if _, err := inSavepoint(ctx, tx, func(sp pgx.Tx) error {
		return q.WithTx(sp).AddOrgMember(ctx, store.AddOrgMemberParams{
			OrgID:  row.OrgID,
			UserID: store.UUID(userID),
			Role:   row.Role,
		})
	}); err != nil {
		return store.GetActiveOrgInviteByTokenHashRow{}, err
	}
	if err := qtx.MarkOrgInviteAccepted(ctx, row.ID); err != nil {
		return store.GetActiveOrgInviteByTokenHashRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.GetActiveOrgInviteByTokenHashRow{}, err
	}
	return row, nil
}
