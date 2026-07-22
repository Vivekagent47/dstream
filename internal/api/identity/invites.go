package identity

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-redis/redis_rate/v10"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// Org-invite rate budgets. Tuned to allow normal workflows ("invite a few
// teammates") while choking spam ("blast 1000 random emails").
var (
	invitePerInviter = redis_rate.PerHour(30)
	invitePerEmail   = redis_rate.PerHour(10)
)

// orgInviteTTL is the default invite lifetime. Long enough that a recipient
// can sit on the email for a week; short enough that a stale link can't be
// dredged up months later.
const orgInviteTTL = 7 * 24 * time.Hour

// isUniqueViolation detects Postgres unique_violation (SQLSTATE 23505)
// via errors.As against *pgconn.PgError.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ListInvites serves GET /api/orgs/{org_id}/invites — any member can read.
// Session-only. Returns the joined invitee/inviter rows directly.
func (d Handlers) ListInvites(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpx.Err(w, http.StatusForbidden, "session required")
		return
	}
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid org_id")
		return
	}
	p, _ := auth.FromContext(r.Context())
	if _, err := d.Queries.GetOrgMember(r.Context(), store.GetOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: store.UUID(p.UserID),
	}); err != nil {
		httpx.Err(w, http.StatusForbidden, "not a member")
		return
	}
	rows, err := d.Queries.ListOrgInvitesByOrg(r.Context(), store.UUID(orgID))
	if err != nil {
		d.Log.Error("list invites", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "list invites")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, inviteListView(row))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// inviteListView shapes the per-row response for GET /api/orgs/{id}/invites.
// CRITICAL: token_hash is omitted. Even though the hash is sha256 of a
// 32-byte secret token (not the token itself), leaking it to every org
// member widens the attack surface — e.g. an attacker who later sees a
// candidate token elsewhere can confirm a match without contacting the
// server. We also omit invited_by (a raw user UUID); the joined
// invited_by_email column gives the UI what it actually needs.
func inviteListView(r store.ListOrgInvitesByOrgRow) map[string]any {
	out := map[string]any{
		"id":         store.GoUUID(r.ID).String(),
		"email":      r.Email,
		"role":       r.Role,
		"expires_at": r.ExpiresAt.Time,
		"created_at": r.CreatedAt.Time,
	}
	if r.InvitedByEmail != "" {
		out["invited_by_email"] = r.InvitedByEmail
	}
	if r.AcceptedAt.Valid {
		out["accepted_at"] = r.AcceptedAt.Time
	}
	return out
}

// CreateInvite serves POST /api/orgs/{org_id}/invites — admin+ only.
// Body: {email, role}. The email link is logged (dev) — SMTP wiring lands
// later. We rate-limit per inviter AND per invitee to choke both mailbox-bomb
// and account-spam attacks.
func (d Handlers) CreateInvite(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpx.Err(w, http.StatusForbidden, "session required")
		return
	}
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid org_id")
		return
	}
	p, _ := auth.FromContext(r.Context())
	caller, err := d.Queries.GetOrgMember(r.Context(), store.GetOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: store.UUID(p.UserID),
	})
	if err != nil {
		httpx.Err(w, http.StatusForbidden, "not a member")
		return
	}
	if auth.Role(caller.Role).LessThan(auth.RoleAdmin) {
		httpx.Err(w, http.StatusForbidden, "admin required")
		return
	}
	var body struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" || !strings.Contains(email, "@") {
		httpx.Err(w, http.StatusBadRequest, "invalid email")
		return
	}
	// Owner-level invites are deliberately not exposed: ownership is
	// transferred via /transfer, not granted directly.
	if body.Role != string(auth.RoleAdmin) && body.Role != string(auth.RoleMember) {
		httpx.Err(w, http.StatusBadRequest, "role must be admin or member")
		return
	}

	// Rate-limit per inviter and per invitee. We fail closed on the limiter
	// being unreachable (503) so misbehaving instances can't sidestep the
	// budgets by hammering Redis until it dies.
	limiter := redis_rate.NewLimiter(d.Redis)
	for _, k := range []struct {
		key   string
		limit redis_rate.Limit
	}{
		{"invite:inviter:" + p.UserID.String(), invitePerInviter},
		{"invite:email:" + email, invitePerEmail},
	} {
		res, lerr := limiter.Allow(r.Context(), k.key, k.limit)
		if lerr != nil {
			d.Log.Error("invite rate limit", "err", lerr)
			httpx.Err(w, http.StatusServiceUnavailable, "rate limiter unavailable")
			return
		}
		if res.Allowed == 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(res.RetryAfter.Seconds())+1, 10))
			httpx.Err(w, http.StatusTooManyRequests, "rate limited")
			return
		}
	}

	// Pre-flight: reject if the user already has a membership row in this
	// org. We swallow non-pgx.ErrNoRows lookup failures (best-effort) since
	// the unique index on org_invites still protects the canonical case.
	if u, err := d.Queries.GetUserByEmail(r.Context(), email); err == nil {
		if _, gerr := d.Queries.GetOrgMember(r.Context(), store.GetOrgMemberParams{
			OrgID:  store.UUID(orgID),
			UserID: u.ID,
		}); gerr == nil {
			httpx.Err(w, http.StatusConflict, "already a member")
			return
		}
	}

	token, err := auth.IssueOrgInvite(r.Context(), d.Queries,
		orgID, p.UserID, email, auth.Role(body.Role), orgInviteTTL)
	if err != nil {
		if isUniqueViolation(err) {
			// Partial unique index on (org_id, email) WHERE accepted_at IS
			// NULL — surface as a clean 409 so callers can re-list to find
			// the pending invite.
			httpx.Err(w, http.StatusConflict, "pending invite exists")
			return
		}
		d.Log.Error("issue invite", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "issue invite")
		return
	}

	// TODO(SMTP): send email. The plaintext invite link is logged ONLY in
	// dev mode — in prod the log is an audit-bypass vector (anyone with
	// log-read access could click the link and join the org as the
	// invited identity).
	if d.DevMode {
		link := strings.TrimRight(d.PublicBaseURL, "/") + "/invites/" + token
		d.Log.Info("org invite issued", "org_id", orgID, "email", email, "link", link)
	} else {
		d.Log.Info("org invite issued", "org_id", orgID, "email", email)
	}

	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "member.invite",
		TargetType: "invite",
		OrgID:      orgID,
		Metadata: map[string]any{
			"email": email,
			"role":  body.Role,
		},
	})
	w.WriteHeader(http.StatusAccepted)
}

