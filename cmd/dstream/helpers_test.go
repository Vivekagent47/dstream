package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
)

// failTracer makes every statement whose SQL contains marker fail with a real
// driver error (its context is cancelled before it is sent), so one query can
// be failed without touching the others. Use an sqlc marker with a trailing
// space, e.g. "-- name: PromoteUserToSuperAdmin ", to match exactly one query.
type failTracer struct{ marker string }

func (f failTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, f.marker) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		return c
	}
	return ctx
}
func (failTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// failingPool is a pool on the test database whose statements matching marker
// fail. It is separate from testPool, so assertions can use a healthy pool.
func failingPool(t *testing.T, marker string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.Tracer = failTracer{marker: marker}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// captureStderr returns what fn wrote to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	func() {
		defer func() { os.Stderr = old; w.Close() }()
		fn()
	}()
	return <-done
}

// execCmd runs cmd through cobra's real Execute path with the given args and
// returns what it wrote to stdout and stderr. The command's own error is
// returned, not asserted, so callers can check the specific message.
func execCmd(t *testing.T, cmd *cobra.Command, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

// useTestDB points config.Load() (and so the command's own pool) at the test
// database. Returns the DSN; skips when it is not configured, like testPool.
func useTestDB(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	t.Setenv("DSTREAM_DB_URL", dsn)
	return dsn
}
