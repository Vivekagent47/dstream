package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"
)

type Deps struct {
	Log     *slog.Logger
	Queries *store.Queries
	Redis   *redis.Client
	Signer  *auth.SessionSigner
	Queue   *dqueue.Client
	Pool    *pgxpool.Pool
	Version string
}

// Mount wires the /admin/* routes onto the parent. All routes here are gated
// behind super-admin session auth.
func Mount(parent chi.Router, d Deps) {
	parent.Route("/admin", func(r chi.Router) {
		r.Use(auth.SuperAdminOnly(d.Queries, d.Signer))

		// Delivery-queue depth snapshot (JSON) for the /console stats card.
		r.Get("/queues", d.handleQueues)
		r.Get("/queues/items", d.handleQueueItems)
		r.Get("/queues/orgs", d.handleQueueOrgs)
		r.Post("/queues/dead/requeue", d.handleRequeueDead)
		r.Post("/queues/scheduled/promote", d.handlePromoteScheduled)
		r.Post("/queues/dead/drain", d.handleDrainDead)

		// Cross-tenant usage view: every org against its own limits.
		r.Get("/usage", d.handleUsage)

		// Plan and quota administration. Quotas are granted by the platform
		// operator, never by the tenant: an org raising its own ceiling is
		// the thing the ceiling exists to prevent. The traffic plane must
		// therefore carry no quota write of its own — see
		// internal/api/router.go.
		r.Get("/plans", d.handleListPlans)
		r.Patch("/orgs/{org_id}/plan", d.handlePatchOrgPlan)

		// Custom admin pages (Phase 1.4 scope).
		r.Get("/overview", d.handleOverview)
		r.Get("/orgs", d.handleListOrgs)
		r.Get("/destinations/hot", d.handleHotDestinations)
		r.Get("/system", d.handleSystem)
	})
}

