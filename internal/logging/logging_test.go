package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// New hardcodes os.Stdout, so we exercise the ctxHandler wrapper directly over a
// buffer — same construction New uses (JSON handler wrapped in ctxHandler).
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(ctxHandler{slog.NewJSONHandler(buf, nil)})
}

func TestCtxHandlerStampsTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:     trace.SpanID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	logger.InfoContext(ctx, "hi")

	out := buf.String()
	if !strings.Contains(out, "0102030405060708090a0b0c0d0e0f10") {
		t.Errorf("expected trace_id hex in output, got: %s", out)
	}
	if !strings.Contains(out, "0a0b0c0d0e0f1011") {
		t.Errorf("expected span_id hex in output, got: %s", out)
	}
}

func TestCtxHandlerNoSpanNoTraceID(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	logger.Info("hi") // no ctx span

	if out := buf.String(); strings.Contains(out, "trace_id") {
		t.Errorf("expected no trace_id key without a span, got: %s", out)
	}
}

// captureStdout runs fn with os.Stdout redirected to a temp file and returns
// what was written. New binds os.Stdout when called, so the swap must wrap New.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	orig := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = orig }()
	fn()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// New picks the level threshold and the output format from its two strings.
func TestNewLevelAndFormat(t *testing.T) {
	emitAll := func(l *slog.Logger) {
		l.Debug("m-debug")
		l.Info("m-info")
		l.Warn("m-warn")
		l.Error("m-error")
	}
	cases := []struct {
		level string
		want  []string // messages that must appear
		not   []string // messages that must not
	}{
		{"debug", []string{"m-debug", "m-info", "m-warn", "m-error"}, nil},
		{"DEBUG", []string{"m-debug", "m-info", "m-warn", "m-error"}, nil}, // case-insensitive
		{"info", []string{"m-info", "m-warn", "m-error"}, []string{"m-debug"}},
		{"", []string{"m-info", "m-warn", "m-error"}, []string{"m-debug"}},      // default is info
		{"bogus", []string{"m-info", "m-warn", "m-error"}, []string{"m-debug"}}, // unknown falls back to info
		{"warn", []string{"m-warn", "m-error"}, []string{"m-debug", "m-info"}},
		{"error", []string{"m-error"}, []string{"m-debug", "m-info", "m-warn"}},
	}
	for _, tc := range cases {
		t.Run("level="+tc.level, func(t *testing.T) {
			out := captureStdout(t, func() { emitAll(New(tc.level, "json")) })
			for _, m := range tc.want {
				if !strings.Contains(out, m) {
					t.Errorf("level %q: %s missing from output %q", tc.level, m, out)
				}
			}
			for _, m := range tc.not {
				if strings.Contains(out, m) {
					t.Errorf("level %q: %s must be filtered out, got %q", tc.level, m, out)
				}
			}
		})
	}

	t.Run("text format", func(t *testing.T) {
		out := captureStdout(t, func() { New("info", "TEXT").Info("hello", "k", "v") })
		if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "msg=hello") || !strings.Contains(out, "k=v") {
			t.Errorf("text output = %q, want key=value form", out)
		}
	})
	t.Run("json format is the default", func(t *testing.T) {
		for _, format := range []string{"json", "", "whatever"} {
			out := captureStdout(t, func() { New("info", format).Info("hello", "k", "v") })
			var rec map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rec); err != nil {
				t.Fatalf("format %q: output %q is not JSON: %v", format, out, err)
			}
			if rec["msg"] != "hello" || rec["k"] != "v" {
				t.Errorf("format %q: record = %v", format, rec)
			}
		}
	})
}

// The trace stamp survives With/WithGroup, which wrap the handler again.
func TestCtxHandlerKeepsStampThroughWithAndGroup(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf).With("svc", "x").WithGroup("g")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a},
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
	})
	logger.InfoContext(trace.ContextWithSpanContext(context.Background(), sc), "hi", "a", 1)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	if rec["svc"] != "x" {
		t.Errorf("With attr lost: %v", rec)
	}
	g, ok := rec["g"].(map[string]any)
	if !ok || g["a"] != float64(1) {
		t.Fatalf("group attr lost: %v", rec)
	}
	// Record attrs added by Handle land inside the open group, like any attr.
	if g["trace_id"] != "aabbccddeeff0102030405060708090a" || g["span_id"] != "0102030405060708" {
		t.Errorf("trace stamp missing or wrong inside the group: %v", g)
	}
}