// DeleteInvite serves DELETE /api/orgs/{org_id}/invites/{id} — admin+ only.
// Soft-revocation isn't a thing for invites in v1: we just hard-delete the
// row, which makes the token instantly un-redeemable.
func (d Handlers) DeleteInvite(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpx.Err(w, http.StatusForbidden, "session required")
		return
	}
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid org_id")
		return
	}
	inviteID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid invite id")
		return
	}
	p, _ := auth.FromContext(r.Context())
	caller, err := d.Queries.GetOrgMember(r.Context(), store.GetOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: store.UUID(p.UserID),
	})
	if err != nil {
		httpx.Err(w, http.StatusForbidden, "not a member")
		return
	}
	if auth.Role(caller.Role).LessThan(auth.RoleAdmin) {
		httpx.Err(w, http.StatusForbidden, "admin required")
		return
	}
	if err := d.Queries.DeleteOrgInvite(r.Context(), store.DeleteOrgInviteParams{
		ID:    store.UUID(inviteID),
		OrgID: store.UUID(orgID),
	}); err != nil {
		d.Log.Error("delete invite", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "delete invite")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "invite.revoke",
		TargetType: "invite",
		TargetID:   audit.PtrUUID(inviteID),
		OrgID:      orgID,
	})
	w.WriteHeader(http.StatusNoContent)
}

// peekInvitePerIP throttles unauthenticated peek probes: a 32-byte token
// is unbruteforceable but flooding /api/invites/{junk} would spam the
// not-found log. 60/h matches a generous human-retry rate.
var peekInvitePerIP = redis_rate.PerHour(60)

// PeekInvite serves GET /api/invites/{token} — fully public. Lets a
// logged-out recipient see what org / role / email they're being invited to
// before they sign in. Token leaks are contained: we don't return the
// invitedBy user_id or anything that could be cross-referenced.
func (d Handlers) PeekInvite(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		httpx.Err(w, http.StatusBadRequest, "missing token")
		return
	}
	// Per-IP rate limit on this unauthenticated endpoint. Failures here
	// shouldn't 503 the legit user — if Redis is down we fail OPEN
	// (allow the peek), matching the existing dev-friendly philosophy.
	limiter := redis_rate.NewLimiter(d.Redis)
	res, lerr := limiter.Allow(r.Context(), "invite:peek:ip:"+clientIP(r), peekInvitePerIP)
	if lerr == nil && res.Allowed == 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(res.RetryAfter.Seconds())+1, 10))
		httpx.Err(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	h := sha256.Sum256([]byte(token))
	row, err := d.Queries.GetActiveOrgInviteByTokenHash(r.Context(), h[:])
	if err != nil {
		// Demote not-found to debug — probe floods shouldn't fill the
		// warn channel. Real DB errors still warn (caller can grep).
		if !errors.Is(err, pgx.ErrNoRows) {
			d.Log.Warn("peek invite", "err", err)
		}
		httpx.Err(w, http.StatusNotFound, "invalid invite")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"org_id":     store.GoUUID(row.OrgID).String(),
		"org_name":   row.OrgName,
		"email":      row.Email,
		"role":       row.Role,
		"expires_at": row.ExpiresAt.Time,
	})
}

