package usage

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// planTestPool mirrors internal/api/audit_test.go's testPool: skip when no
// test database is configured, fail loudly when one is configured but
// unreachable (a stopped container is worth a red test, not a silent pass).
func planTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestFreePresetMatchesSchemaDefaults is the drift guard for the one value
// this design deliberately stores twice: the free tier's limits are both
// Presets["free"] and the organizations column DEFAULTs, because a new org
// must land on the free tier without any Go code running. A release that
// edits one and not the other fails here.
func TestFreePresetMatchesSchemaDefaults(t *testing.T) {
	pool := planTestPool(t)
	free := Presets["free"]

	want := map[string]int64{
		"quota_events_soft":   free.EventsSoft,
		"quota_events_hard":   free.EventsHard,
		"quota_messages_soft": free.MessagesSoft,
		"quota_messages_hard": free.MessagesHard,
	}
	for col, wantVal := range want {
		var def *string
		err := pool.QueryRow(context.Background(),
			`SELECT column_default FROM information_schema.columns
			  WHERE table_schema = 'public' AND table_name = 'organizations'
			    AND column_name = $1`, col).Scan(&def)
		if err != nil {
			t.Fatalf("%s: read default: %v", col, err)
		}
		if def == nil {
			t.Fatalf("%s: no column default; the free preset must be reachable without Go", col)
		}
		// Postgres reports a bigint default as "8000" or "'8000'::bigint"
		// depending on how it was written; compare the leading digits.
		got := strings.TrimSpace(strings.Split(strings.Trim(*def, "'"), "'")[0])
		if got != strconv.FormatInt(wantVal, 10) {
			t.Errorf("%s: schema default %s, Presets[\"free\"] %d — change both or neither", col, got, wantVal)
		}
	}
}

// TestFreeBackfillTouchesOnlyUnconfiguredOrgs is the safety argument for the
// data migration, executed rather than asserted on by eye. It reads the
// UPDATE statement out of the migration file itself — a copy of the predicate
// in this test would drift from the one that actually ships — and runs it
// against four deliberately-shaped orgs.
//
// Re-running it is also the idempotence check: the WHERE excludes rows it has
// already updated, so the second run must be a no-op.
func TestFreeBackfillTouchesOnlyUnconfiguredOrgs(t *testing.T) {
	pool := planTestPool(t)
	ctx := context.Background()
	free := Presets["free"]

	stmt := backfillStatement(t)

	type seed struct {
		name                             string
		plan                             string
		evSoft, evHard, msgSoft, msgHard int64
		wantChanged                      bool
	}
	seeds := []seed{
		{name: "never configured", plan: "free", wantChanged: true},
		{name: "pro tier", plan: "pro", evSoft: 1, evHard: 2, msgSoft: 3, msgHard: 4},
		{name: "free but partly configured", plan: "free", evHard: 500},
		{name: "custom with zeros", plan: "custom"},
	}

	ids := make([]uuid.UUID, len(seeds))
	for i, s := range seeds {
		slug := "backfill-" + uuid.NewString()
		var id uuid.UUID
		err := pool.QueryRow(ctx,
			`INSERT INTO organizations
			   (name, slug, plan, quota_events_soft, quota_events_hard,
			    quota_messages_soft, quota_messages_hard)
			 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			s.name, slug, s.plan, s.evSoft, s.evHard, s.msgSoft, s.msgHard).Scan(&id)
		if err != nil {
			t.Fatalf("seed %q: %v", s.name, err)
		}
		ids[i] = id
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, id)
		})
	}

	for range 2 { // twice: the second pass proves idempotence
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("run backfill: %v", err)
		}
	}

	for i, s := range seeds {
		var evSoft, evHard, msgSoft, msgHard int64
		if err := pool.QueryRow(ctx,
			`SELECT quota_events_soft, quota_events_hard,
			        quota_messages_soft, quota_messages_hard
			   FROM organizations WHERE id = $1`, ids[i]).
			Scan(&evSoft, &evHard, &msgSoft, &msgHard); err != nil {
			t.Fatalf("read back %q: %v", s.name, err)
		}
		if s.wantChanged {
			if evSoft != free.EventsSoft || evHard != free.EventsHard ||
				msgSoft != free.MessagesSoft || msgHard != free.MessagesHard {
				t.Errorf("%q: got %d/%d %d/%d, want the free preset", s.name,
					evSoft, evHard, msgSoft, msgHard)
			}
			continue
		}
		if evSoft != s.evSoft || evHard != s.evHard || msgSoft != s.msgSoft || msgHard != s.msgHard {
			t.Errorf("%q: backfill must not touch this row; got %d/%d %d/%d want %d/%d %d/%d",
				s.name, evSoft, evHard, msgSoft, msgHard, s.evSoft, s.evHard, s.msgSoft, s.msgHard)
		}
	}
}

// backfillStatement returns the single UPDATE from the admin_managed_quotas
// migration. Globbed by suffix because Atlas prefixes a timestamp.
func backfillStatement(t *testing.T) string {
	t.Helper()
	matches, err := filepath.Glob("../../db/migrations/*_admin_managed_quotas.sql")
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one *_admin_managed_quotas.sql migration, got %v (err %v)", matches, err)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	i := strings.Index(string(b), "UPDATE organizations")
	if i < 0 {
		t.Fatalf("migration %s has no UPDATE — the data backfill is missing", matches[0])
	}
	rest := string(b)[i:]
	j := strings.Index(rest, ";")
	if j < 0 {
		t.Fatalf("migration %s: unterminated UPDATE", matches[0])
	}
	return rest[:j+1]
}
