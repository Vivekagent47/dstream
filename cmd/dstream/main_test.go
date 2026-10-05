package main

import (
	"bytes"
	"strings"
	"testing"
)

func runArgs(args ...string) (out, errOut string, err error) {
	var o, e bytes.Buffer
	err = run(args, &o, &e)
	return o.String(), e.String(), err
}

func TestRun_Help(t *testing.T) {
	out, _, err := runArgs("--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"server", "worker", "cli", "migrate", "admin"} {
		if !strings.Contains(out, "\n  "+sub+" ") {
			t.Errorf("help does not list %q:\n%s", sub, out)
		}
	}
}

func TestRun_Version(t *testing.T) {
	out, _, err := runArgs("--version")
	if err != nil {
		t.Fatal(err)
	}
	if want := "dstream version " + version; !strings.Contains(out, want) {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestRun_UnknownSubcommand(t *testing.T) {
	_, _, err := runArgs("frobnicate")
	if err == nil || !strings.Contains(err.Error(), `unknown command "frobnicate"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestRun_SubcommandErrorIsReturned(t *testing.T) {
	t.Setenv("DSTREAM_SESSION_SECRET", "short")
	_, _, err := runArgs("server")
	if err == nil || !strings.Contains(err.Error(), "DSTREAM_SESSION_SECRET must be at least 32 bytes") {
		t.Fatalf("err = %v", err)
	}
}
