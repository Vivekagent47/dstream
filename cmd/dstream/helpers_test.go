package main

import (
	"bytes"
	"os"
	"testing"

	"github.com/spf13/cobra"
)

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
