package auth

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

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

// --- BootstrapSession ---

// runBootstrap wraps BootstrapSession in its own committed transaction, the
// way a login handler does.
func runBootstrap(t *testing.T, pool *pgxpool.Pool, q *store.Queries, email, defaultOrgSlug string, defaultRole Role) store.User {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	u, err := BootstrapSession(ctx, tx, q, email, defaultOrgSlug, defaultRole)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("BootstrapSession: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return u
}

func seedOrg(t *testing.T, q *store.Queries) store.Organization {
	t.Helper()
	o, err := q.CreateOrganization(context.Background(), store.CreateOrganizationParams{
		Name: "Default Co",
		Slug: "default-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return o
}

// seedPendingInvite stages an unexpired, unaccepted invite for email. invited_by
// is NOT NULL with an FK to users, so an inviter is minted alongside it.
func seedPendingInvite(t *testing.T, q *store.Queries, orgID pgtype.UUID, email, role string) {
	t.Helper()
	ctx := context.Background()
	inviter, err := q.CreateUser(ctx, store.CreateUserParams{
		Email: "inviter+" + uuid.NewString() + "@example.test",
	})
	if err != nil {
		t.Fatalf("create inviter: %v", err)
	}
	if _, err := q.CreateOrgInvite(ctx, store.CreateOrgInviteParams{
		OrgID:     orgID,
		Email:     email,
		Role:      role,
		TokenHash: []byte("test-invite-hash-" + uuid.NewString()),
		InvitedBy: inviter.ID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("create invite: %v", err)
	}
}

// BootstrapSession is the shared provisioning path for every human login
// method, so these cover it directly rather than through a magic link.
func TestBootstrapSession_NewUserNoDefaultOrg(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := "boot+" + uuid.NewString() + "@example.test"

	u := runBootstrap(t, pool, q, email, "", RoleMember)

	if u.Email != email {
		t.Fatalf("email: got %q want %q", u.Email, email)
	}
	// No default org configured: the user gets a personal workspace as owner,
	// matching magic-link behavior exactly.
	orgs, err := q.ListOrgsForUser(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("list orgs: %v", err)
	}
	if len(orgs) != 1 {
		t.Fatalf("orgs: got %d want 1", len(orgs))
	}
	if orgs[0].Role != string(RoleOwner) {
		t.Errorf("role: got %q want owner", orgs[0].Role)
	}
}

func TestBootstrapSession_DefaultOrgJoinsAtConfiguredRole(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	org := seedOrg(t, q)
	email := "boot+" + uuid.NewString() + "@example.test"

	u := runBootstrap(t, pool, q, email, org.Slug, RoleMember)

	orgs, err := q.ListOrgsForUser(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("list orgs: %v", err)
	}
	// Exactly the default org, and no personal workspace minted alongside it.
	if len(orgs) != 1 || store.GoUUID(orgs[0].ID) != store.GoUUID(org.ID) {
		t.Fatalf("expected membership in the default org, got %+v", orgs)
	}
	if orgs[0].Role != string(RoleMember) {
		t.Errorf("role: got %q want member", orgs[0].Role)
	}
}

// The no-escalation rule: an existing membership always wins over the
// configured default. Otherwise flipping one env var would silently re-grade
// every existing member at their next login.
func TestBootstrapSession_ExistingMembershipNotEscalated(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	org := seedOrg(t, q)
	email := "boot+" + uuid.NewString() + "@example.test"

	// First login lands them as member.
	u := runBootstrap(t, pool, q, email, org.Slug, RoleMember)
	// Operator later sets DEFAULT_ROLE=admin; this user logs in again.
	u2 := runBootstrap(t, pool, q, email, org.Slug, RoleAdmin)

	if store.GoUUID(u2.ID) != store.GoUUID(u.ID) {
		t.Fatalf("expected the same user, got a new one")
	}
	m, err := q.GetOrgMember(context.Background(), store.GetOrgMemberParams{
		OrgID: org.ID, UserID: u.ID,
	})
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	if m.Role != string(RoleMember) {
		t.Errorf("role: got %q want member (config must not escalate)", m.Role)
	}
}

// A pending invite outranks the configured default org.
func TestBootstrapSession_PendingInviteWinsOverDefaultOrg(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	inviteOrg := seedOrg(t, q)
	defaultOrg := seedOrg(t, q)
	email := "boot+" + uuid.NewString() + "@example.test"
	seedPendingInvite(t, q, inviteOrg.ID, email, string(RoleAdmin))

	u := runBootstrap(t, pool, q, email, defaultOrg.Slug, RoleMember)

	orgs, err := q.ListOrgsForUser(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("list orgs: %v", err)
	}
	// The invite is applied; because the user now has a membership, the
	// default-org join does not also fire.
	if len(orgs) != 1 || store.GoUUID(orgs[0].ID) != store.GoUUID(inviteOrg.ID) {
		t.Fatalf("expected only the invited org, got %+v", orgs)
	}
	if orgs[0].Role != string(RoleAdmin) {
		t.Errorf("role: got %q want admin from the invite", orgs[0].Role)
	}
}

// Two concurrent logins for the same org-less user both pass the
// CountOrgMembershipsForUser check (each other's INSERT is still
// uncommitted, so invisible) and race to add the SAME (org_id, user_id) to
// the configured default org. AddOrgMember is a plain INSERT, so the loser
// takes a 23505 — which, outside a savepoint, aborts its whole transaction
// and turns a valid login into a 500.
//
// Unlike the personal-workspace path, this is genuinely reachable: that path
// inserts into an org created inside its own transaction with a random slug,
// so no concurrent transaction can name the same org_id. Here both racers
// target a pre-existing org, so the key collides by construction. An org-less
// user row is not hypothetical either — DeleteOrganization cascades
// memberships and can leave a sole member with none.
//
// The 23505 is only reachable through a real interleaving, so this drives one:
// it waits for our own backend to actually be lock-blocked (via
// pg_stat_activity) rather than sleeping, then releases the winner.
func TestBootstrapSession_ConcurrentDefaultOrgJoin_NoAbort(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	// testPool caps at 2 conns; this needs three live at once (both
	// transactions plus the lock-wait probe).
	pool, err := store.NewPool(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)

	org := seedOrg(t, q)
	// An existing user with zero memberships — the reachable precondition.
	email := "boot+" + uuid.NewString() + "@example.test"
	u0, err := q.CreateUser(ctx, store.CreateUserParams{Email: email})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	loser, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin loser: %v", err)
	}
	defer func() { _ = loser.Rollback(ctx) }()
	var loserPID int
	if err := loser.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&loserPID); err != nil {
		t.Fatalf("backend pid: %v", err)
	}

	// Winner inserts the membership and holds it uncommitted.
	winner, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin winner: %v", err)
	}
	defer func() { _ = winner.Rollback(ctx) }()
	if _, err := BootstrapSession(ctx, winner, q, email, org.Slug, RoleMember); err != nil {
		t.Fatalf("winner BootstrapSession: %v", err)
	}

	// Loser began before the winner committed, so its membership count still
	// reads 0 and it proceeds to the colliding INSERT, where it blocks.
	type result struct {
		u   store.User
		err error
	}
	done := make(chan result, 1)
	go func() {
		lu, lerr := BootstrapSession(ctx, loser, q, email, org.Slug, RoleMember)
		done <- result{lu, lerr}
	}()

	// Wait for OUR backend to be lock-blocked, so the interleaving is real
	// rather than timing-dependent.
	blocked := false
	for i := 0; i < 500 && !blocked; i++ {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE pid = $1 AND wait_event_type = 'Lock'`, loserPID).Scan(&n); err != nil {
			t.Fatalf("probe pg_stat_activity: %v", err)
		}
		if n == 1 {
			blocked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("loser never blocked on the org_members unique index; " +
			"the intended interleaving did not happen, so this test proved nothing")
	}

	// Releasing the winner turns the loser's wait into a 23505.
	if err := winner.Commit(ctx); err != nil {
		t.Fatalf("winner commit: %v", err)
	}

	got := <-done
	// Without the savepoint this is the 23505 and the loser's tx is aborted.
	if got.err != nil {
		t.Fatalf("loser BootstrapSession: got %v, want nil "+
			"(a duplicate membership means they are already a member)", got.err)
	}
	if store.GoUUID(got.u.ID) != store.GoUUID(u0.ID) {
		t.Errorf("loser user: got %s want %s", store.GoUUID(got.u.ID), store.GoUUID(u0.ID))
	}
	// The transaction must still be usable — this is what the savepoint buys.
	if err := loser.Commit(ctx); err != nil {
		t.Fatalf("loser commit: got %v, want nil (tx was aborted by the 23505)", err)
	}

	// Exactly one membership, at the role the winner set; no escalation, no
	// duplicate, no personal workspace.
	orgs, err := q.ListOrgsForUser(ctx, u0.ID)
	if err != nil {
		t.Fatalf("list orgs: %v", err)
	}
	if len(orgs) != 1 || store.GoUUID(orgs[0].ID) != store.GoUUID(org.ID) {
		t.Fatalf("expected exactly the default org, got %+v", orgs)
	}
	if orgs[0].Role != string(RoleMember) {
		t.Errorf("role: got %q want member", orgs[0].Role)
	}
}

// A default-org slug that does not exist must surface an error, not silently
// fall back to a personal workspace — the operator has misconfigured the
// deployment and needs to know.
func TestBootstrapSession_UnknownDefaultOrgErrors(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := "boot+" + uuid.NewString() + "@example.test"

	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// Must be ErrDefaultOrgNotFound specifically, not merely non-nil: callers
	// distinguish operator misconfiguration (500) from an auth failure (401),
	// and a bare nil-check would also pass on a connection error.
	_, err = BootstrapSession(context.Background(), tx, q, email, "no-such-org-slug", RoleMember)
	if !errors.Is(err, ErrDefaultOrgNotFound) {
		t.Fatalf("err: got %v, want ErrDefaultOrgNotFound", err)
	}
}
