package logging

import (
	"bytes"
	"context"
	"log/slog"
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
