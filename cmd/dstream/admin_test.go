package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// --- helpers ---

// newEmail returns a unique address and registers cleanup: every org the user
// belongs to (cascading members, keys, audit rows, apps), then the user and
// any magic-link tokens minted for it. Orgs go first because the commands
// under test create orgs we never see the id of up front.
func newEmail(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	email := "cli-" + uuid.NewString() + "@example.test"
	t.Cleanup(func() {
		ctx := context.Background()
		for _, sql := range []string{
			// audit_logs.org_id is SET NULL, so a break-glass row outlives its
			// org, and audit_logs_check then forbids nulling its actor when the
			// user goes. Remove the rows we caused before the user.
			`DELETE FROM audit_logs WHERE actor_user_id IN (SELECT id FROM users WHERE email = $1)`,
			`DELETE FROM organizations WHERE id IN (SELECT m.org_id FROM org_members m JOIN users u ON u.id = m.user_id WHERE u.email = $1)`,
			`DELETE FROM magic_link_tokens WHERE email = $1`,
			`DELETE FROM users WHERE email = $1`,
		} {
			if _, err := pool.Exec(ctx, sql, email); err != nil {
				t.Errorf("cleanup %q: %v", sql, err)
			}
		}
	})
	return email
}

func mustUser(t *testing.T, q *store.Queries, email string) store.User {
	t.Helper()
	u, err := q.CreateUser(context.Background(), store.CreateUserParams{Email: email})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func orgSlugOf(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT slug FROM organizations WHERE id = $1`, store.UUID(id)).Scan(&s); err != nil {
		t.Fatalf("org slug: %v", err)
	}
	return s
}

// closedQueries returns Queries over a pool that has been closed, so every
// call fails for real with pgx's closed-pool error.
func closedQueries(t *testing.T) *store.Queries {
	t.Helper()
	pool := testPool(t)
	pool.Close()
	return store.New(pool)
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want one containing %q", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("error = %q, want it to contain %q", err, substr)
	}
}

var keyLine = regexp.MustCompile(`(?m)^(?:api key|key): +(dsk_\S+)$`)

// printedKey pulls the secret out of a command's output and checks it appears
// as the labelled line plus the single `export` hint, nowhere else.
func printedKey(t *testing.T, out string) string {
	t.Helper()
	m := keyLine.FindAllStringSubmatch(out, -1)
	if len(m) != 1 {
		t.Fatalf("want exactly one key line, got %d in:\n%s", len(m), out)
	}
	if n := strings.Count(out, m[0][1]); n != 2 {
		t.Fatalf("key appears %d times, want 2 (key line + export hint):\n%s", n, out)
	}
	return m[0][1]
}

// secretOf splits dsk_<12-char prefix>_<secret>.
func secretOf(full string) (prefix, secret string) {
	rest := strings.TrimPrefix(full, "dsk_")
	return rest[:auth.APIKeyPrefixLen], rest[auth.APIKeyPrefixLen+1:]
}

// assertKeyStoredHashed checks the row for `full` carries only sha256(secret),
// never the secret or the full key, and that the key authenticates.
func assertKeyStoredHashed(t *testing.T, pool *pgxpool.Pool, q *store.Queries, full string, orgID uuid.UUID, name string) {
	t.Helper()
	prefix, secret := secretOf(full)
	var gotOrg pgtype.UUID
	var gotName, role string
	var hash []byte
	err := pool.QueryRow(context.Background(),
		`SELECT org_id, name, role, key_hash FROM api_keys WHERE prefix = $1`, prefix).Scan(&gotOrg, &gotName, &role, &hash)
	if err != nil {
		t.Fatalf("read key row: %v", err)
	}
	want := sha256.Sum256([]byte(secret))
	if string(hash) != string(want[:]) {
		t.Errorf("key_hash is not sha256(secret)")
	}
	if strings.Contains(string(hash), secret) || string(hash) == full {
		t.Errorf("key_hash contains the plaintext secret")
	}
	if store.GoUUID(gotOrg) != orgID || gotName != name || role != "admin" {
		t.Errorf("key row = org %v name %q role %q, want org %v name %q role admin", store.GoUUID(gotOrg), gotName, role, orgID, name)
	}
	if _, err := auth.VerifyAPIKey(context.Background(), q, full); err != nil {
		t.Errorf("printed key does not authenticate: %v", err)
	}
}

// --- slugify ---

func TestSlugify(t *testing.T) {
	suffix := regexp.MustCompile(`^(.*)-([0-9a-f]{12})$`)
	for _, tc := range []struct{ name, in, base string }{
		{"plain", "Acme", "acme"},
		{"spaces collapse to one dash", "Acme   Corp", "acme-corp"},
		{"mixed separators collapse", "a _ - . b", "a-b"},
		{"digits kept", "Team 42", "team-42"},
		{"leading punctuation trimmed", "---!!!Acme", "acme"},
		{"trailing punctuation trimmed", "Acme!!!---", "acme"},
		{"unicode letters dropped as separators", "Café Ünï", "caf-n"},
		{"only unicode falls back", "日本語", "org"},
		{"empty falls back", "", "org"},
		{"only punctuation falls back", "!!!", "org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := suffix.FindStringSubmatch(slugify(tc.in))
			if m == nil {
				t.Fatalf("slugify(%q) = %q, want <base>-<12 hex>", tc.in, slugify(tc.in))
			}
			if m[1] != tc.base {
				t.Errorf("slugify(%q) base = %q, want %q", tc.in, m[1], tc.base)
			}
		})
	}
	if a, b := slugify("Acme"), slugify("Acme"); a == b {
		t.Errorf("identical names produced the same slug %q; suffix must disambiguate", a)
	}
}

// --- magic-link ---

func TestRunMagicLink_MintsTokenAndAuditsBreakGlass(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	email := newEmail(t, pool)
	user := mustUser(t, q, email)
	org := seedOrg(t, pool, "month")
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{OrgID: store.UUID(org), UserID: user.ID, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("USER", "opsuser")

	var out, errOut strings.Builder
	before := time.Now()
	if err := runMagicLink(ctx, q, &out, &errOut, email, "http://app.test/", 15*time.Minute); err != nil {
		t.Fatalf("runMagicLink: %v", err)
	}

	first, rest, _ := strings.Cut(out.String(), "\n")
	u, err := url.Parse(first)
	if err != nil || u.Scheme+"://"+u.Host+u.Path != "http://app.test/auth/verify" {
		t.Fatalf("link = %q, want http://app.test/auth/verify?token=... (trailing slash trimmed)", first)
	}
	token := u.Query().Get("token")
	if token == "" {
		t.Fatalf("link %q carries no token", first)
	}
	if !strings.Contains(rest, "single-use") || !strings.Contains(rest, "15m0s") {
		t.Errorf("notice = %q, want single-use + TTL", rest)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want empty", errOut.String())
	}

	// Persisted: the token's hash is stored (never the token), unused, expiring in ~TTL.
	h := sha256.Sum256([]byte(token))
	var expires time.Time
	var used pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT expires_at, used_at FROM magic_link_tokens WHERE email = $1 AND token_hash = $2`, email, h[:]).Scan(&expires, &used); err != nil {
		t.Fatalf("token row: %v", err)
	}
	if used.Valid {
		t.Errorf("token already used")
	}
	if expires.Before(before.Add(14*time.Minute)) || expires.After(time.Now().Add(15*time.Minute+time.Second)) {
		t.Errorf("expires_at = %v, want ~15m from now", expires)
	}

	// Persisted: exactly one break-glass audit row, filed under the user's org.
	var action, targetType, snap string
	var orgID, actor, target pgtype.UUID
	var meta []byte
	err = pool.QueryRow(ctx, `SELECT org_id, actor_user_id, target_id, action, target_type, actor_email_snapshot, metadata FROM audit_logs WHERE actor_user_id = $1`, user.ID).
		Scan(&orgID, &actor, &target, &action, &targetType, &snap, &meta)
	if err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if action != "auth.break_glass_magic_link" || targetType != "user" || snap != email ||
		store.GoUUID(orgID) != org || actor != user.ID || target != user.ID {
		t.Errorf("audit row = %s/%s/%s org=%v", action, targetType, snap, store.GoUUID(orgID))
	}
	var m map[string]string
	if err := json.Unmarshal(meta, &m); err != nil {
		t.Fatal(err)
	}
	if m["actor"] != "cli" || m["reason"] != "sso_enforced_break_glass" || m["email"] != email || m["os_user"] != "opsuser" || m["host"] == "" {
		t.Errorf("metadata = %v", m)
	}
}

