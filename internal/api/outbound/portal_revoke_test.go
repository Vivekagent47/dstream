package outbound

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

func (f *fx) epoch(app store.Application) int64 {
	f.t.Helper()
	a, err := f.q.GetApplicationForOrg(context.Background(), store.GetApplicationForOrgParams{ID: app.ID, OrgID: app.OrgID})
	if err != nil {
		f.t.Fatal(err)
	}
	return a.PortalEpoch
}

// A portal token is only worth something while the app's epoch matches. Prove
// the token WORKS before revocation and is REFUSED after, through the real
// RequirePortal middleware, and that a fresh mint recovers access.
func TestPortalToken_StopsWorkingAfterRevoke(t *testing.T) {
	f := newFx(t)
	ep := f.mkEp(f.app, "https://ex.test/portal")
	other := f.mkEp(f.app2, "https://ex.test/portal-other")

	rec := f.own(http.MethodPost, appPath(f.app)+"/portal-access", nil)
	want(t, rec, http.StatusOK, "")
	body := obj(t, rec)
	tok := body["token"].(string)
	if body["url"] != "https://app.example.test/portal#token="+tok {
		t.Fatalf("portal url: %v", body["url"])
	}
	if exp, _ := time.Parse(time.RFC3339Nano, body["expires_at"].(string)); time.Until(exp) < 50*time.Minute || time.Until(exp) > time.Hour {
		t.Fatalf("expires_at must be ~TTL from now, got %v", body["expires_at"])
	}

	// Before revocation: scoped to this app only.
	rec = f.portalDo(http.MethodGet, "/api/portal/endpoints", tok)
	want(t, rec, http.StatusOK, "")
	if !strings.Contains(rec.Body.String(), uid36(ep.ID)) || strings.Contains(rec.Body.String(), uid36(other.ID)) {
		t.Fatalf("portal must list its own endpoint only: %s", rec.Body.String())
	}
	want(t, f.portalDo(http.MethodGet, "/api/portal/endpoints/"+uid36(other.ID), tok), http.StatusNotFound, "endpoint not found")
	want(t, f.portalDo(http.MethodGet, "/api/portal/endpoints/"+uid36(other.ID)+"/secret", tok), http.StatusNotFound, "endpoint not found")
	rec = f.portalDo(http.MethodGet, "/api/portal/endpoints/"+uid36(ep.ID)+"/secret", tok)
	want(t, rec, http.StatusOK, "")
	if obj(t, rec)["secret"] != ep.Secret {
		t.Fatalf("portal reads its own endpoint's secret")
	}

	// Revoke (admin) bumps the epoch…
	if f.epoch(f.app) != 0 {
		t.Fatalf("fresh app epoch must be 0")
	}
	rec = f.own(http.MethodPost, appPath(f.app)+"/portal-access/revoke", nil)
	want(t, rec, http.StatusNoContent, "")
	if f.epoch(f.app) != 1 {
		t.Fatalf("revoke must bump the stored epoch to 1, got %d", f.epoch(f.app))
	}
	if f.epoch(f.app2) != 0 {
		t.Fatalf("revoking one app must not touch its sibling")
	}
	// …and the already-issued token is dead on every portal route.
	for _, p := range []string{"/api/portal/endpoints", "/api/portal/app", "/api/portal/messages", "/api/portal/endpoints/" + uid36(ep.ID) + "/secret"} {
		if rec := f.portalDo(http.MethodGet, p, tok); rec.Code != http.StatusUnauthorized {
			t.Fatalf("revoked token on %s: got %d want 401", p, rec.Code)
		}
	}
	// A new mint carries the new epoch and works; the old token stays dead.
	tok2 := obj(t, f.own(http.MethodPost, appPath(f.app)+"/portal-access", nil))["token"].(string)
	want(t, f.portalDo(http.MethodGet, "/api/portal/endpoints", tok2), http.StatusOK, "")
	if rec := f.portalDo(http.MethodGet, "/api/portal/endpoints", tok); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old token revived after re-mint: %d", rec.Code)
	}
	// Deleting the application also kills its links.
	want(t, f.own(http.MethodDelete, appPath(f.app), nil), http.StatusNoContent, "")
	if rec := f.portalDo(http.MethodGet, "/api/portal/endpoints", tok2); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token for a deleted app: got %d want 401", rec.Code)
	}
}

func TestPortalToken_ExpiredIsRefused(t *testing.T) {
	f := newFx(t)
	expired := &auth.PortalSigner{Secret: f.ps.Secret, TTL: -time.Minute}
	tok, _ := expired.Mint(store.GoUUID(f.app.ID), f.oid, 0)
	if rec := f.portalDo(http.MethodGet, "/api/portal/endpoints", tok); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: got %d want 401", rec.Code)
	}
}

func TestPortalAccess_MembersCannotMintOrRevoke(t *testing.T) {
	f := newFx(t)
	member := seedMember(t, f.q, f.oid)
	want(t, f.as(member, f.oid, http.MethodPost, appPath(f.app)+"/portal-access", nil), http.StatusForbidden, "")
	want(t, f.as(member, f.oid, http.MethodPost, appPath(f.app)+"/portal-access/revoke", nil), http.StatusForbidden, "")
	if f.epoch(f.app) != 0 {
		t.Fatalf("a member revoked portal access")
	}
}

func TestPortalAccess_InvalidAppIDAndDatabaseFailure(t *testing.T) {
	f := newFx(t)
	want(t, f.own(http.MethodPost, "/api/applications/not-a-uuid/portal-access", nil), http.StatusBadRequest, "invalid app id")

	g := newFx(t, withQueries(tracedQueries(t, failAll("portal_epoch = portal_epoch + 1"))))
	want(t, g.own(http.MethodPost, appPath(g.app)+"/portal-access/revoke", nil), http.StatusInternalServerError, "revoke failed")
	if g.epoch(g.app) != 0 {
		t.Fatalf("failed revoke must not bump the epoch")
	}
}
