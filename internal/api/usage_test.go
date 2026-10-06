package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"

	"github.com/Vivekagent47/dstream/internal/api/identity"
)

// --- GET /api/usage ---

// TestGetUsage_DayPeriod_ReadsCorrectRollup is the guard for the trap named in
// the task-5 addendum: GetUsageForPeriod is keyed by an EXACT period_start,
// and the sweep writes it at usage.PeriodStart(now, org.quota_period). A
// handler that hardcoded "month" would compute the wrong period_start for a
// 'day'-period org, match no row, and report 0 despite the 42 seeded below.
func TestGetUsage_DayPeriod_ReadsCorrectRollup(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q) // uid is owner
	ctx := context.Background()

	if _, err := q.UpdateOrgQuota(ctx, store.UpdateOrgQuotaParams{
		ID:                store.UUID(oid),
		Plan:              "pro",
		QuotaPeriod:       "day",
		QuotaEventsSoft:   1000,
		QuotaEventsHard:   5000,
		QuotaMessagesSoft: 500,
		QuotaMessagesHard: 2000,
	}); err != nil {
		t.Fatalf("seed quota: %v", err)
	}
	dayStart := usage.PeriodStart(time.Now(), "day")
	if err := q.UpsertUsageRollup(ctx, store.UpsertUsageRollupParams{
		OrgID:       store.UUID(oid),
		PeriodStart: pgtype.Timestamptz{Time: dayStart, Valid: true},
		Metric:      "events",
		Count:       42,
	}); err != nil {
		t.Fatalf("seed rollup: %v", err)
	}

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodGet, "/api/usage", uid, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Plan    string           `json:"plan"`
		Period  string           `json:"period"`
		Partial bool             `json:"partial"`
		Usage   map[string]int64 `json:"usage"`
		Limits  map[string]int64 `json:"limits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Period != "day" {
		t.Errorf("period: got %q want day", resp.Period)
	}
	if !resp.Partial {
		t.Errorf("partial: got false, want true (current period is always still accumulating)")
	}
	if resp.Usage["events"] != 42 {
		t.Fatalf("events: got %d want 42 — lookup must use the org's own quota_period, not a hardcoded month", resp.Usage["events"])
	}
	if resp.Usage["messages"] != 0 {
		t.Errorf("messages: got %d want 0 (no rollup row seeded)", resp.Usage["messages"])
	}
	if resp.Limits["events_soft"] != 1000 || resp.Limits["events_hard"] != 5000 {
		t.Errorf("limits: got %v", resp.Limits)
	}
}

// --- GET /api/usage/history ---

func TestGetUsageHistory_FiltersMetricAndMarksCurrentPartial(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q) // default plan: month period
	ctx := context.Background()

	current := usage.PeriodStart(time.Now(), "month")
	prev := current.AddDate(0, -1, 0)

	for _, row := range []struct {
		t time.Time
		c int64
	}{{current, 10}, {prev, 5}} {
		if err := q.UpsertUsageRollup(ctx, store.UpsertUsageRollupParams{
			OrgID: store.UUID(oid), PeriodStart: pgtype.Timestamptz{Time: row.t, Valid: true},
			Metric: "events", Count: row.c,
		}); err != nil {
			t.Fatalf("seed rollup: %v", err)
		}
	}
	// Different metric at the current period, to prove metric filtering.
	if err := q.UpsertUsageRollup(ctx, store.UpsertUsageRollupParams{
		OrgID: store.UUID(oid), PeriodStart: pgtype.Timestamptz{Time: current, Valid: true},
		Metric: "messages", Count: 999,
	}); err != nil {
		t.Fatalf("seed rollup: %v", err)
	}

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodGet, "/api/usage/history?metric=events&periods=2", uid, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Metric  string `json:"metric"`
		Periods []struct {
			Count   int64 `json:"count"`
			Partial bool  `json:"partial"`
		} `json:"periods"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Metric != "events" {
		t.Errorf("metric: got %q", resp.Metric)
	}
	if len(resp.Periods) != 2 {
		t.Fatalf("periods: got %d want 2: %v", len(resp.Periods), resp.Periods)
	}
	if resp.Periods[0].Count != 10 || !resp.Periods[0].Partial {
		t.Errorf("current period: got count=%d partial=%v, want 10/true", resp.Periods[0].Count, resp.Periods[0].Partial)
	}
	if resp.Periods[1].Count != 5 || resp.Periods[1].Partial {
		t.Errorf("prior period: got count=%d partial=%v, want 5/false", resp.Periods[1].Count, resp.Periods[1].Partial)
	}
}

