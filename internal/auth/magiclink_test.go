package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/store"
)

func TestSlugifyEmail(t *testing.T) {
	cases := []struct {
		email      string
		wantPrefix string
	}{
		{"alice@example.com", "alice-"},
		{"ALICE+work@example.com", "alicework-"}, // '+' stripped, rest lowercased
		{"@only-domain.com", "user-"},            // empty local-part → fallback
		{"!!!@x.com", "user-"},                   // no safe chars → fallback
		{"123-abc@x.com", "123-abc-"},
	}
	for _, c := range cases {
		got := slugifyEmail(c.email)
		if !strings.HasPrefix(got, c.wantPrefix) {
			t.Errorf("slugifyEmail(%q) = %q; want prefix %q", c.email, got, c.wantPrefix)
		}
		// Suffix is 6-byte (12-hex-char) random; total length = prefix + 12.
		if len(got) != len(c.wantPrefix)+12 {
			t.Errorf("slugifyEmail(%q) = %q; unexpected length %d", c.email, got, len(got))
		}
	}
}

// --- DB-gated integration coverage ---

// seedMagicLink stages a magic-link token row for the given email and
// returns the raw token string the test would present back to
// ConsumeMagicLink.
func seedMagicLink(t *testing.T, q *store.Queries, email string) string {
	t.Helper()
	tok, err := IssueMagicLink(context.Background(), q, email, 10*time.Minute)
	if err != nil {
		t.Fatalf("issue magic link: %v", err)
	}
	return tok
}

func TestConsumeMagicLink_NewUser_CreatesPersonalOrg(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)

	email := "newuser+" + uuid.NewString() + "@example.test"
	tok := seedMagicLink(t, q, email)

	u, orgID, err := ConsumeMagicLink(context.Background(), pool, q, tok)
	if err != nil {
		t.Fatalf("ConsumeMagicLink: %v", err)
	}
	if u.Email != email {
		t.Errorf("user email: got %q, want %q", u.Email, email)
	}
	if orgID == uuid.Nil {
		t.Fatal("orgID is uuid.Nil; want a real org")
	}
	// User should be owner of the returned org.
	m, err := q.GetOrgMember(context.Background(), store.GetOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: u.ID,
	})
	if err != nil {
		t.Fatalf("GetOrgMember: %v", err)
	}
	if m.Role != string(RoleOwner) {
		t.Errorf("role: got %q, want %q", m.Role, RoleOwner)
	}
}

func TestConsumeMagicLink_ExistingUser_ReturnsTheirOrg(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)

	uid, oid := seedUserAndOrg(t, q, RoleAdmin)
	u, err := q.GetUserByID(context.Background(), store.UUID(uid))
	if err != nil {
		t.Fatalf("get user: %v", err)
	}

	tok := seedMagicLink(t, q, u.Email)
	gotUser, gotOrg, err := ConsumeMagicLink(context.Background(), pool, q, tok)
	if err != nil {
		t.Fatalf("ConsumeMagicLink: %v", err)
	}
	if store.GoUUID(gotUser.ID) != uid {
		t.Errorf("user id: got %s, want %s", store.GoUUID(gotUser.ID), uid)
	}
	if gotOrg != oid {
		t.Errorf("active org: got %s, want %s", gotOrg, oid)
	}
}

func TestConsumeMagicLink_PendingInvite_AutoJoins(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()

	// Pre-existing org + inviter (owner so the invite has a valid invited_by).
	inviterUID, inviterOrgID := seedUserAndOrg(t, q, RoleOwner)
	_ = inviterUID

	// Invitee email — not yet a user.
	inviteeEmail := "invitee+" + uuid.NewString() + "@example.test"

	// Stage an invite addressed to that email.
	if _, err := q.CreateOrgInvite(ctx, store.CreateOrgInviteParams{
		OrgID:     store.UUID(inviterOrgID),
		Email:     inviteeEmail,
		Role:      string(RoleMember),
		TokenHash: []byte("test-invite-hash-" + uuid.NewString()),
		InvitedBy: store.UUID(inviterUID),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("create invite: %v", err)
	}

	tok := seedMagicLink(t, q, inviteeEmail)
	u, orgID, err := ConsumeMagicLink(ctx, pool, q, tok)
	if err != nil {
		t.Fatalf("ConsumeMagicLink: %v", err)
	}

	// Invitee should be a member of the inviter's org.
	m, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{
		OrgID:  store.UUID(inviterOrgID),
		UserID: u.ID,
	})
	if err != nil {
		t.Fatalf("GetOrgMember: %v", err)
	}
	if m.Role != string(RoleMember) {
		t.Errorf("role: got %q, want %q", m.Role, RoleMember)
	}

	// Active org must be deterministic — the invited org has the older
	// created_at than the (still-not-created) personal workspace, so the
	// invited org wins... but actually since they had no other org,
	// ConsumeMagicLink should NOT have created a personal workspace.
	// First-by-created_at then by org_id is the inviter org.
	if orgID != inviterOrgID {
		t.Errorf("active org: got %s, want %s (the invited org)", orgID, inviterOrgID)
	}

	// The invite should be marked accepted.
	pending, err := q.ListPendingOrgInvitesByEmail(ctx, inviteeEmail)
	if err != nil {
		t.Fatalf("list pending invites: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending invites after consume: got %d, want 0", len(pending))
	}

	// And no personal workspace should have been created — only the
	// invited org.
	memberships, err := q.ListOrgsForUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("list orgs: %v", err)
	}
	if len(memberships) != 1 {
		t.Errorf("memberships: got %d, want 1 (invite only, no personal)", len(memberships))
	}
}

func TestConsumeMagicLink_InvalidToken(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)

	_, _, err := ConsumeMagicLink(context.Background(), pool, q, "not-a-real-token")
	if err != ErrInvalidMagicToken {
		t.Fatalf("err: got %v, want ErrInvalidMagicToken", err)
	}
}
