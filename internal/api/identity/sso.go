package identity

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis_rate/v10"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// ssoStateTTL is the IdP round-trip budget, not a session lifetime.
const ssoStateTTL = 10 * time.Minute

// ssoStateCookieName binds the Redis state row to the browser that began the
// login. It is what makes a GET callback safe.
//
// Without it the callback is a working login-CSRF / session-fixation
// primitive: /sso/start is unauthenticated, so an attacker mints a valid
// state himself, completes the IdP login with HIS account, and phishes the
// victim into a top-level GET of the callback inside the 10-minute TTL. Every
// other check passes — state present, ID token genuine, nonce matched, email
// verified — and the victim's browser is handed a session for the attacker's
// account, after which the victim's work happens inside the attacker's org.
// SameSite=Lax constrains cookie *sending*, not *setting*, and the CSRF
// middleware exempts safe methods, so neither stops it. This cookie does: the
// attacker cannot set a cookie in the victim's browser for this origin, absent
// control of a sibling subdomain. OAuth 2.0 Security BCP §4.7.
//
// That caveat is the named residual, not a theoretical one — a
// Domain=example.com cookie tossed from a compromised sibling subdomain is
// sent here and r.Cookie takes the first match, and this cookie is not
// __Host- prefixed (that requires Secure, false in local HTTP dev). Unlike
// dstream_session (HMAC-signed) and dstream_csrf (bound to the session value),
// this cookie's entire security property is that its value cannot be injected,
// which makes it the one cookie in the repo where a sibling-subdomain write is
// directly exploitable. See internal/middleware/csrf.go for the same caveat on
// the CSRF cookie, and router.go's /sso route comment.
const ssoStateCookieName = "dstream_sso_state"

// ssoStartPerIP budgets /sso/start per client IP. The endpoint is
// unauthenticated and each call writes a 10-minute Redis key into the SAME
// instance that carries the delivery queue (one *redis.Client is shared, see
// cmd/dstream/server.go), so an unmetered flood could push a self-host Redis
// to its maxmemory and have an allkeys-* policy evict queued webhook
// deliveries.
//
// The budget is per EGRESS IP, and SSO's typical deployment is a whole company
// behind one NAT address — so a rate that is generous per human is tight per
// gateway, and a mid-size office would hit 429 on entirely legitimate logins.
// 300/hour. With the 10-minute state TTL that still caps live state keys from
// one IP at ~50, so the real protection is the bounded blast radius rather
// than a tight budget.
var ssoStartPerIP = redis_rate.PerHour(300)

type ssoState struct {
	Nonce    string `json:"nonce"`
	ReturnTo string `json:"return_to"`
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// safeReturnTo reduces a caller-supplied redirect target to an app-relative
// path, defaulting to "/". An open redirect on a login callback is a
// credential-phishing primitive — the victim really did authenticate, so the
// landing page can convincingly ask for anything — so anything that could
// leave this origin (an absolute URL, a scheme-relative "//host", a backslash
// variant) is discarded rather than sanitised.
func safeReturnTo(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") {
		return "/"
	}
	if strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") {
		return "/"
	}
	return raw
}

// ssoDefaultRole is the role a first-time SSO user lands at in the configured
// default org. config.ValidateOIDC accepts an empty DSTREAM_OIDC_DEFAULT_ROLE
// (viper's default fills it in), so the documented default is restated here
// rather than letting an empty role reach a NOT NULL/CHECK column.
func (d Handlers) ssoDefaultRole() auth.Role {
	if d.OIDC.DefaultRole == "" {
		return auth.RoleMember
	}
	return auth.Role(d.OIDC.DefaultRole)
}