func TestRunMagicLink_UnsetUSERIsRecordedAsUnknown(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	email := newEmail(t, pool)
	user := mustUser(t, q, email)
	org := seedOrg(t, pool, "month")
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{OrgID: store.UUID(org), UserID: user.ID, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("USER", "")

	if err := runMagicLink(ctx, q, &strings.Builder{}, &strings.Builder{}, email, "http://app.test", time.Minute); err != nil {
		t.Fatal(err)
	}
	var osUser string
	if err := pool.QueryRow(ctx, `SELECT metadata->>'os_user' FROM audit_logs WHERE actor_user_id = $1`, user.ID).Scan(&osUser); err != nil {
		t.Fatal(err)
	}
	if osUser != "unknown" {
		t.Errorf("os_user = %q, want %q (empty reads as a missing field)", osUser, "unknown")
	}
}

func TestRunMagicLink_UnknownEmailMintsNothing(t *testing.T) {
	pool := testPool(t)
	email := newEmail(t, pool)
	var out strings.Builder

	err := runMagicLink(context.Background(), store.New(pool), &out, &out, email, "http://app.test", time.Minute)

	wantErr(t, err, "user "+email+" does not exist; have them sign in once")
	if out.Len() != 0 {
		t.Errorf("printed %q on failure", out.String())
	}
	if n := count(t, pool, `SELECT count(*) FROM users WHERE email = $1`, email); n != 0 {
		t.Errorf("a typo'd address created %d user rows", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM magic_link_tokens WHERE email = $1`, email); n != 0 {
		t.Errorf("%d tokens minted for an unknown user", n)
	}
}

func TestRunMagicLink_UserWithNoOrgGetsNoLiveToken(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := newEmail(t, pool)
	mustUser(t, q, email)
	var out strings.Builder

	err := runMagicLink(context.Background(), q, &out, &out, email, "http://app.test", time.Minute)

	wantErr(t, err, "belongs to no org")
	if out.Len() != 0 {
		t.Errorf("printed %q on failure", out.String())
	}
	if n := count(t, pool, `SELECT count(*) FROM magic_link_tokens WHERE email = $1`, email); n != 0 {
		t.Errorf("%d tokens minted before the org check failed", n)
	}
}

// --- promote ---

func TestRunPromote_SetsSuperAdmin(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := newEmail(t, pool)
	other := newEmail(t, pool)
	mustUser(t, q, email)
	mustUser(t, q, other)
	var out strings.Builder

	if err := runPromote(context.Background(), q, &out, email); err != nil {
		t.Fatal(err)
	}

	if out.String() != "promoted "+email+" to super-admin\n" {
		t.Errorf("output = %q", out.String())
	}
	for e, want := range map[string]bool{email: true, other: false} {
		var got bool
		if err := pool.QueryRow(context.Background(), `SELECT is_super_admin FROM users WHERE email = $1`, e).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("is_super_admin(%s) = %v, want %v", e, got, want)
		}
	}
}

// Promoting an unknown address must touch nobody: a bystander stays non-super
// (fails if the UPDATE ever loses its WHERE clause) and no user is conjured.
// (It also reports success today — see the task report.)
func TestRunPromote_UnknownEmailPromotesNobody(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := newEmail(t, pool)
	bystander := newEmail(t, pool)
	mustUser(t, q, bystander)
	var out strings.Builder

	err := runPromote(context.Background(), q, &out, email)

	wantErr(t, err, "user "+email+" does not exist; nothing was promoted")
	if out.Len() != 0 {
		t.Errorf("printed %q for a promotion that never happened", out.String())
	}
	var super bool
	if err := pool.QueryRow(context.Background(), `SELECT is_super_admin FROM users WHERE email = $1`, bystander).Scan(&super); err != nil {
		t.Fatal(err)
	}
	if super {
		t.Errorf("bystander was promoted by a promote of %s", email)
	}
	if n := count(t, pool, `SELECT count(*) FROM users WHERE email = $1`, email); n != 0 {
		t.Errorf("promote created %d user rows", n)
	}
}

// The lookup succeeds and only the UPDATE fails: runPromote's own wrap.
func TestRunPromote_UpdateFailureIsReported(t *testing.T) {
	pool := testPool(t)
	email := newEmail(t, pool)
	mustUser(t, store.New(pool), email)
	var out strings.Builder

	err := runPromote(context.Background(), store.New(failingPool(t, "-- name: PromoteUserToSuperAdmin ")), &out, email)

	wantErr(t, err, "promote: ")
	wantErr(t, err, "context canceled")
	if out.Len() != 0 {
		t.Errorf("printed %q despite failing", out.String())
	}
	var super bool
	if err := pool.QueryRow(context.Background(), `SELECT is_super_admin FROM users WHERE email = $1`, email).Scan(&super); err != nil {
		t.Fatal(err)
	}
	if super {
		t.Error("user was promoted although the UPDATE failed")
	}
}

// --- bootstrap ---

func TestRunBootstrap_CreatesUserOrgOwnerAndKey(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	email := newEmail(t, pool)
	slug := "boot-" + uuid.NewString()
	var out, errOut strings.Builder

	if err := runBootstrap(ctx, q, &out, &errOut, "  "+strings.ToUpper(email)+" ", slug, "ci-key"); err != nil {
		t.Fatal(err)
	}

	var userID, orgID pgtype.UUID
	var orgName string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&userID); err != nil {
		t.Fatalf("user row (email must be normalised): %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id, name FROM organizations WHERE slug = $1`, slug).Scan(&orgID, &orgName); err != nil {
		t.Fatalf("org row: %v", err)
	}
	if orgName != slug {
		t.Errorf("org name = %q, want slug %q", orgName, slug)
	}
	var role string
	if err := pool.QueryRow(ctx, `SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, orgID, userID).Scan(&role); err != nil || role != "owner" {
		t.Errorf("membership role = %q err=%v, want owner", role, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM applications WHERE org_id = $1`, orgID); n != 1 {
		t.Errorf("operational apps = %d, want 1", n)
	}
	for _, want := range []string{"user:    " + email + "\n", "org:     " + slug + " (id=" + store.GoUUID(orgID).String() + ")\n", "Set it in your shell:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	assertKeyStoredHashed(t, pool, q, printedKey(t, out.String()), store.GoUUID(orgID), "ci-key")
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestRunBootstrap_TwiceIsIdempotentButMintsAFreshKey(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	email := newEmail(t, pool)
	slug := "boot-" + uuid.NewString()
	var out1, out2, errOut strings.Builder

	if err := runBootstrap(ctx, q, &out1, &errOut, email, slug, ""); err != nil {
		t.Fatal(err)
	}
	if err := runBootstrap(ctx, q, &out2, &errOut, email, slug, ""); err != nil {
		t.Fatalf("second run must tolerate the existing membership: %v", err)
	}

	if n := count(t, pool, `SELECT count(*) FROM users WHERE email = $1`, email); n != 1 {
		t.Errorf("users = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM organizations WHERE slug = $1`, slug); n != 1 {
		t.Errorf("orgs = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM org_members m JOIN organizations o ON o.id = m.org_id WHERE o.slug = $1`, slug); n != 1 {
		t.Errorf("memberships = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM applications a JOIN organizations o ON o.id = a.org_id WHERE o.slug = $1`, slug); n != 1 {
		t.Errorf("operational apps = %d, want 1 (seed is idempotent)", n)
	}
	k1, k2 := printedKey(t, out1.String()), printedKey(t, out2.String())
	if k1 == k2 {
		t.Fatalf("both runs printed the same key")
	}
	var orgID pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM organizations WHERE slug = $1`, slug).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{k1, k2} {
		assertKeyStoredHashed(t, pool, q, k, store.GoUUID(orgID), "bootstrap") // empty --key-name falls back
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestRunBootstrap_ReusesExistingUserAndOrg(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	email := newEmail(t, pool)
	user := mustUser(t, q, email)
	org := seedOrg(t, pool, "month")
	slug := orgSlugOf(t, pool, org)

	if err := runBootstrap(ctx, q, &strings.Builder{}, &strings.Builder{}, email, slug, "k"); err != nil {
		t.Fatal(err)
	}

	if n := count(t, pool, `SELECT count(*) FROM organizations WHERE slug = $1`, slug); n != 1 {
		t.Errorf("a duplicate slug created a second org (%d rows)", n)
	}
	var role string
	if err := pool.QueryRow(ctx, `SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, store.UUID(org), user.ID).Scan(&role); err != nil || role != "owner" {
		t.Errorf("membership of existing user in existing org = %q err=%v", role, err)
	}
}

func TestRunBootstrap_KeyFailureLeavesUserAndOrgButNoKey(t *testing.T) {
	pool := testPool(t)
	email := newEmail(t, pool)
	slug := "boot-" + uuid.NewString()

	// NUL is rejected by Postgres text, so the key insert fails for real
	// after the user and org were already created.
	err := runBootstrap(context.Background(), store.New(pool), &strings.Builder{}, &strings.Builder{}, email, slug, "bad\x00name")

	wantErr(t, err, "create api key:")
	if n := count(t, pool, `SELECT count(*) FROM organizations WHERE slug = $1`, slug); n != 1 {
		t.Errorf("orgs = %d, want 1 (created before the failure)", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM api_keys k JOIN organizations o ON o.id = k.org_id WHERE o.slug = $1`, slug); n != 0 {
		t.Errorf("api keys = %d, want 0", n)
	}
}

// The org lookup is the first statement to touch the slug; a NUL byte makes
// Postgres refuse it, so the lookup itself (not "no rows") fails.
func TestRunBootstrap_OrgLookupFailureIsReportedAndMintsNoKey(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := newEmail(t, pool)

	err := runBootstrap(context.Background(), q, &strings.Builder{}, &strings.Builder{}, email, "bad\x00slug", "k")

	wantErr(t, err, "lookup org:")
	if n := count(t, pool, `SELECT count(*) FROM org_members m JOIN users u ON u.id = m.user_id WHERE u.email = $1`, email); n != 0 {
		t.Errorf("memberships = %d, want 0", n)
	}
}

// --- org create ---

func TestRunOrgCreate_CreatesOrgWithOwner(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	email := newEmail(t, pool)
	user := mustUser(t, q, email)
	var out, errOut strings.Builder

	if err := runOrgCreate(ctx, q, &out, &errOut, "Acme Co", email); err != nil {
		t.Fatal(err)
	}

	m := regexp.MustCompile(`org:   Acme Co \(id=([0-9a-f-]{36}), slug=(acme-co-[0-9a-f]{12})\)\nowner: ` + regexp.QuoteMeta(email) + ` \(id=` + store.GoUUID(user.ID).String() + `\)\n`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("output = %q", out.String())
	}
	var name, slug, role string
	if err := pool.QueryRow(ctx, `SELECT o.name, o.slug, m.role FROM organizations o JOIN org_members m ON m.org_id = o.id WHERE o.id = $1 AND m.user_id = $2`, m[1], user.ID).Scan(&name, &slug, &role); err != nil {
		t.Fatalf("org/membership row: %v", err)
	}
	if name != "Acme Co" || slug != m[2] || role != "owner" {
		t.Errorf("row = %q/%q/%q", name, slug, role)
	}
	if n := count(t, pool, `SELECT count(*) FROM applications WHERE org_id = $1`, m[1]); n != 1 {
		t.Errorf("operational apps = %d, want 1", n)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q", errOut.String())
	}
}

// Two orgs with the same display name must both be creatable: the random slug
// suffix is what keeps the unique slug index from refusing the second.
func TestRunOrgCreate_SameNameTwiceGetsDistinctSlugs(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := newEmail(t, pool)
	user := mustUser(t, q, email)

	for i := 0; i < 2; i++ {
		if err := runOrgCreate(context.Background(), q, &strings.Builder{}, &strings.Builder{}, "Twin", email); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	if n := count(t, pool, `SELECT count(DISTINCT o.slug) FROM organizations o JOIN org_members m ON m.org_id = o.id WHERE m.user_id = $1 AND o.name = 'Twin'`, user.ID); n != 2 {
		t.Errorf("distinct slugs = %d, want 2", n)
	}
}

func TestRunOrgCreate_UnknownOwnerCreatesNoOrg(t *testing.T) {
	pool := testPool(t)
	email := newEmail(t, pool)
	name := "Ghost " + uuid.NewString()
	var out strings.Builder

	err := runOrgCreate(context.Background(), store.New(pool), &out, &out, name, email)

	wantErr(t, err, "user "+email+" does not exist; use `dstream admin bootstrap`")
	if n := count(t, pool, `SELECT count(*) FROM organizations WHERE name = $1`, name); n != 0 {
		t.Errorf("orgs named %q = %d, want 0", name, n)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q on failure", out.String())
	}
}

func TestRunOrgCreate_DatabaseRefusalIsReportedAndLeavesNoMembership(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := newEmail(t, pool)
	user := mustUser(t, q, email)

	// A NUL byte is not valid in a Postgres text column, so the insert fails for real.
	err := runOrgCreate(context.Background(), q, &strings.Builder{}, &strings.Builder{}, "bad\x00name", email)

	wantErr(t, err, "create org:")
	if n := count(t, pool, `SELECT count(*) FROM org_members WHERE user_id = $1`, user.ID); n != 0 {
		t.Errorf("memberships = %d, want 0", n)
	}
}

// --- member add ---

func TestRunMemberAdd_StoresRequestedRole(t *testing.T) {
	for _, role := range []string{"owner", "admin", "member"} {
		t.Run(role, func(t *testing.T) {
			pool := testPool(t)
			q := store.New(pool)
			email := newEmail(t, pool)
			user := mustUser(t, q, email)
			org := seedOrg(t, pool, "month")
			var out strings.Builder

			if err := runMemberAdd(context.Background(), q, &out, org, email, role); err != nil {
				t.Fatal(err)
			}

			if want := "added " + email + " to org " + org.String() + " as " + role + "\n"; out.String() != want {
				t.Errorf("output = %q, want %q", out.String(), want)
			}
			var got string
			if err := pool.QueryRow(context.Background(), `SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, store.UUID(org), user.ID).Scan(&got); err != nil || got != role {
				t.Errorf("stored role = %q err=%v, want %q", got, err, role)
			}
		})
	}
}

func TestRunMemberAdd_FailuresAddNoMembership(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	email := newEmail(t, pool)
	user := mustUser(t, q, email)
	org := seedOrg(t, pool, "month")
	missingOrg := uuid.New()
	ghost := newEmail(t, pool)

	for _, tc := range []struct {
		name  string
		org   uuid.UUID
		email string
		role  string
		want  string
	}{
		{"unknown org", missingOrg, email, "member", "org " + missingOrg.String() + " not found"},
		{"unknown email", org, ghost, "member", "user " + ghost + " does not exist; ask them to sign in first"},
		// The CLI validates the role before it gets here; handed an invalid
		// one directly, the database's own CHECK is the backstop.
		{"role refused by CHECK constraint", org, email, "superuser", "org_members_role_check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			wantErr(t, runMemberAdd(ctx, q, &out, tc.org, tc.email, tc.role), tc.want)
			if out.Len() != 0 {
				t.Errorf("printed %q on failure", out.String())
			}
			if n := count(t, pool, `SELECT count(*) FROM org_members WHERE user_id = $1`, user.ID); n != 0 {
				t.Errorf("memberships = %d, want 0", n)
			}
		})
	}
}

func TestRunMemberAdd_UserLookupFailureIsDistinctFromUnknownUser(t *testing.T) {
	pool := testPool(t)
	org := seedOrg(t, pool, "month")

	err := runMemberAdd(context.Background(), store.New(pool), &strings.Builder{}, org, "bad\x00@example.test", "member")

	wantErr(t, err, "lookup user:")
	if strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a failed lookup was reported as a missing user: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM org_members WHERE org_id = $1`, store.UUID(org)); n != 0 {
		t.Errorf("memberships = %d, want 0", n)
	}
}

func TestRunMemberAdd_AlreadyMemberKeepsOriginalRole(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	email := newEmail(t, pool)
	user := mustUser(t, q, email)
	org := seedOrg(t, pool, "month")
	if err := runMemberAdd(ctx, q, &strings.Builder{}, org, email, "member"); err != nil {
		t.Fatal(err)
	}

	err := runMemberAdd(ctx, q, &strings.Builder{}, org, email, "admin")

	wantErr(t, err, "add member:")
	wantErr(t, err, "org_members_pkey")
	var role string
	if err := pool.QueryRow(ctx, `SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, store.UUID(org), user.ID).Scan(&role); err != nil || role != "member" {
		t.Errorf("role = %q err=%v, want unchanged %q", role, err, "member")
	}
}

// --- key create ---

func TestRunKeyCreate_PrintsSecretOnceAndStoresOnlyItsHash(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	org := seedOrg(t, pool, "month")
	var o1, o2 strings.Builder

	if err := runKeyCreate(context.Background(), q, &o1, org, "deploy"); err != nil {
		t.Fatal(err)
	}
	if err := runKeyCreate(context.Background(), q, &o2, org, "deploy"); err != nil {
		t.Fatal(err)
	}

	k1, k2 := printedKey(t, o1.String()), printedKey(t, o2.String())
	if k1 == k2 {
		t.Fatal("two mints printed the same secret")
	}
	assertKeyStoredHashed(t, pool, q, k1, org, "deploy")
	assertKeyStoredHashed(t, pool, q, k2, org, "deploy")
	if !strings.Contains(o1.String(), "name:   deploy\n") || !strings.Contains(o1.String(), "not retrievable later") {
		t.Errorf("output = %q", o1.String())
	}
	var id pgtype.UUID
	p1, _ := secretOf(k1)
	if err := pool.QueryRow(context.Background(), `SELECT id FROM api_keys WHERE prefix = $1`, p1).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(o1.String(), "key id: "+store.GoUUID(id).String()+"\n") {
		t.Errorf("output does not name the stored key id %v:\n%s", store.GoUUID(id), o1.String())
	}
	// No other column ever holds the secret.
	_, secret := secretOf(k1)
	if n := count(t, pool, `SELECT count(*) FROM api_keys WHERE org_id = $1 AND (name LIKE '%'||$2||'%' OR prefix LIKE '%'||$2||'%')`, store.UUID(org), secret); n != 0 {
		t.Errorf("secret found in a plaintext column")
	}
}

func TestRunKeyCreate_FailuresStoreNoKey(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	org := seedOrg(t, pool, "month")
	missing := uuid.New()

	for _, tc := range []struct {
		name string
		org  uuid.UUID
		key  string
		want string
	}{
		{"unknown org", missing, "x", "org " + missing.String() + " not found"},
		{"database refuses the name", org, "bad\x00name", "create key:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			wantErr(t, runKeyCreate(context.Background(), q, &out, tc.org, tc.key), tc.want)
			if out.Len() != 0 {
				t.Errorf("printed a secret on failure: %q", out.String())
			}
			if n := count(t, pool, `SELECT count(*) FROM api_keys WHERE org_id IN ($1, $2)`, store.UUID(org), store.UUID(missing)); n != 0 {
				t.Errorf("api keys = %d, want 0", n)
			}
		})
	}
}

// --- database unreachable: every command reports its own lookup failure ---

func TestAdminCommands_ReportDatabaseFailures(t *testing.T) {
	q := closedQueries(t)
	ctx := context.Background()
	var out strings.Builder
	id := uuid.New()

	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"magic-link": {runMagicLink(ctx, q, &out, &out, "a@b.test", "http://x", time.Minute), "lookup user:"},
		"promote":    {runPromote(ctx, q, &out, "a@b.test"), "lookup user:"},
		"bootstrap":  {runBootstrap(ctx, q, &out, &out, "a@b.test", "s", ""), "lookup user:"},
		"org create": {runOrgCreate(ctx, q, &out, &out, "n", "a@b.test"), "lookup user:"},
		"member add": {runMemberAdd(ctx, q, &out, id, "a@b.test", "member"), "lookup org:"},
		"key create": {runKeyCreate(ctx, q, &out, id, "n"), "lookup org:"},
	} {
		t.Run(name, func(t *testing.T) {
			wantErr(t, tc.err, tc.want)
			wantErr(t, tc.err, "closed pool")
		})
	}
	if out.Len() != 0 {
		t.Errorf("printed %q despite failing", out.String())
	}
}

