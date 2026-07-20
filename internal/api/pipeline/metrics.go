package pipeline

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// parseMetricsWindow reads the `bucket` + `after` query params for the detail-
// page metric endpoints. bucket defaults to hour; after defaults to the last 7
// days. Both mirror the events histogram, so the graph and the summary numbers
// cover the same window the UI's range picker selected.
func parseMetricsWindow(r *http.Request) (bucket string, after pgtype.Timestamptz) {
	bucket = r.URL.Query().Get("bucket")
	switch bucket {
	case "minute", "hour", "day", "week":
	default:
		bucket = "hour"
	}
	if v := r.URL.Query().Get("after"); v != "" {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			return bucket, pgtype.Timestamptz{Time: ts, Valid: true}
		}
	}
	return bucket, pgtype.Timestamptz{Time: time.Now().Add(-7 * 24 * time.Hour), Valid: true}
}

// histBucket is one gap-filled time bucket with per-status counts, matching the
// events histogram shape so the frontend chart component is reused as-is.
type histBucket struct {
	Ts     string           `json:"ts"`
	Counts map[string]int64 `json:"counts"`
	Total  int64            `json:"total"`
}

// DestinationMetrics returns the delivery timeline + delivery-rate + avg-latency
// for one destination over the selected window. GET /api/destinations/{id}/metrics
func (d Handlers) DestinationMetrics(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid id")
		return
	}
	// Ownership check scopes the metrics to the caller's org (the queries also
	// filter by org_id as defense-in-depth).
	if _, err := d.Queries.GetDestinationForOrg(r.Context(), store.GetDestinationForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	}); err != nil {
		httpx.Err(w, http.StatusNotFound, "not found")
		return
	}

	bucket, after := parseMetricsWindow(r)
	rows, err := d.Queries.DestinationDeliveryHistogram(r.Context(), store.DestinationDeliveryHistogramParams{
		Bucket:        bucket,
		After:         after,
		DestinationID: store.UUID(id),
		OrgID:         store.UUID(p.OrgID),
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "metrics")
		return
	}
	stats, err := d.Queries.DestinationDeliveryStats(r.Context(), store.DestinationDeliveryStatsParams{
		DestinationID: store.UUID(id),
		OrgID:         store.UUID(p.OrgID),
		After:         after,
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "metrics")
		return
	}

	series := make([]*histBucket, 0, len(rows))
	idx := make(map[string]*histBucket)
	for _, row := range rows {
		key := row.Bucket.Time.UTC().Format(time.RFC3339)
		b := idx[key]
		if b == nil {
			b = &histBucket{Ts: key, Counts: map[string]int64{}}
			idx[key] = b
			series = append(series, b)
		}
		if row.Status != nil {
			b.Counts[*row.Status] += row.Count
			b.Total += row.Count
		}
	}

	// delivery_rate/avg_latency are null (not 0) when there's nothing to average,
	// so the UI can show "—" rather than a misleading 0%.
	var deliveryRate, avgLatency *float64
	if stats.Total > 0 {
		rate := float64(stats.Delivered) / float64(stats.Total)
		deliveryRate = &rate
		lat := stats.AvgLatencyMs
		avgLatency = &lat
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"bucket":         bucket,
		"series":         series,
		"total":          stats.Total,
		"delivered":      stats.Delivered,
		"delivery_rate":  deliveryRate,
		"avg_latency_ms": avgLatency,
	})
}

// srcBucket is one gap-filled request-volume bucket (single series, no status).
type srcBucket struct {
	Ts    string `json:"ts"`
	Count int64  `json:"count"`
}

// SourceMetrics returns the ingest-request timeline + request rate + fan-out for
// one source over the selected window. GET /api/sources/{id}/metrics
func (d Handlers) SourceMetrics(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := d.Queries.GetSourceForOrg(r.Context(), store.GetSourceForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	}); err != nil {
		httpx.Err(w, http.StatusNotFound, "not found")
		return
	}

	bucket, after := parseMetricsWindow(r)
	rows, err := d.Queries.SourceRequestHistogram(r.Context(), store.SourceRequestHistogramParams{
		Bucket:   bucket,
		After:    after,
		SourceID: store.UUID(id),
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "metrics")
		return
	}
	stats, err := d.Queries.SourceRequestStats(r.Context(), store.SourceRequestStatsParams{
		SourceID: store.UUID(id),
		After:    after,
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "metrics")
		return
	}

	series := make([]srcBucket, 0, len(rows))
	for _, row := range rows {
		series = append(series, srcBucket{Ts: row.Bucket.Time.UTC().Format(time.RFC3339), Count: row.Count})
	}

	// req/day over the window; avg events per request is null when no requests.
	windowDays := time.Since(after.Time).Hours() / 24
	if windowDays < 1 {
		windowDays = 1
	}
	requestsRate := float64(stats.Requests) / windowDays
	var avgEventsPerReq *float64
	if stats.Requests > 0 {
		v := float64(stats.Events) / float64(stats.Requests)
		avgEventsPerReq = &v
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"bucket":                 bucket,
		"series":                 series,
		"requests":               stats.Requests,
		"events":                 stats.Events,
		"requests_rate":          requestsRate,
		"avg_events_per_request": avgEventsPerReq,
	})
}
