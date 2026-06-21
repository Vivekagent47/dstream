package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/spf13/cobra"

	"github.com/Vivekagent47/dstream/db"
	"github.com/Vivekagent47/dstream/internal/config"
	"github.com/Vivekagent47/dstream/internal/logging"
)

// revisionsTable is the table Atlas uses to track which migration files have
// been applied. The schema mirrors Atlas' canonical `atlas_schema_revisions`
// table so the directory remains compatible with the `atlas` CLI for future
// inspection (`atlas migrate status`, etc.) even though we don't shell out
// to it.
const revisionsTable = "atlas_schema_revisions"

func migrateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending database migrations (Atlas)",
		Long:  "Applies all pending Atlas migrations embedded in the binary. Idempotent: re-running with nothing pending is a no-op.",
		RunE:  runMigrateUp,
	}
	c.AddCommand(
		&cobra.Command{Use: "up", Short: "Apply all pending migrations", RunE: runMigrateUp},
	)
	return c
}

func runMigrateUp(_ *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logging.New(cfg.LogLevel, cfg.LogFormat)
	ctx := context.Background()

	sqlDB, err := sql.Open("pgx", cfg.DB.URL)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer sqlDB.Close()

	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping db: %w", err)
	}

	drv, err := postgres.Open(sqlDB)
	if err != nil {
		return fmt.Errorf("open atlas postgres driver: %w", err)
	}

	dir, err := db.MigrationsDir()
	if err != nil {
		return fmt.Errorf("load embedded migrations: %w", err)
	}

	rrw, err := newPGRevisionReadWriter(ctx, sqlDB)
	if err != nil {
		return fmt.Errorf("init revisions table: %w", err)
	}

	ex, err := migrate.NewExecutor(drv, dir, rrw, migrate.WithLogger(&slogMigrateLogger{log: log}))
	if err != nil {
		return fmt.Errorf("new executor: %w", err)
	}

	switch err := ex.ExecuteN(ctx, 0); {
	case err == nil:
		log.Info("migrations applied")
		return nil
	case errors.Is(err, migrate.ErrNoPendingFiles):
		log.Info("no pending migrations")
		return nil
	default:
		return fmt.Errorf("apply migrations: %w", err)
	}
}

// pgRevisionReadWriter implements migrate.RevisionReadWriter against a
// Postgres `atlas_schema_revisions` table. The shape mirrors Atlas' own
// canonical revisions table so the directory stays interoperable with the
// `atlas` CLI for ad-hoc inspection.
type pgRevisionReadWriter struct {
	db     *sql.DB
	schema string
	table  string
}

func newPGRevisionReadWriter(ctx context.Context, db *sql.DB) (*pgRevisionReadWriter, error) {
	rrw := &pgRevisionReadWriter{db: db, schema: "public", table: revisionsTable}
	if err := rrw.ensureTable(ctx); err != nil {
		return nil, err
	}
	return rrw, nil
}

func (r *pgRevisionReadWriter) ensureTable(ctx context.Context) error {
	const stmt = `CREATE TABLE IF NOT EXISTS "public"."` + revisionsTable + `" (
  "version"          varchar       NOT NULL PRIMARY KEY,
  "description"      varchar       NOT NULL,
  "type"             bigint        NOT NULL DEFAULT 2,
  "applied"          bigint        NOT NULL DEFAULT 0,
  "total"            bigint        NOT NULL DEFAULT 0,
  "executed_at"      timestamptz   NOT NULL,
  "execution_time"   bigint        NOT NULL,
  "error"            text          NULL,
  "error_stmt"       text          NULL,
  "hash"             varchar       NOT NULL,
  "partial_hashes"   jsonb         NULL,
  "operator_version" varchar       NOT NULL
);`
	_, err := r.db.ExecContext(ctx, stmt)
	return err
}

// Ident implements migrate.RevisionReadWriter; Atlas uses it to skip the
// revisions table when running CheckClean against a target database.
func (r *pgRevisionReadWriter) Ident() *migrate.TableIdent {
	return &migrate.TableIdent{Name: r.table, Schema: r.schema}
}

func (r *pgRevisionReadWriter) ReadRevisions(ctx context.Context) ([]*migrate.Revision, error) {
	const q = `SELECT version, description, type, applied, total, executed_at, execution_time,
		COALESCE(error, ''), COALESCE(error_stmt, ''), hash, partial_hashes, operator_version
		FROM "public"."` + revisionsTable + `" ORDER BY version`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*migrate.Revision
	for rows.Next() {
		rev, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rev)
	}
	return out, rows.Err()
}

