package store_test

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/store"
)

// --- DB-gated harness ---
//
// Mirrors the t.Skip(env) pattern used by the api/auth packages so all
// integration tests share one toggle (DSTREAM_TEST_DB_URL). Each test seeds
// fresh orgs (unique slugs via uuid) so the suite can run in parallel and
// against a shared database without conflicts.

func isolationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := store.NewPool(ctx, dsn, 2)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type seededOrg struct {
	OrgID  uuid.UUID
	UserID uuid.UUID
}

func seedIsolationOrg(t *testing.T, q *store.Queries, label string) seededOrg {
	t.Helper()
	ctx := context.Background()
	u, err := q.CreateUser(ctx, store.CreateUserParams{
		Email: label + "+" + uuid.NewString() + "@example.test",
	})
	if err != nil {
		t.Fatalf("create user (%s): %v", label, err)
	}
	o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: label,
		Slug: label + "-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create org (%s): %v", label, err)
	}
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
		OrgID:  o.ID,
		UserID: u.ID,
		Role:   "owner",
	}); err != nil {
		t.Fatalf("add member (%s): %v", label, err)
	}
	return seededOrg{
		OrgID:  store.GoUUID(o.ID),
		UserID: store.GoUUID(u.ID),
	}
}

func mustToken() string {
	return "tok_" + uuid.NewString()
}

// --- sources ---

func TestIsolation_ListSourcesByOrg_DoesNotLeakOrgB(t *testing.T) {
	pool := isolationPool(t)
	q := store.New(pool)
	orgA := seedIsolationOrg(t, q, "iso-srcA")
	orgB := seedIsolationOrg(t, q, "iso-srcB")

	ctx := context.Background()
	srcA, err := q.CreateSource(ctx, store.CreateSourceParams{
		OrgID:         store.UUID(orgA.OrgID),
		Name:          "stripe",
		Type:          "stripe",
		IngestToken:   mustToken(),
		SigningConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create src A: %v", err)
	}
	if _, err := q.CreateSource(ctx, store.CreateSourceParams{
		OrgID:         store.UUID(orgB.OrgID),
		Name:          "github",
		Type:          "github",
		IngestToken:   mustToken(),
		SigningConfig: []byte("{}"),
	}); err != nil {
		t.Fatalf("create src B: %v", err)
	}

	rows, err := q.ListSourcesByOrg(ctx, store.UUID(orgA.OrgID))
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("orgA: want 1 source, got %d", len(rows))
	}
	if store.GoUUID(rows[0].ID) != store.GoUUID(srcA.ID) {
		t.Fatalf("orgA returned wrong source id: %v want %v",
			store.GoUUID(rows[0].ID), store.GoUUID(srcA.ID))
	}
	if store.GoUUID(rows[0].OrgID) != orgA.OrgID {
		t.Fatalf("orgA row has wrong org_id: %v want %v",
			store.GoUUID(rows[0].OrgID), orgA.OrgID)
	}
}

