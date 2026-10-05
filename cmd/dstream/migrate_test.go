package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"ariga.io/atlas/sql/migrate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Vivekagent47/dstream/db"
)

const scratchPrefix = "dstream_mig_"

// newScratchDB creates a throwaway database, drops it on cleanup, and returns
// its DSN. It only ever drops the one name it generated here, and refuses
// anything without the scratch prefix.
func newScratchDB(t *testing.T) string {
	t.Helper()
	testDSN := os.Getenv("DSTREAM_TEST_DB_URL")
	if testDSN == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	u, err := url.Parse(testDSN)
	if err != nil {
		t.Fatalf("parse DSTREAM_TEST_DB_URL: %v", err)
	}
	name := scratchPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin := *u
	admin.Path = "/postgres"

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		t.Fatalf("connect admin db: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, admin.String())
		if err != nil {
			t.Errorf("connect admin db for drop: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop scratch db: %v", err)
		}
	})
	scratch := *u
	scratch.Path = "/" + name
	return scratch.String()
}

func openSQL(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	d, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// captureStdout returns what fn wrote to os.Stdout; runMigrateUp's logger is
// bound to it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	func() {
		defer func() { os.Stdout = old; w.Close() }()
		fn()
	}()
	return <-done
}

func migrationVersions(t *testing.T) []string {
	t.Helper()
	dir, err := db.MigrationsDir()
	if err != nil {
		t.Fatal(err)
	}
	files, err := dir.Files()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		out = append(out, f.Version())
	}
	return out
}

