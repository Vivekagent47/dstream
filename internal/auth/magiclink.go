package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/opevents"
	"github.com/Vivekagent47/dstream/internal/store"
)

// TxBeginner is satisfied by *pgxpool.Pool (and *pgx.Conn). Threading
// just this interface — rather than the full pool — keeps ConsumeMagicLink
// testable with a mock and keeps auth's dependency surface narrow.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

var ErrInvalidMagicToken = errors.New("auth: invalid or expired magic link")

// ErrDefaultOrgNotFound means the configured default org slug matches no
// organization. It is an operator misconfiguration, not an auth failure —
// callers should surface it as a 500, never as a 401.
var ErrDefaultOrgNotFound = errors.New("auth: configured default org not found")

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
//  2. BootstrapSession: get-or-create the user, apply pending invites, and
//     mint a personal workspace if they are still org-less. Shared with
//     every other login method; no default org applies to magic links.
//  3. Mark the magic-link token used.
//  4. Commit. Pick the active org deterministically via GetFirstOrgForUser.
//
// The whole thing runs in a transaction so a partial failure (e.g. ctx
// cancellation between step 2 and step 3) rolls back cleanly — no orphan
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

	u, err := BootstrapSession(ctx, tx, q, row.Email, "", RoleMember)
	if err != nil {
		return store.User{}, uuid.Nil, err
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

// BootstrapSession turns a verified email address into a user holding at
// least one org membership, inside the caller's transaction. It is the shared
// account-provisioning path for every human login method — magic-link
// redemption and the OIDC callback both route through here, so they cannot
// drift on invite application, workspace creation, or the rule that a fresh
// login never escalates an existing member's role.
//
// defaultOrgSlug, when non-empty, joins a user who has no membership to that
// org at defaultRole instead of minting a personal workspace. An existing
// membership always wins: this function never changes a role a user already
// holds. An unknown slug is an error, not a silent fallback — it means the
// deployment is misconfigured.
//
// Callers own the transaction so the whole login is atomic: the magic-link
// path also marks its token used, and a partial failure must leave no orphan
// workspace and no consumed-but-unbootstrapped token.
func BootstrapSession(
	ctx context.Context,
	tx pgx.Tx,
	q *store.Queries,
	email string,
	defaultOrgSlug string,
	defaultRole Role,
) (store.User, error) {
	qtx := q.WithTx(tx)

	// Get-or-create the user. CreateUser may race against another concurrent
	// login for the same email — handle the unique violation by reloading
	// rather than failing.
	u, err := qtx.GetUserByEmail(ctx, email)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return store.User{}, err
		}
		violated, sErr := inSavepoint(ctx, tx, func(sp pgx.Tx) error {
			var e error
			u, e = q.WithTx(sp).CreateUser(ctx, store.CreateUserParams{Email: email})
			return e
		})
		if sErr != nil {
			return store.User{}, sErr
		}
		if violated {
			// Concurrent create with the same email — fetch the existing row.
			if u, err = qtx.GetUserByEmail(ctx, email); err != nil {
				return store.User{}, err
			}
		}
	}

	// Apply pending invites. Best-effort per invite: a unique violation means
	// they are already a member, in which case the existing role is preserved
	// rather than overwritten — invite acceptance is idempotent and does not
	// silently re-grade an established member. Other errors abort the tx.
	invites, err := qtx.ListPendingOrgInvitesByEmail(ctx, u.Email)
	if err != nil {
		return store.User{}, err
	}
	for _, inv := range invites {
		if _, err := inSavepoint(ctx, tx, func(sp pgx.Tx) error {
			return q.WithTx(sp).AddOrgMember(ctx, store.AddOrgMemberParams{
				OrgID:  inv.OrgID,
				UserID: u.ID,
				Role:   inv.Role,
			})
		}); err != nil {
			return store.User{}, err
		}
		if err := qtx.MarkOrgInviteAccepted(ctx, inv.ID); err != nil {
			return store.User{}, err
		}
	}

	count, err := qtx.CountOrgMembershipsForUser(ctx, u.ID)
	if err != nil {
		return store.User{}, err
	}
	if count > 0 {
		// Already a member of something. Never touch an existing role.
		return u, nil
	}

	if defaultOrgSlug != "" {
		org, err := qtx.GetOrganizationBySlug(ctx, defaultOrgSlug)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.User{}, fmt.Errorf("%w: %q", ErrDefaultOrgNotFound, defaultOrgSlug)
			}
			return store.User{}, err
		}
		// Savepoint-wrapped like the invite loop above: unlike the personal
		// workspace below (whose org is created in this tx, so its id is
		// unguessable), two concurrent logins for the same org-less user race
		// to insert the *same* (org_id, user_id). AddOrgMember is a plain
		// INSERT, so the loser takes 23505 and would abort the whole tx. The
		// violation means they are already a member — exactly what we wanted.
		if _, err := inSavepoint(ctx, tx, func(sp pgx.Tx) error {
			return q.WithTx(sp).AddOrgMember(ctx, store.AddOrgMemberParams{
				OrgID:  org.ID,
				UserID: u.ID,
				Role:   string(defaultRole),
			})
		}); err != nil {
			return store.User{}, err
		}
		return u, nil
	}

	// Still org-less and no default configured: mint a personal workspace.
	org, err := qtx.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: personalOrgName(u.Email),
		Slug: slugifyEmail(u.Email),
	})
	if err != nil {
		return store.User{}, err
	}
	if err := qtx.AddOrgMember(ctx, store.AddOrgMemberParams{
		OrgID:  org.ID,
		UserID: u.ID,
		Role:   string(RoleOwner),
	}); err != nil {
		return store.User{}, err
	}
	if _, err := opevents.SeedOperationalApp(ctx, qtx, store.GoUUID(org.ID)); err != nil {
		return store.User{}, err
	}
	return u, nil
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
		suffix = fallbackSuffix(email, time.Now())
	}
	return string(b) + "-" + hex.EncodeToString(suffix[:])
}

// fallbackSuffix derives slugifyEmail's suffix from the email and the clock
// when the system RNG is unavailable.
func fallbackSuffix(email string, now time.Time) (suffix [6]byte) {
	h := sha256.Sum256([]byte(email + now.String()))
	copy(suffix[:], h[:6])
	return suffix
}

// inSavepoint runs fn inside a nested transaction (a SAVEPOINT). A failed
// statement aborts the whole Postgres tx, so callers that want to catch a
// unique violation and keep using the outer tx must isolate the statement
// here. On a unique violation, the savepoint is rolled back (leaving the
// outer tx usable) and (true, nil) is returned. Any other error rolls back
// and is returned. On success the savepoint is released.
func inSavepoint(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) (violated bool, err error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	if e := fn(sp); e != nil {
		if rb := sp.Rollback(ctx); rb != nil {
			return false, rb
		}
		if isUniqueViolation(e) {
			return true, nil
		}
		return false, e
	}
	return false, sp.Commit(ctx)
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
