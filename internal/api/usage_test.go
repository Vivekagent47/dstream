package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"
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