// --- cobra wiring: real Execute, real config.Load, real pool ---

func TestAdminCmd_MagicLinkExecute(t *testing.T) {
	pool := testPool(t)
	useTestDB(t)
	t.Setenv("DSTREAM_APP_BASE_URL", "http://app.test")
	t.Setenv("DSTREAM_MAGIC_LINK_TTL", "7m")
	q := store.New(pool)
	email := newEmail(t, pool)
	user := mustUser(t, q, email)
	org := seedOrg(t, pool, "month")
	if err := q.AddOrgMember(context.Background(), store.AddOrgMemberParams{OrgID: store.UUID(org), UserID: user.ID, Role: "owner"}); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := execCmd(t, adminCmd(), "magic-link", "  "+strings.ToUpper(email)+" ")

	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout, "http://app.test/auth/verify?token=") || !strings.Contains(stdout, "7m0s") {
		t.Errorf("stdout = %q (config AppBaseURL/TTL must reach the command)", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q", stderr)
	}
	if n := count(t, pool, `SELECT count(*) FROM audit_logs WHERE actor_user_id = $1 AND action = 'auth.break_glass_magic_link'`, user.ID); n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
}

func TestAdminCmd_PromoteExecute(t *testing.T) {
	pool := testPool(t)
	useTestDB(t)
	email := newEmail(t, pool)
	mustUser(t, store.New(pool), email)

	stdout, _, err := execCmd(t, adminCmd(), "promote", email)

	if err != nil || stdout != "promoted "+email+" to super-admin\n" {
		t.Fatalf("stdout=%q err=%v", stdout, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM users WHERE email = $1 AND is_super_admin`, email); n != 1 {
		t.Errorf("user not promoted in the database")
	}
}

func TestAdminCmd_BootstrapExecute(t *testing.T) {
	pool := testPool(t)
	useTestDB(t)
	email := newEmail(t, pool)
	slug := "boot-" + uuid.NewString()

	stdout, _, err := execCmd(t, adminCmd(), "bootstrap", "--email", email, "--org", slug, "--key-name", "wired")

	if err != nil {
		t.Fatal(err)
	}
	var orgID pgtype.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id FROM organizations WHERE slug = $1`, slug).Scan(&orgID); err != nil {
		t.Fatalf("org row: %v", err)
	}
	assertKeyStoredHashed(t, pool, store.New(pool), printedKey(t, stdout), store.GoUUID(orgID), "wired")
}

func TestAdminCmd_BootstrapRequiresEmailAndOrg(t *testing.T) {
	for _, args := range [][]string{{"bootstrap"}, {"bootstrap", "--email", "a@b.test"}, {"bootstrap", "--org", "x"}} {
		stdout, _, err := execCmd(t, adminCmd(), args...)
		wantErr(t, err, "--email and --org are required")
		if stdout != "" {
			t.Errorf("%v printed %q", args, stdout)
		}
	}
}

func TestAdminCmd_OrgCreateExecute(t *testing.T) {
	pool := testPool(t)
	useTestDB(t)
	email := newEmail(t, pool)
	user := mustUser(t, store.New(pool), email)

	stdout, _, err := execCmd(t, adminCmd(), "org", "create", " Wired Org ", email)

	if err != nil || !strings.Contains(stdout, "org:   Wired Org (id=") {
		t.Fatalf("stdout=%q err=%v (name must be trimmed)", stdout, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM organizations o JOIN org_members m ON m.org_id = o.id WHERE m.user_id = $1 AND o.name = 'Wired Org' AND m.role = 'owner'`, user.ID); n != 1 {
		t.Errorf("owned org rows = %d, want 1", n)
	}

	_, _, err = execCmd(t, adminCmd(), "org", "create", "   ", email)
	wantErr(t, err, "name required")
}

func TestAdminCmd_MemberAddExecute(t *testing.T) {
	pool := testPool(t)
	useTestDB(t)
	email := newEmail(t, pool)
	user := mustUser(t, store.New(pool), email)
	org := seedOrg(t, pool, "month")

	stdout, _, err := execCmd(t, adminCmd(), "member", "add", org.String(), strings.ToUpper(email), " ADMIN ")

	if err != nil || stdout != "added "+email+" to org "+org.String()+" as admin\n" {
		t.Fatalf("stdout=%q err=%v", stdout, err)
	}
	var role string
	if err := pool.QueryRow(context.Background(), `SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, store.UUID(org), user.ID).Scan(&role); err != nil || role != "admin" {
		t.Errorf("stored role = %q err=%v", role, err)
	}
}

func TestAdminCmd_MemberAddRejectsBadInputBeforeTouchingTheDatabase(t *testing.T) {
	// No DSTREAM_DB_URL: if validation ran after the pool opened these would
	// fail with a connection error instead.
	t.Setenv("DSTREAM_DB_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable")
	org := uuid.NewString()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"member", "add", "not-a-uuid", "a@b.test", "member"}, "invalid org_id:"},
		{[]string{"member", "add", org, "a@b.test", "superuser"}, `role must be owner, admin, or member (got "superuser")`},
		{[]string{"key", "create", "not-a-uuid", "n"}, "invalid org_id:"},
		{[]string{"key", "create", org, "  "}, "name required"},
	} {
		_, _, err := execCmd(t, adminCmd(), tc.args...)
		wantErr(t, err, tc.want)
	}
}

