package tracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
)

func TestInitDisabledIsNoop(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown must be non-nil even when disabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown: %v", err)
	}
	if otel.GetTextMapPropagator() == nil {
		t.Error("expected a global propagator")
	}
}
