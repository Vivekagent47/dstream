package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/store"
)

// TxBeginner is satisfied by *pgxpool.Pool (and *pgx.Conn). Threading
// just this interface — rather than the full pool — keeps ConsumeMagicLink
// testable with a mock and keeps auth's dependency surface narrow.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

var ErrInvalidMagicToken = errors.New("auth: invalid or expired magic link")

const magicTokenBytes = 32

// IssueMagicLink generates a token, persists its hash, and returns the
// human-deliverable token (caller embeds it in an email link).
func IssueMagicLink(ctx context.Context, q *store.Queries, email string, ttl time.Duration) (token string, err error) {
	b := make([]byte, magicTokenBytes)
	if _, err = rand.Read(b); err != nil {
		return "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(token))

	if _, err = q.CreateMagicLinkToken(ctx, store.CreateMagicLinkTokenParams{
		Email:     email,
		TokenHash: h[:],
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(ttl), Valid: true},
	}); err != nil {
		return "", err
	}
	return token, nil
}

// ConsumeMagicLink validates a presented token and returns the user it
// belongs to (creating the user if they're new), the active_org_id to embed
// in the session cookie, and any error. The token is single-use.
//
// Bootstrap flow inside ONE Postgres transaction:
//
//  1. Load + validate the magic-link row.
//  2. Get-or-create the user (race-tolerant via unique-violation handling).
//  3. Apply any pending org_invites for the user's email — upgrade role
//     when the invite grants more privilege than current membership.
//  4. If the user is still not a member of any org, create a personal
//     workspace and add them as owner.
//  5. Mark the magic-link token used.
//  6. Commit. Pick the active org deterministically via GetFirstOrgForUser.
//
// The whole thing runs in a transaction so a partial failure (e.g. ctx
// cancellation between step 4 and step 5) rolls back cleanly — no orphan
// workspaces with zero members, no consumed-but-unbootstrapped tokens.
func ConsumeMagicLink(ctx context.Context, pool TxBeginner, q *store.Queries, token string) (store.User, uuid.UUID, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return store.User{}, uuid.Nil, err
	}
	defer func() {
		// Best-effort rollback. If the function returns via the happy
		// path the tx is already committed; Rollback on a committed tx
		// returns ErrTxClosed which we deliberately swallow.
		_ = tx.Rollback(ctx)
	}()
	qtx := q.WithTx(tx)

	h := sha256.Sum256([]byte(token))
	row, err := qtx.GetActiveMagicLinkToken(ctx, h[:])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.User{}, uuid.Nil, ErrInvalidMagicToken
		}
		return store.User{}, uuid.Nil, err
	}

	// Get-or-create the user. CreateUser may race against another
	// concurrent verify for the same email — handle the unique violation
	// by reloading rather than 500ing.
	u, err := qtx.GetUserByEmail(ctx, row.Email)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return store.User{}, uuid.Nil, err
		}
		u, err = qtx.CreateUser(ctx, store.CreateUserParams{Email: row.Email})
		if err != nil {
			if !isUniqueViolation(err) {
				return store.User{}, uuid.Nil, err
			}
			u, err = qtx.GetUserByEmail(ctx, row.Email)
			if err != nil {
				return store.User{}, uuid.Nil, err
			}
		}
	}

	// Apply pending invites. Best-effort per invite: if adding a member
	// fails on unique violation (already a member), upgrade role rather
	// than 500ing. Other errors abort the whole tx.
	invites, err := qtx.ListPendingOrgInvitesByEmail(ctx, u.Email)
	if err != nil {
		return store.User{}, uuid.Nil, err
	}
	for _, inv := range invites {
		if err := qtx.AddOrgMember(ctx, store.AddOrgMemberParams{
			OrgID:  inv.OrgID,
			UserID: u.ID,
			Role:   inv.Role,
		}); err != nil {
			if !isUniqueViolation(err) {
				return store.User{}, uuid.Nil, err
			}
			if err := upgradeMemberRole(ctx, qtx, inv.OrgID, u.ID, Role(inv.Role)); err != nil {
				return store.User{}, uuid.Nil, err
			}
		}
		if err := qtx.MarkOrgInviteAccepted(ctx, inv.ID); err != nil {
			return store.User{}, uuid.Nil, err
		}
	}

	// If still org-less, mint a personal workspace.
	count, err := qtx.CountOrgMembershipsForUser(ctx, u.ID)
	if err != nil {
		return store.User{}, uuid.Nil, err
	}
	if count == 0 {
		org, err := qtx.CreateOrganization(ctx, store.CreateOrganizationParams{
			Name: personalOrgName(u.Email),
			Slug: slugifyEmail(u.Email),
		})
		if err != nil {
			return store.User{}, uuid.Nil, err
		}
		if err := qtx.AddOrgMember(ctx, store.AddOrgMemberParams{
			OrgID:  org.ID,
			UserID: u.ID,
			Role:   string(RoleOwner),
		}); err != nil {
			return store.User{}, uuid.Nil, err
		}
	}

	// Mark the magic-link token used LAST — only after the bootstrap
	// fully succeeds. If any step above errored, the rollback above
	// leaves the token unused so the user can retry.
	if err := qtx.MarkMagicLinkUsed(ctx, row.ID); err != nil {
		return store.User{}, uuid.Nil, err
	}

	// Pick the active org deterministically — STILL inside the tx so we
	// see our own writes (the just-added membership / personal workspace).
	activeOrg, err := qtx.GetFirstOrgForUser(ctx, u.ID)
	if err != nil {
		return store.User{}, uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.User{}, uuid.Nil, err
	}
	return u, store.GoUUID(activeOrg), nil
}

// personalOrgName returns the human-facing display name for a new
// auto-created workspace, e.g. "alice@example.com's Workspace".
func personalOrgName(email string) string {
	return email + "'s Workspace"
}

// slugifyEmail derives a URL-safe slug from an email address. The output is
// always non-empty and includes a 6-byte (48-bit) random hex suffix. The
// suffix has two jobs:
//   - collision avoidance for users sharing a local-part (alice@a.com vs
//     alice@b.com)
//   - keeping the slug unguessable enough that an attacker who knows the
//     email can't enumerate the personal-workspace slug by brute-force.
//     3 bytes (16M) was online-feasible; 6 bytes (281T) is not.
func slugifyEmail(email string) string {
	local := strings.Split(email, "@")[0]
	b := make([]byte, 0, len(local))
	for _, r := range strings.ToLower(local) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b = append(b, byte(r))
		}
	}
	if len(b) == 0 {
		b = []byte("user")
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		// crypto/rand.Read failing is catastrophic — fall back to a
		// hash of the email + time hash so we at least don't return an
		// all-zero suffix that collides for every user.
		h := sha256.Sum256([]byte(email + time.Now().String()))
		copy(suffix[:], h[:6])
	}
	return string(b) + "-" + hex.EncodeToString(suffix[:])
}

// isUniqueViolation detects Postgres unique_violation (SQLSTATE 23505).
// Uses errors.As against *pgconn.PgError so a future change to pgx error
// formatting can't silently break the bootstrap loop (the old substring
// sniff on "23505" was brittle: a constraint name happening to contain
// "23505" would have matched).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
