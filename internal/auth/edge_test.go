package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/store"
)

// --- fault injection over a REAL pool and REAL transactions ------------------

var errInjected = errors.New("injected failure")

// faultTx wraps a real pgx.Tx and fails the sqlc statement whose text contains
// match, or the Commit / a savepoint's Begin / a savepoint's Rollback. Every
// other call reaches Postgres untouched.
type faultTx struct {
	pgx.Tx
	f  *faults
	sp bool // this is a savepoint
}

type faults struct {
	match         string
	failCommit    bool
	failSavepoint bool // Begin on a transaction (i.e. opening a savepoint)
	failSPRollbk  bool
	// missUserLookupOnce makes the first GetUserByEmail report "no rows" even
	// though the row exists: the window a concurrent signup slips through.
	missUserLookupOnce bool
	// failUserReload fails the second GetUserByEmail.
	failUserReload bool
	lookups        int
}

func (t *faultTx) hit(sql string) bool {
	if t.f.match != "" && strings.Contains(sql, t.f.match) {
		return true
	}
	if strings.Contains(sql, "-- name: GetUserByEmail ") {
		t.f.lookups++
		return (t.f.missUserLookupOnce && t.f.lookups == 1) || (t.f.failUserReload && t.f.lookups == 2)
	}
	return false
}

func (t *faultTx) failure(sql string) error {
	if strings.Contains(sql, "-- name: GetUserByEmail ") && t.f.missUserLookupOnce && t.f.lookups == 1 {
		return pgx.ErrNoRows
	}
	return errInjected
}

func (t *faultTx) Exec(ctx context.Context, sql string, a ...any) (pgconn.CommandTag, error) {
	if t.hit(sql) {
		return pgconn.CommandTag{}, t.failure(sql)
	}
	return t.Tx.Exec(ctx, sql, a...)
}
func (t *faultTx) Query(ctx context.Context, sql string, a ...any) (pgx.Rows, error) {
	if t.hit(sql) {
		return nil, t.failure(sql)
	}
	return t.Tx.Query(ctx, sql, a...)
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

func (t *faultTx) QueryRow(ctx context.Context, sql string, a ...any) pgx.Row {
	if t.hit(sql) {
		return errRow{t.failure(sql)}
	}
	return t.Tx.QueryRow(ctx, sql, a...)
}
func (t *faultTx) Begin(ctx context.Context) (pgx.Tx, error) {
	if t.f.failSavepoint {
		return nil, errInjected
	}
	sp, err := t.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: sp, f: t.f, sp: true}, nil
}
func (t *faultTx) Commit(ctx context.Context) error {
	if t.f.failCommit && !t.sp {
		return errInjected
	}
	return t.Tx.Commit(ctx)
}
func (t *faultTx) Rollback(ctx context.Context) error {
	err := t.Tx.Rollback(ctx)
	if t.sp && t.f.failSPRollbk {
		return errInjected
	}
	return err
}

// faultPool is a TxBeginner over the real pool.
type faultPool struct {
	pool      *pgxpool.Pool
	f         *faults
	failBegin bool
}

func (p *faultPool) Begin(ctx context.Context) (pgx.Tx, error) {
	if p.failBegin {
		return nil, errInjected
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, f: p.f}, nil
}

func stmt(name string) string { return "-- name: " + name + " " }

func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("DSTREAM_TEST_DB_URL"), 1)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	pool.Close()
	return pool
}

func userExists(t *testing.T, q *store.Queries, email string) bool {
	t.Helper()
	_, err := q.GetUserByEmail(context.Background(), email)
	if err == nil {
		return true
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lookup %s: %v", email, err)
	}
	return false
}