func TestIsolation_GetSourceForOrg_WrongOrg_NoRows(t *testing.T) {
	pool := isolationPool(t)
	q := store.New(pool)
	orgA := seedIsolationOrg(t, q, "iso-getA")
	orgB := seedIsolationOrg(t, q, "iso-getB")

	ctx := context.Background()
	srcA, err := q.CreateSource(ctx, store.CreateSourceParams{
		OrgID:         store.UUID(orgA.OrgID),
		Name:          "s",
		Type:          "generic",
		IngestToken:   mustToken(),
		SigningConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create src: %v", err)
	}

	// Sanity: orgA can load its own source.
	if _, err := q.GetSourceForOrg(ctx, store.GetSourceForOrgParams{
		ID:    srcA.ID,
		OrgID: store.UUID(orgA.OrgID),
	}); err != nil {
		t.Fatalf("orgA loading own source: %v", err)
	}

	// Cross-tenant attempt → ErrNoRows.
	_, err = q.GetSourceForOrg(ctx, store.GetSourceForOrgParams{
		ID:    srcA.ID,
		OrgID: store.UUID(orgB.OrgID),
	})
	if err == nil {
		t.Fatal("expected ErrNoRows loading orgA's source as orgB; got nil")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected pgx.ErrNoRows, got %v", err)
	}
}

// --- destinations ---

func TestIsolation_ListDestinationsByOrg_DoesNotLeak(t *testing.T) {
	pool := isolationPool(t)
	q := store.New(pool)
	orgA := seedIsolationOrg(t, q, "iso-dstA")
	orgB := seedIsolationOrg(t, q, "iso-dstB")

	ctx := context.Background()
	urlA := "https://a.example.test/hook"
	urlB := "https://b.example.test/hook"
	dstA, err := q.CreateDestination(ctx, store.CreateDestinationParams{
		OrgID:      store.UUID(orgA.OrgID),
		Name:       "primary",
		Type:       "http",
		Url:        &urlA,
		AuthConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create dst A: %v", err)
	}
	if _, err := q.CreateDestination(ctx, store.CreateDestinationParams{
		OrgID:      store.UUID(orgB.OrgID),
		Name:       "primary",
		Type:       "http",
		Url:        &urlB,
		AuthConfig: []byte("{}"),
	}); err != nil {
		t.Fatalf("create dst B: %v", err)
	}

	rows, err := q.ListDestinationsByOrg(ctx, store.UUID(orgA.OrgID))
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("orgA: want 1 destination, got %d", len(rows))
	}
	if store.GoUUID(rows[0].ID) != store.GoUUID(dstA.ID) {
		t.Fatalf("orgA returned wrong destination id: %v want %v",
			store.GoUUID(rows[0].ID), store.GoUUID(dstA.ID))
	}
}

func TestIsolation_GetDestinationForOrg_WrongOrg_NoRows(t *testing.T) {
	pool := isolationPool(t)
	q := store.New(pool)
	orgA := seedIsolationOrg(t, q, "iso-dgetA")
	orgB := seedIsolationOrg(t, q, "iso-dgetB")

	ctx := context.Background()
	urlA := "https://a.example.test/hook"
	dstA, err := q.CreateDestination(ctx, store.CreateDestinationParams{
		OrgID:      store.UUID(orgA.OrgID),
		Name:       "d",
		Type:       "http",
		Url:        &urlA,
		AuthConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create dst: %v", err)
	}

	if _, err := q.GetDestinationForOrg(ctx, store.GetDestinationForOrgParams{
		ID:    dstA.ID,
		OrgID: store.UUID(orgA.OrgID),
	}); err != nil {
		t.Fatalf("orgA loading own destination: %v", err)
	}

	_, err = q.GetDestinationForOrg(ctx, store.GetDestinationForOrgParams{
		ID:    dstA.ID,
		OrgID: store.UUID(orgB.OrgID),
	})
	if err == nil {
		t.Fatal("expected ErrNoRows loading orgA's destination as orgB; got nil")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected pgx.ErrNoRows, got %v", err)
	}
}

// --- api keys ---

func TestIsolation_ListAPIKeysByOrg_DoesNotLeak(t *testing.T) {
	pool := isolationPool(t)
	q := store.New(pool)
	orgA := seedIsolationOrg(t, q, "iso-keyA")
	orgB := seedIsolationOrg(t, q, "iso-keyB")

	ctx := context.Background()
	keyA, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		OrgID:   store.UUID(orgA.OrgID),
		Name:    "kA",
		Prefix:  "dsk_" + uuid.NewString()[:8],
		KeyHash: []byte("hashA"),
	})
	if err != nil {
		t.Fatalf("create key A: %v", err)
	}
	if _, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		OrgID:   store.UUID(orgB.OrgID),
		Name:    "kB",
		Prefix:  "dsk_" + uuid.NewString()[:8],
		KeyHash: []byte("hashB"),
	}); err != nil {
		t.Fatalf("create key B: %v", err)
	}

	rows, err := q.ListAPIKeysByOrg(ctx, store.UUID(orgA.OrgID))
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("orgA: want 1 api key, got %d", len(rows))
	}
	if store.GoUUID(rows[0].ID) != store.GoUUID(keyA.ID) {
		t.Fatalf("orgA returned wrong key id: %v want %v",
			store.GoUUID(rows[0].ID), store.GoUUID(keyA.ID))
	}
}

// --- events (joins through source.org_id) ---

