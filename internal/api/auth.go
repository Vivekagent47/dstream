package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/streamingo/dstream/internal/auth"
	"github.com/streamingo/dstream/internal/store"
)

const magicLinkTTL = 15 * time.Minute

type magicLinkRequest struct {
	Email string `json:"email"`
}

// POST /api/auth/magic-link/request — issues a fresh single-use link for the
// given email. Always returns 202 to avoid leaking which addresses exist.
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

	token, err := auth.IssueMagicLink(r.Context(), d.Queries, email, magicLinkTTL)
	if err != nil {
		d.Log.Error("auth: issue magic link", "err", err, "email", email)
		// still return 202 to avoid leaking the failure mode
	} else {
		// TODO(phase-1.4): send email via SMTP. For dev we log the link.
		d.Log.Info("magic link issued (dev: open in browser)",
			"email", email,
			"link", "/api/auth/magic-link/verify?token="+url.QueryEscape(token))
	}
	w.WriteHeader(http.StatusAccepted)
}

// GET /api/auth/magic-link/verify?token=... — consumes the token, sets the
// session cookie, redirects to dashboard root.
func (d Deps) verifyMagicLink(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		httpErr(w, http.StatusBadRequest, "missing token")
		return
	}
	u, err := auth.ConsumeMagicLink(r.Context(), d.Queries, token)
	if err != nil {
		httpErr(w, http.StatusUnauthorized, "invalid or expired link")
		return
	}
	d.Signer.Issue(w, store.GoUUID(u.ID))
	http.Redirect(w, r, "/", http.StatusFound)
}

func (d Deps) logout(w http.ResponseWriter, _ *http.Request) {
	d.Signer.Clear(w)
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) me(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil {
		httpErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	out := map[string]any{}
	if p.UserID != [16]byte{} {
		u, err := d.Queries.GetUserByID(r.Context(), store.UUID(p.UserID))
		if err == nil {
			out["user"] = map[string]any{
				"id":             store.GoUUID(u.ID).String(),
				"email":          u.Email,
				"name":           u.Name,
				"is_super_admin": u.IsSuperAdmin,
			}
		}
	}
	if p.ProjectID != [16]byte{} {
		out["project_id"] = p.ProjectID.String()
	}
	writeJSON(w, http.StatusOK, out)
}
