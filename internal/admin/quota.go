package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"
)

// planView is one row of GET /admin/plans: a plan the console can offer, with
// the limits choosing it would apply. Custom carries no limits and is flagged,
// so the UI renders number inputs for it instead of a preset summary.
type planView struct {
	Plan         string `json:"plan"`
	EventsSoft   int64  `json:"events_soft"`
	EventsHard   int64  `json:"events_hard"`
	MessagesSoft int64  `json:"messages_soft"`
	MessagesHard int64  `json:"messages_hard"`
	Period       string `json:"period"`
	Custom       bool   `json:"custom,omitempty"`
}

// planOrder fixes the order GET /admin/plans returns, cheapest tier first.
// Ranging usage.Presets directly would reshuffle the console's plan picker on
// every reload. TestPlanOrderCoversEveryPreset keeps it in step with the map.
var planOrder = []string{"free", "pro", "enterprise"}

// handleListPlans serves GET /admin/plans — the preset table, so the console
// can show what a tier grants without restating the numbers in TypeScript.
func (d Deps) handleListPlans(w http.ResponseWriter, r *http.Request) {
	out := make([]planView, 0, len(planOrder)+1)
	for _, name := range planOrder {
		l := usage.Presets[name]
		out = append(out, planView{
			Plan: name,
			EventsSoft: l.EventsSoft, EventsHard: l.EventsHard,
			MessagesSoft: l.MessagesSoft, MessagesHard: l.MessagesHard,
			Period: l.Period,
		})
	}
	// Appended rather than mapped: custom is not in usage.Presets, because it
	// means "numbers a super-admin types" and has nothing to preset.
	out = append(out, planView{Plan: usage.PlanCustom, Period: "month", Custom: true})
	writeJSON(w, http.StatusOK, out)
}

