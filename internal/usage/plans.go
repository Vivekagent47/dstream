package usage

// PlanCustom is the plan name that means "per-org numbers a super-admin
// typed", as opposed to a product tier. It is the marker that makes the
// precedence rule in internal/admin/quota.go work without a second column.
const PlanCustom = "custom"

// Limits is defined in gate.go: one org's quota row, the same shape a plan
// preset below fills in. Reused rather than redeclared — same name, same
// five fields, same 0-means-unlimited rule as the organizations columns.

// Presets maps a plan name to its limits. These are product tiers, so they
// are constants rather than rows: changing a tier is a release, not an admin
// action.
//
// PlanCustom is deliberately absent. It means per-org numbers, so it has
// nothing to preset, and PresetFor's second return value is what callers
// branch on.
//
// enterprise is all zeros on purpose: an enterprise agreement is negotiated
// outside the product, and a ceiling the operator forgot to raise is worse
// than no ceiling for that customer.
//
// The free row is duplicated as the column DEFAULTs in db/schema/schema.sql,
// so a brand-new org lands on the free tier with no Go involvement at all.
// TestFreePresetMatchesSchemaDefaults asserts the two agree — change both or
// neither.
var Presets = map[string]Limits{
	"free": {
		EventsSoft: 8_000, EventsHard: 10_000,
		MessagesSoft: 8_000, MessagesHard: 10_000,
		Period: "month",
	},
	"pro": {
		EventsSoft: 800_000, EventsHard: 1_000_000,
		MessagesSoft: 800_000, MessagesHard: 1_000_000,
		Period: "month",
	},
	"enterprise": {Period: "month"},
}

// PresetFor reports a plan's limits and whether that plan is preset-driven.
// A false second return means PlanCustom or an unknown name; callers validate
// the name with ValidPlan separately.
func PresetFor(plan string) (Limits, bool) {
	l, ok := Presets[plan]
	return l, ok
}

// ValidPlan reports whether plan is one the organizations.plan CHECK
// constraint accepts. Derived from Presets rather than restated, so a new tier
// becomes valid by being added to the map.
func ValidPlan(plan string) bool {
	if plan == PlanCustom {
		return true
	}
	_, ok := Presets[plan]
	return ok
}

// ValidPeriod mirrors the organizations.quota_period CHECK constraint. Keep in
// step with PeriodStart, which is what actually buckets a rollup.
func ValidPeriod(p string) bool { return p == "day" || p == "month" }
