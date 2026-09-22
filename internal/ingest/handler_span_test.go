package ingest

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

// TestHandleIngestChildSpans drives one happy-path ingest (seed source +
// enabled connection, POST /e/{token}) with an SDK tracer provider recording
// spans, and asserts the handler emitted its sub-operation child spans —
// notably ingest.fanout, whose ctx must be current when events are enqueued so
// the consumer's deliver span links back to it.
func TestHandleIngestChildSpans(t *testing.T) {
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	ctx := context.Background()

	pool, err := store.NewPool(ctx, dsn, 2)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)

	addr := os.Getenv("DSTREAM_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skip("no redis at " + addr)
	}
	prefix := "ingesttest-" + uuid.NewString()[:8]
	dq := dqueue.NewClient(rdb).WithPrefix(prefix)
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+":*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
		_ = rdb.Close()
	})

	// Seed org → source(ingest_token) → destination → enabled connection so
	// the fan-out has one event to create + enqueue.
	o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{Name: "T", Slug: "t-" + uuid.NewString()})
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	token := "tok-" + uuid.NewString()
	src, err := q.CreateSource(ctx, store.CreateSourceParams{
		OrgID: o.ID, Name: "s-" + uuid.NewString(), Type: "generic", Description: "", IngestToken: token, SigningConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	url := "https://example.test/hook"
	dest, err := q.CreateDestination(ctx, store.CreateDestinationParams{
		OrgID: o.ID, Name: "d-" + uuid.NewString(), Type: "http", Url: &url, AuthConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("destination: %v", err)
	}
	if _, err := q.CreateConnection(ctx, store.CreateConnectionParams{
		SourceID: src.ID, DestinationID: dest.ID, Enabled: true,
	}); err != nil {
		t.Fatalf("connection: %v", err)
	}

	// Record spans. ingestTracer is a package var bound to the global provider's
	// delegate, so setting the provider here routes its spans to the recorder.
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	h := &Handler{
		Log:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Queries:   q,
		Redis:     rdb,
		Queue:     dq,
		BodyStore: NewPostgresBodyStore(q),
	}
	r := chi.NewRouter()
	h.Mount(r)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/e/"+token, strings.NewReader(`{"hello":"world"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("ingest status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}

	got := map[string]bool{}
	for _, s := range sr.Ended() {
		got[s.Name()] = true
	}
	for _, want := range []string{"ingest.resolve_source", "ingest.read_body", "ingest.dedup", "ingest.persist", "ingest.fanout"} {
		if !got[want] {
			t.Errorf("missing span %q; recorded=%v", want, spanNames(sr))
		}
	}
}

func spanNames(sr *tracetest.SpanRecorder) []string {
	var out []string
	for _, s := range sr.Ended() {
		out = append(out, s.Name())
	}
	return out
}
