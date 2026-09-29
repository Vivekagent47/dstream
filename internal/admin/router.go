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
