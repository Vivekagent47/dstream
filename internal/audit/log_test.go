package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// captureLogger returns a slog.Logger that writes JSON records into the
// returned bytes.Buffer so tests can assert on log output.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

// --- Pure unit tests: no DB required ---

func TestLog_NoPrincipal_NoOp(t *testing.T) {
	log, buf := captureLogger()
	// q is nil — must not be dereferenced because we should bail out before
	// any DB call.
	Log(context.Background(), nil, log, Entry{Action: "source.create", TargetType: "source"})
	if !strings.Contains(buf.String(), "no principal in ctx") {
		t.Fatalf("expected warning about missing principal; got: %s", buf.String())
	}
}

func TestLog_UnknownSource_NoOp(t *testing.T) {
	log, buf := captureLogger()
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Source: auth.Source("bogus"),
		OrgID:  uuid.New(),
	})
	// q is nil — we must short-circuit before any DB call.
	Log(ctx, nil, log, Entry{Action: "source.create", TargetType: "source"})
	if !strings.Contains(buf.String(), "unknown principal source") {
		t.Fatalf("expected warning about unknown source; got: %s", buf.String())
	}
}

// --- DB-gated integration tests ---

func testPool(t *testing.T) *pgxpool.Pool {
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

// seedUserAndOrg creates a fresh user + org + membership and returns ids.
func seedUserAndOrg(t *testing.T, q *store.Queries) (userID, orgID uuid.UUID, email string) {
	t.Helper()
	ctx := context.Background()
	email = "test+" + uuid.NewString() + "@example.test"
	u, err := q.CreateUser(ctx, store.CreateUserParams{Email: email})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: "Test " + email,
		Slug: "test-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
		OrgID:  o.ID,
		UserID: u.ID,
		Role:   "owner",
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return store.GoUUID(u.ID), store.GoUUID(o.ID), email
}

func TestLog_SessionPrincipal_WritesUserAndEmailSnapshot(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid, email := seedUserAndOrg(t, q)

	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Source: auth.SourceSession,
		UserID: uid,
		OrgID:  oid,
	})
	tid := uuid.New()
	Log(ctx, q, slog.Default(), Entry{
		Action:     "source.create",
		TargetType: "source",
		TargetID:   &tid,
		Metadata:   map[string]any{"name": "alpha"},
	})

	rows, err := q.ListAuditLogsByOrg(context.Background(), store.ListAuditLogsByOrgParams{
		OrgID: store.UUID(oid),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Action != "source.create" || r.TargetType != "source" {
		t.Errorf("action/target_type: %q %q", r.Action, r.TargetType)
	}
	if store.GoUUID(r.ActorUserID) != uid {
		t.Errorf("actor_user_id: got %s want %s", store.GoUUID(r.ActorUserID), uid)
	}
	if r.ActorApiKeyID.Valid {
		t.Errorf("actor_api_key_id should be NULL for session principal")
	}
	if r.ActorEmailSnapshot == nil || *r.ActorEmailSnapshot != email {
		t.Errorf("email snapshot: got %v want %s", r.ActorEmailSnapshot, email)
	}
	if store.GoUUID(r.TargetID) != tid {
		t.Errorf("target_id: got %s want %s", store.GoUUID(r.TargetID), tid)
	}
	var got map[string]any
	if err := json.Unmarshal(r.Metadata, &got); err != nil {
		t.Fatalf("metadata json: %v", err)
	}
	if got["name"] != "alpha" {
		t.Errorf("metadata.name: got %v want alpha", got["name"])
	}
}

func TestLog_APIKeyPrincipal_WritesAPIKeyID(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid, _ := seedUserAndOrg(t, q)

	_, prefix, hash, err := auth.NewAPIKey()
	if err != nil {
		t.Fatalf("new api key: %v", err)
	}
	row, err := q.CreateAPIKey(context.Background(), store.CreateAPIKeyParams{
		OrgID:   store.UUID(oid),
		Name:    "audit-test",
		Prefix:  prefix,
		KeyHash: hash,
	})
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	keyID := store.GoUUID(row.ID)

	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Source:   auth.SourceAPIKey,
		APIKeyID: keyID,
		OrgID:    oid,
	})
	Log(ctx, q, slog.Default(), Entry{
		Action:     "destination.delete",
		TargetType: "destination",
	})

	rows, err := q.ListAuditLogsByOrg(context.Background(), store.ListAuditLogsByOrgParams{
		OrgID: store.UUID(oid),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d, want 1", len(rows))
	}
	r := rows[0]
	if r.ActorUserID.Valid {
		t.Errorf("actor_user_id should be NULL for api_key principal")
	}
	if store.GoUUID(r.ActorApiKeyID) != keyID {
		t.Errorf("actor_api_key_id: got %s want %s", store.GoUUID(r.ActorApiKeyID), keyID)
	}
	if r.ActorEmailSnapshot != nil {
		t.Errorf("email snapshot should be nil for api_key; got %v", *r.ActorEmailSnapshot)
	}
}

func TestLog_MetadataMarshalFail_FallsBackToEmptyObject(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid, _ := seedUserAndOrg(t, q)

	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Source: auth.SourceSession,
		UserID: uid,
		OrgID:  oid,
	})
	log, buf := captureLogger()
	// json.Marshal refuses to encode function values — guarantees marshal fail.
	Log(ctx, q, log, Entry{
		Action:     "source.create",
		TargetType: "source",
		Metadata:   map[string]any{"bad": func() {}},
	})

	if !strings.Contains(buf.String(), "metadata marshal failed") {
		t.Fatalf("expected marshal-fail warning; got: %s", buf.String())
	}
	rows, err := q.ListAuditLogsByOrg(context.Background(), store.ListAuditLogsByOrgParams{
		OrgID: store.UUID(oid),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d, want 1", len(rows))
	}
	if string(rows[0].Metadata) != "{}" {
		t.Errorf("metadata fallback: got %q want {}", string(rows[0].Metadata))
	}
}
