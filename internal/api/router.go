package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"github.com/streamingo/dstream/internal/auth"
	"github.com/streamingo/dstream/internal/queue"
	"github.com/streamingo/dstream/internal/store"
)

// Deps bundles everything an API handler might need so we can wire them via
// a single struct instead of an explosion of constructor arguments.
type Deps struct {
	Log     *slog.Logger
	Queries *store.Queries
	Redis   *redis.Client
	Queue   *queue.Client
	Signer  *auth.SessionSigner
}

// Mount wires the full /api router onto the parent.
func Mount(parent chi.Router, d Deps) {
	parent.Route("/api", func(r chi.Router) {
		// Unauthenticated.
		r.Route("/auth", func(r chi.Router) {
			r.Post("/magic-link/request", d.requestMagicLink)
			r.Get("/magic-link/verify", d.verifyMagicLink)
			r.Post("/logout", d.logout)
		})

		// API-key authenticated control plane.
		r.Group(func(r chi.Router) {
			r.Use(auth.APIKeyOrSession(d.Queries, d.Signer))

			r.Get("/me", d.me)

			r.Route("/sources", func(r chi.Router) {
				r.Get("/", d.listSources)
				r.Post("/", d.createSource)
				r.Get("/{id}", d.getSource)
			})

			r.Route("/destinations", func(r chi.Router) {
				r.Get("/", d.listDestinations)
				r.Post("/", d.createDestination)
				r.Get("/{id}", d.getDestination)
				r.Patch("/{id}", d.patchDestination)
			})

			r.Route("/connections", func(r chi.Router) {
				r.Get("/", d.listConnections)
				r.Post("/", d.createConnection)
				r.Get("/{id}", d.getConnection)
				r.Patch("/{id}", d.patchConnection)
			})

			r.Route("/events", func(r chi.Router) {
				r.Get("/", d.listEvents)
				r.Get("/{id}", d.getEvent)
				r.Post("/{id}/retry", d.retryEvent)
			})

			// CLI control-plane lookups + WS tunnel registration.
			r.Get("/cli/sources", d.cliListSources)
			r.Get("/cli/connect", d.cliConnect)
		})
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
