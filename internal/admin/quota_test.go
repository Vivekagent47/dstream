package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"
)

// router with ONLY the quota routes, no SuperAdminOnly (logic-under-test;
// same pattern as usageRouter in usage_test.go).
func quotaRouter(d Deps) chi.Router {
	r := chi.NewRouter()
	r.Patch("/admin/orgs/{org_id}/plan", d.handlePatchOrgPlan)
	r.Get("/admin/plans", d.handleListPlans)
	return r
}

// seedQuotaOrg creates an org with an explicit plan + limits and removes it
// afterwards, so a failing assertion doesn't leave rows behind for the next
// run to trip over.
func seedQuotaOrg(t *testing.T, d Deps, plan, period string, evSoft, evHard, msgSoft, msgHard int64) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	org, err := d.Queries.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: "Quota Org", Slug: "quota-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	id := store.GoUUID(org.ID)
	// Clean up through the query layer, not d.Pool — testDeps (queues_test.go:23)
	// sets Queries but leaves Pool nil, so a d.Pool.Exec here nil-panics.
	t.Cleanup(func() {
		_ = d.Queries.DeleteOrganization(context.Background(), org.ID)
	})
	if _, err := d.Queries.UpdateOrgQuota(ctx, store.UpdateOrgQuotaParams{
		ID: org.ID, Plan: plan, QuotaPeriod: period,
		QuotaEventsSoft: evSoft, QuotaEventsHard: evHard,
		QuotaMessagesSoft: msgSoft, QuotaMessagesHard: msgHard,
	}); err != nil {
		t.Fatalf("seed quota: %v", err)
	}
	return id
}

func patchPlan(t *testing.T, d Deps, orgID uuid.UUID, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch,
		"/admin/orgs/"+orgID.String()+"/plan", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	quotaRouter(d).ServeHTTP(rec, req)
	return rec
}

// TestPatchOrgPlan_PresetApplied: choosing a tier applies all four limits and
// the period from usage.Presets, with no numbers in the request at all. That
// is the whole point of presets.
func TestPatchOrgPlan_PresetApplied(t *testing.T) {
	d, _ := testDeps(t)
	if d.Queries == nil {
		t.Skip("no DSTREAM_TEST_DB_URL")
	}
	id := seedQuotaOrg(t, d, "free", "month", 0, 0, 0, 0)

	rec := patchPlan(t, d, id, map[string]any{"plan": "pro"})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	row, err := d.Queries.GetOrgQuota(context.Background(), store.UUID(id))
	if err != nil {
		t.Fatalf("get org quota: %v", err)
	}
	pro := usage.Presets["pro"]
	if row.Plan != "pro" || row.QuotaEventsSoft != pro.EventsSoft ||
		row.QuotaEventsHard != pro.EventsHard || row.QuotaMessagesSoft != pro.MessagesSoft ||
		row.QuotaMessagesHard != pro.MessagesHard || row.QuotaPeriod != pro.Period {
		t.Errorf("preset not applied: %+v want %+v", row, pro)
	}
}

// TestPatchOrgPlan_PresetOverwritesCustomPeriod pins Review Focus #4: an org
// moved off a day-period custom plan must land on the preset's month, or the
// sweep buckets its rollups at one period while the gate reads another and
// the org's usage reads as zero.
func TestPatchOrgPlan_PresetOverwritesCustomPeriod(t *testing.T) {
	d, _ := testDeps(t)
	if d.Queries == nil {
		t.Skip("no DSTREAM_TEST_DB_URL")
	}
	id := seedQuotaOrg(t, d, "custom", "day", 10, 20, 30, 40)

	if rec := patchPlan(t, d, id, map[string]any{"plan": "pro"}); rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	row, err := d.Queries.GetOrgQuota(context.Background(), store.UUID(id))
	if err != nil {
		t.Fatalf("get org quota: %v", err)
	}
	if row.QuotaPeriod != "month" {
		t.Errorf("quota_period: got %q want \"month\" (the preset owns the period)", row.QuotaPeriod)
	}
}

// TestPatchOrgPlan_CustomAcceptsRawLimits: the escape hatch works, and omitted
// fields keep their current value.
func TestPatchOrgPlan_CustomAcceptsRawLimits(t *testing.T) {
	d, _ := testDeps(t)
	if d.Queries == nil {
		t.Skip("no DSTREAM_TEST_DB_URL")
	}
	id := seedQuotaOrg(t, d, "custom", "month", 1, 2, 3, 4)

	rec := patchPlan(t, d, id, map[string]any{"quota_events_hard": 2000000})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	row, err := d.Queries.GetOrgQuota(context.Background(), store.UUID(id))
	if err != nil {
		t.Fatalf("get org quota: %v", err)
	}
	if row.QuotaEventsHard != 2000000 {
		t.Errorf("quota_events_hard: got %d want 2000000", row.QuotaEventsHard)
	}
	if row.QuotaMessagesHard != 4 {
		t.Errorf("quota_messages_hard: got %d want 4 untouched (partial update)", row.QuotaMessagesHard)
	}
}