func TestGetUsageHistory_InvalidMetric_400(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodGet, "/api/usage/history?metric=bogus", uid, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- PATCH /api/orgs/{org_id}/plan is gone ---

// TestTenantCannotPatchOwnPlan is the hole this change closes, pinned.
//
// 5c shipped an owner-only PATCH here, which let the owner of an org raise
// their own ceiling — the one thing the ceiling exists to prevent. Quotas are
// now granted by the platform operator at PATCH /admin/orgs/{org_id}/plan,
// behind auth.SuperAdminOnly.
//
// Asserting on "not 200, and the row is unchanged" rather than one exact
// status: with the route deleted and no sibling method on the pattern, chi
// answers 404, but adding a future GET /plan would make the same request a
// 405. Either is correct; a 200, or a changed row, is not.
func TestTenantCannotPatchOwnPlan(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	owner, oid := seedUserAndOrg(t, q)
	ctx := context.Background()

	before, err := q.GetOrgQuota(ctx, store.UUID(oid))
	if err != nil {
		t.Fatalf("get org quota: %v", err)
	}

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPatch,
		"/api/orgs/"+oid.String()+"/plan", owner, oid, map[string]any{
			"plan":              "enterprise",
			"quota_events_hard": 999999999,
		})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("owner patch plan: got %d, want 404 or 405 — the tenant-side quota write must not exist; body=%s",
			rec.Code, rec.Body.String())
	}

	after, err := q.GetOrgQuota(ctx, store.UUID(oid))
	if err != nil {
		t.Fatalf("get org quota: %v", err)
	}
	if after != before {
		t.Errorf("quota changed via the tenant plane: %+v -> %+v", before, after)
	}
}

// =============================================================================
// Usage: empty orgs, limits shaping, every metric, period maths, isolation.
// =============================================================================

func (e *idEnv) setQuota(org uuid.UUID, plan, period string, evSoft, evHard, msgSoft, msgHard int64) {
	e.t.Helper()
	if _, err := e.q.UpdateOrgQuota(context.Background(), store.UpdateOrgQuotaParams{
		ID: store.UUID(org), Plan: plan, QuotaPeriod: period,
		QuotaEventsSoft: evSoft, QuotaEventsHard: evHard, QuotaMessagesSoft: msgSoft, QuotaMessagesHard: msgHard,
	}); err != nil {
		e.t.Fatalf("set quota: %v", err)
	}
}

func (e *idEnv) rollup(org uuid.UUID, at time.Time, metric string, n int64) {
	e.t.Helper()
	if err := e.q.UpsertUsageRollup(context.Background(), store.UpsertUsageRollupParams{
		OrgID: store.UUID(org), PeriodStart: pgtype.Timestamptz{Time: at, Valid: true}, Metric: metric, Count: n,
	}); err != nil {
		e.t.Fatalf("seed rollup: %v", err)
	}
}

type usageResp struct {
	Plan        string           `json:"plan"`
	Period      string           `json:"period"`
	PeriodStart time.Time        `json:"period_start"`
	Partial     bool             `json:"partial"`
	Usage       map[string]int64 `json:"usage"`
	Limits      map[string]int64 `json:"limits"`
}

func (e *idEnv) usageFor(uid, org uuid.UUID) usageResp {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/api/usage", uid, org, nil)
	wantStatus(e.t, rec, http.StatusOK)
	var r usageResp
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		e.t.Fatalf("decode usage: %v", err)
	}
	return r
}

func TestGetUsage_OrgWithNoRows_AllMetricsZeroDefaultLimits(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	monthStart := usage.PeriodStart(time.Now(), "month")
	r := e.usageFor(owner, org)

	if len(r.Usage) != 4 {
		t.Fatalf("usage must name all four metrics even with no rows: %v", r.Usage)
	}
	for _, m := range []string{"requests", "events", "messages", "attempts"} {
		if n, ok := r.Usage[m]; !ok || n != 0 {
			t.Errorf("%s: got %d (present=%v) want 0", m, n, ok)
		}
	}
	if r.Plan != "free" || r.Period != "month" || !r.Partial {
		t.Errorf("shape: %+v", r)
	}
	if !r.PeriodStart.Equal(monthStart) {
		t.Errorf("period_start %v want the month start %v", r.PeriodStart, monthStart)
	}
	want := map[string]int64{"events_soft": 8000, "events_hard": 10000, "messages_soft": 8000, "messages_hard": 10000}
	for k, v := range want {
		if got, ok := r.Limits[k]; !ok || got != v {
			t.Errorf("limit %s: got %d (present=%v) want %d", k, got, ok, v)
		}
	}
}