func tokenUsed(t *testing.T, pool *pgxpool.Pool, token string) bool {
	t.Helper()
	h := sha256.Sum256([]byte(token))
	var used *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT used_at FROM magic_link_tokens WHERE token_hash = $1`, h[:]).Scan(&used); err != nil {
		t.Fatalf("token row: %v", err)
	}
	return used != nil
}

// --- magic link: single use, expiry, and the session epoch --------------------

func TestMagicLink_TokenIsSingleUse(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	email := "reuse+" + uuid.NewString() + "@example.test"
	tok := seedMagicLink(t, q, email)

	u, orgID, err := ConsumeMagicLink(ctx, pool, q, tok)
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if !tokenUsed(t, pool, tok) {
		t.Fatal("used_at must be stamped by the first consume")
	}
	if _, _, err := ConsumeMagicLink(ctx, pool, q, tok); !errors.Is(err, ErrInvalidMagicToken) {
		t.Fatalf("second consume err = %v, want ErrInvalidMagicToken", err)
	}
	orgs, err := q.ListOrgsForUser(ctx, u.ID)
	if err != nil || len(orgs) != 1 || store.GoUUID(orgs[0].ID) != orgID {
		t.Fatalf("replay must not mint another workspace: %d orgs, %v", len(orgs), err)
	}
}

func TestMagicLink_ExpiredTokenIsRefusedAndProvisionsNothing(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := "expired+" + uuid.NewString() + "@example.test"
	tok, err := IssueMagicLink(context.Background(), q, email, -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ConsumeMagicLink(context.Background(), pool, q, tok); !errors.Is(err, ErrInvalidMagicToken) {
		t.Fatalf("err = %v, want ErrInvalidMagicToken", err)
	}
	if userExists(t, q, email) || tokenUsed(t, pool, tok) {
		t.Fatal("an expired link must create no user and must not be marked used")
	}
}

func TestMagicLink_StoresOnlyTheHashOfTheToken(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	tok := seedMagicLink(t, q, "hash+"+uuid.NewString()+"@example.test")
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM magic_link_tokens WHERE token_hash = $1`, []byte(tok)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("raw token found in the table (n=%d, err=%v)", n, err)
	}
	if !strings.ContainsAny(tok, "-_") && len(tok) < 40 {
		t.Fatalf("token %q looks too short", tok)
	}
}

// The login's session epoch is read from the user row, so a link redeemed after
// a logout-all yields a cookie at the NEW epoch, while the cookie from before
// the revocation stays dead.
func TestMagicLink_LoginAfterRevocationGetsTheNewEpoch(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	signer := newSigner(t)
	email := "epoch+" + uuid.NewString() + "@example.test"

	u, orgID, err := ConsumeMagicLink(ctx, pool, q, seedMagicLink(t, q, email))
	if err != nil {
		t.Fatal(err)
	}
	oldRec := httptest.NewRecorder()
	signer.Issue(oldRec, store.GoUUID(u.ID), orgID, int64(u.SessionEpoch))

	if err := q.BumpUserSessionEpoch(ctx, u.ID); err != nil { // logout-all
		t.Fatal(err)
	}
	u2, _, err := ConsumeMagicLink(ctx, pool, q, seedMagicLink(t, q, email))
	if err != nil {
		t.Fatal(err)
	}
	if u2.SessionEpoch != u.SessionEpoch+1 {
		t.Fatalf("epoch after revocation = %d, want %d", u2.SessionEpoch, u.SessionEpoch+1)
	}
	newRec := httptest.NewRecorder()
	signer.Issue(newRec, store.GoUUID(u2.ID), orgID, int64(u2.SessionEpoch))

	status := func(rec *httptest.ResponseRecorder) int {
		r := chi.NewRouter()
		r.Use(Authenticate(q, signer))
		r.Get("/x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		for _, c := range rec.Result().Cookies() {
			req.AddCookie(c)
		}
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		return out.Code
	}
	if got := status(oldRec); got != http.StatusUnauthorized {
		t.Errorf("pre-revocation cookie status = %d, want 401", got)
	}
	if got := status(newRec); got != http.StatusOK {
		t.Errorf("post-revocation login cookie status = %d, want 200", got)
	}
}

