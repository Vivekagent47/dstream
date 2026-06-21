package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/store"
)

// TestIssueOrgInvite_StoresHashedToken verifies that IssueOrgInvite returns
// a plaintext token whose sha256 hash is the only thing persisted — i.e. a
// DB leak can't be replayed against the accept endpoint.
func TestIssueOrgInvite_StoresHashedToken(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()

	inviterUID, orgID := seedUserAndOrg(t, q, RoleOwner)
	email := "invitee+" + uuid.NewString() + "@example.test"

	tok, err := IssueOrgInvite(ctx, q, orgID, inviterUID, email, RoleMember, time.Hour)
	if err != nil {
		t.Fatalf("IssueOrgInvite: %v", err)
	}
	if tok == "" {
		t.Fatal("expected non-empty token")
	}

	// Token should resolve via its hash, not its plaintext.
	h := sha256.Sum256([]byte(tok))
	row, err := q.GetActiveOrgInviteByTokenHash(ctx, h[:])
	if err != nil {
		t.Fatalf("GetActiveOrgInviteByTokenHash: %v", err)
	}
	if row.Email != email {
		t.Errorf("email: got %q want %q", row.Email, email)
	}
	if row.Role != string(RoleMember) {
		t.Errorf("role: got %q want %q", row.Role, RoleMember)
	}
	// The stored hash must be the sha256 of the plaintext token, not the
	// plaintext itself.
	if string(row.TokenHash) == tok {
		t.Errorf("token stored in plaintext")
	}
	if len(row.TokenHash) != sha256.Size {
		t.Errorf("token_hash length: got %d want %d", len(row.TokenHash), sha256.Size)
	}
}

// TestConsumeOrgInvite_AddsMemberMarksAccepted verifies the happy path:
// invite resolves, the invitee becomes a member at the invited role, and
// the invite row is no longer pending.
func TestConsumeOrgInvite_AddsMemberMarksAccepted(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()

	inviterUID, orgID := seedUserAndOrg(t, q, RoleOwner)
	inviteeEmail := "invitee+" + uuid.NewString() + "@example.test"
	invitee, err := q.CreateUser(ctx, store.CreateUserParams{Email: inviteeEmail})
	if err != nil {
		t.Fatalf("create invitee: %v", err)
	}
	tok, err := IssueOrgInvite(ctx, q, orgID, inviterUID, inviteeEmail, RoleAdmin, time.Hour)
	if err != nil {
		t.Fatalf("IssueOrgInvite: %v", err)
	}

	row, err := ConsumeOrgInvite(ctx, q, tok, store.GoUUID(invitee.ID))
	if err != nil {
		t.Fatalf("ConsumeOrgInvite: %v", err)
	}
	if store.GoUUID(row.OrgID) != orgID {
		t.Errorf("row.OrgID: got %s want %s", store.GoUUID(row.OrgID), orgID)
	}
	if row.Role != string(RoleAdmin) {
		t.Errorf("role: got %q want %q", row.Role, RoleAdmin)
	}

	m, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: invitee.ID,
	})
	if err != nil {
		t.Fatalf("GetOrgMember: %v", err)
	}
	if m.Role != string(RoleAdmin) {
		t.Errorf("member role: got %q want %q", m.Role, RoleAdmin)
	}

	// Pending list for this email should be empty post-accept.
	pending, err := q.ListPendingOrgInvitesByEmail(ctx, inviteeEmail)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending invites: got %d want 0", len(pending))
	}
}

