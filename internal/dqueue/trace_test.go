package dqueue

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestTraceRoundTripThroughPayload(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "produce")
	defer span.End()
	wantTrace := span.SpanContext().TraceID().String()

	// producer side
	var p Payload
	injectTrace(ctx, &p)
	if p.Trace == nil {
		t.Fatal("expected trace carrier populated")
	}

	// consumer side: extract into a fresh ctx
	got := otel.GetTextMapPropagator().Extract(context.Background(), propagation.MapCarrier(p.Trace))
	gotTrace := trace.SpanContextFromContext(got).TraceID().String()
	if gotTrace != wantTrace {
		t.Errorf("trace id lost across payload: got %s want %s", gotTrace, wantTrace)
	}
}