// handlePatchOrgPlan serves PATCH /admin/orgs/{org_id}/plan — sets an org's
// plan, and on the custom plan its raw limits.
//
// Super-admin only by virtue of the surface: everything under /admin/* sits
// behind auth.SuperAdminOnly. There is deliberately NO membership or role
// check here — the operator granting quota is not a member of the org they
// grant it to, so a membership lookup would make this unusable for its
// purpose. This replaced an owner-only PATCH /api/orgs/{org_id}/plan, which
// let a tenant raise their own ceiling; do not reintroduce a tenant-side
// equivalent.
//
// Precedence (design §4): a preset plan owns all four limits AND the period,
// and any quota_* field sent alongside one is rejected rather than
// overwritten. Raising a single tenant therefore reads as two explicit acts —
// move them to custom, then set the number — and the audit log can tell a
// tier change from a negotiated exception.
//
// Changing quota_period costs an hour of under-enforcement, not just stale
// reporting. A preset plan carries its own period, so moving an org onto one
// — e.g. a custom/day org to pro/month — can change quota_period in the same
// write. The gate immediately starts counting against the new PeriodStart's
// Redis key, which begins at 0 regardless of how much the org already did
// this period in Postgres; only cmd/dstream/maintenance.go's hourly sweep
// reconciles that key to the real count. Until it runs, the org is
// under-enforced against its new limits, and GET /api/usage and
// GET /admin/usage — which both look up a usage_rollups row at that same new
// PeriodStart — read it as zero usage, because the sweep hasn't written that
// row yet either. Self-healing, and only ever triggered by an operator's own
// PATCH — but worth knowing before the first support ticket about an org
// that "shows zero usage right after we upgraded it".
func (d Deps) handlePatchOrgPlan(w http.ResponseWriter, r *http.Request) {
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		http.Error(w, "invalid org_id", http.StatusBadRequest)
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
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	current, err := d.Queries.GetOrgQuota(r.Context(), store.UUID(orgID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "org not found", http.StatusNotFound)
			return
		}
		d.Log.Error("admin patch plan: get org quota", "err", err)
		http.Error(w, "patch plan", http.StatusInternalServerError)
		return
	}

	// An omitted plan means "keep the current one", and the precedence rule
	// is then read against the STORED plan — so a bare quota_* PATCH is
	// accepted on an org already on custom and rejected on one that isn't.
	plan := current.Plan
	if body.Plan != nil {
		plan = *body.Plan
	}
	if !usage.ValidPlan(plan) {
		http.Error(w, "plan must be one of free|pro|enterprise|custom", http.StatusBadRequest)
		return
	}

	quotaFieldsGiven := body.QuotaEventsSoft != nil || body.QuotaEventsHard != nil ||
		body.QuotaMessagesSoft != nil || body.QuotaMessagesHard != nil ||
		body.QuotaPeriod != nil

	next := store.UpdateOrgQuotaParams{
		ID:                store.UUID(orgID),
		Plan:              plan,
		QuotaEventsSoft:   current.QuotaEventsSoft,
		QuotaEventsHard:   current.QuotaEventsHard,
		QuotaMessagesSoft: current.QuotaMessagesSoft,
		QuotaMessagesHard: current.QuotaMessagesHard,
		QuotaPeriod:       current.QuotaPeriod,
	}

	preset, isPreset := usage.PresetFor(plan)
	if isPreset {
		if quotaFieldsGiven {
			http.Error(w,
				"limits and quota_period can only be set on plan=custom; move the org to custom first",
				http.StatusBadRequest)
			return
		}
		// The period comes from the preset too, not just the four counts: pro
		// at 1,000,000 events per DAY is thirty times the tier the operator
		// thinks they granted. It also keeps the org's rollup bucket and the
		// gate's lookup on the same period.
		next.QuotaEventsSoft = preset.EventsSoft
		next.QuotaEventsHard = preset.EventsHard
		next.QuotaMessagesSoft = preset.MessagesSoft
		next.QuotaMessagesHard = preset.MessagesHard
		next.QuotaPeriod = preset.Period
	} else {
		// plan == custom: partial update, so switching an org to custom with
		// no other field is a no-op that just unlocks the numbers.
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
		if body.QuotaPeriod != nil {
			next.QuotaPeriod = *body.QuotaPeriod
		}
	}

	// Validate before writing: plan and quota_period are CHECK-constrained in
	// the schema, so an unvalidated bad value arrives as a 500 from Postgres
	// instead of a 400 from here.
	if !usage.ValidPeriod(next.QuotaPeriod) {
		http.Error(w, "quota_period must be one of day|month", http.StatusBadRequest)
		return
	}
	for _, v := range [...]int64{next.QuotaEventsSoft, next.QuotaEventsHard,
		next.QuotaMessagesSoft, next.QuotaMessagesHard} {
		if v < 0 {
			http.Error(w, "quota values must be >= 0", http.StatusBadRequest)
			return
		}
	}

	updated, err := d.Queries.UpdateOrgQuota(r.Context(), next)
	if err != nil {
		d.Log.Error("admin patch plan: update", "err", err)
		http.Error(w, "patch plan", http.StatusInternalServerError)
		return
	}

	// OrgID is the TARGET org, not the actor's: the entry lands in that org's
	// own audit log, so an owner can see that their provider changed their
	// quota and when. auth.SuperAdminOnly puts a SourceSession principal in
	// the context, so audit.Log resolves the actor normally.
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "org.plan.update",
		TargetType: "org",
		TargetID:   audit.PtrUUID(orgID),
		OrgID:      orgID,
		Metadata: map[string]any{
			"preset":              isPreset,
			"plan":                updated.Plan,
			"quota_period":        updated.QuotaPeriod,
			"quota_events_soft":   updated.QuotaEventsSoft,
			"quota_events_hard":   updated.QuotaEventsHard,
			"quota_messages_soft": updated.QuotaMessagesSoft,
			"quota_messages_hard": updated.QuotaMessagesHard,
		},
	})

	// Same shape as GET /api/usage's plan block, so the two surfaces agree.
	writeJSON(w, http.StatusOK, map[string]any{
		"plan":   updated.Plan,
		"period": updated.QuotaPeriod,
		"limits": map[string]any{
			"events_soft":   updated.QuotaEventsSoft,
			"events_hard":   updated.QuotaEventsHard,
			"messages_soft": updated.QuotaMessagesSoft,
			"messages_hard": updated.QuotaMessagesHard,
		},
	})
}
