package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
)

// --- Changing a delivery URL requires admin ---
//
// These cannot live in rbacMatrix (rbac_test.go): the matrix keys on method +
// route, and this gate depends on the request BODY — the same PATCH is allowed
// or refused depending on whether it moves the URL. A member editing a name is
// ordinary member work; a member repointing live traffic at a host they choose
// is not, and nothing notifies the org when it happens.
//
// The SSRF and loop guards do not help here. They stop a URL reaching dstream
// itself or a private address; an attacker's public endpoint passes both.

func seedDestination(t *testing.T, q *store.Queries, orgID uuid.UUID, url string) uuid.UUID {
	t.Helper()
	d, err := q.CreateDestination(context.Background(), store.CreateDestinationParams{
		OrgID: store.UUID(orgID),
		Name:  "dest-" + uuid.NewString()[:8],
		Type:  "http",
		Url:   &url,
		// NOT NULL with no default in the schema; the handler always sends a
		// JSON object, so an empty one is what an unconfigured destination has.
		AuthConfig: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}
	return store.GoUUID(d.ID)
}

// TestMemberCannotRepointDestination is the security case: a member moving a
// destination's URL is refused, and the stored URL is unchanged afterwards —
// asserting the status alone would pass even if the write had landed.
func TestMemberCannotRepointDestination(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedUserAndOrg(t, q)
	member := seedUser(t, q)
	addMember(t, q, oid, member, "member")
	did := seedDestination(t, q, oid, "https://original.example.com/hook")

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPatch,
		"/api/destinations/"+did.String(), member, oid, map[string]any{
			"url": "https://attacker.example.com/collect",
		})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("member repoint: got %d want 403; body=%s", rec.Code, rec.Body.String())
	}
	row, err := q.GetDestinationForOrg(context.Background(), store.GetDestinationForOrgParams{
		ID: store.UUID(did), OrgID: store.UUID(oid),
	})
	if err != nil {
		t.Fatalf("get destination: %v", err)
	}
	if row.Url == nil || *row.Url != "https://original.example.com/hook" {
		t.Errorf("destination url changed despite the 403: %v", row.Url)
	}
}

// TestMemberCanEditDestinationWithoutMovingURL is the other half, and the
// reason this is a body check rather than an admin-only route: the dashboard
// PATCHes whole forms, so a member renaming a destination resends the
// unchanged URL. That must still succeed, or the gate breaks ordinary work.
func TestMemberCanEditDestinationWithoutMovingURL(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedUserAndOrg(t, q)
	member := seedUser(t, q)
	addMember(t, q, oid, member, "member")
	did := seedDestination(t, q, oid, "https://original.example.com/hook")

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPatch,
		"/api/destinations/"+did.String(), member, oid, map[string]any{
			"description": "renamed by a member",
			"url":         "https://original.example.com/hook", // resent unchanged
		})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("member edit with unchanged url: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAdminCanRepointDestination pins that the gate is a role check and not an
// accidental block on everyone.
func TestAdminCanRepointDestination(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedUserAndOrg(t, q)
	admin := seedUser(t, q)
	addMember(t, q, oid, admin, "admin")
	did := seedDestination(t, q, oid, "https://original.example.com/hook")

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPatch,
		"/api/destinations/"+did.String(), admin, oid, map[string]any{
			"url": "https://moved.example.com/hook",
		})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("admin repoint: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	row, err := q.GetDestinationForOrg(context.Background(), store.GetDestinationForOrgParams{
		ID: store.UUID(did), OrgID: store.UUID(oid),
	})
	if err != nil {
		t.Fatalf("get destination: %v", err)
	}
	if row.Url == nil || *row.Url != "https://moved.example.com/hook" {
		t.Errorf("admin repoint did not apply: %v", row.Url)
	}
}