// Whichever step fails, the whole consume rolls back: the error comes back, the
// token stays unused, no user or workspace is left behind, and the same link
// then works.
func TestConsumeMagicLink_FailureAtAnyStepRollsBackAndTokenSurvives(t *testing.T) {
	steps := []struct {
		name string
		f    faults
		pool func(*testing.T, *pgxpool.Pool) *faultPool
	}{
		{name: "lookup token", f: faults{match: stmt("GetActiveMagicLinkToken")}},
		{name: "lookup user", f: faults{match: stmt("GetUserByEmail")}},
		{name: "open savepoint", f: faults{failSavepoint: true}},
		{name: "create user", f: faults{match: stmt("CreateUser")}},
		{name: "list invites", f: faults{match: stmt("ListPendingOrgInvitesByEmail")}},
		{name: "count memberships", f: faults{match: stmt("CountOrgMembershipsForUser")}},
		{name: "create workspace", f: faults{match: stmt("CreateOrganization")}},
		{name: "add owner", f: faults{match: stmt("AddOrgMember")}},
		{name: "seed operational app", f: faults{match: stmt("EnsureOperationalApp")}},
		{name: "mark used", f: faults{match: stmt("MarkMagicLinkUsed")}},
		{name: "pick active org", f: faults{match: stmt("GetFirstOrgForUser")}},
		{name: "commit", f: faults{failCommit: true}},
		{name: "rollback of a failed savepoint", f: faults{match: stmt("CreateUser"), failSPRollbk: true}},
	}
	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			q := store.New(pool)
			ctx := context.Background()
			email := "rollback+" + uuid.NewString() + "@example.test"
			tok := seedMagicLink(t, q, email)

			f := tc.f
			if _, _, err := ConsumeMagicLink(ctx, &faultPool{pool: pool, f: &f}, q, tok); !errors.Is(err, errInjected) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
			if tokenUsed(t, pool, tok) || userExists(t, q, email) {
				t.Fatalf("a failed consume must leave the token unused and no user behind")
			}
			if _, _, err := ConsumeMagicLink(ctx, pool, q, tok); err != nil {
				t.Fatalf("the same link must still work afterwards: %v", err)
			}
		})
	}
}

func TestConsumeMagicLink_BeginFailure(t *testing.T) {
	q := store.New(testPool(t))
	if _, _, err := ConsumeMagicLink(context.Background(), &faultPool{failBegin: true}, q, "x"); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v, want the begin failure", err)
	}
	if _, _, err := ConsumeMagicLink(context.Background(), closedPool(t), q, "x"); err == nil || errors.Is(err, ErrInvalidMagicToken) {
		t.Fatalf("closed pool err = %v, want a connection error, not an invalid-token answer", err)
	}
}

func TestConsumeMagicLink_PendingInviteFailuresRollBack(t *testing.T) {
	for _, step := range []string{"AddOrgMember", "MarkOrgInviteAccepted"} {
		t.Run(step, func(t *testing.T) {
			pool := testPool(t)
			q := store.New(pool)
			ctx := context.Background()
			_, orgID := seedUserAndOrg(t, q, RoleOwner)
			email := "invitee+" + uuid.NewString() + "@example.test"
			seedPendingInvite(t, q, store.UUID(orgID), email, string(RoleMember))
			tok := seedMagicLink(t, q, email)

			f := faults{match: stmt(step)}
			if _, _, err := ConsumeMagicLink(ctx, &faultPool{pool: pool, f: &f}, q, tok); !errors.Is(err, errInjected) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
			if tokenUsed(t, pool, tok) || userExists(t, q, email) {
				t.Fatal("rollback must undo the user, membership and invite acceptance")
			}
			if pending, _ := q.ListPendingOrgInvitesByEmail(ctx, email); len(pending) != 1 {
				t.Fatalf("pending invites after rollback = %d, want the invite still pending", len(pending))
			}
			if _, got, err := ConsumeMagicLink(ctx, pool, q, tok); err != nil || got != orgID {
				t.Fatalf("retry: org %v err %v, want the invited org %v", got, err, orgID)
			}
		})
	}
}

func TestBootstrapSession_DefaultOrgFailures(t *testing.T) {
	for _, step := range []string{"GetOrganizationBySlug", "AddOrgMember"} {
		t.Run(step, func(t *testing.T) {
			pool := testPool(t)
			q := store.New(pool)
			ctx := context.Background()
			org := seedOrg(t, q)
			email := "default+" + uuid.NewString() + "@example.test"
			tx, err := (&faultPool{pool: pool, f: &faults{match: stmt(step)}}).Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := BootstrapSession(ctx, tx, q, email, org.Slug, RoleMember); !errors.Is(err, errInjected) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
		})
	}
}

