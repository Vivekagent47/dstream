package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/config"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

// These tests live in package api (not internal/api/identity) because they
// drive the real chi router end to end, so the route wiring is covered with
// the handler. The DB/Redis harness they need (testPool, seeding helpers) is
// already here, and an identity-side test importing api would invert the
// package dependency.

// ssoStateCookieName mirrors identity.ssoStateCookieName (unexported there).
// Spelling it out here is deliberate: the cookie name is part of the
// browser-visible contract, and a rename that broke the binding would fail
// startSSO loudly rather than silently.
const ssoStateCookieName = "dstream_sso_state"

// fakeAuthenticator lets the callback tests drive every branch without a live
// IdP or a signing key. No test in this file touches the network.
type fakeAuthenticator struct {
	identity auth.Identity
	err      error
	lastCode string
}

func (f *fakeAuthenticator) AuthCodeURL(state, nonce string) string {
	return "https://idp.test/authorize?state=" + url.QueryEscape(state) +
		"&nonce=" + url.QueryEscape(nonce)
}

func (f *fakeAuthenticator) Exchange(_ context.Context, code string) (auth.Identity, error) {
	f.lastCode = code
	return f.identity, f.err
}

// ssoRedis is the SSO sibling of newTestRouterRedis. SSO stores its state in
// Redis, so every test here needs a real client. Both env spellings are
// accepted: internal/api historically gates on DSTREAM_TEST_REDIS_ADDR while
// the rest of the repo uses DSTREAM_REDIS_ADDR, and an SSO suite that silently
// skipped would be worse than useless.
func ssoRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("DSTREAM_TEST_REDIS_ADDR")
	if addr == "" {
		addr = os.Getenv("DSTREAM_REDIS_ADDR")
	}
	if addr == "" {
		t.Skip("DSTREAM_TEST_REDIS_ADDR / DSTREAM_REDIS_ADDR not set")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// newSSORouter mounts /api with SSO wired to the given (possibly nil)
// authenticator. A nil authenticator is the unconfigured deployment.
func newSSORouter(t *testing.T, pool *pgxpool.Pool, q *store.Queries, a auth.Authenticator, oidc config.OIDCConfig) (*chi.Mux, *auth.SessionSigner, *redis.Client) {
	t.Helper()
	rdb := ssoRedis(t)
	r := chi.NewRouter()
	s := &auth.SessionSigner{Secret: []byte("test-secret-do-not-use-in-prod")}
	Mount(r, Deps{
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       q,
		Pool:          pool,
		Redis:         rdb,
		Signer:        s,
		AppBaseURL:    "http://app.test",
		PublicBaseURL: "http://api.test",
		Authenticator: a,
		OIDC:          oidc,
	})
	return r, s, rdb
}

// uniqueClientAddr gives every /sso/start a distinct client IP. The endpoint
// is rate-limited per IP and httptest's default RemoteAddr is shared by every
// request in the package, so without this the suite's own starts would share
// one budget — and the Redis counter outlives the run, so a second run within
// the hour would start failing.
func uniqueClientAddr() string {
	h := uuid.NewString()
	return "[2001:db8::" + h[:4] + ":" + h[4:8] + "]:1234"
}

// startSSO runs GET /api/auth/sso/start and returns the state and nonce the
// server minted (read back out of the redirect it issued) plus the
// state-binding cookie it set, which the callback requires.
func startSSO(t *testing.T, router http.Handler, query string) (state, nonce string, bind *http.Cookie) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/sso/start"+query, nil)
	req.RemoteAddr = uniqueClientAddr()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("start: got %d (want 302); body=%s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("start: parse Location: %v", err)
	}
	state, nonce = loc.Query().Get("state"), loc.Query().Get("nonce")
	if state == "" || nonce == "" {
		t.Fatalf("start: Location missing state/nonce: %s", rec.Header().Get("Location"))
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == ssoStateCookieName {
			bind = c
		}
	}
	if bind == nil {
		t.Fatalf("start: no %s cookie set: %v", ssoStateCookieName, rec.Result().Cookies())
	}
	if bind.Value != state {
		t.Fatalf("start: binding cookie %q != state %q", bind.Value, state)
	}
	return state, nonce, bind
}

// callbackSSO drives the callback. Cookies are variadic so the login-CSRF
// test can present none.
func callbackSSO(t *testing.T, router http.Handler, query string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/sso/callback"+query, nil)
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	router.ServeHTTP(rec, req)
	return rec
}

// sessionCookie returns the session cookie from a response, or nil.
func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	return nil
}

// verifiedIdentity is the shape of a well-behaved IdP response.
func verifiedIdentity(email, nonce string) auth.Identity {
	return auth.Identity{
		Subject:       "sub-" + uuid.NewString(),
		Email:         email,
		EmailVerified: true,
		Name:          "SSO User",
		Nonce:         nonce,
	}
}

func ssoEmail() string { return "sso+" + uuid.NewString() + "@example.test" }

// --- state handling ---

// Unknown state: a callback that was not initiated by this server. Without
// this check the callback would accept an authorization code from any source.
func TestCallbackSSO_UnknownState(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	fake := &fakeAuthenticator{identity: verifiedIdentity(ssoEmail(), "n")}
	router, _, _ := newSSORouter(t, pool, q, fake, config.OIDCConfig{Issuer: "https://idp.test"})

	rec := callbackSSO(t, router, "?state=never-issued&code=abc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d (want 400); body=%s", rec.Code, rec.Body.String())
	}
	if fake.lastCode != "" {
		t.Fatalf("code was exchanged despite an unknown state (%q)", fake.lastCode)
	}
}

func TestCallbackSSO_MissingStateOrCode(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	router, _, _ := newSSORouter(t, pool, q, &fakeAuthenticator{}, config.OIDCConfig{Issuer: "https://idp.test"})

	for _, qs := range []string{"", "?state=x", "?code=y"} {
		if rec := callbackSSO(t, router, qs); rec.Code != http.StatusBadRequest {
			t.Errorf("query %q: got %d (want 400); body=%s", qs, rec.Code, rec.Body.String())
		}
	}
}

// Single-use state. The second callback with the same state must fail, or a
// leaked callback URL (referrer, proxy log, shared screenshot) becomes a
// reusable login for anyone who replays it.
func TestCallbackSSO_StateIsSingleUse(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := ssoEmail()
	fake := &fakeAuthenticator{}
	router, _, _ := newSSORouter(t, pool, q, fake, config.OIDCConfig{Issuer: "https://idp.test"})

	state, nonce, bind := startSSO(t, router, "")
	fake.identity = verifiedIdentity(email, nonce)

	first := callbackSSO(t, router, "?state="+state+"&code=code-1", bind)
	if first.Code != http.StatusFound {
		t.Fatalf("first callback: got %d (want 302); body=%s", first.Code, first.Body.String())
	}

	second := callbackSSO(t, router, "?state="+state+"&code=code-2", bind)
	if second.Code != http.StatusBadRequest {
		t.Fatalf("replayed callback: got %d (want 400) — state is not single-use; body=%s",
			second.Code, second.Body.String())
	}
	if sessionCookie(second) != nil {
		t.Fatal("replayed callback issued a session")
	}
}

