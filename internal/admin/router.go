package admin

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
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
		http.Error(w, "queue stats: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (d Deps) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgs, _ := d.Queries.CountOrganizations(ctx)
	users, _ := d.Queries.CountUsers(ctx)

	since := pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true}
	events24h, _ := d.Queries.AdminEventsSince(ctx, since)
	topRows, _ := d.Queries.AdminTopSources(ctx, since)
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
		http.Error(w, "list orgs: "+err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "hot destinations: "+err.Error(), http.StatusInternalServerError)
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