func TestMigrateUp_AppliesThenIsIdempotent(t *testing.T) {
	dsn := newScratchDB(t)
	t.Setenv("DSTREAM_DB_URL", dsn)
	d := openSQL(t, dsn)
	want := migrationVersions(t)

	var err error
	out := captureStdout(t, func() { _, _, err = execCmd(t, migrateCmd(), "up") })
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !strings.Contains(out, "migrations applied") {
		t.Fatalf("first run log lacks 'migrations applied':\n%s", out)
	}

	revs := func() map[string]time.Time {
		rows, err := d.Query(`SELECT version, executed_at FROM atlas_schema_revisions ORDER BY version`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		m := map[string]time.Time{}
		for rows.Next() {
			var v string
			var at time.Time
			if err := rows.Scan(&v, &at); err != nil {
				t.Fatal(err)
			}
			m[v] = at
		}
		return m
	}
	first := revs()
	if len(first) != len(want) {
		t.Fatalf("revisions = %d rows, want %d (one per embedded migration)", len(first), len(want))
	}
	for _, v := range want {
		if _, ok := first[v]; !ok {
			t.Errorf("no revision row for embedded migration %s", v)
		}
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_name = 'organizations'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("organizations table present = %d (err %v): migrations did not run", n, err)
	}

	// Run again, through the bare `migrate` command: nothing pending.
	out = captureStdout(t, func() { _, _, err = execCmd(t, migrateCmd()) })
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !strings.Contains(out, "no pending migrations") || strings.Contains(out, "migrations applied") {
		t.Fatalf("second run should report no pending migrations:\n%s", out)
	}
	second := revs()
	for v, at := range first {
		if !second[v].Equal(at) {
			t.Errorf("revision %s rewritten on a no-op run", v)
		}
	}
}

func TestMigrateUp_UnreachableDatabase(t *testing.T) {
	t.Setenv("DSTREAM_DB_URL", "postgres://nobody:nopass@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	_, _, err := execCmd(t, migrateCmd(), "up")
	if err == nil || !strings.HasPrefix(err.Error(), "ping db:") {
		t.Fatalf("err = %v, want a 'ping db:' error", err)
	}
}

func TestMigrateUp_RefusesANonEmptyDatabaseWithNoRevisions(t *testing.T) {
	dsn := newScratchDB(t)
	t.Setenv("DSTREAM_DB_URL", dsn)
	d := openSQL(t, dsn)
	// Pre-existing schema with no revision history: Atlas cannot know what
	// state it is in and must refuse rather than layer migrations on top.
	if _, err := d.Exec(`CREATE TABLE stray (id int)`); err != nil {
		t.Fatal(err)
	}
	_, _, err := execCmd(t, migrateCmd(), "up")
	if err == nil || !strings.HasPrefix(err.Error(), "apply migrations:") || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("err = %v, want 'apply migrations: ... not clean'", err)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM atlas_schema_revisions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("revision rows = %d (err %v), want 0 after a refusal", n, err)
	}
}

func TestPGRevisionReadWriter_RoundTrip(t *testing.T) {
	ctx := context.Background()
	d := openSQL(t, newScratchDB(t))
	rrw, err := newPGRevisionReadWriter(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if id := rrw.Ident(); id.Name != revisionsTable || id.Schema != "public" {
		t.Fatalf("Ident = %+v", id)
	}
	// ensureTable is idempotent.
	if err := rrw.ensureTable(ctx); err != nil {
		t.Fatalf("second ensureTable: %v", err)
	}

	if _, err := rrw.ReadRevision(ctx, "1"); !errors.Is(err, migrate.ErrRevisionNotExist) {
		t.Fatalf("ReadRevision on empty table = %v, want ErrRevisionNotExist", err)
	}

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	full := &migrate.Revision{
		Version: "2", Description: "second", Type: migrate.RevisionTypeExecute,
		Applied: 1, Total: 3, ExecutedAt: at, ExecutionTime: 42 * time.Millisecond,
		Error: "boom", ErrorStmt: "SELECT 1", Hash: "h2",
		PartialHashes: []string{"p1", "p2"}, OperatorVersion: "op",
	}
	plain := &migrate.Revision{
		Version: "1", Description: "first", Type: migrate.RevisionTypeResolved,
		Applied: 2, Total: 2, ExecutedAt: at, Hash: "h1", OperatorVersion: "op",
	}
	for _, r := range []*migrate.Revision{full, plain} {
		if err := rrw.WriteRevision(ctx, r); err != nil {
			t.Fatalf("WriteRevision %s: %v", r.Version, err)
		}
	}

	got, err := rrw.ReadRevisions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Version != "1" || got[1].Version != "2" {
		t.Fatalf("ReadRevisions order/len wrong: %+v", got)
	}
	g := got[1]
	if g.Description != "second" || g.Type != migrate.RevisionTypeExecute || g.Applied != 1 || g.Total != 3 ||
		!g.ExecutedAt.Equal(at) || g.ExecutionTime != 42*time.Millisecond || g.Error != "boom" ||
		g.ErrorStmt != "SELECT 1" || g.Hash != "h2" || g.OperatorVersion != "op" ||
		strings.Join(g.PartialHashes, ",") != "p1,p2" {
		t.Fatalf("round trip lost data: %+v", g)
	}
	if got[0].Error != "" || got[0].PartialHashes != nil {
		t.Fatalf("empty error/partial hashes should read back empty: %+v", got[0])
	}

	// WriteRevision upserts.
	full.Applied = 3
	full.Error = ""
	if err := rrw.WriteRevision(ctx, full); err != nil {
		t.Fatal(err)
	}
	one, err := rrw.ReadRevision(ctx, "2")
	if err != nil || one.Applied != 3 || one.Error != "" {
		t.Fatalf("upsert not applied: %+v err %v", one, err)
	}

	if err := rrw.DeleteRevision(ctx, "2"); err != nil {
		t.Fatal(err)
	}
	if _, err := rrw.ReadRevision(ctx, "2"); !errors.Is(err, migrate.ErrRevisionNotExist) {
		t.Fatalf("after delete: %v, want ErrRevisionNotExist", err)
	}
}

func TestPGRevisionReadWriter_ErrorPaths(t *testing.T) {
	ctx := context.Background()
	d := openSQL(t, newScratchDB(t))
	rrw, err := newPGRevisionReadWriter(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO atlas_schema_revisions
		(version, description, executed_at, execution_time, hash, partial_hashes, operator_version)
		VALUES ('9','bad',now(),0,'h','"not-a-list"','op')`); err != nil {
		t.Fatal(err)
	}
	if _, err := rrw.ReadRevisions(ctx); err == nil || !strings.Contains(err.Error(), "decode partial_hashes") {
		t.Fatalf("ReadRevisions err = %v, want decode partial_hashes", err)
	}
	if _, err := rrw.ReadRevision(ctx, "9"); err == nil || !strings.Contains(err.Error(), "decode partial_hashes") {
		t.Fatalf("ReadRevision err = %v, want decode partial_hashes", err)
	}

	d.Close()
	if _, err := newPGRevisionReadWriter(ctx, d); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("ensureTable on closed db: %v", err)
	}
	if _, err := rrw.ReadRevisions(ctx); err == nil {
		t.Fatal("ReadRevisions on closed db should fail")
	}
	if _, err := rrw.ReadRevision(ctx, "1"); err == nil || errors.Is(err, migrate.ErrRevisionNotExist) {
		t.Fatalf("ReadRevision on closed db = %v, want a real error", err)
	}
	if err := rrw.WriteRevision(ctx, &migrate.Revision{Version: "1"}); err == nil {
		t.Fatal("WriteRevision on closed db should fail")
	}
	if err := rrw.DeleteRevision(ctx, "1"); err == nil {
		t.Fatal("DeleteRevision on closed db should fail")
	}
}

func TestSlogMigrateLogger(t *testing.T) {
	cases := []struct {
		name  string
		entry migrate.LogEntry
		want  string
	}{
		{"execution", migrate.LogExecution{From: "a", To: "b", Files: []migrate.File{}}, "migrate: starting execution"},
		{"file", migrate.LogFile{Version: "v1", Desc: "d1"}, "migrate: applying file"},
		{"stmt", migrate.LogStmt{SQL: "SELECT 42"}, "SELECT 42"},
		{"done", migrate.LogDone{}, "migrate: done"},
		{"error", migrate.LogError{Error: errors.New("kaput")}, "kaput"},
		{"other entries fall through", migrate.LogChecks{}, "migrate.LogChecks"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := &slogMigrateLogger{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
			l.Log(c.entry)
			if !strings.Contains(buf.String(), c.want) {
				t.Fatalf("log output %q lacks %q", buf.String(), c.want)
			}
		})
	}
}

func TestMigrateUp_ReportsBadConfig(t *testing.T) {
	t.Setenv("DSTREAM_LOG_LEVEL", "error")
	t.Setenv("DSTREAM_DB_MAX_CONNS", "not-a-number")
	_, _, err := execCmd(t, migrateCmd(), "up")
	if err == nil || !strings.Contains(err.Error(), "unmarshal config") {
		t.Fatalf("err = %v, want the config unmarshal failure", err)
	}
}

// A type already named like the revisions table makes CREATE TABLE fail even
// with IF NOT EXISTS (that only checks relations). The operator must see
// which step failed, and no migration may have run.
func TestMigrateUp_RevisionsTableCannotBeCreated(t *testing.T) {
	dsn := newScratchDB(t)
	t.Setenv("DSTREAM_DB_URL", dsn)
	t.Setenv("DSTREAM_LOG_LEVEL", "error")
	d := openSQL(t, dsn)
	if _, err := d.Exec(`CREATE TYPE public.` + revisionsTable + ` AS ENUM ('x')`); err != nil {
		t.Fatal(err)
	}

	_, _, err := execCmd(t, migrateCmd(), "up")

	if err == nil || !strings.HasPrefix(err.Error(), "init revisions table:") || !strings.Contains(err.Error(), `"`+revisionsTable+`" already exists`) {
		t.Fatalf("err = %v, want 'init revisions table: ... already exists'", err)
	}
	var applied *string
	if err := d.QueryRow(`SELECT to_regclass('public.organizations')::text`).Scan(&applied); err != nil || applied != nil {
		t.Fatalf("organizations = %v (err %v): a migration ran despite the failure", applied, err)
	}
}