// AcceptInvite serves POST /api/invites/{token}/accept — handles two flows:
//
//   - Path A: caller has a valid session AND their user.email matches the
//     invite. We consume the invite, re-issue the cookie pointing at the
//     newly-joined org, and return 200.
//   - Path B: caller has no session (or wrong session). We mint a fresh
//     magic-link for the invite's email — the existing ConsumeMagicLink
//     bootstrap will auto-apply the pending invite when they verify.
//
// We deliberately do NOT consume the invite in Path B — that happens inside
// ConsumeMagicLink so the user's first real cookie carries the right
// active_org_id.
func (d Handlers) AcceptInvite(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		httpx.Err(w, http.StatusBadRequest, "missing token")
		return
	}
	h := sha256.Sum256([]byte(token))
	inv, err := d.Queries.GetActiveOrgInviteByTokenHash(r.Context(), h[:])
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			d.Log.Warn("accept invite lookup", "err", err)
		}
		httpx.Err(w, http.StatusNotFound, "invalid invite")
		return
	}

	// Path A — session present + matches invite email.
	if uid, _, epoch, err := d.Signer.Parse(r); err == nil {
		u, gerr := d.Queries.GetUserByID(r.Context(), store.UUID(uid))
		if gerr == nil && int64(u.SessionEpoch) == epoch && strings.EqualFold(u.Email, inv.Email) {
			row, cerr := auth.ConsumeOrgInvite(r.Context(), d.Pool, d.Queries, token, uid)
			if cerr != nil {
				if errors.Is(cerr, auth.ErrInvalidOrgInvite) {
					httpx.Err(w, http.StatusNotFound, "invalid invite")
					return
				}
				d.Log.Error("consume invite", "err", cerr)
				httpx.Err(w, http.StatusInternalServerError, "consume invite")
				return
			}
			// Re-issue cookie so subsequent requests land in the newly-joined
			// org without a manual /orgs/select hop.
			orgUUID := store.GoUUID(row.OrgID)
			d.Signer.Issue(w, uid, orgUUID, int64(u.SessionEpoch))
			// Construct ctx with refreshed principal so audit.Log records
			// the right active_org_id (the request still carries the old
			// cookie). We rebuild Principal minimally — just enough for
			// audit.Log to resolve actor + org_id.
			ctx := auth.WithPrincipal(r.Context(), auth.Principal{
				Source: auth.SourceSession,
				UserID: uid,
				OrgID:  orgUUID,
				Role:   auth.Role(row.Role),
			})
			audit.Log(ctx, d.Queries, d.Log, audit.Entry{
				Action:     "member.accept_invite",
				TargetType: "member",
				TargetID:   audit.PtrUUID(uid),
				OrgID:      orgUUID,
				Metadata: map[string]any{
					"org_id": orgUUID.String(),
					"role":   row.Role,
				},
			})
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"org_id": orgUUID.String(),
				"role":   row.Role,
			})
			return
		}
		// Wrong account signed in — bounce so the user can switch.
		if gerr == nil {
			httpx.Err(w, http.StatusForbidden, "invite addressed to different email")
			return
		}
	}

	// Path B — issue a magic link to the invite's email. The user will land
	// in the bootstrap flow on verify, which auto-applies pending invites.
	//
	// Apply the SAME budgets as POST /api/auth/magic-link/request, keyed on
	// the invite's email + caller's IP. Without this, anyone holding ANY
	// valid invite token can mail-bomb the invitee by POSTing accept in a
	// loop, bypassing the limiter that the public request endpoint enforces.
	ip := clientIP(r)
	limiter := redis_rate.NewLimiter(d.Redis)
	for _, k := range []struct {
		key   string
		limit redis_rate.Limit
	}{
		{"magic_link:email:" + inv.Email, magicLinkPerEmail},
		{"magic_link:ip:" + ip, magicLinkPerIP},
	} {
		res, lerr := limiter.Allow(r.Context(), k.key, k.limit)
		if lerr != nil {
			d.Log.Error("accept invite rate limit", "err", lerr)
			httpx.Err(w, http.StatusServiceUnavailable, "rate limiter unavailable")
			return
		}
		if res.Allowed == 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(res.RetryAfter.Seconds())+1, 10))
			httpx.Err(w, http.StatusTooManyRequests, "too many requests")
			return
		}
	}

	mlTok, err := auth.IssueMagicLink(r.Context(), d.Queries, inv.Email, 15*time.Minute)
	if err != nil {
		d.Log.Error("issue magic link for invite", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "magic link")
		return
	}
	if d.DevMode {
		d.Log.Info("magic link for invite accept (dev)",
			"email", inv.Email,
			"link", "/api/auth/magic-link/verify?token="+url.QueryEscape(mlTok))
	} else {
		d.Log.Info("magic link for invite accept", "email", inv.Email)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"requires_login": true})
}