func TestAdminCmd_KeyCreateExecute(t *testing.T) {
	pool := testPool(t)
	useTestDB(t)
	org := seedOrg(t, pool, "month")

	stdout, _, err := execCmd(t, adminCmd(), "key", "create", org.String(), " ci ")

	if err != nil {
		t.Fatal(err)
	}
	assertKeyStoredHashed(t, pool, store.New(pool), printedKey(t, stdout), org, "ci")
}

func TestAdminCmd_ReportsConfigAndConnectionErrors(t *testing.T) {
	t.Run("invalid config", func(t *testing.T) {
		t.Setenv("DSTREAM_MAGIC_LINK_TTL", "not-a-duration")
		stdout, _, err := execCmd(t, adminCmd(), "promote", "a@b.test")
		wantErr(t, err, "unmarshal config")
		if stdout != "" {
			t.Errorf("printed %q", stdout)
		}
	})
	t.Run("database unreachable", func(t *testing.T) {
		t.Setenv("DSTREAM_DB_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable")
		stdout, _, err := execCmd(t, adminCmd(), "promote", "a@b.test")
		wantErr(t, err, "ping db:")
		if stdout != "" {
			t.Errorf("printed %q", stdout)
		}
	})
}

// --- mid-command database failures ---
//
// Each case needs an early statement to succeed and a later one to fail, which
// no closed pool or bad argument can give. So: a scratch database with the
// real schema, plus one deliberate break per case, undone afterwards.

