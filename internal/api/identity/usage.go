package identity

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"
)

// usageMetrics is every metric usage_rollups tracks (design §3). GET
// /api/usage seeds all four at 0 so a metric with no rollup row yet (a new
// org, or a sweep that hasn't run) reads as zero instead of being absent.
var usageMetrics = [...]string{"requests", "events", "messages", "attempts"}

// validPlans / validPeriods mirror the CHECK constraints added by
// db/migrations/20261002043243_usage_metering.sql, so PatchOrgPlan rejects a
// bad value with 400 instead of letting Postgres reject it with 500.
var validPlans = map[string]bool{"free": true, "pro": true, "enterprise": true, "custom": true}
var validPeriods = map[string]bool{"day": true, "month": true}

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

// quotaView shapes an org's plan + limits, shared by GET /api/usage and the
// PATCH /plan response so the two surfaces agree on shape.
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

// PatchOrgPlan serves PATCH /api/orgs/{org_id}/plan — sets the org's plan and
// quota limits. Partial update: an omitted field keeps its current value.
//
// Owner-only, deliberately stricter than this phase's default of
// admin-for-destructive (internal/auth/rbac.go): changing a quota is a spend
// decision, not a destructive op in the delete-a-resource sense, and an org
// admin is not the right authority to raise their own ceiling or grant
// themselves more capacity. A future reader may be tempted to "fix" this down
// to admin to match the rest of the identity group's write gating — don't;
// this is the one write in the group that is intentionally narrower.
func (d Handlers) PatchOrgPlan(w http.ResponseWriter, r *http.Request) {
	if err := auth.RequireSession(r.Context()); err != nil {
		httpx.Err(w, http.StatusForbidden, "session required")
		return
	}
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid org_id")
		return
	}
	p, _ := auth.FromContext(r.Context())
	caller, err := d.Queries.GetOrgMember(r.Context(), store.GetOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: store.UUID(p.UserID),
	})
	if err != nil {
		httpx.Err(w, http.StatusForbidden, "not a member")
		return
	}
	if auth.Role(caller.Role) != auth.RoleOwner {
		httpx.Err(w, http.StatusForbidden, "owner required")
		return
	}

	var body struct {
		Plan              *string `json:"plan,omitempty"`
		QuotaEventsSoft   *int64  `json:"quota_events_soft,omitempty"`
		QuotaEventsHard   *int64  `json:"quota_events_hard,omitempty"`
		QuotaMessagesSoft *int64  `json:"quota_messages_soft,omitempty"`
		QuotaMessagesHard *int64  `json:"quota_messages_hard,omitempty"`
		QuotaPeriod       *string `json:"quota_period,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}

	current, err := d.Queries.GetOrgQuota(r.Context(), store.UUID(orgID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "org not found")
			return
		}
		d.Log.Error("patch plan: get org quota", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "patch plan")
		return
	}

	next := store.UpdateOrgQuotaParams{
		ID:                store.UUID(orgID),
		Plan:              current.Plan,
		QuotaEventsSoft:   current.QuotaEventsSoft,
		QuotaEventsHard:   current.QuotaEventsHard,
		QuotaMessagesSoft: current.QuotaMessagesSoft,
		QuotaMessagesHard: current.QuotaMessagesHard,
		QuotaPeriod:       current.QuotaPeriod,
	}
	if body.Plan != nil {
		next.Plan = *body.Plan
	}
	if body.QuotaPeriod != nil {
		next.QuotaPeriod = *body.QuotaPeriod
	}
	if body.QuotaEventsSoft != nil {
		next.QuotaEventsSoft = *body.QuotaEventsSoft
	}
	if body.QuotaEventsHard != nil {
		next.QuotaEventsHard = *body.QuotaEventsHard
	}
	if body.QuotaMessagesSoft != nil {
		next.QuotaMessagesSoft = *body.QuotaMessagesSoft
	}
	if body.QuotaMessagesHard != nil {
		next.QuotaMessagesHard = *body.QuotaMessagesHard
	}

	// Validate before writing: plan and quota_period are CHECK-constrained in
	// the schema (db/migrations/20261002043243_usage_metering.sql), so an
	// invalid value here is a 500 from Postgres unless it's rejected first.
	if !validPlans[next.Plan] {
		httpx.Err(w, http.StatusBadRequest, "plan must be one of free|pro|enterprise|custom")
		return
	}
	if !validPeriods[next.QuotaPeriod] {
		httpx.Err(w, http.StatusBadRequest, "quota_period must be one of day|month")
		return
	}
	for _, v := range [...]int64{next.QuotaEventsSoft, next.QuotaEventsHard, next.QuotaMessagesSoft, next.QuotaMessagesHard} {
		if v < 0 {
			httpx.Err(w, http.StatusBadRequest, "quota values must be >= 0")
			return
		}
	}

	updated, err := d.Queries.UpdateOrgQuota(r.Context(), next)
	if err != nil {
		d.Log.Error("patch plan: update", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "patch plan")
		return
	}

	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "org.plan.update",
		TargetType: "org",
		TargetID:   audit.PtrUUID(orgID),
		OrgID:      orgID,
		Metadata: map[string]any{
			"plan":                updated.Plan,
			"quota_period":        updated.QuotaPeriod,
			"quota_events_soft":   updated.QuotaEventsSoft,
			"quota_events_hard":   updated.QuotaEventsHard,
			"quota_messages_soft": updated.QuotaMessagesSoft,
			"quota_messages_hard": updated.QuotaMessagesHard,
		},
	})
	httpx.WriteJSON(w, http.StatusOK, quotaView(updated.Plan, updated.QuotaPeriod,
		updated.QuotaEventsSoft, updated.QuotaEventsHard, updated.QuotaMessagesSoft, updated.QuotaMessagesHard))
}