// Two logins for the same new email race: the loser's CreateUser blocks on the
// winner's uncommitted row, takes the unique violation once the winner commits,
// and must adopt the winner's user instead of failing. Real concurrency, made
// deterministic by waiting until Postgres reports the loser is blocked.
func TestBootstrapSession_ConcurrentFirstLoginAdoptsTheWinnersUser(t *testing.T) {
	// testPool's two connections are both held by the transactions below; the
	// poll needs a third.
	pool, err := store.NewPool(context.Background(), os.Getenv("DSTREAM_TEST_DB_URL"), 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	ctx := context.Background()
	email := "race+" + uuid.NewString() + "@example.test"

	winner, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = winner.Rollback(ctx) }()
	won, err := q.WithTx(winner).CreateUser(ctx, store.CreateUserParams{Email: email})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM organizations WHERE id IN (SELECT org_id FROM org_members WHERE user_id = $1)`, won.ID); err != nil {
			t.Errorf("cleanup orgs: %v", err)
		}
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, won.ID); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})
	loser, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = loser.Rollback(ctx) }()
	var pid int32
	if err := loser.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	type result struct {
		u   store.User
		err error
	}
	done := make(chan result, 1)
	go func() {
		u, err := BootstrapSession(ctx, loser, q, email, "", RoleMember)
		done <- result{u, err}
	}()

	deadline := time.Now().Add(15 * time.Second)
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1)) > 0`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second login never blocked on the first one's row")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := winner.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("BootstrapSession lost the race and failed: %v", r.err)
		}
		if r.u.ID != won.ID {
			t.Fatalf("adopted user %v, want the winner's %v", r.u.ID, won.ID)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("BootstrapSession did not finish after the winner committed")
	}
	if err := loser.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if orgs, err := q.ListOrgsForUser(ctx, won.ID); err != nil || len(orgs) != 1 {
		t.Fatalf("orgs for the user = %d (%v), want exactly one personal workspace", len(orgs), err)
	}
}

// The same window, but the winner's row cannot be reloaded: the error surfaces.
func TestBootstrapSession_RaceReloadFailureSurfaces(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	email := "reload+" + uuid.NewString() + "@example.test"
	if _, err := q.CreateUser(ctx, store.CreateUserParams{Email: email}); err != nil {
		t.Fatal(err)
	}
	tx, err := (&faultPool{pool: pool, f: &faults{missUserLookupOnce: true, failUserReload: true}}).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := BootstrapSession(ctx, tx, q, email, "", RoleMember); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v, want the reload failure", err)
	}
}

// --- org invites ---

func TestConsumeOrgInvite_FailuresRollBackAndInviteSurvives(t *testing.T) {
	steps := []struct {
		name string
		f    faults
	}{
		{"lookup invite", faults{match: stmt("GetActiveOrgInviteByTokenHash")}},
		{"add member", faults{match: stmt("AddOrgMember")}},
		{"mark accepted", faults{match: stmt("MarkOrgInviteAccepted")}},
		{"commit", faults{failCommit: true}},
	}
	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			q := store.New(pool)
			ctx := context.Background()
			inviter, orgID := seedUserAndOrg(t, q, RoleOwner)
			email := "invitee+" + uuid.NewString() + "@example.test"
			invitee, err := q.CreateUser(ctx, store.CreateUserParams{Email: email})
			if err != nil {
				t.Fatal(err)
			}
			tok, err := IssueOrgInvite(ctx, q, orgID, inviter, email, RoleAdmin, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			f := tc.f
			if _, err := ConsumeOrgInvite(ctx, &faultPool{pool: pool, f: &f}, q, tok, store.GoUUID(invitee.ID)); !errors.Is(err, errInjected) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
			if _, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{OrgID: store.UUID(orgID), UserID: invitee.ID}); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("membership must not survive a failed accept: %v", err)
			}
			if row, err := ConsumeOrgInvite(ctx, pool, q, tok, store.GoUUID(invitee.ID)); err != nil || row.Role != string(RoleAdmin) {
				t.Fatalf("retry: %+v %v, want the invite to still be usable", row, err)
			}
		})
	}
}