// migratedScratchDB applies the real migrations to a fresh scratch database
// and returns a pool over it and its DSN.
func migratedScratchDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := newScratchDB(t)
	t.Setenv("DSTREAM_DB_URL", dsn)
	t.Setenv("DSTREAM_LOG_LEVEL", "error")
	var err error
	captureStdout(t, func() { _, _, err = execCmd(t, migrateCmd(), "up") })
	if err != nil {
		t.Fatalf("migrate scratch db: %v", err)
	}
	pool, err := store.NewPool(context.Background(), dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, dsn
}

// breakDB runs ddl now and undo when the (sub)test ends.
func breakDB(t *testing.T, pool *pgxpool.Pool, ddl, undo string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), ddl); err != nil {
		t.Fatalf("%s: %v", ddl, err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), undo); err != nil {
			// Siblings share this schema; record the break so each of them
			// stops at its first statement instead of failing misleadingly.
			brokenSchemas.Store(pool, t.Name())
			t.Fatalf("%s: %v", undo, err)
		}
	})
}

// brokenSchemas maps a scratch pool to the subtest whose undo failed.
var brokenSchemas sync.Map

// requireIntactSchema fails the subtest at once if an earlier sibling left the
// shared schema broken, naming that sibling.
func requireIntactSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if who, ok := brokenSchemas.Load(pool); ok {
		t.Fatalf("shared scratch schema was left broken by %v; fix that failure first", who)
	}
}

