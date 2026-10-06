package metrics

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Vivekagent47/dstream/internal/store"
)

// These tests run the scrape-time collector over real Postgres rows. Failures
// are injected on a real pool (the failing statement's context is cancelled
// before it is sent), never by substituting the query surface.

// faultTracer observes every statement on a real pool: it counts those whose
// SQL contains marker and, while fail is set, cancels their context so the
// driver returns a real error.
type faultTracer struct {
	marker string
	fail   *atomic.Bool
	hits   *atomic.Int64
}

func (f faultTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if !strings.Contains(strings.ToLower(d.SQL), f.marker) {
		return ctx
	}
	f.hits.Add(1)
	if f.fail != nil && f.fail.Load() {
		c, cancel := context.WithCancel(ctx)
		cancel()
		return c
	}
	return ctx
}
func (faultTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func dbPool(t *testing.T, tr pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 2
	if tr != nil {
		cfg.ConnConfig.Tracer = tr
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type fixture struct {
	src, dstA, dstB, connA, connB uuid.UUID
	srcName, dstAName, connAName  string
}

// seedFixture creates one org with a source, two destinations and two
// connections (one named, one not) and removes the org on cleanup.
func seedFixture(t *testing.T) fixture {
	t.Helper()
	q := store.New(dbPool(t, nil))
	ctx := context.Background()
	o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: "metrics " + uuid.NewString(), Slug: "metrics-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	t.Cleanup(func() { _ = q.DeleteOrganization(context.Background(), o.ID) })
	f := fixture{srcName: "src-" + uuid.NewString(), dstAName: "dstA-" + uuid.NewString(), connAName: "connA-" + uuid.NewString()}
	s, err := q.CreateSource(ctx, store.CreateSourceParams{
		OrgID: o.ID, Name: f.srcName, Type: "generic", IngestToken: "tok-" + uuid.NewString(), SigningConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	url := "https://example.test/hook"
	da, err := q.CreateDestination(ctx, store.CreateDestinationParams{
		OrgID: o.ID, Name: f.dstAName, Type: "http", Url: &url, AuthConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("destination A: %v", err)
	}
	db, err := q.CreateDestination(ctx, store.CreateDestinationParams{
		OrgID: o.ID, Name: "dstB-" + uuid.NewString(), Type: "http", Url: &url, AuthConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("destination B: %v", err)
	}
	ca, err := q.CreateConnection(ctx, store.CreateConnectionParams{SourceID: s.ID, DestinationID: da.ID, Enabled: true, Name: &f.connAName})
	if err != nil {
		t.Fatalf("connection A: %v", err)
	}
	cb, err := q.CreateConnection(ctx, store.CreateConnectionParams{SourceID: s.ID, DestinationID: db.ID, Enabled: true})
	if err != nil {
		t.Fatalf("connection B: %v", err)
	}
	f.src, f.dstA, f.dstB = store.GoUUID(s.ID), store.GoUUID(da.ID), store.GoUUID(db.ID)
	f.connA, f.connB = store.GoUUID(ca.ID), store.GoUUID(cb.ID)
	return f
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// scrape gathers the collector through a throwaway registry.
func scrape(t *testing.T, c prometheus.Collector) map[string][]*dto.Metric {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string][]*dto.Metric{}
	for _, f := range fams {
		out[f.GetName()] = f.GetMetric()
	}
	return out
}

// label returns the value of the named label on m.
func label(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// nameFor returns the name label of the series whose idLabel is id, and whether
// any such series exists.
func nameFor(ms []*dto.Metric, idLabel, nameLabel string, id uuid.UUID) (string, bool) {
	for _, m := range ms {
		if label(m, idLabel) == id.String() {
			if m.GetGauge().GetValue() != 1 {
				return "", false
			}
			return label(m, nameLabel), true
		}
	}
	return "", false
}

func TestCollectorOverRealRows(t *testing.T) {
	f := seedFixture(t)
	got := scrape(t, NewCollector(store.New(dbPool(t, nil)), discard()))

	for _, w := range []struct {
		fam, idLabel, nameLabel string
		id                      uuid.UUID
		name                    string
	}{
		{"dstream_source_info", "source_id", "source_name", f.src, f.srcName},
		{"dstream_destination_info", "destination_id", "destination_name", f.dstA, f.dstAName},
		{"dstream_connection_info", "connection_id", "connection_name", f.connA, f.connAName},
		{"dstream_connection_info", "connection_id", "connection_name", f.connB, ""}, // unnamed connection: empty label, not a panic
	} {
		name, ok := nameFor(got[w.fam], w.idLabel, w.nameLabel, w.id)
		if !ok || name != w.name {
			t.Errorf("%s{%s=%s} = (%q, present=%v), want name %q", w.fam, w.idLabel, w.id, name, ok, w.name)
		}
	}
}

// One scrape costs the DB one read per query; scrapes inside the TTL cost none.
func TestCollectorCachesWithinTTL(t *testing.T) {
	seedFixture(t)
	var hits atomic.Int64
	c := NewCollector(store.New(dbPool(t, faultTracer{marker: "from sources", hits: &hits})), discard())
	scrape(t, c)
	scrape(t, c)
	scrape(t, c)
	if hits.Load() != 1 {
		t.Fatalf("source query ran %d times over 3 scrapes inside the TTL, want 1", hits.Load())
	}
}

// A failing query is counted per kind and drops only its own series; the
// others still scrape.
func TestCollectorQueryFailureIsCountedPerKind(t *testing.T) {
	f := seedFixture(t)
	cases := []struct {
		kind, marker, fam, idLabel, nameLabel string
		id                                    uuid.UUID
	}{
		{"source_info", "from sources", "dstream_source_info", "source_id", "source_name", f.src},
		{"destination_info", "from destinations", "dstream_destination_info", "destination_id", "destination_name", f.dstA},
		{"connection_info", "from connections", "dstream_connection_info", "connection_id", "connection_name", f.connA},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			var fail atomic.Bool
			fail.Store(true)
			var hits atomic.Int64
			before := counterValue(t, scrapeErrors.WithLabelValues(tc.kind))
			got := scrape(t, NewCollector(store.New(dbPool(t, faultTracer{marker: tc.marker, fail: &fail, hits: &hits})), discard()))
			if d := counterValue(t, scrapeErrors.WithLabelValues(tc.kind)) - before; d != 1 {
				t.Errorf("scrape_errors{%s} rose by %v, want 1", tc.kind, d)
			}
			if _, ok := nameFor(got[tc.fam], tc.idLabel, tc.nameLabel, tc.id); ok {
				t.Errorf("%s still has the series whose query failed", tc.fam)
			}
			for _, o := range cases {
				if o.kind == tc.kind {
					continue
				}
				if _, ok := nameFor(got[o.fam], o.idLabel, o.nameLabel, o.id); !ok {
					t.Errorf("%s lost its series because %s failed", o.fam, tc.kind)
				}
			}
		})
	}
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// When a refresh fails the last good snapshot is served instead of dropping the
// series, the expiry is NOT advanced (so the next scrape retries), and the
// scrape after recovery refreshes.
func TestCollectorServesLastGoodSnapshotThroughOutage(t *testing.T) {
	f := seedFixture(t)
	var fail atomic.Bool
	var hits atomic.Int64
	c := NewCollector(store.New(dbPool(t, faultTracer{marker: "from sources", fail: &fail, hits: &hits})), discard()).(*dbCollector)

	if _, ok := nameFor(scrape(t, c)["dstream_source_info"], "source_id", "source_name", f.src); !ok {
		t.Fatal("first scrape is missing the seeded source")
	}

	c.mu.Lock()
	c.expiry = time.Now().Add(-time.Second) // TTL lapsed
	c.mu.Unlock()
	fail.Store(true)
	before := counterValue(t, scrapeErrors.WithLabelValues("source_info"))

	if name, ok := nameFor(scrape(t, c)["dstream_source_info"], "source_id", "source_name", f.src); !ok || name != f.srcName {
		t.Fatalf("outage dropped the series: (%q, present=%v); want the last good snapshot", name, ok)
	}
	if d := counterValue(t, scrapeErrors.WithLabelValues("source_info")) - before; d != 1 {
		t.Errorf("scrape_errors rose by %v during the outage, want 1", d)
	}
	c.mu.Lock()
	stillExpired := !time.Now().Before(c.expiry)
	c.mu.Unlock()
	if !stillExpired {
		t.Error("a failed refresh advanced the expiry; the next scrape would not retry")
	}

	fail.Store(false)
	scrape(t, c)
	c.mu.Lock()
	fresh := time.Now().Before(c.expiry)
	c.mu.Unlock()
	if !fresh {
		t.Error("scrape after recovery did not refresh the snapshot")
	}
}
