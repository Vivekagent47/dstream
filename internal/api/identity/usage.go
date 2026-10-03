package identity

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"
)

// usageMetrics is every metric usage_rollups tracks (design §3). GET
// /api/usage seeds all four at 0 so a metric with no rollup row yet (a new
// org, or a sweep that hasn't run) reads as zero instead of being absent.
var usageMetrics = [...]string{"requests", "events", "messages", "attempts"}

const (
	defaultHistoryPeriods = 12
	// maxHistoryPeriods bounds the `periods` query param so it can't turn
	// GetUsageHistory into an unbounded scan.
	maxHistoryPeriods = 100
)

func isUsageMetric(m string) bool {
	for _, x := range usageMetrics {
		if x == m {
			return true
		}
	}
	return false
}

// quotaView shapes an org's plan + limits for GET /api/usage. The super-admin
// write at PATCH /admin/orgs/{org_id}/plan returns the same shape, so the two
// surfaces agree.
func quotaView(plan, period string, eventsSoft, eventsHard, messagesSoft, messagesHard int64) map[string]any {
	return map[string]any{
		"plan":   plan,
		"period": period,
		"limits": map[string]any{
			"events_soft":   eventsSoft,
			"events_hard":   eventsHard,
			"messages_soft": messagesSoft,
			"messages_hard": messagesHard,
		},
	}
}

// GetUsage serves GET /api/usage — current-period totals for the active org
// plus its limits, so the dashboard renders progress without arithmetic. Any
// member can read (RequireOrg in router.go already resolved the caller's
// role); same auth shape as the rest of the traffic plane.
//
// "partial":true always, because this is by definition the still-accumulating
// period — design §8 requires the marker so a chart doesn't render a
// half-elapsed period as a dip.
func (d Handlers) GetUsage(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	q, err := d.Queries.GetOrgQuota(r.Context(), store.UUID(p.OrgID))
	if err != nil {
		d.Log.Error("get usage: org quota", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "get usage")
		return
	}
	// Derive the period from the ORG's own quota_period, never a hardcoded
	// "month": the sweep buckets this org's rollup at
	// PeriodStart(now, org.quota_period), and GetUsageForPeriod is keyed by an
	// exact period_start. A 'day'-period org looked up at a hardcoded month
	// start would match no row and read back as zero despite active traffic.
	periodStart := usage.PeriodStart(time.Now(), q.QuotaPeriod)
	rows, err := d.Queries.GetUsageForPeriod(r.Context(), store.GetUsageForPeriodParams{
		OrgID:       store.UUID(p.OrgID),
		PeriodStart: pgtype.Timestamptz{Time: periodStart, Valid: true},
	})
	if err != nil {
		d.Log.Error("get usage: rollup lookup", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "get usage")
		return
	}
	counts := make(map[string]int64, len(usageMetrics))
	for _, m := range usageMetrics {
		counts[m] = 0
	}
	for _, row := range rows {
		counts[row.Metric] = row.Count
	}
	out := quotaView(q.Plan, q.QuotaPeriod, q.QuotaEventsSoft, q.QuotaEventsHard, q.QuotaMessagesSoft, q.QuotaMessagesHard)
	out["period_start"] = periodStart
	out["partial"] = true
	out["usage"] = counts
	httpx.WriteJSON(w, http.StatusOK, out)
}

// GetUsageHistory serves GET /api/usage/history?metric=&periods= — prior
// periods of one metric, for a chart. `periods` defaults to 12 and is capped
// at maxHistoryPeriods so the query string can't force an unbounded scan.
// The most recent entry (the current, still-open period) is marked partial
// for the same reason GetUsage always is.
func (d Handlers) GetUsageHistory(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	metric := r.URL.Query().Get("metric")
	if !isUsageMetric(metric) {
		httpx.Err(w, http.StatusBadRequest, "metric must be one of requests|events|messages|attempts")
		return
	}
	periods := defaultHistoryPeriods
	if v := r.URL.Query().Get("periods"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n <= 0 {
			httpx.Err(w, http.StatusBadRequest, "periods must be a positive integer")
			return
		}
		periods = n
	}
	if periods > maxHistoryPeriods {
		periods = maxHistoryPeriods
	}

	q, err := d.Queries.GetOrgQuota(r.Context(), store.UUID(p.OrgID))
	if err != nil {
		d.Log.Error("get usage history: org quota", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "get usage history")
		return
	}
	// Same org-period rule as GetUsage: never hardcode "month" here either.
	current := usage.PeriodStart(time.Now(), q.QuotaPeriod)
	cutoff := periodsAgo(current, q.QuotaPeriod, periods)

	rows, err := d.Queries.GetUsageHistory(r.Context(), store.GetUsageHistoryParams{
		OrgID:       store.UUID(p.OrgID),
		Metric:      metric,
		PeriodStart: pgtype.Timestamptz{Time: cutoff, Valid: true},
	})
	if err != nil {
		d.Log.Error("get usage history: query", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "get usage history")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"period_start": row.PeriodStart.Time,
			"count":        row.Count,
			"partial":      row.PeriodStart.Time.Equal(current),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"metric":  metric,
		"period":  q.QuotaPeriod,
		"periods": out,
	})
}

// periodsAgo returns the period_start N periods before and including
// current, which is already truncated by usage.PeriodStart. Calendar-correct
// for "month" (so e.g. 3 periods back from Oct 1 lands on Aug 1, not
// "Oct 1 - 90 days"), via time.Time.AddDate.
func periodsAgo(current time.Time, period string, n int) time.Time {
	if period == "day" {
		return current.AddDate(0, 0, -(n - 1))
	}
	return current.AddDate(0, -(n - 1), 0)
}