// Login CSRF / session fixation — the attack the dstream_sso_state cookie
// exists to stop, and the reason a GET callback is acceptable at all.
//
// /sso/start is unauthenticated, so the attacker can mint a valid state and a
// genuine ID token for HIS OWN account, then phish the victim into a
// top-level GET of the callback. The state is in Redis, the token verifies,
// the nonce matches and the email is verified — every other guard passes. The
// only thing the attacker cannot produce is a cookie set by this origin in
// the victim's browser, so that is what the callback must require. Without
// it, the victim gets a session for the attacker's account and does their
// work inside the attacker's org.
//
// Two shapes: no cookie at all (the phished navigation), and a cookie
// carrying some other state (an attacker who has started his own login in the
// victim's browser cannot make it name the state he wants replayed).
func TestCallbackSSO_StateNotBoundToBrowser(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := ssoEmail()

	for _, tc := range []struct {
		name   string
		cookie func(bind *http.Cookie) *http.Cookie
	}{
		{"no cookie", func(*http.Cookie) *http.Cookie { return nil }},
		{"mismatched cookie", func(bind *http.Cookie) *http.Cookie {
			return &http.Cookie{Name: ssoStateCookieName, Value: bind.Value + "-tampered"}
		}},
		{"empty cookie", func(*http.Cookie) *http.Cookie {
			return &http.Cookie{Name: ssoStateCookieName, Value: ""}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAuthenticator{}
			router, _, _ := newSSORouter(t, pool, q, fake, config.OIDCConfig{Issuer: "https://idp.test"})

			// The attacker's own, fully legitimate login attempt.
			state, nonce, bind := startSSO(t, router, "")
			fake.identity = verifiedIdentity(email, nonce)

			// Replayed in a browser that never visited /sso/start.
			rec := callbackSSO(t, router, "?state="+state+"&code=attacker-code", tc.cookie(bind))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d (want 400) — login CSRF / session fixation; body=%s",
					rec.Code, rec.Body.String())
			}
			if sessionCookie(rec) != nil {
				t.Fatal("a session was issued to a browser that never started this login")
			}
		})
	}
}

// The binding cookie must not survive the callback: a state that has been
// presented must not be presentable again, whatever the outcome.
func TestCallbackSSO_ClearsBindingCookie(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	fake := &fakeAuthenticator{}
	router, _, _ := newSSORouter(t, pool, q, fake, config.OIDCConfig{Issuer: "https://idp.test"})

	// Success path and a refusal path both have to clear it.
	for _, tc := range []struct{ name, nonceOverride string }{
		{"success", ""},
		{"refusal", "wrong-nonce"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, nonce, bind := startSSO(t, router, "")
			if tc.nonceOverride != "" {
				nonce = tc.nonceOverride
			}
			fake.identity = verifiedIdentity(ssoEmail(), nonce)

			rec := callbackSSO(t, router, "?state="+state+"&code=abc", bind)
			var cleared bool
			for _, c := range rec.Result().Cookies() {
				if c.Name == ssoStateCookieName && c.Value == "" && c.MaxAge < 0 {
					cleared = true
				}
			}
			if !cleared {
				t.Fatalf("binding cookie not cleared: %v", rec.Result().Cookies())
			}
		})
	}
}

// --- nonce binding ---

// A valid, properly signed ID token minted for a *different* login attempt
// must be refused. Without the nonce check, an attacker who can obtain any ID
// token for the audience (e.g. from their own concurrent login) could present
// it on a state they hold.
func TestCallbackSSO_NonceMismatch(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	fake := &fakeAuthenticator{}
	router, _, _ := newSSORouter(t, pool, q, fake, config.OIDCConfig{Issuer: "https://idp.test"})

	state, _, bind := startSSO(t, router, "")
	fake.identity = verifiedIdentity(ssoEmail(), "nonce-from-another-login")

	rec := callbackSSO(t, router, "?state="+state+"&code=abc", bind)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d (want 401); body=%s", rec.Code, rec.Body.String())
	}
	if sessionCookie(rec) != nil {
		t.Fatal("session issued on nonce mismatch")
	}
}

// An ID token carrying no nonce at all is equally unacceptable: an empty
// stored nonce must never compare equal to an empty asserted one.
func TestCallbackSSO_EmptyNonceRefused(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	fake := &fakeAuthenticator{}
	router, _, rdb := newSSORouter(t, pool, q, fake, config.OIDCConfig{Issuer: "https://idp.test"})

	state, _, bind := startSSO(t, router, "")
	// The trap only exists when BOTH sides are empty. startSSO always stores a
	// real random nonce, so an empty asserted nonce would be caught by the
	// mismatch arm and this test would pass without the empty check ever
	// running — overwrite the row so empty == empty is what the handler sees.
	payload, _ := json.Marshal(map[string]string{"nonce": "", "return_to": "/"})
	if err := rdb.Set(context.Background(), "sso:state:"+state, payload, 10*time.Minute).Err(); err != nil {
		t.Fatalf("overwrite state: %v", err)
	}
	id := verifiedIdentity(ssoEmail(), "")
	fake.identity = id

	rec := callbackSSO(t, router, "?state="+state+"&code=abc", bind)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d (want 401); body=%s", rec.Code, rec.Body.String())
	}
	assertNoUser(t, q, id.Email)
}

// --- email_verified: the join key guard ---
//
// The verified email is the join key against users.email (CITEXT UNIQUE), so
// an IdP asserting an address it does not control would sign straight into the
// matching dstream account. Both "false" and "absent" must be refused.

func TestCallbackSSO_EmailVerifiedFalse(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	fake := &fakeAuthenticator{}
	router, _, _ := newSSORouter(t, pool, q, fake, config.OIDCConfig{Issuer: "https://idp.test"})

	state, nonce, bind := startSSO(t, router, "")
	id := verifiedIdentity(ssoEmail(), nonce)
	id.EmailVerified = false
	fake.identity = id

	rec := callbackSSO(t, router, "?state="+state+"&code=abc", bind)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d (want 401); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not verified") {
		t.Errorf("error message should name the unverified email: %s", rec.Body.String())
	}
	assertNoUser(t, q, id.Email)
}

// An absent email_verified claim reaches this handler as EmailVerified=false:
// auth.Exchange maps a missing claim to false (pinned by
// TestExchange_EmailVerifiedAbsentIsFalse in internal/auth, where the real
// claim decoding lives). This test pins the other half — that the handler
// refuses it rather than treating "not stated" as "verified".
func TestCallbackSSO_EmailVerifiedAbsent(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	fake := &fakeAuthenticator{}
	router, _, _ := newSSORouter(t, pool, q, fake, config.OIDCConfig{Issuer: "https://idp.test"})

	state, nonce, bind := startSSO(t, router, "")
	fake.identity = auth.Identity{
		Subject: "sub-absent",
		Email:   ssoEmail(),
		Nonce:   nonce,
		// EmailVerified left at its zero value: that is what an ID token
		// without the claim produces.
	}

	rec := callbackSSO(t, router, "?state="+state+"&code=abc", bind)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d (want 401); body=%s", rec.Code, rec.Body.String())
	}
	assertNoUser(t, q, fake.identity.Email)
}

func TestCallbackSSO_EmptyEmail(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	fake := &fakeAuthenticator{}
	router, _, _ := newSSORouter(t, pool, q, fake, config.OIDCConfig{Issuer: "https://idp.test"})

	state, nonce, bind := startSSO(t, router, "")
	fake.identity = auth.Identity{Subject: "s", EmailVerified: true, Nonce: nonce}

	rec := callbackSSO(t, router, "?state="+state+"&code=abc", bind)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d (want 401); body=%s", rec.Code, rec.Body.String())
	}
	if sessionCookie(rec) != nil {
		t.Fatal("session issued for an identity with no email")
	}
	// No assertNoUser here: there is no address to look a row up by, which is
	// precisely the refusal's reason.
}

// IdP-side declines (consent denied, policy block) are not our failure and
// must not be reported as a server error.
func TestCallbackSSO_IdPError(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	router, _, _ := newSSORouter(t, pool, q, &fakeAuthenticator{}, config.OIDCConfig{Issuer: "https://idp.test"})

	rec := callbackSSO(t, router, "?error=access_denied")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d (want 401); body=%s", rec.Code, rec.Body.String())
	}
}

// --- happy path ---