func (r *pgRevisionReadWriter) ReadRevision(ctx context.Context, version string) (*migrate.Revision, error) {
	const q = `SELECT version, description, type, applied, total, executed_at, execution_time,
		COALESCE(error, ''), COALESCE(error_stmt, ''), hash, partial_hashes, operator_version
		FROM "public"."` + revisionsTable + `" WHERE version = $1`
	row := r.db.QueryRowContext(ctx, q, version)
	rev, err := scanRevision(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, migrate.ErrRevisionNotExist
	}
	if err != nil {
		return nil, err
	}
	return rev, nil
}

func (r *pgRevisionReadWriter) WriteRevision(ctx context.Context, rev *migrate.Revision) error {
	partialHashes, err := encodePartialHashes(rev.PartialHashes)
	if err != nil {
		return fmt.Errorf("encode partial_hashes: %w", err)
	}
	const stmt = `INSERT INTO "public"."` + revisionsTable + `"
		(version, description, type, applied, total, executed_at, execution_time,
		 error, error_stmt, hash, partial_hashes, operator_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (version) DO UPDATE SET
			description=EXCLUDED.description,
			type=EXCLUDED.type,
			applied=EXCLUDED.applied,
			total=EXCLUDED.total,
			executed_at=EXCLUDED.executed_at,
			execution_time=EXCLUDED.execution_time,
			error=EXCLUDED.error,
			error_stmt=EXCLUDED.error_stmt,
			hash=EXCLUDED.hash,
			partial_hashes=EXCLUDED.partial_hashes,
			operator_version=EXCLUDED.operator_version`
	_, err = r.db.ExecContext(ctx, stmt,
		rev.Version,
		rev.Description,
		int64(rev.Type),
		int64(rev.Applied),
		int64(rev.Total),
		rev.ExecutedAt,
		int64(rev.ExecutionTime),
		nullString(rev.Error),
		nullString(rev.ErrorStmt),
		rev.Hash,
		partialHashes,
		rev.OperatorVersion,
	)
	return err
}

func (r *pgRevisionReadWriter) DeleteRevision(ctx context.Context, version string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM "public"."`+revisionsTable+`" WHERE version = $1`, version)
	return err
}

// rowScanner unifies *sql.Row and *sql.Rows for scanRevision.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRevision(s rowScanner) (*migrate.Revision, error) {
	var (
		rev           migrate.Revision
		typeVal       int64
		applied       int64
		total         int64
		executionTime int64
		partialHashes []byte
	)
	if err := s.Scan(
		&rev.Version,
		&rev.Description,
		&typeVal,
		&applied,
		&total,
		&rev.ExecutedAt,
		&executionTime,
		&rev.Error,
		&rev.ErrorStmt,
		&rev.Hash,
		&partialHashes,
		&rev.OperatorVersion,
	); err != nil {
		return nil, err
	}
	rev.Type = migrate.RevisionType(typeVal)
	rev.Applied = int(applied)
	rev.Total = int(total)
	rev.ExecutionTime = time.Duration(executionTime)
	if len(partialHashes) > 0 {
		if err := json.Unmarshal(partialHashes, &rev.PartialHashes); err != nil {
			return nil, fmt.Errorf("decode partial_hashes: %w", err)
		}
	}
	return &rev, nil
}

func encodePartialHashes(hashes []string) (any, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(hashes)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// slogMigrateLogger adapts slog.Logger to Atlas' migrate.Logger interface.
type slogMigrateLogger struct {
	log *slog.Logger
}

func (l *slogMigrateLogger) Log(e migrate.LogEntry) {
	switch t := e.(type) {
	case migrate.LogExecution:
		l.log.Info("migrate: starting execution", "from", t.From, "to", t.To, "files", len(t.Files))
	case migrate.LogFile:
		l.log.Info("migrate: applying file", "version", t.Version, "description", t.Desc)
	case migrate.LogStmt:
		l.log.Debug("migrate: stmt", "sql", t.SQL)
	case migrate.LogDone:
		l.log.Info("migrate: done")
	case migrate.LogError:
		l.log.Error("migrate: error", "err", t.Error.Error())
	default:
		l.log.Debug("migrate: log", "entry", fmt.Sprintf("%T", e))
	}
}