// 0 means "unlimited, per tier independently" (web/src/lib/quota.ts). The API
// must hand 0 through untouched and present: dropping the key, or rewriting 0
// to a ceiling, would flip the UI between "no limit" and "limit of nothing".
func TestGetUsage_ZeroLimitsPassThroughPerTier(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	e.setQuota(org, "custom", "month", 500, 0, 0, 900) // warn-only events, hard-only messages
	r := e.usageFor(owner, org)
	want := map[string]int64{"events_soft": 500, "events_hard": 0, "messages_soft": 0, "messages_hard": 900}
	for k, v := range want {
		if got, ok := r.Limits[k]; !ok || got != v {
			t.Errorf("limit %s: got %d (present=%v) want %d", k, got, ok, v)
		}
	}
	e.setQuota(org, "enterprise", "month", 0, 0, 0, 0)
	r = e.usageFor(owner, org)
	if r.Plan != "enterprise" {
		t.Errorf("plan: %q", r.Plan)
	}
	for k, got := range r.Limits {
		if got != 0 {
			t.Errorf("enterprise %s: got %d want 0 (unlimited)", k, got)
		}
	}
	if len(r.Limits) != 4 {
		t.Errorf("limits keys: %v", r.Limits)
	}
}

func TestGetUsage_RollupsAreScopedToOrgAndPeriod(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	ownerA, orgA := e.seedOrg()
	ownerB, orgB := e.seedOrg()
	cur := usage.PeriodStart(time.Now(), "month")
	for m, n := range map[string]int64{"requests": 11, "events": 22, "messages": 33, "attempts": 44} {
		e.rollup(orgA, cur, m, n)
	}
	e.rollup(orgA, cur.AddDate(0, -1, 0), "events", 7777) // last period: not in the current totals

	a := e.usageFor(ownerA, orgA)
	if a.Usage["requests"] != 11 || a.Usage["events"] != 22 || a.Usage["messages"] != 33 || a.Usage["attempts"] != 44 {
		t.Fatalf("org A usage: %v", a.Usage)
	}
	for m, n := range e.usageFor(ownerB, orgB).Usage {
		if n != 0 {
			t.Errorf("org B reads %s=%d from org A's rollups", m, n)
		}
	}
}

func TestUsage_StoreFailures500(t *testing.T) {
	for _, c := range []struct {
		name, frag, path, msg string
	}{
		{"quota", "from organizations where id", "/api/usage", "get usage"},
		{"rollup", "from usage_rollups where org_id = $1 and period_start", "/api/usage", "get usage"},
		{"history quota", "from organizations where id", "/api/usage/history?metric=events", "get usage history"},
		{"history query", "from usage_rollups where org_id = $1 and metric", "/api/usage/history?metric=events", "get usage history"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newIDEnv(t, failOn(c.frag), nil)
			owner, org := e.seedOrg()
			wantErr(t, e.do(http.MethodGet, c.path, owner, org, nil), 500, c.msg)
		})
	}
}

