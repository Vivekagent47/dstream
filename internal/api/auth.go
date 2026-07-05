package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

const magicLinkTTL = 15 * time.Minute

// Magic-link request budgets. Picked to allow normal use (occasional
// re-requests) while choking abuse (mailbox bombing, token spam).
var (
	magicLinkPerEmail = redis_rate.PerHour(5)
	magicLinkPerIP    = redis_rate.PerHour(30)
)

type magicLinkRequest struct {
	Email string `json:"email"`
}

// POST /api/auth/magic-link/request — issues a fresh single-use link for the
// given email. Always returns 202 (or 429) without leaking which addresses
// exist.
func (d Deps) requestMagicLink(w http.ResponseWriter, r *http.Request) {
	var body magicLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	email := strings.TrimSpace(strings.ToLower(body.Email))
	if email == "" || !strings.Contains(email, "@") {
		httpErr(w, http.StatusBadRequest, "invalid email")
		return
	}

	ip := clientIP(r)
	limiter := redis_rate.NewLimiter(d.Redis)
	for _, k := range []struct {
		key   string
		limit redis_rate.Limit
	}{
		{"magic_link:email:" + email, magicLinkPerEmail},
		{"magic_link:ip:" + ip, magicLinkPerIP},
	} {
		res, err := limiter.Allow(r.Context(), k.key, k.limit)
		if err != nil {
			d.Log.Error("auth: magic link rate limit", "err", err)
			httpErr(w, http.StatusServiceUnavailable, "rate limiter unavailable")
			return
		}
		if res.Allowed == 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(res.RetryAfter.Seconds())+1, 10))
			httpErr(w, http.StatusTooManyRequests, "too many requests")
			return
		}
	}

	token, err := auth.IssueMagicLink(r.Context(), d.Queries, email, magicLinkTTL)
	if err != nil {
		d.Log.Error("auth: issue magic link", "err", err, "email", email)
		// still return 202 to avoid leaking the failure mode
	} else {
		// TODO(phase-1.4): send email via SMTP. The plaintext token is
		// logged ONLY in dev mode — in prod the log is an audit-bypass
		// vector (anyone with log-read access could grab the link, click
		// it, and pin the victim's session to themselves).
		if d.DevMode {
			d.Log.Info("magic link issued (dev: open in browser)",
				"email", email,
				"link", "/auth/verify?token="+url.QueryEscape(token))
		} else {
			d.Log.Info("magic link issued", "email", email)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// POST /api/auth/magic-link/verify {token} — consumes the token, runs the
// org-bootstrap step (auto-join invites, mint a personal workspace if the
// user is new), and sets the session cookie carrying (user_id, active_org_id).
//
// POST with a JSON body (not GET): a GET would be reachable cross-site and
// CSRF-exempt, enabling login-CSRF / session fixation. The JSON body forces a
// CORS preflight so a foreign origin can't drive it. Returns 204; the SPA calls
// this via XHR from /auth/verify and drives navigation itself.
func (d Deps) verifyMagicLink(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	token := strings.TrimSpace(body.Token)
	if token == "" {
		httpErr(w, http.StatusBadRequest, "missing token")
		return
	}
	u, orgID, err := auth.ConsumeMagicLink(r.Context(), d.Pool, d.Queries, token)
	if err != nil {
		httpErr(w, http.StatusUnauthorized, "invalid or expired link")
		return
	}
	d.Signer.Issue(w, store.GoUUID(u.ID), orgID, int64(u.SessionEpoch))
	w.WriteHeader(http.StatusNoContent)
}

// logout clears the cookie AND bumps the user's session_epoch, invalidating
// every outstanding session for that user (logout-all), not just this cookie.
// Unauthenticated route, so we parse the cookie ourselves for the user id.
func (d Deps) logout(w http.ResponseWriter, r *http.Request) {
	if uid, _, _, err := d.Signer.Parse(r); err == nil {
		if err := d.Queries.BumpUserSessionEpoch(r.Context(), store.UUID(uid)); err != nil {
			d.Log.Warn("logout: bump session epoch", "err", err)
		}
	}
	d.Signer.Clear(w)
	w.WriteHeader(http.StatusNoContent)
}

// me returns the calling principal's identity. For a session principal this
// is {user, orgs[], active_org_id}; for an API-key principal it's just the
// scoped org id so the UI can still render "logged in as <key>".
//
// User + orgs lookups run concurrently — this handler is hit on every page
// navigation in the SPA, and the two queries are independent.
func (d Deps) me(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil {
		httpErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	out := map[string]any{}
	switch p.Source {
	case auth.SourceSession:
		if p.UserID != uuid.Nil {
			var (
				u       store.User
				orgs    []store.ListOrgsForUserRow
				uErr    error
				orgsErr error
			)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				u, uErr = d.Queries.GetUserByID(r.Context(), store.UUID(p.UserID))
			}()
			go func() {
				defer wg.Done()
				orgs, orgsErr = d.Queries.ListOrgsForUser(r.Context(), store.UUID(p.UserID))
			}()
			wg.Wait()
			if uErr != nil {
				// HMAC-valid cookie but user vanished (deleted out-of-band):
				// surface 401 so the SPA forces re-login instead of rendering
				// a broken state with no user object.
				d.Log.Warn("me: load user", "err", uErr)
				httpErr(w, http.StatusUnauthorized, "session user not found")
				return
			}
			userOut := map[string]any{
				"id":    store.GoUUID(u.ID).String(),
				"email": u.Email,
				"name":  u.Name,
			}
			// Only emit is_super_admin when actually super-admin — a stolen
			// session shouldn't have escalation hints painted into /me.
			if u.IsSuperAdmin {
				userOut["is_super_admin"] = true
			}
			out["user"] = userOut
			if orgsErr != nil {
				d.Log.Warn("me: list orgs", "err", orgsErr)
			}
			out["orgs"] = orgs
		}
		if p.OrgID != uuid.Nil {
			out["active_org_id"] = p.OrgID.String()
		}
	case auth.SourceAPIKey:
		out["api_key"] = map[string]any{"org_id": p.OrgID.String()}
	}
	writeJSON(w, http.StatusOK, out)
}