func (d Deps) handleQueues(w http.ResponseWriter, r *http.Request) {
	s, err := d.Queue.Stats(r.Context())
	if err != nil {
		d.Log.Error("admin queues: queue stats", "err", err)
		http.Error(w, "queues", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (d Deps) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgs, err := d.Queries.CountOrganizations(ctx)
	if err != nil {
		d.Log.Error("admin overview: count organizations", "err", err)
		http.Error(w, "overview", http.StatusInternalServerError)
		return
	}
	users, err := d.Queries.CountUsers(ctx)
	if err != nil {
		d.Log.Error("admin overview: count users", "err", err)
		http.Error(w, "overview", http.StatusInternalServerError)
		return
	}

	since := pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true}
	events24h, err := d.Queries.AdminEventsSince(ctx, since)
	if err != nil {
		d.Log.Error("admin overview: events since", "err", err)
		http.Error(w, "overview", http.StatusInternalServerError)
		return
	}
	topRows, err := d.Queries.AdminTopSources(ctx, since)
	if err != nil {
		d.Log.Error("admin overview: top sources", "err", err)
		http.Error(w, "overview", http.StatusInternalServerError)
		return
	}
	topSources := make([]map[string]any, 0, len(topRows))
	for _, s := range topRows {
		topSources = append(topSources, map[string]any{
			"source_id":   store.GoUUID(s.SourceID).String(),
			"source_name": s.SourceName,
			"events":      s.Events,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"organizations":  orgs,
		"users":          users,
		"events_24h":     events24h,
		"events_per_min": float64(events24h) / 1440.0, // avg over the 24h window
		"top_sources":    topSources,
	})
}

func (d Deps) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Queries.ListAllOrganizations(r.Context())
	if err != nil {
		d.Log.Error("admin orgs: list organizations", "err", err)
		http.Error(w, "orgs", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, o := range rows {
		out = append(out, map[string]any{
			"id":         store.GoUUID(o.ID).String(),
			"name":       o.Name,
			"slug":       o.Slug,
			"created_at": o.CreatedAt.Time,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUsage serves GET /admin/usage: every org's current-period totals
// against its own limits, for the super-admin console.
//
// Reuses ListOrgQuotas rather than adding a capped variant like
// ListAllOrganizations (200-row cap, used by handleListOrgs above).
// ListOrgQuotas is already documented (db/queries/usage.sql) as driving both
// the reconciliation sweep and this view, and unlike the orgs list — which
// users page through — the whole point of a cross-tenant usage view is
// "where does every org sit against its limit"; capping it would silently
// hide the orgs past the cap from the one surface meant to show overage
// across the whole deployment. Admin-only and infrequent, the same tradeoff
// the sweep itself already makes on this query.
//
// ponytail: N+1 usage lookups, one GetUsageForPeriod per org, same shape as
// handleQueueOrgs's N+1 org-name lookups above — admin-only + infrequent.
// Batch (one query keyed by (org_id, period_start) IN (...)) if org counts
// ever make this slow.
func (d Deps) handleUsage(w http.ResponseWriter, r *http.Request) {
	orgs, err := d.Queries.ListOrgQuotas(r.Context())
	if err != nil {
		d.Log.Error("admin usage: list org quotas", "err", err)
		http.Error(w, "usage", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	out := make([]map[string]any, 0, len(orgs))
	for _, o := range orgs {
		// Each org's OWN period, never a hardcoded "month" — same rule as
		// GET /api/usage and for the same reason: the sweep reconciles this
		// org's rollup at PeriodStart(now, o.QuotaPeriod), so a 'day' org
		// looked up at a hardcoded month start would read back as zero.
		periodStart := usage.PeriodStart(now, o.QuotaPeriod)
		rows, err := d.Queries.GetUsageForPeriod(r.Context(), store.GetUsageForPeriodParams{
			OrgID:       o.ID,
			PeriodStart: pgtype.Timestamptz{Time: periodStart, Valid: true},
		})
		if err != nil {
			// One bad org must not blank the whole console page.
			d.Log.Error("admin usage: get usage for period", "err", err, "org_id", store.GoUUID(o.ID).String())
			continue
		}
		counts := map[string]int64{"requests": 0, "events": 0, "messages": 0, "attempts": 0}
		for _, row := range rows {
			counts[row.Metric] = row.Count
		}
		out = append(out, map[string]any{
			"org_id":              store.GoUUID(o.ID).String(),
			"org_name":            o.Name,
			"org_slug":            o.Slug,
			"plan":                o.Plan,
			"period":              o.QuotaPeriod,
			"period_start":        periodStart,
			"partial":             true,
			"usage":               counts,
			"quota_events_soft":   o.QuotaEventsSoft,
			"quota_events_hard":   o.QuotaEventsHard,
			"quota_messages_soft": o.QuotaMessagesSoft,
			"quota_messages_hard": o.QuotaMessagesHard,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) handleHotDestinations(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Queries.HotDestinations(r.Context())
	if err != nil {
		d.Log.Error("admin hot destinations", "err", err)
		http.Error(w, "hot destinations", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		rate := 0.0
		if row.Total > 0 {
			rate = float64(row.Failed) / float64(row.Total)
		}
		out = append(out, map[string]any{
			"destination_id":   store.GoUUID(row.DestinationID).String(),
			"destination_name": row.DestinationName,
			"total":            row.Total,
			"failed":           row.Failed,
			"failure_rate":     rate,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) handleSystem(w http.ResponseWriter, r *http.Request) {
	info, err := d.Redis.Info(r.Context(), "server", "memory").Result()
	if err != nil {
		info = err.Error()
	}
	// Postgres pool stats from this server process. Worker count is intentionally
	// omitted — workers run as separate processes the API server can't poll.
	st := d.Pool.Stat()
	writeJSON(w, http.StatusOK, map[string]any{
		"version": d.Version,
		"postgres": map[string]any{
			"total_conns":    st.TotalConns(),
			"acquired_conns": st.AcquiredConns(),
			"idle_conns":     st.IdleConns(),
			"max_conns":      st.MaxConns(),
		},
		"redis_info":            info,
		"queue_deliveries_name": "deliveries",
	})
}

func (d Deps) handleQueueItems(w http.ResponseWriter, r *http.Request) {
	lane := r.URL.Query().Get("lane")
	switch lane {
	case "dead", "scheduled", "processing", "pending":
	default:
		http.Error(w, "unknown lane", http.StatusBadRequest)
		return
	}
	org := r.URL.Query().Get("org")
	if lane == "pending" && org == "" {
		http.Error(w, "pending lane requires org", http.StatusBadRequest)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	items, truncated, err := d.Queue.Items(r.Context(), lane, org, limit)
	if err != nil {
		d.Log.Error("admin queue items", "err", err)
		http.Error(w, "items", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "truncated": truncated})
}

func (d Deps) handleQueueOrgs(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Queue.AllOrgPending(r.Context())
	if err != nil {
		d.Log.Error("admin queue orgs", "err", err)
		http.Error(w, "orgs", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	// ponytail: N+1 org-name lookups, uncapped — admin-only + infrequent; batch the
	// name lookup (one WHERE id = ANY query) if org counts grow large.
	for _, row := range rows {
		name := ""
		if id, err := uuid.Parse(row.OrgID); err == nil {
			if o, err := d.Queries.GetOrganizationByID(r.Context(), store.UUID(id)); err == nil {
				name = o.Name
			}
		}
		out = append(out, map[string]any{"org_id": row.OrgID, "org_name": name, "pending": row.Pending})
	}
	writeJSON(w, http.StatusOK, out)
}

type rawReq struct {
	Raw string `json:"raw"`
}

func (d Deps) handleRequeueDead(w http.ResponseWriter, r *http.Request) {
	var body rawReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Raw == "" {
		http.Error(w, "raw required", http.StatusBadRequest)
		return
	}
	ok, err := d.Queue.RequeueDead(r.Context(), body.Raw)
	if err != nil {
		d.Log.Error("admin requeue dead", "err", err)
		http.Error(w, "requeue", http.StatusInternalServerError)
		return
	}
	if ok {
		// Best-effort: flip the event row back to queued so DB agrees with the
		// resurrected queue item. A miss here isn't fatal — the worker's pickup
		// would transition it anyway.
		var p dqueue.Payload
		if json.Unmarshal([]byte(body.Raw), &p) == nil {
			if err := d.Queries.MarkEventQueued(r.Context(), store.UUID(p.EventID)); err != nil {
				d.Log.Warn("admin requeue: mark event queued", "err", err, "event_id", p.EventID)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"requeued": ok})
}

func (d Deps) handlePromoteScheduled(w http.ResponseWriter, r *http.Request) {
	var body rawReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Raw == "" {
		http.Error(w, "raw required", http.StatusBadRequest)
		return
	}
	ok, err := d.Queue.PromoteScheduled(r.Context(), body.Raw)
	if err != nil {
		d.Log.Error("admin promote scheduled", "err", err)
		http.Error(w, "promote", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"promoted": ok})
}

func (d Deps) handleDrainDead(w http.ResponseWriter, r *http.Request) {
	n, err := d.Queue.DrainDead(r.Context())
	if err != nil {
		d.Log.Error("admin drain dead", "err", err)
		http.Error(w, "drain", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"drained": n})
}