// Session cookie set, and it parses back to the provisioned user with a real
// active org. The cookie must be the same one the magic-link path issues, so
// session_epoch revocation covers SSO users too.
func TestCallbackSSO_SetsSession(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := ssoEmail()
	fake := &fakeAuthenticator{}
	router, signer, _ := newSSORouter(t, pool, q, fake, config.OIDCConfig{Issuer: "https://idp.test"})

	state, nonce, bind := startSSO(t, router, "?return_to=/events")
	fake.identity = verifiedIdentity(email, nonce)

	rec := callbackSSO(t, router, "?state="+state+"&code=the-code", bind)
	if rec.Code != http.StatusFound {
		t.Fatalf("got %d (want 302); body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Location"), "http://app.test/events"; got != want {
		t.Errorf("Location: got %q want %q", got, want)
	}
	if fake.lastCode != "the-code" {
		t.Errorf("exchanged code: got %q want %q", fake.lastCode, "the-code")
	}

	cookie := sessionCookie(rec)
	if cookie == nil {
		t.Fatalf("no %s cookie set: %v", auth.SessionCookieName, rec.Result().Cookies())
	}

	// Parse the cookie the way the middleware would.
	probe := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	probe.AddCookie(cookie)
	uid, oid, _, err := signer.Parse(probe)
	if err != nil {
		t.Fatalf("parse session: %v", err)
	}
	u, err := q.GetUserByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("user was not provisioned: %v", err)
	}
	if uid != store.GoUUID(u.ID) {
		t.Errorf("session user: got %s want %s", uid, store.GoUUID(u.ID))
	}
	if oid == uuid.Nil {
		t.Error("session carries no active org; the bootstrap should have minted a personal workspace")
	}
}

// A second SSO login for the same address signs into the same account rather
// than creating a parallel one — email is the join key.
func TestCallbackSSO_SecondLoginReusesAccount(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := ssoEmail()
	fake := &fakeAuthenticator{}
	router, _, _ := newSSORouter(t, pool, q, fake, config.OIDCConfig{Issuer: "https://idp.test"})

	var ids []uuid.UUID
	for i := 0; i < 2; i++ {
		state, nonce, bind := startSSO(t, router, "")
		fake.identity = verifiedIdentity(email, nonce)
		if rec := callbackSSO(t, router, "?state="+state+"&code=c", bind); rec.Code != http.StatusFound {
			t.Fatalf("login %d: got %d; body=%s", i, rec.Code, rec.Body.String())
		}
		u, err := q.GetUserByEmail(context.Background(), email)
		if err != nil {
			t.Fatalf("login %d: load user: %v", i, err)
		}
		ids = append(ids, store.GoUUID(u.ID))
	}
	if ids[0] != ids[1] {
		t.Fatalf("two logins produced two users: %s vs %s", ids[0], ids[1])
	}
}

// --- default org misconfiguration ---

// A DSTREAM_OIDC_DEFAULT_ORG slug that matches no org is not validated at boot
// (it would couple startup to DB seeding state), so the first login is the only
// signal an operator gets. It must be legible: a 500 naming the
// misconfiguration, never the generic auth failure and never a 401 — a 401
// would read as the user's fault and send them chasing their IdP.
func TestCallbackSSO_DefaultOrgNotFound(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := ssoEmail()
	fake := &fakeAuthenticator{}
	router, _, _ := newSSORouter(t, pool, q, fake, config.OIDCConfig{
		Issuer:         "https://idp.test",
		DefaultOrgSlug: "no-such-org-" + uuid.NewString(),
		DefaultRole:    "member",
	})

	state, nonce, bind := startSSO(t, router, "")
	fake.identity = verifiedIdentity(email, nonce)

	rec := callbackSSO(t, router, "?state="+state+"&code=abc", bind)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d (want 500); body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "DSTREAM_OIDC_DEFAULT_ORG") {
		t.Errorf("error should name the misconfigured variable; got %s", body)
	}
	assertNoUser(t, q, email)
}

// The configured default org is actually used: a first-time SSO user joins it
// instead of getting a personal workspace.
func TestCallbackSSO_JoinsDefaultOrg(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	slug := "sso-default-" + uuid.NewString()
	org, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{Name: "Default", Slug: slug})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	email := ssoEmail()
	fake := &fakeAuthenticator{}
	router, _, _ := newSSORouter(t, pool, q, fake, config.OIDCConfig{
		Issuer: "https://idp.test", DefaultOrgSlug: slug, DefaultRole: "member",
	})

	state, nonce, bind := startSSO(t, router, "")
	fake.identity = verifiedIdentity(email, nonce)
	if rec := callbackSSO(t, router, "?state="+state+"&code=abc", bind); rec.Code != http.StatusFound {
		t.Fatalf("got %d; body=%s", rec.Code, rec.Body.String())
	}

	u, err := q.GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	m, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{OrgID: org.ID, UserID: u.ID})
	if err != nil {
		t.Fatalf("user did not join the default org: %v", err)
	}
	if m.Role != "member" {
		t.Fatalf("role in default org: got %q want \"member\"", m.Role)
	}
}

// --- return_to / open redirect ---

// An open redirect on a login callback is a credential-phishing primitive: the
// victim really did authenticate, so the landing page can convincingly ask for
// anything. Anything that could leave this origin falls back to "/".
func TestStartSSO_RejectsForeignReturnTo(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	fake := &fakeAuthenticator{}
	router, _, rdb := newSSORouter(t, pool, q, fake, config.OIDCConfig{Issuer: "https://idp.test"})

	for _, bad := range []string{
		"https://evil.test/x",
		"//evil.test/x",
		"/\\evil.test",
		"http://localhost:1/x",
		"javascript:alert(1)",
	} {
		state, _, _ := startSSO(t, router, "?return_to="+url.QueryEscape(bad))
		raw, err := rdb.Get(context.Background(), "sso:state:"+state).Bytes()
		if err != nil {
			t.Fatalf("%q: read stored state: %v", bad, err)
		}
		var st struct {
			ReturnTo string `json:"return_to"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatalf("%q: decode stored state: %v", bad, err)
		}
		if st.ReturnTo != "/" {
			t.Errorf("return_to %q was stored as %q; want \"/\"", bad, st.ReturnTo)
		}
	}
}

// ...and the final redirect obeys the same rule even if a state row somehow
// carries a foreign target, so the guard is not only on the way in.
func TestCallbackSSO_ForeignReturnToInStateIgnored(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	fake := &fakeAuthenticator{}
	router, _, rdb := newSSORouter(t, pool, q, fake, config.OIDCConfig{Issuer: "https://idp.test"})

	state, nonce, bind := startSSO(t, router, "")
	// Overwrite the stored state with a hostile return_to, as a tampered or
	// pre-seeded Redis row would.
	payload, _ := json.Marshal(map[string]string{"nonce": nonce, "return_to": "https://evil.test/steal"})
	if err := rdb.Set(context.Background(), "sso:state:"+state, payload, 10*time.Minute).Err(); err != nil {
		t.Fatalf("overwrite state: %v", err)
	}
	fake.identity = verifiedIdentity(ssoEmail(), nonce)

	rec := callbackSSO(t, router, "?state="+state+"&code=abc", bind)
	if rec.Code != http.StatusFound {
		t.Fatalf("got %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "http://app.test/" {
		t.Fatalf("Location: got %q, want %q — open redirect", got, "http://app.test/")
	}
}

// A legitimate app-relative return_to survives, so the guard above is not just
// "always /".
func TestStartSSO_KeepsRelativeReturnTo(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	router, _, rdb := newSSORouter(t, pool, q, &fakeAuthenticator{}, config.OIDCConfig{Issuer: "https://idp.test"})

	state, _, _ := startSSO(t, router, "?return_to=%2Fconnections%3Ftab%3Dlive")
	raw, err := rdb.Get(context.Background(), "sso:state:"+state).Bytes()
	if err != nil {
		t.Fatalf("read stored state: %v", err)
	}
	var st struct {
		ReturnTo string `json:"return_to"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.ReturnTo != "/connections?tab=live" {
		t.Fatalf("return_to: got %q", st.ReturnTo)
	}
}

// --- rate limit ---

// /sso/start is unauthenticated and each call writes a 10-minute Redis key
// into the SAME instance that carries the delivery queue (one *redis.Client is
// shared), so an unmetered flood could push a self-host Redis to maxmemory and
// have an allkeys-* policy evict queued webhook deliveries — an unauthenticated
// endpoint degrading the data plane.
//
// Asserts that a limit exists and bites, not its exact number: the budget is a
// tuning knob, the presence of one is the contract.
func TestStartSSO_RateLimitedPerIP(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	router, _, _ := newSSORouter(t, pool, q, &fakeAuthenticator{}, config.OIDCConfig{Issuer: "https://idp.test"})

	addr := uniqueClientAddr() // one budget, isolated from the rest of the suite
	// Must exceed ssoStartPerIP's burst, which is deliberately generous (the
	// budget is per egress IP and SSO deployments sit behind one NAT address).
	const maxTries = 400
	for i := 0; i < maxTries; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/auth/sso/start", nil)
		req.RemoteAddr = addr
		router.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			return // limit is wired
		}
		if rec.Code != http.StatusFound {
			t.Fatalf("request %d: got %d (want 302 or 429); body=%s", i, rec.Code, rec.Body.String())
		}
	}
	t.Fatalf("%d starts from one IP were all accepted — /sso/start is unmetered", maxTries)
}

