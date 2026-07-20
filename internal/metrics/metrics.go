// Package metrics defines the dstream Prometheus collectors and thin record
// helpers. Everything registers on a private registry (Reg) served at /metrics.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Reg is the process-wide registry for dstream metrics. Later wiring registers
// the scrape-time dbCollector here too.
var Reg = prometheus.NewRegistry()

var f = promauto.With(Reg)

// --- entity metrics (sources / connections / destinations) ---
var (
	ingestRequests = f.NewCounterVec(prometheus.CounterOpts{
		Name: "dstream_ingest_requests_total",
		Help: "Webhook ingest requests, by source and dedup outcome.",
	}, []string{"source_id", "deduped"})

	ingestDuration = f.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dstream_ingest_duration_seconds",
		Help:    "Ingest handler latency (receive→respond), by source.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1},
	}, []string{"source_id"})

	deliveries = f.NewCounterVec(prometheus.CounterOpts{
		Name: "dstream_deliveries_total",
		Help: "Delivery outcomes, by destination, connection, status (delivered|failed|error).",
	}, []string{"destination_id", "connection_id", "status"})

	deliveryDuration = f.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dstream_delivery_duration_seconds",
		Help:    "Outbound delivery HTTP latency, by destination.",
		Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"destination_id"})

	deliveryAttempts = f.NewCounterVec(prometheus.CounterOpts{
		Name: "dstream_delivery_attempts_total",
		Help: "Delivery attempts, by connection and outcome (success|retry|deadletter).",
	}, []string{"connection_id", "outcome"})

	rateLimited = f.NewCounterVec(prometheus.CounterOpts{
		Name: "dstream_rate_limited_total",
		Help: "Deliveries deferred by the destination rate-limit gate.",
	}, []string{"destination_id"})

	inflightDeferred = f.NewCounterVec(prometheus.CounterOpts{
		Name: "dstream_inflight_deferred_total",
		Help: "Deliveries deferred by an in-flight gate (scope=dest|org).",
	}, []string{"destination_id", "scope"})
)

// --- subsystem metrics (web / auth / CLI tunnel) ---
var (
	httpRequests = f.NewCounterVec(prometheus.CounterOpts{
		Name: "dstream_http_requests_total",
		Help: "HTTP requests, by chi route pattern, method, status.",
	}, []string{"route", "method", "status"})

	httpDuration = f.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dstream_http_request_duration_seconds",
		Help:    "HTTP request latency, by chi route pattern and method.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method"})

	magicLinks = f.NewCounterVec(prometheus.CounterOpts{
		Name: "dstream_magic_links_total",
		Help: "Magic-link auth events, by action (issued|issue_error|rate_limited|verified|verify_failed).",
	}, []string{"action"})

	cliSessionsActive = f.NewGauge(prometheus.GaugeOpts{
		Name: "dstream_cli_sessions_active",
		Help: "CLI tunnel sessions currently connected to this instance.",
	})

	cliConnects = f.NewCounter(prometheus.CounterOpts{
		Name: "dstream_cli_connects_total",
		Help: "CLI tunnel WebSocket connections accepted.",
	})

	cliDisconnects = f.NewCounterVec(prometheus.CounterOpts{
		Name: "dstream_cli_disconnects_total",
		Help: "CLI tunnel disconnects, by reason (register_failed|closed).",
	}, []string{"reason"})

	// scrapeErrors counts scrape-time collector query failures. Without it a DB
	// blip makes the dbCollector gauges silently vanish (the scrape still 200s),
	// which dashboards read as "0" rather than an outage — alert on this instead.
	scrapeErrors = f.NewCounterVec(prometheus.CounterOpts{
		Name: "dstream_metrics_scrape_errors_total",
		Help: "Scrape-time collector query failures, by query.",
	}, []string{"query"})
)

// --- record helpers (called from instrument points) ---

func IngestRequest(sourceID uuid.UUID, deduped bool) {
	ingestRequests.WithLabelValues(sourceID.String(), strconv.FormatBool(deduped)).Inc()
}

func IngestDuration(sourceID uuid.UUID, d time.Duration) {
	ingestDuration.WithLabelValues(sourceID.String()).Observe(d.Seconds())
}

func Delivery(destID, connID uuid.UUID, status string) {
	deliveries.WithLabelValues(destID.String(), connID.String(), status).Inc()
}

func DeliveryDuration(destID uuid.UUID, d time.Duration) {
	deliveryDuration.WithLabelValues(destID.String()).Observe(d.Seconds())
}

func Attempt(connID uuid.UUID, outcome string) {
	deliveryAttempts.WithLabelValues(connID.String(), outcome).Inc()
}

func RateLimited(destID uuid.UUID) {
	rateLimited.WithLabelValues(destID.String()).Inc()
}

func InflightDeferred(destID uuid.UUID, scope string) {
	inflightDeferred.WithLabelValues(destID.String(), scope).Inc()
}

func MagicLink(action string) { magicLinks.WithLabelValues(action).Inc() }

func CLIConnected() {
	cliConnects.Inc()
	cliSessionsActive.Inc()
}

func CLIDisconnected(reason string) {
	cliDisconnects.WithLabelValues(reason).Inc()
	cliSessionsActive.Dec()
}

// Handler serves the registry in Prometheus text exposition format.
func Handler() http.Handler {
	return promhttp.HandlerFor(Reg, promhttp.HandlerOpts{})
}
