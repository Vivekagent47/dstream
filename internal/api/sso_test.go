package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/config"
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