func TestIsolation_ListEventsByOrg_DoesNotLeak(t *testing.T) {
	pool := isolationPool(t)
	q := store.New(pool)
	orgA := seedIsolationOrg(t, q, "iso-evA")
	orgB := seedIsolationOrg(t, q, "iso-evB")

	ctx := context.Background()

	// Helper to seed source+destination+connection+request+event for an org.
	seedEvent := func(t *testing.T, org seededOrg, label string) uuid.UUID {
		t.Helper()
		src, err := q.CreateSource(ctx, store.CreateSourceParams{
			OrgID:         store.UUID(org.OrgID),
			Name:          label + "-src",
			Type:          "generic",
			IngestToken:   mustToken(),
			SigningConfig: []byte("{}"),
		})
		if err != nil {
			t.Fatalf("%s create source: %v", label, err)
		}
		dstURL := "https://" + label + ".example.test/hook"
		dst, err := q.CreateDestination(ctx, store.CreateDestinationParams{
			OrgID:      store.UUID(org.OrgID),
			Name:       label + "-dst",
			Type:       "http",
			Url:        &dstURL,
			AuthConfig: []byte("{}"),
		})
		if err != nil {
			t.Fatalf("%s create destination: %v", label, err)
		}
		conn, err := q.CreateConnection(ctx, store.CreateConnectionParams{
			SourceID:      src.ID,
			DestinationID: dst.ID,
			Enabled:       true,
		})
		if err != nil {
			t.Fatalf("%s create connection: %v", label, err)
		}
		ip := netip.MustParseAddr("127.0.0.1")
		ct := "application/json"
		reqID := store.UUID(uuid.New())
		req, err := q.CreateRequest(ctx, store.CreateRequestParams{
			ID:          reqID,
			SourceID:    src.ID,
			HTTPMethod:  "POST",
			HTTPPath:    "/" + label,
			Headers:     []byte("{}"),
			BodyHash:    "sha256:" + label,
			BodyRef:     "mem://" + label,
			BodySize:    0,
			ContentType: &ct,
			SigVerified: false,
			IngestIP:    &ip,
		})
		if err != nil {
			t.Fatalf("%s create request: %v", label, err)
		}
		ev, err := q.CreateEvent(ctx, store.CreateEventParams{
			RequestID:    req.ID,
			ConnectionID: conn.ID,
		})
		if err != nil {
			t.Fatalf("%s create event: %v", label, err)
		}
		return store.GoUUID(ev.ID)
	}

	evA := seedEvent(t, orgA, "isoevA")
	evB := seedEvent(t, orgB, "isoevB")

	rows, err := q.ListEventsByOrg(ctx, store.ListEventsByOrgParams{
		OrgID:  store.UUID(orgA.OrgID),
		Limit:  100,
		Offset: 0,
	})
	if err != nil {
		t.Fatalf("list events A: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("orgA: want 1 event, got %d", len(rows))
	}
	if got := store.GoUUID(rows[0].ID); got != evA {
		t.Fatalf("orgA returned wrong event: got %v want %v (orgB ev=%v)", got, evA, evB)
	}
}

// --- audit logs ---

func TestIsolation_ListAuditLogsByOrg_DoesNotLeak(t *testing.T) {
	pool := isolationPool(t)
	q := store.New(pool)
	orgA := seedIsolationOrg(t, q, "iso-audA")
	orgB := seedIsolationOrg(t, q, "iso-audB")

	ctx := context.Background()
	insert := func(t *testing.T, org seededOrg, action string) {
		t.Helper()
		if err := q.InsertAuditLog(ctx, store.InsertAuditLogParams{
			OrgID:       store.UUID(org.OrgID),
			ActorUserID: store.UUID(org.UserID),
			Action:      action,
			TargetType:  "source",
			Metadata:    []byte("{}"),
		}); err != nil {
			t.Fatalf("insert audit (%s): %v", action, err)
		}
	}
	insert(t, orgA, "source.create")
	insert(t, orgA, "source.delete")
	insert(t, orgB, "source.create")

	rows, err := q.ListAuditLogsByOrg(ctx, store.ListAuditLogsByOrgParams{
		OrgID: store.UUID(orgA.OrgID),
		Limit: 100,
	})
	if err != nil {
		t.Fatalf("list audit A: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("orgA: want 2 audit rows, got %d", len(rows))
	}
	for _, r := range rows {
		if store.GoUUID(r.OrgID) != orgA.OrgID {
			t.Fatalf("orgA audit row has wrong org_id: %v want %v",
				store.GoUUID(r.OrgID), orgA.OrgID)
		}
	}
}