func TestConsumeOrgInvite_BeginFailure(t *testing.T) {
	q := store.New(testPool(t))
	if _, err := ConsumeOrgInvite(context.Background(), &faultPool{failBegin: true}, q, "x", uuid.New()); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
}

// --- API keys ---

func TestVerifyAPIKey(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	_, orgID := seedUserAndOrg(t, q, RoleOwner)
	full, prefix, hash, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	row, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{OrgID: store.UUID(orgID), Name: "k", Prefix: prefix, KeyHash: hash, Role: string(RoleAdmin)})
	if err != nil {
		t.Fatal(err)
	}

	got, err := VerifyAPIKey(ctx, q, full)
	if err != nil || got.ID != row.ID || got.Role != string(RoleAdmin) {
		t.Fatalf("valid key: %+v %v", got, err)
	}
	var used *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_used_at FROM api_keys WHERE id = $1`, row.ID).Scan(&used); err != nil || used == nil {
		t.Fatalf("a successful verify must stamp last_used_at (%v, %v)", used, err)
	}

	wrongSecret := "dsk_" + prefix + "_" + strings.Repeat("A", 43)
	for name, tc := range map[string]struct {
		raw  string
		want error
	}{
		"empty":          {"", ErrMissingAPIKey},
		"no dsk_ mark":   {"Bearer-ish", ErrInvalidAPIKey},
		"too short":      {"dsk_abc", ErrInvalidAPIKey},
		"wrong secret":   {wrongSecret, ErrInvalidAPIKey},
		"unknown prefix": {"dsk_" + strings.Repeat("Z", APIKeyPrefixLen) + "_" + strings.Repeat("A", 43), ErrInvalidAPIKey},
	} {
		if _, err := VerifyAPIKey(ctx, q, tc.raw); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}

	// A database failure is not an authentication verdict.
	if _, err := VerifyAPIKey(ctx, store.New(closedPool(t)), full); err == nil || errors.Is(err, ErrInvalidAPIKey) || !strings.Contains(err.Error(), "get api key") {
		t.Errorf("closed pool err = %v, want a wrapped 'get api key' error", err)
	}
}

func TestExtractAPIKey(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "",
		"Basic abc":         "",
		"bearer dsk_x":      "", // scheme match is exact
		"Bearer dsk_abc_de": "dsk_abc_de",
	} {
		if got := ExtractAPIKey(in); got != want {
			t.Errorf("ExtractAPIKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- middleware ---

func TestAuthenticate_SessionForAVanishedUserFailsClosed(t *testing.T) {
	q := store.New(testPool(t))
	signer := newSigner(t)
	rec := httptest.NewRecorder()
	signer.Issue(rec, uuid.New(), uuid.New(), 0) // validly signed, but no such user
	r := chi.NewRouter()
	r.Use(Authenticate(q, signer))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != http.StatusUnauthorized || !strings.Contains(out.Body.String(), "unauthorized") {
		t.Fatalf("status = %d %q, want 401", out.Code, out.Body)
	}
}

func TestRequireOrg_MembershipLookupFailureIs500(t *testing.T) {
	q := store.New(closedPool(t))
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			p := Principal{Source: SourceSession, UserID: uuid.New(), OrgID: uuid.New()}
			next.ServeHTTP(w, req.WithContext(WithPrincipal(req.Context(), p)))
		})
	})
	r.Use(RequireOrg(q))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	out := httptest.NewRecorder()
	r.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/x", nil))
	if out.Code != http.StatusInternalServerError || !strings.Contains(out.Body.String(), "membership lookup failed") {
		t.Fatalf("status = %d %q, want 500 membership lookup failed", out.Code, out.Body)
	}
}

func TestRequirePortal_LookupFailureIs500(t *testing.T) {
	ps := newPortalSigner(t)
	tok, _ := ps.Mint(uuid.New(), uuid.New(), 0)
	r := chi.NewRouter()
	r.Use(RequirePortal(store.New(closedPool(t)), ps))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != http.StatusInternalServerError || !strings.Contains(out.Body.String(), "portal lookup failed") {
		t.Fatalf("status = %d %q, want 500 portal lookup failed", out.Code, out.Body)
	}
}

// --- portal token structure ---

func TestPortalParse_StructuralRejections(t *testing.T) {
	s := newPortalSigner(t)
	signed := func(payload []byte) string {
		mac := hmac.New(sha256.New, s.Secret)
		mac.Write(payload)
		return base64.RawURLEncoding.EncodeToString(append(append([]byte{}, payload...), mac.Sum(nil)...))
	}
	good := make([]byte, portalPayloadLen)
	good[0] = portalTypeTag
	binary.BigEndian.PutUint64(good[41:49], uint64(time.Now().Add(time.Hour).Unix()))
	wrongTag := append([]byte{}, good...)
	wrongTag[0] = 0x01
	for name, tok := range map[string]string{
		"not base64":            "!!! not base64 !!!",
		"too short":             base64.RawURLEncoding.EncodeToString([]byte("short")),
		"right MAC, wrong tag":  signed(wrongTag),
		"right length, bad MAC": base64.RawURLEncoding.EncodeToString(append(append([]byte{}, good...), make([]byte, sha256.Size)...)),
	} {
		if _, _, _, err := s.Parse(tok); !errors.Is(err, ErrInvalidPortalToken) {
			t.Errorf("%s: err = %v, want ErrInvalidPortalToken", name, err)
		}
	}
	if _, _, _, err := s.Parse(signed(good)); err != nil {
		t.Errorf("control: a well-formed signed token must parse: %v", err)
	}
}

// --- OIDC claims ---

func TestExchange_UndecodableClaimsAreRejected(t *testing.T) {
	idp := newFakeIdP(t)
	idp.claims = map[string]any{"email": 12345} // email must be a string
	_, err := idp.authenticator(t, nil).Exchange(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "oidc: decode claims") {
		t.Fatalf("err = %v, want a decode-claims error", err)
	}
}

var _ = pgtype.UUID{}

func TestFallbackSuffix_IsNeverZeroAndDiffersPerEmailAndTime(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	a := fallbackSuffix("alice@example.test", now)
	if a == ([6]byte{}) {
		t.Fatal("fallback suffix is all zero: every user would collide")
	}
	if a != fallbackSuffix("alice@example.test", now) {
		t.Error("fallback must be deterministic for the same inputs")
	}
	if a == fallbackSuffix("bob@example.test", now) || a == fallbackSuffix("alice@example.test", now.Add(time.Nanosecond)) {
		t.Error("fallback must vary with the email and the clock")
	}
}

func TestIssueTokens_DatabaseFailureIsReturnedAndNoTokenIsHanded(t *testing.T) {
	q := store.New(closedPool(t))
	ctx := context.Background()
	if tok, err := IssueMagicLink(ctx, q, "x@example.test", time.Minute); err == nil || tok != "" {
		t.Errorf("IssueMagicLink = %q, %v, want an error and no token", tok, err)
	}
	if tok, err := IssueOrgInvite(ctx, q, uuid.New(), uuid.New(), "x@example.test", RoleMember, time.Minute); err == nil || tok != "" {
		t.Errorf("IssueOrgInvite = %q, %v, want an error and no token", tok, err)
	}
}

func TestRequireOrg_NoPrincipalIs401(t *testing.T) {
	r := chi.NewRouter()
	r.Use(RequireOrg(store.New(closedPool(t))))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	out := httptest.NewRecorder()
	r.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/x", nil))
	if out.Code != http.StatusUnauthorized || !strings.Contains(out.Body.String(), "unauthorized") {
		t.Fatalf("status = %d %q, want 401", out.Code, out.Body)
	}
}

func TestRequirePortal_TokenForAnApplicationThatNoLongerExistsIs401(t *testing.T) {
	q := store.New(testPool(t))
	ps := newPortalSigner(t)
	tok, _ := ps.Mint(uuid.New(), uuid.New(), 0) // validly signed, but no such app
	r := chi.NewRouter()
	r.Use(RequirePortal(q, ps))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", out.Code)
	}
}