func (e *idEnv) history(uid, org uuid.UUID, query string) (period string, got map[time.Time]int64, partial map[time.Time]bool) {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/api/usage/history"+query, uid, org, nil)
	wantStatus(e.t, rec, http.StatusOK)
	var r struct {
		Period  string `json:"period"`
		Periods []struct {
			PeriodStart time.Time `json:"period_start"`
			Count       int64     `json:"count"`
			Partial     bool      `json:"partial"`
		} `json:"periods"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		e.t.Fatalf("decode history: %v", err)
	}
	if r.Periods == nil {
		e.t.Fatalf("periods must be [] not null: %s", rec.Body.String())
	}
	got, partial = map[time.Time]int64{}, map[time.Time]bool{}
	for _, p := range r.Periods {
		got[p.PeriodStart.UTC()] = p.Count
		partial[p.PeriodStart.UTC()] = p.Partial
	}
	return r.Period, got, partial
}

func TestGetUsageHistory_EachMetricReturnsOnlyItself_EmptyIsEmptyList(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	cur := usage.PeriodStart(time.Now(), "month")
	metrics := []string{"requests", "events", "messages", "attempts"}

	// No rows at all: an empty list, not null, and still a 200.
	if _, got, _ := e.history(owner, org, "?metric=events"); len(got) != 0 {
		t.Fatalf("empty org history: %v", got)
	}
	for i, m := range metrics {
		e.rollup(org, cur, m, int64(100+i))
	}
	for i, m := range metrics {
		period, got, partial := e.history(owner, org, "?metric="+m)
		if period != "month" || len(got) != 1 || got[cur] != int64(100+i) || !partial[cur] {
			t.Errorf("%s: period=%q got=%v partial=%v want only the current period at %d", m, period, got, partial, 100+i)
		}
	}
}

func TestGetUsageHistory_RefusesBadParams(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	const metricMsg = "metric must be one of requests|events|messages|attempts"
	for _, c := range []struct{ query, msg string }{
		{"", metricMsg},
		{"?metric=", metricMsg},
		{"?metric=bogus", metricMsg},
		{"?metric=EVENTS", metricMsg},
		{"?metric=events&periods=0", "periods must be a positive integer"},
		{"?metric=events&periods=-4", "periods must be a positive integer"},
		{"?metric=events&periods=abc", "periods must be a positive integer"},
	} {
		wantErr(t, e.do(http.MethodGet, "/api/usage/history"+c.query, owner, org, nil), 400, c.msg)
	}
}

// periodsAgo is reached only through this handler, so its day/month maths and
// the 100-period cap are pinned by which seeded rows come back.
func TestGetUsageHistory_PeriodWindow(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()

	t.Run("month: window is calendar months, current included", func(t *testing.T) {
		cur := usage.PeriodStart(time.Now(), "month")
		// 13 rows: the default of 12 periods must return exactly 12.
		for i := 0; i <= 12; i++ {
			e.rollup(org, cur.AddDate(0, -i, 0), "events", int64(10+i))
		}
		for _, c := range []struct {
			periods string
			back    int // how many periods back the oldest returned row sits
		}{{"1", 0}, {"2", 1}, {"3", 2}, {"", 11}} {
			q := "?metric=events"
			if c.periods != "" {
				q += "&periods=" + c.periods
			}
			_, got, _ := e.history(owner, org, q)
			if len(got) != c.back+1 {
				t.Errorf("periods=%q: got %d rows want %d (%v)", c.periods, len(got), c.back+1, got)
			}
			if _, ok := got[cur.AddDate(0, -c.back, 0)]; !ok {
				t.Errorf("periods=%q: oldest row %d months back missing", c.periods, c.back)
			}
			if _, ok := got[cur.AddDate(0, -(c.back+1), 0)]; ok {
				t.Errorf("periods=%q: returned a row from beyond the window", c.periods)
			}
		}
	})

	t.Run("day: window is days, current included", func(t *testing.T) {
		e.setQuota(org, "custom", "day", 1, 2, 3, 4)
		cur := usage.PeriodStart(time.Now(), "day")
		for i := 0; i <= 3; i++ {
			e.rollup(org, cur.AddDate(0, 0, -i), "messages", int64(20+i))
		}
		for _, c := range []struct {
			periods string
			back    int
		}{{"1", 0}, {"2", 1}, {"3", 2}} {
			period, got, partial := e.history(owner, org, "?metric=messages&periods="+c.periods)
			if period != "day" || len(got) != c.back+1 {
				t.Errorf("periods=%s: period=%q rows=%d want day/%d", c.periods, period, len(got), c.back+1)
			}
			if got[cur] != 20 || !partial[cur] {
				t.Errorf("periods=%s: current row %d partial=%v", c.periods, got[cur], partial[cur])
			}
			if len(got) > 1 && partial[cur.AddDate(0, 0, -1)] {
				t.Errorf("periods=%s: a closed period is marked partial", c.periods)
			}
		}
	})

	t.Run("periods is capped at 100", func(t *testing.T) {
		e.setQuota(org, "custom", "day", 1, 2, 3, 4)
		cur := usage.PeriodStart(time.Now(), "day")
		e.rollup(org, cur.AddDate(0, 0, -99), "attempts", 1)  // the 100th period: inside
		e.rollup(org, cur.AddDate(0, 0, -100), "attempts", 2) // the 101st: outside
		for _, n := range []int{100, 101, 100000} {
			_, got, _ := e.history(owner, org, "?metric=attempts&periods="+strconv.Itoa(n))
			if _, ok := got[cur.AddDate(0, 0, -99)]; !ok {
				t.Errorf("periods=%d: lost the 100th period", n)
			}
			if _, ok := got[cur.AddDate(0, 0, -100)]; ok {
				t.Errorf("periods=%d: served the 101st period; the cap is gone", n)
			}
		}
	})
}

// RequireOrg (409 / API-key org) means these guards cannot be reached through
// the router; they are the handlers' own defence if a route is ever mounted
// without it. Called directly, the only way to reach them.
func TestHandlers_PrincipalGuards(t *testing.T) {
	var h identity.Handlers
	session := func(org uuid.UUID) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{
			Source: auth.SourceSession, UserID: uuid.New(), OrgID: org,
		})
	}
	for _, c := range []struct {
		name string
		fn   http.HandlerFunc
		ctx  context.Context
		msg  string
	}{
		{"usage no principal", h.GetUsage, context.Background(), "active org required"},
		{"usage nil org", h.GetUsage, session(uuid.Nil), "active org required"},
		{"history no principal", h.GetUsageHistory, context.Background(), "active org required"},
		{"history nil org", h.GetUsageHistory, session(uuid.Nil), "active org required"},
		{"audit nil org", h.ListAudit, session(uuid.Nil), "active org required"},
		{"me no principal", h.Me, context.Background(), "unauthorized"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.fn(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(c.ctx))
			wantErr(t, rec, http.StatusUnauthorized, c.msg)
		})
	}
}
