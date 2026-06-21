package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/hibiken/asynq"

	"github.com/Vivekagent47/dstream/internal/admin"
	"github.com/Vivekagent47/dstream/internal/api"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/config"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/logging"
	mw "github.com/Vivekagent47/dstream/internal/middleware"
	"github.com/Vivekagent47/dstream/internal/queue"
	"github.com/Vivekagent47/dstream/internal/store"
)

func serverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "Run the HTTP API + dashboard server",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if len(cfg.SessionSecret) < 32 {
				return errors.New("DSTREAM_SESSION_SECRET must be at least 32 bytes")
			}
			log := logging.New(cfg.LogLevel, cfg.LogFormat)
			log.Info("starting server", "addr", cfg.HTTPAddr, "version", version)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			pool, err := store.NewPool(ctx, cfg.DB.URL, cfg.DB.MaxConns)
			if err != nil {
				return err
			}
			defer pool.Close()
			q := store.New(pool)

			rdb := redis.NewClient(&redis.Options{
				Addr:     cfg.Redis.Addr,
				Password: cfg.Redis.Password,
				DB:       cfg.Redis.DB,
			})
			defer rdb.Close()

			qc := queue.NewClient(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
			defer qc.Close()

			signer := &auth.SessionSigner{
				Secret: []byte(cfg.SessionSecret),
				Secure: cfg.CookieSecure,
			}
			bodyStore := ingest.NewPostgresBodyStore(q)

			realIP, err := mw.TrustedRealIP(cfg.TrustedProxies)
			if err != nil {
				return err
			}

			r := chi.NewRouter()
			r.Use(middleware.RequestID)
			r.Use(realIP)
			r.Use(middleware.Recoverer)
			// 30s deadline applies to every request EXCEPT long-lived
			// websockets — those handlers detach from r.Context() onto a
			// fresh background context (see internal/api/cli.go).
			r.Use(middleware.Timeout(30 * time.Second))

			r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			})
			r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ready"))
			})

			ih := &ingest.Handler{
				Log:       log,
				Queries:   q,
				Redis:     rdb,
				Queue:     qc,
				BodyStore: bodyStore,
			}
			ih.Mount(r)

			api.Mount(r, api.Deps{
				Log:           log,
				Queries:       q,
				Pool:          pool,
				Redis:         rdb,
				Queue:         qc,
				Signer:        signer,
				PublicBaseURL: cfg.PublicBaseURL,
				DevMode:       cfg.DevMode,
			}, mw.CSRF(cfg.CookieSecure))

			admin.Mount(r, admin.Deps{
				Log:     log,
				Queries: q,
				Redis:   rdb,
				Signer:  signer,
				Asynq: asynq.RedisClientOpt{
					Addr:     cfg.Redis.Addr,
					Password: cfg.Redis.Password,
					DB:       cfg.Redis.DB,
				},
			})

			// TODO(phase-1.4 follow-up): mount /web/* dashboard.

			srv := &http.Server{
				Addr:              cfg.HTTPAddr,
				Handler:           r,
				ReadHeaderTimeout: 10 * time.Second,
			}

			errCh := make(chan error, 1)
			go func() {
				if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					errCh <- err
				}
			}()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			select {
			case err := <-errCh:
				return err
			case <-sigCh:
				log.Info("shutting down server")
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer shutdownCancel()
				return srv.Shutdown(shutdownCtx)
			}
		},
	}
}
