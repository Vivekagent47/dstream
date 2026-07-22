package metrics

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Vivekagent47/dstream/internal/store"
)

// scrapeCacheTTL decouples DB load from scrape frequency: the four collector
// queries run at most once per TTL regardless of how many Prometheus replicas
// scrape /metrics.
const scrapeCacheTTL = 15 * time.Second

// CollectorQueries is the read surface the scrape-time collector needs.
// *store.Queries satisfies it.
type CollectorQueries interface {
	CountEventsByStatePerConnection(ctx context.Context) ([]store.CountEventsByStatePerConnectionRow, error)
	ListSourceInfo(ctx context.Context) ([]store.ListSourceInfoRow, error)
	ListDestinationInfo(ctx context.Context) ([]store.ListDestinationInfoRow, error)
	ListConnectionInfo(ctx context.Context) ([]store.ListConnectionInfoRow, error)
}

var (
	eventsInStateDesc = prometheus.NewDesc(
		"dstream_events_in_state",
		"Events in a non-terminal state, by connection and status.",
		[]string{"connection_id", "status"}, nil)
	sourceInfoDesc = prometheus.NewDesc(
		"dstream_source_info", "Source id→name mapping (value always 1).",
		[]string{"source_id", "source_name"}, nil)
	destinationInfoDesc = prometheus.NewDesc(
		"dstream_destination_info", "Destination id→name mapping (value always 1).",
		[]string{"destination_id", "destination_name"}, nil)
	connectionInfoDesc = prometheus.NewDesc(
		"dstream_connection_info", "Connection id→name mapping (value always 1).",
		[]string{"connection_id", "connection_name"}, nil)
)

type dbCollector struct {
	q   CollectorQueries
	log *slog.Logger

	mu     sync.Mutex
	cached []prometheus.Metric
	expiry time.Time
}

// NewCollector returns a scrape-time collector. Register it on metrics.Reg.
// The DB reads (one GROUP BY + three small SELECTs) are cached for scrapeCacheTTL
// so scrape frequency doesn't drive DB load, and the last good snapshot is served
// through a transient DB error rather than dropping the series to zero.
func NewCollector(q CollectorQueries, log *slog.Logger) prometheus.Collector {
	return &dbCollector{q: q, log: log}
}

func (c *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- eventsInStateDesc
	ch <- sourceInfoDesc
	ch <- destinationInfoDesc
	ch <- connectionInfoDesc
}

func (c *dbCollector) Collect(ch chan<- prometheus.Metric) {
	for _, m := range c.snapshot() {
		ch <- m
	}
}

// snapshot returns the cached metric set, refreshing it from the DB once the
// TTL lapses. On a refresh error it keeps serving the last good snapshot (and
// does not advance the expiry, so the next scrape retries); the failure is
// visible via dstream_metrics_scrape_errors_total.
func (c *dbCollector) snapshot() []prometheus.Metric {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cached != nil && time.Now().Before(c.expiry) {
		return c.cached
	}
	fresh, ok := c.gather()
	if ok {
		c.cached = fresh
		c.expiry = time.Now().Add(scrapeCacheTTL)
		return fresh
	}
	if c.cached != nil {
		return c.cached
	}
	return fresh
}

// gather runs the four collector queries and builds the metric set. ok is false
// if any query failed (scrapeErrors already incremented); the returned slice
// still holds whatever succeeded.
func (c *dbCollector) gather() ([]prometheus.Metric, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out []prometheus.Metric
	ok := true

	if rows, err := c.q.CountEventsByStatePerConnection(ctx); err != nil {
		c.log.Warn("metrics: events_in_state query failed", "err", err)
		scrapeErrors.WithLabelValues("events_in_state").Inc()
		ok = false
	} else {
		for _, r := range rows {
			out = append(out, prometheus.MustNewConstMetric(eventsInStateDesc, prometheus.GaugeValue,
				float64(r.N), store.GoUUID(r.ConnectionID).String(), r.Status))
		}
	}

	// Live entity id sets, collected here so pruneStaleSeries can drop counter
	// series for entities that no longer exist (audit #18). Per-dimension ok
	// flags gate pruning so a failed query doesn't wipe live counters.
	liveSources, liveDests, liveConns := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	var srcOK, destOK, connOK bool

	if rows, err := c.q.ListSourceInfo(ctx); err != nil {
		c.log.Warn("metrics: source_info query failed", "err", err)
		scrapeErrors.WithLabelValues("source_info").Inc()
		ok = false
	} else {
		srcOK = true
		for _, r := range rows {
			id := store.GoUUID(r.ID).String()
			liveSources[id] = struct{}{}
			out = append(out, prometheus.MustNewConstMetric(sourceInfoDesc, prometheus.GaugeValue, 1,
				id, r.Name))
		}
	}

	if rows, err := c.q.ListDestinationInfo(ctx); err != nil {
		c.log.Warn("metrics: destination_info query failed", "err", err)
		scrapeErrors.WithLabelValues("destination_info").Inc()
		ok = false
	} else {
		destOK = true
		for _, r := range rows {
			id := store.GoUUID(r.ID).String()
			liveDests[id] = struct{}{}
			out = append(out, prometheus.MustNewConstMetric(destinationInfoDesc, prometheus.GaugeValue, 1,
				id, r.Name))
		}
	}

	if rows, err := c.q.ListConnectionInfo(ctx); err != nil {
		c.log.Warn("metrics: connection_info query failed", "err", err)
		scrapeErrors.WithLabelValues("connection_info").Inc()
		ok = false
	} else {
		connOK = true
		for _, r := range rows {
			id := store.GoUUID(r.ID).String()
			liveConns[id] = struct{}{}
			name := ""
			if r.Name != nil {
				name = *r.Name
			}
			out = append(out, prometheus.MustNewConstMetric(connectionInfoDesc, prometheus.GaugeValue, 1,
				id, name))
		}
	}

	// Drop counter series for entities that have since been deleted, so their
	// label cardinality doesn't accumulate for the process lifetime (audit #18).
	pruneStaleSeries(srcOK, liveSources, destOK, liveDests, connOK, liveConns)

	return out, ok
}