// --- unconfigured deployment ---

// A deployment that sets no DSTREAM_OIDC_* variable must behave exactly as it
// does today. The routes stay mounted (so the route table does not change
// shape with configuration) but 404.
func TestStartSSO_DisabledWhenNotConfigured(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	router, _, _ := newSSORouter(t, pool, q, nil, config.OIDCConfig{})

	for _, path := range []string{"/api/auth/sso/start", "/api/auth/sso/callback?state=a&code=b"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d (want 404); body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

// --- GET /api/auth/methods ---

func TestAuthMethods(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)

	read := func(router http.Handler) map[string]bool {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/methods", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d; body=%s", rec.Code, rec.Body.String())
		}
		var out map[string]bool
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	unconfigured, _, _ := newSSORouter(t, pool, q, nil, config.OIDCConfig{})
	if got := read(unconfigured); got["sso"] || !got["magic_link"] {
		t.Errorf("unconfigured: got %v, want sso=false magic_link=true", got)
	}

	enforced, _, _ := newSSORouter(t, pool, q, &fakeAuthenticator{}, config.OIDCConfig{
		Issuer: "https://idp.test", Enforce: true,
	})
	if got := read(enforced); !got["sso"] || got["magic_link"] {
		t.Errorf("enforced: got %v, want sso=true magic_link=false", got)
	}
}

// --- enforcement (DSTREAM_OIDC_ENFORCE) ---