func renameTable(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	breakDB(t, pool, "ALTER TABLE "+table+" RENAME TO "+table+"_gone", "ALTER TABLE "+table+"_gone RENAME TO "+table)
}

// refuseInserts leaves reads working and makes every insert into table fail
// with a check violation (not a unique one, so no idempotency path swallows it).
func refuseInserts(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	breakDB(t, pool, "ALTER TABLE "+table+" ADD CONSTRAINT refuse_inserts CHECK (false) NOT VALID",
		"ALTER TABLE "+table+" DROP CONSTRAINT refuse_inserts")
}

const refused = `violates check constraint "refuse_inserts"`

func TestAdminCommands_MidCommandDatabaseFailures(t *testing.T) {
	pool, _ := migratedScratchDB(t)
	q := store.New(pool)
	ctx := context.Background()

	// member returns a user who owns one org.
	member := func(t *testing.T) (email string, user store.User, org store.CreateOrganizationRow) {
		t.Helper()
		email = "mid-" + uuid.NewString() + "@example.test"
		user = mustUser(t, q, email)
		org, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{Name: "mid", Slug: "mid-" + uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{OrgID: org.ID, UserID: user.ID, Role: "owner"}); err != nil {
			t.Fatal(err)
		}
		return email, user, org
	}

	t.Run("magic-link: org lookup fails, nothing is minted", func(t *testing.T) {
		requireIntactSchema(t, pool)
		email, _, _ := member(t)
		renameTable(t, pool, "org_members")
		var out, errOut strings.Builder

		err := runMagicLink(ctx, q, &out, &errOut, email, "http://app.test", time.Minute)

		wantErr(t, err, "list orgs for "+email+":")
		wantErr(t, err, `"org_members" does not exist`)
		if out.String() != "" {
			t.Errorf("printed %q", out.String())
		}
		if n := count(t, pool, `SELECT count(*) FROM magic_link_tokens WHERE email = $1`, email); n != 0 {
			t.Errorf("tokens = %d, want 0", n)
		}
	})

	t.Run("magic-link: minting fails, nothing is printed", func(t *testing.T) {
		requireIntactSchema(t, pool)
		email, _, _ := member(t)
		renameTable(t, pool, "magic_link_tokens")
		var out, errOut strings.Builder

		err := runMagicLink(ctx, q, &out, &errOut, email, "http://app.test", time.Minute)

		wantErr(t, err, "issue magic link:")
		wantErr(t, err, `"magic_link_tokens" does not exist`)
		if out.String() != "" {
			t.Errorf("printed %q", out.String())
		}
	})

	t.Run("magic-link: an unauditable sign-in is never handed out", func(t *testing.T) {
		requireIntactSchema(t, pool)
		email, _, _ := member(t)
		renameTable(t, pool, "audit_logs")
		var out, errOut strings.Builder

		err := runMagicLink(ctx, q, &out, &errOut, email, "http://app.test", time.Minute)

		wantErr(t, err, "record break-glass audit row:")
		wantErr(t, err, `"audit_logs" does not exist`)
		if out.String() != "" {
			t.Errorf("printed %q: the link must not be shown without an audit row", out.String())
		}
	})

	bootstrap := func(t *testing.T, email, slug string) (string, string, error) {
		t.Helper()
		var out, errOut strings.Builder
		err := runBootstrap(ctx, q, &out, &errOut, email, slug, "k")
		return out.String(), errOut.String(), err
	}

	t.Run("bootstrap: user insert fails, nothing else is created", func(t *testing.T) {
		requireIntactSchema(t, pool)
		email, slug := "mid-"+uuid.NewString()+"@example.test", "mid-"+uuid.NewString()
		refuseInserts(t, pool, "users")

		out, _, err := bootstrap(t, email, slug)

		wantErr(t, err, "create user:")
		wantErr(t, err, refused)
		if n := count(t, pool, `SELECT count(*) FROM organizations WHERE slug = $1`, slug); n != 0 || out != "" {
			t.Errorf("orgs = %d, printed %q; want nothing", n, out)
		}
	})

	t.Run("bootstrap: org insert fails, no membership or key", func(t *testing.T) {
		requireIntactSchema(t, pool)
		email, slug := "mid-"+uuid.NewString()+"@example.test", "mid-"+uuid.NewString()
		refuseInserts(t, pool, "organizations")

		out, _, err := bootstrap(t, email, slug)

		wantErr(t, err, "create org:")
		wantErr(t, err, refused)
		if n := count(t, pool, `SELECT count(*) FROM org_members m JOIN users u ON u.id = m.user_id WHERE u.email = $1`, email); n != 0 || out != "" {
			t.Errorf("memberships = %d, printed %q; want nothing", n, out)
		}
	})

	t.Run("bootstrap: a non-duplicate membership failure is fatal", func(t *testing.T) {
		requireIntactSchema(t, pool)
		email, slug := "mid-"+uuid.NewString()+"@example.test", "mid-"+uuid.NewString()
		refuseInserts(t, pool, "org_members")

		out, _, err := bootstrap(t, email, slug)

		wantErr(t, err, "add member:")
		wantErr(t, err, refused)
		if n := count(t, pool, `SELECT count(*) FROM api_keys k JOIN organizations o ON o.id = k.org_id WHERE o.slug = $1`, slug); n != 0 || out != "" {
			t.Errorf("api keys = %d, printed %q; want none", n, out)
		}
	})

	t.Run("bootstrap: a failed operational-app seed is a warning, the key is still minted", func(t *testing.T) {
		requireIntactSchema(t, pool)
		email, slug := "mid-"+uuid.NewString()+"@example.test", "mid-"+uuid.NewString()
		refuseInserts(t, pool, "applications")

		out, errOut, err := bootstrap(t, email, slug)

		if err != nil {
			t.Fatalf("err = %v, want the seed failure tolerated", err)
		}
		if !strings.Contains(errOut, "warn: seed operational app:") || !strings.Contains(errOut, refused) {
			t.Errorf("stderr = %q, want the warning with its cause", errOut)
		}
		assertKeyStoredHashed(t, pool, q, printedKey(t, out), orgIDBySlug(t, pool, slug), "k")
	})

	t.Run("org create: owner insert fails and prints nothing", func(t *testing.T) {
		requireIntactSchema(t, pool)
		email, _, _ := member(t)
		name := "mid-" + uuid.NewString()
		refuseInserts(t, pool, "org_members")
		var out, errOut strings.Builder

		err := runOrgCreate(ctx, q, &out, &errOut, name, email)

		wantErr(t, err, "add owner:")
		wantErr(t, err, refused)
		if n := count(t, pool, `SELECT count(*) FROM org_members m JOIN organizations o ON o.id = m.org_id WHERE o.name = $1`, name); n != 0 || out.String() != "" {
			t.Errorf("memberships = %d, printed %q; want nothing", n, out.String())
		}
	})

	t.Run("org create: a failed operational-app seed is a warning, the org is still reported", func(t *testing.T) {
		requireIntactSchema(t, pool)
		email, user, _ := member(t)
		name := "mid-" + uuid.NewString()
		refuseInserts(t, pool, "applications")
		var out, errOut strings.Builder

		err := runOrgCreate(ctx, q, &out, &errOut, name, email)

		if err != nil {
			t.Fatalf("err = %v, want the seed failure tolerated", err)
		}
		if !strings.Contains(errOut.String(), "warn: seed operational app:") || !strings.Contains(errOut.String(), refused) {
			t.Errorf("stderr = %q, want the warning with its cause", errOut.String())
		}
		if !strings.Contains(out.String(), "org:   "+name) {
			t.Errorf("stdout = %q, want the created org reported", out.String())
		}
		if n := count(t, pool, `SELECT count(*) FROM org_members m JOIN organizations o ON o.id = m.org_id WHERE o.name = $1 AND m.user_id = $2 AND m.role = 'owner'`, name, user.ID); n != 1 {
			t.Errorf("owner memberships = %d, want 1", n)
		}
	})
}

func orgIDBySlug(t *testing.T, pool *pgxpool.Pool, slug string) uuid.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id FROM organizations WHERE slug = $1`, slug).Scan(&id); err != nil {
		t.Fatalf("org %q: %v", slug, err)
	}
	return store.GoUUID(id)
}