// TestPatchOrgPlan_PresetRejectsExplicitQuotaFields is the precedence rule:
// a preset plan plus a number is a contradiction, and silently picking one
// would make the audit log unreadable — you could not tell a tier change from
// a negotiated exception.
func TestPatchOrgPlan_PresetRejectsExplicitQuotaFields(t *testing.T) {
	d, _ := testDeps(t)
	if d.Queries == nil {
		t.Skip("no DSTREAM_TEST_DB_URL")
	}
	id := seedQuotaOrg(t, d, "free", "month", 8000, 10000, 8000, 10000)

	for _, body := range []map[string]any{
		{"plan": "pro", "quota_events_hard": 99},
		{"plan": "pro", "quota_period": "day"},
		{"plan": "free", "quota_messages_soft": 1},
	} {
		if rec := patchPlan(t, d, id, body); rec.Code != http.StatusBadRequest {
			t.Errorf("body=%v: got %d want 400; resp=%s", body, rec.Code, rec.Body.String())
		}
	}
}

// TestPatchOrgPlan_QuotaFieldsNeedCustom: with no plan in the body the rule is
// read against the STORED plan, so a bare number PATCH is rejected on a
// preset org and accepted on a custom one.
func TestPatchOrgPlan_QuotaFieldsNeedCustom(t *testing.T) {
	d, _ := testDeps(t)
	if d.Queries == nil {
		t.Skip("no DSTREAM_TEST_DB_URL")
	}
	onFree := seedQuotaOrg(t, d, "free", "month", 8000, 10000, 8000, 10000)
	onCustom := seedQuotaOrg(t, d, "custom", "month", 1, 2, 3, 4)

	if rec := patchPlan(t, d, onFree, map[string]any{"quota_events_hard": 50}); rec.Code != http.StatusBadRequest {
		t.Errorf("free org: got %d want 400; resp=%s", rec.Code, rec.Body.String())
	}
	if rec := patchPlan(t, d, onCustom, map[string]any{"quota_events_hard": 50}); rec.Code != http.StatusOK {
		t.Errorf("custom org: got %d want 200; resp=%s", rec.Code, rec.Body.String())
	}
}

// TestPatchOrgPlan_InvalidValues_400NotServerError carries over the 5c guard:
// plan and quota_period are CHECK-constrained, so an unvalidated bad value
// reaches Postgres as a 500.
func TestPatchOrgPlan_InvalidValues_400NotServerError(t *testing.T) {
	d, _ := testDeps(t)
	if d.Queries == nil {
		t.Skip("no DSTREAM_TEST_DB_URL")
	}
	id := seedQuotaOrg(t, d, "custom", "month", 1, 2, 3, 4)

	for _, body := range []map[string]any{
		{"plan": "bogus"},
		{"plan": "custom", "quota_period": "week"},
		{"plan": "custom", "quota_events_soft": -1},
	} {
		if rec := patchPlan(t, d, id, body); rec.Code != http.StatusBadRequest {
			t.Errorf("body=%v: got %d want 400 (must reject before the DB CHECK); resp=%s",
				body, rec.Code, rec.Body.String())
		}
	}
}

// TestPatchOrgPlan_UnknownOrg404 — a typo'd id must not read as success.
func TestPatchOrgPlan_UnknownOrg404(t *testing.T) {
	d, _ := testDeps(t)
	if d.Queries == nil {
		t.Skip("no DSTREAM_TEST_DB_URL")
	}
	if rec := patchPlan(t, d, uuid.New(), map[string]any{"plan": "pro"}); rec.Code != http.StatusNotFound {
		t.Errorf("got %d want 404; resp=%s", rec.Code, rec.Body.String())
	}
}

// TestPlanOrderCoversEveryPreset: planOrder exists to keep GET /admin/plans in
// a stable, cheapest-first order, which means a tier added to usage.Presets
// and not to planOrder would silently never appear in the console's picker.
func TestPlanOrderCoversEveryPreset(t *testing.T) {
	// Deps{} rather than testDeps(t): handleListPlans reads only the preset
	// map, so this must not skip merely because Redis is down.
	d := Deps{}

	inOrder := map[string]bool{}
	for _, p := range planOrder {
		if _, ok := usage.Presets[p]; !ok {
			t.Errorf("planOrder has %q, which is not in usage.Presets", p)
		}
		inOrder[p] = true
	}
	for name := range usage.Presets {
		if !inOrder[name] {
			t.Errorf("usage.Presets has %q, missing from planOrder — it would never reach the console", name)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/plans", nil)
	rec := httptest.NewRecorder()
	quotaRouter(d).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []planView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(usage.Presets)+1 {
		t.Fatalf("got %d plans, want %d presets + custom", len(got), len(usage.Presets))
	}
	last := got[len(got)-1]
	if last.Plan != usage.PlanCustom || !last.Custom {
		t.Errorf("last entry must be the custom marker, got %+v", last)
	}
	free := usage.Presets["free"]
	if got[0].Plan != "free" || got[0].EventsHard != free.EventsHard {
		t.Errorf("first entry must be free with its real limits, got %+v", got[0])
	}
}
