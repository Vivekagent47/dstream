package admin

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/hibiken/asynq"
	"github.com/hibiken/asynqmon"
	"github.com/redis/go-redis/v9"

	"github.com/streamingo/dstream/internal/auth"
	"github.com/streamingo/dstream/internal/queue"
	"github.com/streamingo/dstream/internal/store"
)

type Deps struct {
	Log     *slog.Logger
	Queries *store.Queries
	Redis   *redis.Client
	Signer  *auth.SessionSigner
	Asynq   asynq.RedisConnOpt
}

// Mount wires the /admin/* routes onto the parent. All routes here are gated
// behind super-admin session auth.
func Mount(parent chi.Router, d Deps) {
	mon := asynqmon.New(asynqmon.Options{
		RootPath:     "/admin/queues",
		RedisConnOpt: d.Asynq,
	})

	parent.Route("/admin", func(r chi.Router) {
		r.Use(auth.SuperAdminOnly(d.Queries, d.Signer))

		// asynqmon — Sidekiq/BullMQ-equivalent queue UI.
		r.Mount("/queues", mon)

		// Custom admin pages (Phase 1.4 scope).
		r.Get("/overview", d.handleOverview)
		r.Get("/orgs", d.handleListOrgs)
		r.Get("/destinations/hot", d.handleHotDestinations)
		r.Get("/system", d.handleSystem)
	})
}

func (d Deps) handleOverview(w http.ResponseWriter, r *http.Request) {
	// TODO(phase-1.4 follow-up): real cross-tenant metrics. For now: row counts.
	ctx := r.Context()
	orgs, _ := d.Queries.CountOrganizations(ctx)
	users, _ := d.Queries.CountUsers(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"organizations": orgs,
		"users":         users,
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

func (d Deps) handleHotDestinations(w http.ResponseWriter, _ *http.Request) {
	// TODO(phase-1.4 follow-up): real failure-rate + rate-limit-breach query.
	writeJSON(w, http.StatusOK, []any{})
}

func (d Deps) handleSystem(w http.ResponseWriter, r *http.Request) {
	info, err := d.Redis.Info(r.Context(), "server", "memory").Result()
	if err != nil {
		info = err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"redis_info":     info,
		"queue_deliveries_name": queue.QueueDeliveries,
	})
}
