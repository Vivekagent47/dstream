package tracing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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

// collector is a real HTTP server standing in for an OTLP/HTTP endpoint: it
// records the path and body of every export request.
type collector struct {
	mu   sync.Mutex
	reqs []export
}

type export struct {
	path string
	body []byte
}

func (c *collector) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.reqs = append(c.reqs, export{r.URL.Path, b})
	c.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (c *collector) snapshot() []export {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]export(nil), c.reqs...)
}

// installed runs Init against url and restores the process-global tracer
// provider and propagator afterwards.
func installed(t *testing.T, cfg Config) func(context.Context) error {
	t.Helper()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	shutdown, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return shutdown
}

func TestInitEnabledExportsSampledSpans(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()

	shutdown := installed(t, Config{Enabled: true, OTLPEndpoint: srv.URL, ServiceName: "dstream-tracing-test", SampleRatio: 1})
	_, span := otel.Tracer("t").Start(context.Background(), "span-under-test")
	span.End()
	if err := shutdown(context.Background()); err != nil { // flushes the batcher synchronously
		t.Fatalf("shutdown: %v", err)
	}

	got := c.snapshot()
	if len(got) != 1 {
		t.Fatalf("collector saw %d export requests, want 1", len(got))
	}
	if got[0].path != "/v1/traces" {
		t.Errorf("export path = %q, want /v1/traces", got[0].path)
	}
	for _, want := range []string{"dstream-tracing-test", "span-under-test"} {
		if !bytes.Contains(got[0].body, []byte(want)) {
			t.Errorf("export body is missing %q (resource service name / span name)", want)
		}
	}
}

// SampleRatio 0 samples nothing: the exporter is wired but never has a span to
// send, so the collector stays silent.
func TestInitSampleRatioZeroExportsNothing(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()

	shutdown := installed(t, Config{Enabled: true, OTLPEndpoint: srv.URL, ServiceName: "svc", SampleRatio: 0})
	_, span := otel.Tracer("t").Start(context.Background(), "unsampled")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := c.snapshot(); len(got) != 0 {
		t.Fatalf("collector saw %d requests, want 0 for SampleRatio 0", len(got))
	}
}

// Init reports an exporter that cannot start (here: the caller's context is
// already done) instead of installing a half-built provider.
func TestInitExporterStartFailure(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	shutdown, err := Init(ctx, Config{Enabled: true, OTLPEndpoint: "http://127.0.0.1:4318", ServiceName: "svc", SampleRatio: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Init err = %v, want context.Canceled", err)
	}
	if shutdown != nil {
		t.Error("Init returned a shutdown func alongside an error")
	}
	if otel.GetTracerProvider() != prevTP {
		t.Error("a failed Init replaced the global tracer provider")
	}
}
