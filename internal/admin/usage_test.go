package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"
)

// router with ONLY the usage route, no SuperAdminOnly (logic-under-test;
// same pattern as queueRouter in queues_test.go).
func usageRouter(d Deps) chi.Router {
	r := chi.NewRouter()
	r.Get("/admin/usage", d.handleUsage)
	return r
}

// TestHandleUsage_DayPeriodOrg_ReadsCorrectRollup is the admin-view twin of
// the same guard in internal/api/usage_test.go: the cross-tenant view must
// key each org's lookup by THAT org's own quota_period, not a hardcoded
// "month" — otherwise a 'day'-period org reads back as zero usage here too.
func TestHandleUsage_DayPeriodOrg_ReadsCorrectRollup(t *testing.T) {
	d, _ := testDeps(t)
	if d.Queries == nil {
		t.Skip("no DSTREAM_TEST_DB_URL")
	}
	ctx := context.Background()

	org, err := d.Queries.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: "Day Org", Slug: "day-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if _, err := d.Queries.UpdateOrgQuota(ctx, store.UpdateOrgQuotaParams{
		ID: org.ID, Plan: "pro", QuotaPeriod: "day",
		QuotaEventsSoft: 100, QuotaEventsHard: 200,
		QuotaMessagesSoft: 50, QuotaMessagesHard: 100,
	}); err != nil {
		t.Fatalf("seed quota: %v", err)
	}
	dayStart := usage.PeriodStart(time.Now(), "day")
	if err := d.Queries.UpsertUsageRollup(ctx, store.UpsertUsageRollupParams{
		OrgID: org.ID, PeriodStart: pgtype.Timestamptz{Time: dayStart, Valid: true},
		Metric: "events", Count: 77,
	}); err != nil {
		t.Fatalf("seed rollup: %v", err)
	}

	r := usageRouter(d)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}

	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantID := store.GoUUID(org.ID).String()
	var found map[string]any
	for _, row := range rows {
		if row["org_id"] == wantID {
			found = row
			break
		}
	}
	if found == nil {
		t.Fatalf("org %s not present in admin usage view (%d orgs returned)", wantID, len(rows))
	}
	if found["period"] != "day" {
		t.Errorf("period: got %v want day", found["period"])
	}
	um, ok := found["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage field missing/wrong shape: %v", found)
	}
	if got := um["events"]; got != float64(77) {
		t.Errorf("events: got %v want 77 — admin view must key the lookup by this org's own quota_period", got)
	}
}