// With enforcement on, magic links must not be a way around IdP policy (MFA,
// conditional access, deprovisioning): possession of the mailbox would be
// enough to sign in, which is exactly what enforcement exists to stop.
//
// A 403 naming SSO, not a silent "check your email" that never arrives — and
// no token minted, or enforcement would be in name only.
func TestRequestMagicLink_RefusedWhenSSOEnforced(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := ssoEmail()
	router, _, _ := newSSORouter(t, pool, q, &fakeAuthenticator{}, config.OIDCConfig{
		Issuer: "https://idp.test", Enforce: true,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/request",
		strings.NewReader(`{"email":"`+email+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = uniqueClientAddr()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d (want 403); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "single sign-on") {
		t.Errorf("refusal must name SSO so the SPA can explain it; got %s", rec.Body.String())
	}
	assertNoMagicLinkToken(t, pool, email)
}

// The same budget-free request must still work when enforcement is off, so the
// guard above is "Enforce closed it", not "this route is broken".
func TestRequestMagicLink_AllowedWhenNotEnforced(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := ssoEmail()
	router, _, _ := newSSORouter(t, pool, q, &fakeAuthenticator{}, config.OIDCConfig{
		Issuer: "https://idp.test",
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/request",
		strings.NewReader(`{"email":"`+email+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = uniqueClientAddr()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d (want 202); body=%s", rec.Code, rec.Body.String())
	}
	if countMagicLinkTokens(t, pool, email) != 1 {
		t.Fatalf("no token minted for %s with enforcement off", email)
	}
}

// The invite-accept fallback is the OTHER magic-link mint path: it mails a
// sign-in link to the invite's address with no session involved at all
// (internal/api/identity/invites.go, "Path B"). Guarding only
// /api/auth/magic-link/request would leave anyone holding a live invite token
// — including a user the IdP has since deprovisioned — a working bypass of
// enforcement, which is the whole thing enforcement is for.
func TestAcceptInvite_MagicLinkFallbackRefusedWhenSSOEnforced(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	inviterID, orgID := seedUserAndOrg(t, q)

	// Staged inline rather than via stageInvite: the test has to POST the
	// plaintext token, so it must be the one that chose it.
	token := "inv-" + uuid.NewString()
	h := sha256.Sum256([]byte(token))
	invitee := ssoEmail()
	if _, err := q.CreateOrgInvite(ctx, store.CreateOrgInviteParams{
		OrgID:     store.UUID(orgID),
		Email:     invitee,
		Role:      "member",
		TokenHash: h[:],
		InvitedBy: store.UUID(inviterID),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("stage invite: %v", err)
	}

	router, _, _ := newSSORouter(t, pool, q, &fakeAuthenticator{}, config.OIDCConfig{
		Issuer: "https://idp.test", Enforce: true,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invites/"+token+"/accept", nil)
	req.RemoteAddr = uniqueClientAddr()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d (want 403) — invite-accept still mints magic links under enforcement; body=%s",
			rec.Code, rec.Body.String())
	}
	assertNoMagicLinkToken(t, pool, invitee)
}

// The break-glass only works because enforcement gates MINTING, not
// redemption: `dstream admin magic-link` mints a token out of band (host shell
// access, no HTTP), and POST /api/auth/magic-link/verify has to stay open for
// it. If a future change "closes every magic-link path" including verify, a
// broken IdP locks every human out of the deployment with no way back in — so
// this pins the open half deliberately.
//
// IssueMagicLink here is exactly what the CLI calls; what is under test is the
// HTTP verify under Enforce:true.
func TestVerifyMagicLink_BreakGlassTokenStillRedeemableUnderEnforce(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	email := ssoEmail()
	router, _, _ := newSSORouter(t, pool, q, &fakeAuthenticator{}, config.OIDCConfig{
		Issuer: "https://idp.test", Enforce: true,
	})

	token, err := auth.IssueMagicLink(context.Background(), q, email, 15*time.Minute)
	if err != nil {
		t.Fatalf("issue magic link: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/verify",
		strings.NewReader(`{"token":"`+token+`"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d (want 204) — the break-glass link is not redeemable under enforcement; body=%s",
			rec.Code, rec.Body.String())
	}
	if sessionCookie(rec) == nil {
		t.Fatalf("no session cookie issued: %v", rec.Result().Cookies())
	}
}

// Enforcement is about humans. Machines do not do SSO, so an API key must be
// entirely unaffected — breaking every CI integration the moment an operator
// flips DSTREAM_OIDC_ENFORCE would be a severe regression.
//
// Asserts the exact 200, not merely "not 403": a key that stopped
// authenticating at all would 401 and satisfy "not 403" while failing the very
// guarantee this test pins.
func TestAPIKeyAuthUnaffectedByEnforce(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, orgID := seedUserAndOrg(t, q)
	router, _, _ := newSSORouter(t, pool, q, &fakeAuthenticator{}, config.OIDCConfig{
		Issuer: "https://idp.test", Enforce: true,
	})
	key := newKeyAt(t, q, orgID, string(auth.RoleAdmin))

	if code := keyRequest(t, router, key, http.MethodGet, "/api/sources"); code != http.StatusOK {
		t.Fatalf("api-key GET /api/sources under Enforce: got %d, want 200", code)
	}
}

// countMagicLinkTokens counts live rows for an address. Read straight off the
// pool: there is no sqlc query that counts by email, and a refusal that still
// wrote a redeemable token is exactly what these tests have to catch.
func countMagicLinkTokens(t *testing.T, pool *pgxpool.Pool, email string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM magic_link_tokens WHERE email = $1`, email).Scan(&n); err != nil {
		t.Fatalf("count magic link tokens: %v", err)
	}
	return n
}

func assertNoMagicLinkToken(t *testing.T, pool *pgxpool.Pool, email string) {
	t.Helper()
	if n := countMagicLinkTokens(t, pool, email); n != 0 {
		t.Errorf("%d magic-link token(s) minted for %s despite enforcement", n, email)
	}
}

// assertNoUser fails if a refused login nevertheless provisioned an account.
// A refusal that still creates the user would squat the email address.
func assertNoUser(t *testing.T, q *store.Queries, email string) {
	t.Helper()
	if email == "" {
		return
	}
	if _, err := q.GetUserByEmail(context.Background(), email); err == nil {
		t.Errorf("refused login provisioned a user for %s", email)
	}
}

// =============================================================================
// Magic link: request, rate limits, client IP keying, verify, logout, /me.
// Harness (idEnv, failOn, wantErr, ...) lives in orgs_test.go.
// =============================================================================

// ssoEnv is an idEnv with a live Redis and, optionally, SSO configured.
func ssoEnv(t *testing.T, tr pgx.QueryTracer, mod func(*Deps)) (*idEnv, *redis.Client) {
	t.Helper()
	rdb := ssoRedis(t)
	return newIDEnv(t, tr, func(d *Deps) {
		d.Redis = rdb
		if mod != nil {
			mod(d)
		}
	}), rdb
}

// delRateKeys removes exactly the limiter keys a test created, so -count=2 and
// the other packages sharing this Redis never see them.
func delRateKeys(t *testing.T, rdb *redis.Client, keys ...string) {
	t.Helper()
	t.Cleanup(func() { rdb.Del(context.Background(), keys...) })
}

func (e *idEnv) magic(body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/request", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = uniqueClientAddr()
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func magicBody(email string) string { return `{"email":"` + email + `"}` }

func (e *idEnv) verify(token string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/verify", strings.NewReader(`{"token":"`+token+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestRequestMagicLink_RejectsBadInputWithoutMinting(t *testing.T) {
	e, _ := ssoEnv(t, nil, nil)
	wantErr(t, e.magic(`{"email":`, nil), 400, "invalid json")
	for _, in := range []string{"", "   ", "no-at-sign"} {
		wantErr(t, e.magic(magicBody(in), nil), 400, "invalid email")
	}
}

func TestRequestMagicLink_MintsHashedLowercasedTokenAndEnqueuesMail(t *testing.T) {
	rdb := ssoRedis(t)
	pfx := "mltest-" + uuid.NewString()
	t.Cleanup(func() {
		if keys, _ := rdb.Keys(context.Background(), pfx+":*").Result(); len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
	})
	e, _ := ssoEnv(t, nil, func(d *Deps) { d.Queue = dqueue.NewClient(rdb).WithPrefix(pfx) })
	email := uniqEmail("MiXed")
	lower := strings.ToLower(email)
	delRateKeys(t, rdb, "rate:magic_link:email:"+lower)

	wantStatus(t, e.magic(magicBody("  "+email+" "), nil), http.StatusAccepted)

	var n int
	var exp time.Time
	var used *time.Time
	var hashLen int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*), max(expires_at), max(used_at), max(length(token_hash)) FROM magic_link_tokens WHERE email=$1`, lower).
		Scan(&n, &exp, &used, &hashLen); err != nil {
		t.Fatalf("read token: %v", err)
	}
	if n != 1 || used != nil || hashLen != 32 {
		t.Fatalf("token rows: n=%d used=%v hashLen=%d want 1/nil/32 (sha256, unused)", n, used, hashLen)
	}
	if d := time.Until(exp); d < 14*time.Minute || d > 16*time.Minute {
		t.Fatalf("expires in %v want ~15m", d)
	}
	keys, err := rdb.Keys(context.Background(), pfx+":*").Result()
	if err != nil || len(keys) == 0 {
		t.Fatalf("no email task enqueued under %s (err=%v)", pfx, err)
	}
}

// The response must not depend on what happened to the mail or the token: a
// 202 either way, or the endpoint becomes an oracle for which addresses work.
func TestRequestMagicLink_InternalFailuresStill202(t *testing.T) {
	t.Run("token insert fails", func(t *testing.T) {
		e, rdb := ssoEnv(t, failOn("insert into magic_link_tokens"), nil)
		email := uniqEmail("ins")
		delRateKeys(t, rdb, "rate:magic_link:email:"+email)
		wantStatus(t, e.magic(magicBody(email), nil), http.StatusAccepted)
		if n := e.magicLinks(email); n != 0 {
			t.Fatalf("minted %d tokens despite the failed insert", n)
		}
	})
	t.Run("enqueue fails", func(t *testing.T) {
		e, rdb := ssoEnv(t, nil, func(d *Deps) { d.Queue = dqueue.NewClient(deadRedis(t)) })
		email := uniqEmail("enq")
		delRateKeys(t, rdb, "rate:magic_link:email:"+email)
		wantStatus(t, e.magic(magicBody(email), nil), http.StatusAccepted)
		if n := e.magicLinks(email); n != 1 {
			t.Fatalf("token rows: %d want 1 (minted before the enqueue failed)", n)
		}
	})
}

func TestRequestMagicLink_LimiterDownFailsClosed(t *testing.T) {
	e := newIDEnv(t, nil, func(d *Deps) { d.Redis = deadRedis(t) })
	email := uniqEmail("down")
	wantErr(t, e.magic(magicBody(email), nil), 503, "rate limiter unavailable")
	if n := e.magicLinks(email); n != 0 {
		t.Fatalf("minted %d tokens with the limiter down", n)
	}
}

func TestRequestMagicLink_PerEmailBudget(t *testing.T) {
	e, rdb := ssoEnv(t, nil, nil)
	email := uniqEmail("bomb")
	delRateKeys(t, rdb, "rate:magic_link:email:"+email)
	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		last = e.magic(magicBody(email), nil) // a fresh client address each time
		if i < 5 {
			wantStatus(t, last, http.StatusAccepted)
		}
	}
	wantErr(t, last, 429, "too many requests")
	ra, err := strconv.Atoi(last.Header().Get("Retry-After"))
	if err != nil || ra < 1 || ra > 3601 {
		t.Fatalf("Retry-After %q: want 1..3601 seconds", last.Header().Get("Retry-After"))
	}
	if n := e.magicLinks(email); n != 5 {
		t.Fatalf("tokens: got %d want the 5 inside the budget", n)
	}
	// Case and padding do not buy a fresh budget.
	wantErr(t, e.magic(magicBody("  "+strings.ToUpper(email)+" "), nil), 429, "too many requests")
}

// Documents CURRENT behaviour, not contract: the per-IP budget keys on the TCP
// peer (RemoteAddr) and clientIP never reads forwarding headers. This test
// router has no TrustedRealIP; in production, when DSTREAM_TRUSTED_PROXIES is
// set, internal/middleware/realip.go (wired at cmd/dstream/server.go:192)
// rewrites RemoteAddr from X-Forwarded-For before the handler runs. This pins
// the handler layer only; do not teach clientIP to read XFF itself, that is
// the vulnerability.
func TestRequestMagicLink_PerIPBudgetIgnoresForwardingHeaders(t *testing.T) {
	e, rdb := ssoEnv(t, nil, nil)
	b := uuid.New()
	peer := "10." + strconv.Itoa(int(b[0])) + "." + strconv.Itoa(int(b[1])) + "." + strconv.Itoa(int(b[2]))
	delRateKeys(t, rdb, "rate:magic_link:ip:"+peer)
	var last *httptest.ResponseRecorder
	for i := 0; i < 31; i++ {
		i := i
		email := uniqEmail("ip")
		delRateKeys(t, rdb, "rate:magic_link:email:"+email)
		last = e.magic(magicBody(email), func(r *http.Request) {
			r.RemoteAddr = peer + ":4000"
			r.Header.Set("X-Forwarded-For", "203.0.113."+strconv.Itoa(i)+", 198.51.100.9")
		})
		if i < 30 {
			wantStatus(t, last, http.StatusAccepted)
		}
	}
	wantErr(t, last, 429, "too many requests")
	if last.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
}

// Documents CURRENT behaviour, not contract: clientIP uses RemoteAddr as-is
// (host part, or the whole string when it has no port) and ignores forwarding
// headers. Proxy handling lives in TrustedRealIP (server.go:192), not here.
func TestClientIP_KeyedOnPeerNeverOnHeaders(t *testing.T) {
	e, rdb := ssoEnv(t, nil, nil)
	rnd := func() string { b := uuid.New(); return hex.EncodeToString(b[:3]) }
	v4 := func() string {
		b := uuid.New()
		return "10." + strconv.Itoa(int(b[0])) + "." + strconv.Itoa(int(b[1])) + "." + strconv.Itoa(int(b[2]))
	}
	// documentation-range (2001:db8::/32) addresses unique to this run, so "the spoofed key does not
	// exist" cannot be tripped by a concurrent run.
	spoof := func() string {
		b := uuid.New()
		return "2001:db8::" + hex.EncodeToString(b[:2]) + ":" + hex.EncodeToString(b[2:4])
	}
	s1, s2, s3, s4 := spoof(), spoof(), spoof(), spoof()
	cases := []struct {
		name   string
		remote func(host string) string
		host   func() string
		hdr    map[string]string
	}{
		{"ipv4 with port", func(h string) string { return h + ":5555" }, v4, nil},
		{"ipv6 with port", func(h string) string { return "[" + h + "]:5555" }, func() string { return "fd00::" + rnd() }, nil},
		{"no port is used whole", func(h string) string { return h }, v4, nil},
		{"spoofed X-Forwarded-For", func(h string) string { return h + ":1" }, v4,
			map[string]string{"X-Forwarded-For": s1}},
		{"multi-hop X-Forwarded-For", func(h string) string { return h + ":1" }, v4,
			map[string]string{"X-Forwarded-For": s2 + ", " + s3 + ", " + s4}},
		{"X-Real-IP and Forwarded", func(h string) string { return h + ":1" }, v4,
			map[string]string{"X-Real-IP": s1, "Forwarded": "for=" + s4}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host := c.host()
			email := uniqEmail("cip")
			keys := []string{"rate:magic_link:ip:" + host, "rate:magic_link:email:" + email}
			spoofed := map[string]string{}
			for _, h := range c.hdr {
				for _, part := range strings.Split(h, ",") {
					if v := strings.TrimSpace(strings.TrimPrefix(part, "for=")); v != "" {
						k := "rate:magic_link:ip:" + v
						spoofed[v] = k
						keys = append(keys, k)
					}
				}
			}
			delRateKeys(t, rdb, keys...)
			wantStatus(t, e.magic(magicBody(email), func(r *http.Request) {
				r.RemoteAddr = c.remote(host)
				for k, v := range c.hdr {
					r.Header.Set(k, v)
				}
			}), http.StatusAccepted)
			ctx := context.Background()
			if n, _ := rdb.Exists(ctx, "rate:magic_link:ip:"+host).Result(); n != 1 {
				t.Fatalf("no limiter key for the peer host %q", host)
			}
			for v, k := range spoofed {
				if n, _ := rdb.Exists(ctx, k).Result(); n != 0 {
					t.Fatalf("a client-supplied address %q was used as the limiter key", v)
				}
			}
		})
	}
}

func TestVerifyMagicLink_RejectsBadRequests(t *testing.T) {
	e, _ := ssoEnv(t, nil, nil)
	post := func(ct, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/verify", strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
		rec := post(ct, `{"token":"x"}`)
		wantErr(t, rec, 415, "content-type must be application/json")
		if sessionCookie(rec) != nil {
			t.Fatalf("%q: session cookie issued on a refused request", ct)
		}
	}
	wantErr(t, post("application/json", `{"token":`), 400, "invalid json")
	wantErr(t, post("application/json", `{}`), 400, "missing token")
	wantErr(t, post("application/json", `{"token":"   "}`), 400, "missing token")
	wantErr(t, post("application/json; charset=utf-8", `{"token":"x"}`), 401, "invalid or expired link")
}

func TestVerifyMagicLink_UnusableTokensAreRefusedWithoutProvisioning(t *testing.T) {
	e, _ := ssoEnv(t, nil, nil)
	ctx := context.Background()

	expired := uniqEmail("expired")
	expTok, err := auth.IssueMagicLink(ctx, e.q, expired, -time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	live := uniqEmail("tamper")
	liveTok, err := auth.IssueMagicLink(ctx, e.q, live, time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	flipped := liveTok[:len(liveTok)-1] + map[bool]string{true: "B", false: "A"}[liveTok[len(liveTok)-1] == 'A']

	for name, tok := range map[string]string{
		"expired":   expTok,
		"malformed": "!!! not a token !!!",
		"unknown":   strings.Repeat("A", 43),
		"tampered":  flipped,
		"truncated": liveTok[:10],
	} {
		rec := e.verify(tok)
		if rec.Code != 401 {
			t.Fatalf("%s: got %d want 401", name, rec.Code)
		}
		wantErr(t, rec, 401, "invalid or expired link")
		if sessionCookie(rec) != nil {
			t.Fatalf("%s: session cookie issued", name)
		}
	}
	if e.userExists(expired) || e.userExists(live) {
		t.Fatal("a refused redemption provisioned a user")
	}
	// The refused attempts did not burn the genuine token.
	wantStatus(t, e.verify(liveTok), http.StatusNoContent)
}

func TestVerifyMagicLink_SingleUseAndSessionMatchesStoredUser(t *testing.T) {
	e, _ := ssoEnv(t, nil, nil)
	email := uniqEmail("once")
	tok, err := auth.IssueMagicLink(context.Background(), e.q, email, time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	rec := e.verify(" " + tok + " ")
	wantStatus(t, rec, http.StatusNoContent)
	uid, org := e.cookieOrg(rec)
	u, err := e.q.GetUserByEmail(context.Background(), email)
	if err != nil || store.GoUUID(u.ID) != uid {
		t.Fatalf("cookie user %s does not match stored user (%v)", uid, err)
	}
	if e.members(org)[uid] != "owner" {
		t.Fatalf("new user is not owner of the active org %s: %v", org, e.members(org))
	}
	var used *time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT used_at FROM magic_link_tokens WHERE email=$1`, email).Scan(&used); err != nil || used == nil {
		t.Fatalf("token not stamped used: %v %v", used, err)
	}

	// Replay: refused, no session, and still exactly one user and one workspace.
	again := e.verify(tok)
	wantErr(t, again, 401, "invalid or expired link")
	if sessionCookie(again) != nil {
		t.Fatal("replay issued a session")
	}
	if n := len(e.members(org)); n != 1 {
		t.Fatalf("members after replay: %d", n)
	}
}

// Documents CURRENT behaviour, not contract: a token minted for a user who is
// then deleted still redeems. It provisions a NEW user (fresh id), and the
// deleted user's old session does not come back to life.
func TestVerifyMagicLink_TokenForDeletedUser(t *testing.T) {
	e, _ := ssoEnv(t, nil, nil)
	ctx := context.Background()
	email := uniqEmail("gone")
	first := e.verify(mustIssue(t, e, email))
	wantStatus(t, first, http.StatusNoContent)
	oldUser, _ := e.cookieOrg(first)
	oldCookie := sessionCookie(first)

	tok := mustIssue(t, e, email)
	if _, err := e.pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, oldUser); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if e.userExists(email) {
		t.Fatal("user not deleted")
	}

	rec := e.verify(tok)
	wantStatus(t, rec, http.StatusNoContent)
	newUser, _ := e.cookieOrg(rec)
	if newUser == oldUser {
		t.Fatal("deleted user's id was resurrected")
	}
	if !e.userExists(email) {
		t.Fatal("redeeming did not provision the account")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(oldCookie)
	me := httptest.NewRecorder()
	e.h.ServeHTTP(me, req)
	if me.Code != http.StatusUnauthorized {
		t.Fatalf("deleted user's old session: got %d want 401", me.Code)
	}
}

func mustIssue(t *testing.T, e *idEnv, email string) string {
	t.Helper()
	tok, err := auth.IssueMagicLink(context.Background(), e.q, email, time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return tok
}

func TestRequestMagicLink_EnforcedRefusesBeforeReadingTheRequest(t *testing.T) {
	e, rdb := ssoEnv(t, nil, func(d *Deps) { d.OIDC = config.OIDCConfig{Enforce: true} })
	// Garbage body: if the guard ran after the decode this would be a 400.
	wantErr(t, e.magic(`{"email":`, nil), 403, "magic-link sign-in is disabled; use single sign-on")
	email := uniqEmail("enf")
	wantErr(t, e.magic(magicBody(email), nil), 403, "magic-link sign-in is disabled; use single sign-on")
	if n, _ := rdb.Exists(context.Background(), "rate:magic_link:email:"+email).Result(); n != 0 {
		t.Fatal("an enforced refusal still spent rate-limit budget")
	}
	if n := e.magicLinks(email); n != 0 {
		t.Fatalf("minted %d tokens under enforcement", n)
	}
}

func TestLogout_BumpsEpochClearsCookieAndKillsEverySession(t *testing.T) {
	e, _ := ssoEnv(t, nil, nil)
	uid, org := e.seedOrg()
	bystander, bystanderOrg := e.seedOrg()
	epoch := func(u uuid.UUID) int {
		var n int
		if err := e.pool.QueryRow(context.Background(), `SELECT session_epoch FROM users WHERE id=$1`, u).Scan(&n); err != nil {
			t.Fatalf("epoch: %v", err)
		}
		return n
	}
	// Two live sessions for one user (two devices).
	req1 := requestWithSession(t, e.signer, http.MethodGet, "/api/me", uid, org)
	req2 := requestWithSession(t, e.signer, http.MethodGet, "/api/me", uid, org)
	for _, r := range []*http.Request{req1, req2} {
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, r)
		wantStatus(t, rec, http.StatusOK)
	}

	out := requestWithSession(t, e.signer, http.MethodPost, "/api/auth/logout", uid, org)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, out)
	wantStatus(t, rec, http.StatusNoContent)
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.Value == "" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("session cookie not cleared: %v", rec.Result().Cookies())
	}
	if epoch(uid) != 1 || epoch(bystander) != 0 {
		t.Fatalf("epochs: user=%d bystander=%d want 1/0", epoch(uid), epoch(bystander))
	}
	// Both outstanding sessions are dead, not just the one that logged out.
	for i, r := range []*http.Request{req1, req2} {
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("session %d after logout-all: got %d want 401", i, rec.Code)
		}
	}
	// A bystander's session is untouched.
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, requestWithSession(t, e.signer, http.MethodGet, "/api/me", bystander, bystanderOrg))
	wantStatus(t, rec, http.StatusOK)
}

func TestLogout_WithoutValidCookieIsHarmless(t *testing.T) {
	e, _ := ssoEnv(t, nil, nil)
	uid, _ := e.seedOrg()
	forged := &auth.SessionSigner{Secret: []byte("some-other-secret")}
	bad := requestWithSession(t, forged, http.MethodPost, "/api/auth/logout", uid, uuid.New())
	for name, req := range map[string]*http.Request{
		"no cookie":        httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil),
		"forged signature": bad,
	} {
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		wantStatus(t, rec, http.StatusNoContent)
		var epoch int
		if err := e.pool.QueryRow(context.Background(), `SELECT session_epoch FROM users WHERE id=$1`, uid).Scan(&epoch); err != nil || epoch != 0 {
			t.Fatalf("%s: forged logout moved the victim's epoch to %d (%v)", name, epoch, err)
		}
	}
}

func TestLogout_EpochWriteFailureStill204(t *testing.T) {
	e := newIDEnv(t, failOn("update users set session_epoch"), nil)
	uid, org := e.seedOrg()
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, requestWithSession(t, e.signer, http.MethodPost, "/api/auth/logout", uid, org))
	wantStatus(t, rec, http.StatusNoContent)
	var epoch int
	if err := e.pool.QueryRow(context.Background(), `SELECT session_epoch FROM users WHERE id=$1`, uid).Scan(&epoch); err != nil || epoch != 0 {
		t.Fatalf("epoch %d (%v) want 0 after the failed write", epoch, err)
	}
}

func TestMe_ShapesAndFailureModes(t *testing.T) {
	get := func(e *idEnv, uid, org uuid.UUID) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, requestWithSession(t, e.signer, http.MethodGet, "/api/me", uid, org))
		return rec
	}
	t.Run("super admin flag only when true", func(t *testing.T) {
		e := newIDEnv(t, nil, nil)
		uid, org := e.seedOrg()
		plain, plainOrg := e.seedOrg()
		if _, err := e.pool.Exec(context.Background(), `UPDATE users SET is_super_admin=true WHERE id=$1`, uid); err != nil {
			t.Fatalf("promote: %v", err)
		}
		var r struct {
			User map[string]any `json:"user"`
		}
		rec := get(e, uid, org)
		wantStatus(t, rec, http.StatusOK)
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		if r.User["is_super_admin"] != true {
			t.Fatalf("super admin not flagged: %v", r.User)
		}
		rec = get(e, plain, plainOrg)
		r.User = nil
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		if _, ok := r.User["is_super_admin"]; ok {
			t.Fatalf("non-admin carries an escalation hint: %v", r.User)
		}
	})
	t.Run("no active org", func(t *testing.T) {
		e := newIDEnv(t, nil, nil)
		uid, _ := e.seedOrg()
		rec := get(e, uid, uuid.Nil)
		wantStatus(t, rec, http.StatusOK)
		if strings.Contains(rec.Body.String(), "active_org_id") {
			t.Fatalf("active_org_id emitted without an active org: %s", rec.Body.String())
		}
	})
	t.Run("deleted user's cookie is refused", func(t *testing.T) {
		e := newIDEnv(t, nil, nil)
		uid, org := e.seedOrg()
		if _, err := e.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, uid); err != nil {
			t.Fatalf("delete: %v", err)
		}
		wantStatus(t, get(e, uid, org), http.StatusUnauthorized)
	})
	t.Run("user vanishes between middleware and handler", func(t *testing.T) {
		// Authenticate loads the user first; the handler's own load is the
		// second matching statement.
		tr := &stmtTracer{match: has("from users where id"), nth: 2}
		e := newIDEnv(t, tr, nil)
		uid, org := e.seedOrg()
		wantErr(t, get(e, uid, org), 401, "session user not found")
	})
	t.Run("org list failure degrades, keeps the user", func(t *testing.T) {
		e := newIDEnv(t, failOn("from organizations o join org_members m"), nil)
		uid, org := e.seedOrg()
		rec := get(e, uid, org)
		wantStatus(t, rec, http.StatusOK)
		var r map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		if r["user"] == nil || r["active_org_id"] != org.String() {
			t.Fatalf("body: %s", rec.Body.String())
		}
	})
}

func TestPatchMe_StoresTrimmedNameAndClearsOnBlank(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	uid, org := e.seedOrg()
	other, _ := e.seedOrg()
	nameOf := func(u uuid.UUID) *string {
		var n *string
		if err := e.pool.QueryRow(context.Background(), `SELECT name FROM users WHERE id=$1`, u).Scan(&n); err != nil {
			t.Fatalf("name: %v", err)
		}
		return n
	}

	rec := e.do(http.MethodPatch, "/api/me", uid, org, map[string]any{"name": "  Ada Lovelace  "})
	wantStatus(t, rec, http.StatusOK)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["name"] != "Ada Lovelace" || out["id"] != uid.String() || out["is_super_admin"] != false {
		t.Fatalf("response: %v", out)
	}
	if n := nameOf(uid); n == nil || *n != "Ada Lovelace" {
		t.Fatalf("stored name: %v", n)
	}
	if nameOf(other) != nil {
		t.Fatal("PATCH /me touched another user")
	}
	for _, blank := range []string{"", "   "} {
		wantStatus(t, e.do(http.MethodPatch, "/api/me", uid, org, map[string]any{"name": blank}), http.StatusOK)
		if n := nameOf(uid); n != nil {
			t.Fatalf("blank %q stored %q, want NULL", blank, *n)
		}
	}
}

func TestPatchMe_Refusals(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	uid, org := e.seedOrg()
	name := "Keep Me"
	if _, err := e.q.UpdateUserName(context.Background(), store.UpdateUserNameParams{ID: store.UUID(uid), Name: &name}); err != nil {
		t.Fatalf("seed name: %v", err)
	}
	key := newKeyAt(t, e.q, org, "admin")
	wantErr(t, e.withKey(key, http.MethodPatch, "/api/me", map[string]any{"name": "x"}), 401, "session required")
	wantErr(t, e.do(http.MethodPatch, "/api/me", uid, org, map[string]any{}), 400, "name required")
	wantErr(t, e.do(http.MethodPatch, "/api/me", uid, org, map[string]any{"name": nil}), 400, "name required")
	req := requestWithSessionBody(t, e.signer, http.MethodPatch, "/api/me", uid, org, nil)
	req.Body = io.NopCloser(strings.NewReader(`{"name":`))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	wantErr(t, rec, 400, "invalid json")

	var got *string
	if err := e.pool.QueryRow(context.Background(), `SELECT name FROM users WHERE id=$1`, uid).Scan(&got); err != nil || got == nil || *got != name {
		t.Fatalf("a refused PATCH changed the name: %v (%v)", got, err)
	}
}

func TestPatchMe_StoreFailure500LeavesNameAlone(t *testing.T) {
	e := newIDEnv(t, failOn("update users set name"), nil)
	uid, org := e.seedOrg()
	wantErr(t, e.do(http.MethodPatch, "/api/me", uid, org, map[string]any{"name": "New"}), 500, "update profile")
	var got *string
	if err := e.pool.QueryRow(context.Background(), `SELECT name FROM users WHERE id=$1`, uid).Scan(&got); err != nil || got != nil {
		t.Fatalf("name %v (%v) want NULL", got, err)
	}
}

// =============================================================================
// SSO branches the suite above leaves.
// =============================================================================

// ssoStack is an idEnv with SSO wired to a fake authenticator and a live Redis.
func ssoStack(t *testing.T, tr pgx.QueryTracer, fa *fakeAuthenticator, mod func(*Deps)) (*idEnv, *redis.Client) {
	t.Helper()
	return ssoEnv(t, tr, func(d *Deps) {
		d.Authenticator = fa
		d.OIDC = config.OIDCConfig{Issuer: "https://idp.test"}
		d.PublicBaseURL = "http://api.test"
		if mod != nil {
			mod(d)
		}
	})
}

func TestStartSSO_RedisFailuresRefuseAndSetNoCookie(t *testing.T) {
	t.Run("limiter down", func(t *testing.T) {
		e := newIDEnv(t, nil, func(d *Deps) {
			d.Authenticator = &fakeAuthenticator{}
			d.Redis = deadRedis(t)
		})
		rec := e.anon(http.MethodGet, "/api/auth/sso/start")
		wantErr(t, rec, 503, "sso unavailable")
		if len(rec.Result().Cookies()) != 0 || rec.Header().Get("Location") != "" {
			t.Fatalf("a failed start set a cookie or redirected: %v %q", rec.Result().Cookies(), rec.Header().Get("Location"))
		}
	})
	t.Run("state store fails", func(t *testing.T) {
		rdb := ssoRedis(t)
		rdb.AddHook(failCmdHook{name: "set"})
		e := newIDEnv(t, nil, func(d *Deps) { d.Authenticator = &fakeAuthenticator{}; d.Redis = rdb })
		rec := e.anon(http.MethodGet, "/api/auth/sso/start")
		wantErr(t, rec, 503, "sso unavailable")
		if len(rec.Result().Cookies()) != 0 || rec.Header().Get("Location") != "" {
			t.Fatalf("a failed start set a cookie or redirected")
		}
	})
}

// failCmdHook fails one named Redis command on an otherwise real connection.
type failCmdHook struct{ name string }

func (failCmdHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h failCmdHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == h.name {
			return errors.New("injected redis failure")
		}
		return next(ctx, cmd)
	}
}
func (failCmdHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestCallbackSSO_IdPErrorIsTruncatedBeforeLogging(t *testing.T) {
	buf := &syncBuf{}
	e, _ := ssoStack(t, nil, &fakeAuthenticator{}, func(d *Deps) { d.Log = slog.New(slog.NewTextHandler(buf, nil)) })
	rec := e.anon(http.MethodGet, "/api/auth/sso/callback?error="+strings.Repeat("z", 400))
	wantErr(t, rec, 401, "sso sign-in was declined")
	logged := buf.String()
	if !strings.Contains(logged, strings.Repeat("z", 128)) || strings.Contains(logged, strings.Repeat("z", 129)) {
		t.Fatalf("attacker-controlled error not capped at 128 bytes: %d z's logged", strings.Count(logged, "z"))
	}
}

func TestCallbackSSO_CorruptStateIsRefusedAndConsumed(t *testing.T) {
	e, rdb := ssoStack(t, nil, &fakeAuthenticator{}, nil)
	state := "corrupt-" + uuid.NewString()
	if err := rdb.Set(context.Background(), "sso:state:"+state, "{not json", time.Minute).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { rdb.Del(context.Background(), "sso:state:"+state) })
	req := httptest.NewRequest(http.MethodGet, "/api/auth/sso/callback?code=c&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: ssoStateCookieName, Value: state})
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	wantErr(t, rec, 400, "unknown or expired sso state")
	if sessionCookie(rec) != nil {
		t.Fatal("session issued from a corrupt state")
	}
	if n, _ := rdb.Exists(context.Background(), "sso:state:"+state).Result(); n != 0 {
		t.Fatal("the corrupt state row is still presentable")
	}
}

func TestCallbackSSO_ExchangeFailureIsRefusedWithoutProvisioning(t *testing.T) {
	fa := &fakeAuthenticator{err: errors.New("idp said no")}
	e, _ := ssoStack(t, nil, fa, nil)
	state, _, bind := ssoStart(t, e)
	rec := ssoCallback(e, "?code=bad&state="+state, bind)
	wantErr(t, rec, 401, "sso sign-in failed")
	if fa.lastCode != "bad" || sessionCookie(rec) != nil {
		t.Fatalf("lastCode=%q session=%v", fa.lastCode, sessionCookie(rec))
	}
}

func ssoStart(t *testing.T, e *idEnv) (state, nonce string, bind *http.Cookie) {
	t.Helper()
	return startSSO(t, e.h, "")
}

func ssoCallback(e *idEnv, query string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	return callbackSSO(e.t, e.h, query, cookies...)
}

// Every failure past the identity checks must roll the login back whole: no
// user, no workspace, no session.
func TestCallbackSSO_ProvisioningFailuresRollBackAndRefuse(t *testing.T) {
	run := func(t *testing.T, tr pgx.QueryTracer, mod func(*Deps), req func(*http.Request) *http.Request, wantMsg string) {
		t.Helper()
		email := ssoEmail()
		fa := &fakeAuthenticator{}
		e, _ := ssoStack(t, tr, fa, mod)
		state, nonce, bind := ssoStart(t, e)
		fa.identity = verifiedIdentity(email, nonce)
		r := httptest.NewRequest(http.MethodGet, "/api/auth/sso/callback?code=c&state="+state, nil)
		r.AddCookie(bind)
		if req != nil {
			r = req(r)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, r)
		wantErr(t, rec, 500, wantMsg)
		if sessionCookie(rec) != nil {
			t.Fatal("session issued for a failed login")
		}
		assertNoUser(t, e.q, email)
		var orgs int
		if err := e.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM organizations WHERE name LIKE $1`, email+"%").Scan(&orgs); err != nil || orgs != 0 {
			t.Fatalf("a failed login left %d workspaces (%v)", orgs, err)
		}
	}
	t.Run("transaction cannot begin", func(t *testing.T) {
		run(t, nil, func(d *Deps) {
			closed := testPool(t)
			closed.Close()
			d.Pool = closed
		}, nil, "sso unavailable")
	})
	t.Run("bootstrap statement fails", func(t *testing.T) {
		run(t, failOn("select count(*) from org_members where user_id"), nil, nil, "sso sign-in failed")
	})
	t.Run("active org lookup fails", func(t *testing.T) {
		run(t, failOn("order by m.created_at asc, m.org_id asc"), nil, nil, "sso sign-in failed")
	})
	t.Run("commit fails", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		// Let the org lookup finish, then drop the request context: COMMIT
		// is the next statement and cannot be sent.
		tr := &stmtTracer{match: has("order by m.created_at asc, m.org_id asc"), after: cancel}
		run(t, tr, nil, func(r *http.Request) *http.Request { return r.WithContext(ctx) }, "sso sign-in failed")
	})
}