// TestConsumeOrgInvite_AlreadyMember_StillAccepts verifies that a user who
// is already a member (e.g. added out-of-band) can still "consume" the
// invite — the row just gets marked accepted with no membership change.
func TestConsumeOrgInvite_AlreadyMember_StillAccepts(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()

	inviterUID, orgID := seedUserAndOrg(t, q, RoleOwner)
	inviteeEmail := "invitee+" + uuid.NewString() + "@example.test"
	invitee, err := q.CreateUser(ctx, store.CreateUserParams{Email: inviteeEmail})
	if err != nil {
		t.Fatalf("create invitee: %v", err)
	}
	// Pre-add as member.
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: invitee.ID,
		Role:   string(RoleMember),
	}); err != nil {
		t.Fatalf("pre-add member: %v", err)
	}
	tok, err := IssueOrgInvite(ctx, q, orgID, inviterUID, inviteeEmail, RoleAdmin, time.Hour)
	if err != nil {
		t.Fatalf("IssueOrgInvite: %v", err)
	}

	if _, err := ConsumeOrgInvite(ctx, q, tok, store.GoUUID(invitee.ID)); err != nil {
		t.Fatalf("ConsumeOrgInvite: %v", err)
	}

	// Role should remain at the pre-existing value (we don't upgrade
	// silently — invite acceptance is idempotent on already-member).
	m, _ := q.GetOrgMember(ctx, store.GetOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: invitee.ID,
	})
	if m.Role != string(RoleMember) {
		t.Errorf("role: got %q want %q (pre-existing role preserved)", m.Role, RoleMember)
	}
}

// TestConsumeOrgInvite_Expired_Returns_ErrInvalid verifies that an expired
// invite is rejected with the sentinel error rather than silently consumed.
func TestConsumeOrgInvite_Expired_Returns_ErrInvalid(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()

	inviterUID, orgID := seedUserAndOrg(t, q, RoleOwner)
	inviteeEmail := "expired+" + uuid.NewString() + "@example.test"
	invitee, err := q.CreateUser(ctx, store.CreateUserParams{Email: inviteeEmail})
	if err != nil {
		t.Fatalf("create invitee: %v", err)
	}

	// Stage the invite directly with a past expires_at so we don't have to
	// sleep. Issue/Consume would otherwise refuse to create an already-dead
	// invite via the convenience helper.
	tokRaw := uuid.NewString() // any non-empty plaintext is fine
	h := sha256.Sum256([]byte(tokRaw))
	if _, err := q.CreateOrgInvite(ctx, store.CreateOrgInviteParams{
		OrgID:     store.UUID(orgID),
		Email:     inviteeEmail,
		Role:      string(RoleMember),
		TokenHash: h[:],
		InvitedBy: store.UUID(inviterUID),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("create expired invite: %v", err)
	}

	if _, err := ConsumeOrgInvite(ctx, q, tokRaw, store.GoUUID(invitee.ID)); !errors.Is(err, ErrInvalidOrgInvite) {
		t.Fatalf("err: got %v, want ErrInvalidOrgInvite", err)
	}
}

// TestConsumeOrgInvite_AlreadyAccepted_Returns_ErrInvalid verifies that
// re-using an accepted invite fails — single-use semantics.
func TestConsumeOrgInvite_AlreadyAccepted_Returns_ErrInvalid(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()

	inviterUID, orgID := seedUserAndOrg(t, q, RoleOwner)
	inviteeEmail := "twice+" + uuid.NewString() + "@example.test"
	invitee, err := q.CreateUser(ctx, store.CreateUserParams{Email: inviteeEmail})
	if err != nil {
		t.Fatalf("create invitee: %v", err)
	}
	tok, err := IssueOrgInvite(ctx, q, orgID, inviterUID, inviteeEmail, RoleMember, time.Hour)
	if err != nil {
		t.Fatalf("IssueOrgInvite: %v", err)
	}

	if _, err := ConsumeOrgInvite(ctx, q, tok, store.GoUUID(invitee.ID)); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := ConsumeOrgInvite(ctx, q, tok, store.GoUUID(invitee.ID)); !errors.Is(err, ErrInvalidOrgInvite) {
		t.Fatalf("second consume err: got %v, want ErrInvalidOrgInvite", err)
	}
}

// TestConsumeOrgInvite_UnknownToken_Returns_ErrInvalid sanity-checks the
// rejection path for a token that was never issued.
func TestConsumeOrgInvite_UnknownToken_Returns_ErrInvalid(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()

	_, _ = seedUserAndOrg(t, q, RoleOwner)
	bogusUser := uuid.New()

	if _, err := ConsumeOrgInvite(ctx, q, "not-a-real-token", bogusUser); !errors.Is(err, ErrInvalidOrgInvite) {
		t.Fatalf("err: got %v, want ErrInvalidOrgInvite", err)
	}
}