// StartSSO handles GET /api/auth/sso/start — it redirects the browser to the
// IdP.
//
// State and nonce are generated here and stored in Redis for the callback to
// consume exactly once.
func (d Handlers) StartSSO(w http.ResponseWriter, r *http.Request) {
	if d.Authenticator == nil {
		httpx.Err(w, http.StatusNotFound, "sso not configured")
		return
	}
	res, err := redis_rate.NewLimiter(d.Redis).Allow(r.Context(),
		"sso_start:ip:"+clientIP(r), ssoStartPerIP)
	if err != nil {
		d.Log.Error("sso: rate limit", "err", err)
		httpx.Err(w, http.StatusServiceUnavailable, "sso unavailable")
		return
	}
	if res.Allowed == 0 {
		// Same Retry-After the sibling per-IP limiters set (auth.go,
		// invites.go), so a NAT'd client knows when to come back.
		w.Header().Set("Retry-After", strconv.FormatInt(int64(res.RetryAfter.Seconds())+1, 10))
		httpx.Err(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	state, err := randomToken()
	if err != nil {
		d.Log.Error("sso: generate state", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "sso unavailable")
		return
	}
	nonce, err := randomToken()
	if err != nil {
		d.Log.Error("sso: generate nonce", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "sso unavailable")
		return
	}
	payload, err := json.Marshal(ssoState{
		Nonce:    nonce,
		ReturnTo: safeReturnTo(r.URL.Query().Get("return_to")),
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "sso unavailable")
		return
	}
	if err := d.Redis.Set(r.Context(), "sso:state:"+state, payload, ssoStateTTL).Err(); err != nil {
		d.Log.Error("sso: store state", "err", err)
		httpx.Err(w, http.StatusServiceUnavailable, "sso unavailable")
		return
	}
	// Bind the state to this browser. SameSite=Lax still sends it on the
	// IdP's top-level redirect back, so the legitimate flow is unaffected.
	http.SetCookie(w, &http.Cookie{
		Name:     ssoStateCookieName,
		Value:    state,
		Path:     "/",
		MaxAge:   int(ssoStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   d.Signer.Secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, d.Authenticator.AuthCodeURL(state, nonce), http.StatusFound)
}

// clearSSOStateCookie expires the binding cookie. Called once at the top of
// the callback so EVERY exit path below drops it — a state that has been
// presented must not be presentable again, whatever the outcome.
func (d Handlers) clearSSOStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     ssoStateCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   d.Signer.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// CallbackSSO handles GET /api/auth/sso/callback — it verifies the IdP
// response, provisions the user through the shared bootstrap, and issues the
// same session cookie a magic-link login would.
func (d Handlers) CallbackSSO(w http.ResponseWriter, r *http.Request) {
	if d.Authenticator == nil {
		httpx.Err(w, http.StatusNotFound, "sso not configured")
		return
	}
	d.clearSSOStateCookie(w)

	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		// The IdP declined (consent denied, etc). Not an error on our side.
		// Attacker-controlled and unbounded, so truncate before it reaches a
		// log sink.
		if len(e) > 128 {
			e = e[:128]
		}
		d.Log.Info("sso: idp returned error", "error", e)
		httpx.Err(w, http.StatusUnauthorized, "sso sign-in was declined")
		return
	}
	state, code := q.Get("state"), q.Get("code")
	if state == "" || code == "" {
		httpx.Err(w, http.StatusBadRequest, "missing state or code")
		return
	}

	// GETDEL makes the state single-use: a replayed callback finds nothing, so
	// a leaked callback URL is not a reusable login.
	raw, err := d.Redis.GetDel(r.Context(), "sso:state:"+state).Bytes()
	if err != nil || len(raw) == 0 {
		httpx.Err(w, http.StatusBadRequest, "unknown or expired sso state")
		return
	}
	var st ssoState
	if err := json.Unmarshal(raw, &st); err != nil {
		httpx.Err(w, http.StatusBadRequest, "unknown or expired sso state")
		return
	}

	// The state must have been issued to THIS browser. See
	// ssoStateCookieName: without this the callback is a login-CSRF /
	// session-fixation primitive, because a valid state is free to obtain
	// from the unauthenticated /sso/start.
	c, err := r.Cookie(ssoStateCookieName)
	if err != nil || c.Value != state {
		d.Log.Warn("sso: state not bound to this browser")
		httpx.Err(w, http.StatusBadRequest, "sso state did not originate in this browser")
		return
	}

	id, err := d.Authenticator.Exchange(r.Context(), code)
	if err != nil {
		d.Log.Warn("sso: exchange failed", "err", err)
		httpx.Err(w, http.StatusUnauthorized, "sso sign-in failed")
		return
	}
	// Nonce binds the ID token to this login attempt. The empty check is not
	// redundant: without it, a token carrying no nonce would pass whenever the
	// stored state somehow carried none either.
	if id.Nonce == "" || id.Nonce != st.Nonce {
		d.Log.Warn("sso: nonce mismatch")
		httpx.Err(w, http.StatusUnauthorized, "sso sign-in failed")
		return
	}
	email := strings.TrimSpace(strings.ToLower(id.Email))
	if email == "" {
		d.Log.Warn("sso: id token carried no email claim")
		httpx.Err(w, http.StatusUnauthorized, "sso sign-in failed: no email claim")
		return
	}
	// The verified email is the join key against users.email, so an
	// unverified address would let the IdP assert an address it does not
	// control and sign into the matching dstream account.
	if !id.EmailVerified {
		d.Log.Warn("sso: refusing unverified email claim")
		httpx.Err(w, http.StatusUnauthorized, "sso sign-in failed: email not verified by the identity provider")
		return
	}

	tx, err := d.Pool.Begin(r.Context())
	if err != nil {
		d.Log.Error("sso: begin tx", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "sso unavailable")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	u, err := auth.BootstrapSession(r.Context(), tx, d.Queries, email,
		d.OIDC.DefaultOrgSlug, d.ssoDefaultRole())
	if err != nil {
		// A default-org slug matching no org is an operator misconfiguration,
		// not an authentication failure. It is deliberately not validated at
		// boot (that would couple startup to DB seeding state and brick a
		// deployment whose default org is created by a later seed step), so
		// this is the ONLY signal an operator gets — it has to name the
		// variable, and it must not be a 401 that reads as the user's fault.
		if errors.Is(err, auth.ErrDefaultOrgNotFound) {
			d.Log.Error("sso: DSTREAM_OIDC_DEFAULT_ORG names an org that does not exist — create it or unset the variable",
				"default_org", d.OIDC.DefaultOrgSlug, "err", err)
			httpx.Err(w, http.StatusInternalServerError,
				"sso is misconfigured: DSTREAM_OIDC_DEFAULT_ORG does not match any organization")
			return
		}
		d.Log.Error("sso: bootstrap", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "sso sign-in failed")
		return
	}
	activeOrg, err := d.Queries.WithTx(tx).GetFirstOrgForUser(r.Context(), u.ID)
	if err != nil {
		d.Log.Error("sso: resolve active org", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "sso sign-in failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		d.Log.Error("sso: commit", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "sso sign-in failed")
		return
	}

	// Same session issue as the magic-link path: shared cookie flags, shared
	// epoch revocation. A divergent session here would mean logout-all did
	// not cover SSO users.
	d.Signer.Issue(w, store.GoUUID(u.ID), store.GoUUID(activeOrg), int64(u.SessionEpoch))
	http.Redirect(w, r, strings.TrimRight(d.AppBaseURL, "/")+safeReturnTo(st.ReturnTo), http.StatusFound)
}

// AuthMethods handles GET /api/auth/methods — it tells the unauthenticated
// login page which login methods to render. No secret: a deployment's use of
// SSO is visible from its login page regardless.
func (d Handlers) AuthMethods(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"sso":        d.Authenticator != nil,
		"magic_link": !d.OIDC.Enforce,
	})
}
