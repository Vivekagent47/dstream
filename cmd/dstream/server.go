package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/Vivekagent47/dstream/internal/admin"
	"github.com/Vivekagent47/dstream/internal/api"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/config"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/logging"
	"github.com/Vivekagent47/dstream/internal/metrics"
	mw "github.com/Vivekagent47/dstream/internal/middleware"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/tracing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// isLocalBaseURL reports whether raw points at a loopback host, used to allow
// the insecure-cookie dev opt-out only for local origins.
func isLocalBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		strings.HasSuffix(host, ".localhost")
}

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
			// Refuse to issue session cookies without the Secure attribute over a
			// non-local origin — that would ship the 30-day session token in
			// cleartext on any plaintext hop. Local HTTP dev may opt out.
			if !cfg.CookieSecure && !isLocalBaseURL(cfg.PublicBaseURL) {
				return errors.New("DSTREAM_COOKIE_SECURE must be true when DSTREAM_PUBLIC_BASE_URL is not localhost")
			}
			// DevMode logs plaintext magic-link tokens — an auth-bypass vector if
			// left on in production. Refuse to boot outside localhost.
			if cfg.DevMode && !isLocalBaseURL(cfg.PublicBaseURL) {
				return errors.New("DSTREAM_DEV_MODE must be false when DSTREAM_PUBLIC_BASE_URL is not localhost (it logs plaintext magic-link tokens)")
			}
			log := logging.New(cfg.LogLevel, cfg.LogFormat)
			log.Info("starting server", "addr", cfg.HTTPAddr, "version", version)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			tshutdown, err := tracing.Init(ctx, tracing.Config{
				Enabled:      cfg.Tracing.Enabled,
				OTLPEndpoint: cfg.Tracing.OTLPEndpoint,
				ServiceName:  cfg.Tracing.ServiceName,
				SampleRatio:  cfg.Tracing.SampleRatio,
			})
			if err != nil {
				return err
			}
			defer func() { _ = tshutdown(context.Background()) }()
			if cfg.Tracing.Enabled {
				log.Info("tracing enabled", "otlp_endpoint", cfg.Tracing.OTLPEndpoint, "sample_ratio", cfg.Tracing.SampleRatio)
			}

			pool, err := store.NewPool(ctx, cfg.DB.URL, cfg.DB.MaxConns)
			if err != nil {
				return err
			}
			defer pool.Close()
			q := store.New(pool)
			metrics.Reg.MustRegister(metrics.NewCollector(q, log))

			rdb := redis.NewClient(&redis.Options{
				Addr:     cfg.Redis.Addr,
				Password: cfg.Redis.Password,
				DB:       cfg.Redis.DB,
			})
			defer rdb.Close()

			dq := dqueue.NewClient(rdb)

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
			r.Use(metrics.HTTPMiddleware)
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
			r.With(auth.SuperAdminOnly(q, signer)).Handle("/metrics", metrics.Handler())

			ih := &ingest.Handler{
				Log:            log,
				Queries:        q,
				Redis:          rdb,
				Queue:          dq,
				BodyStore:      bodyStore,
				Limiter:        redis_rate.NewLimiter(rdb),
				RateLimitRPS:   cfg.IngestRateLimitRPS,
				RateLimitBurst: cfg.IngestRateLimitBurst,
			}
			ih.Mount(r)

			api.Mount(r, api.Deps{
				Log:              log,
				Queries:          q,
				Pool:             pool,
				Redis:            rdb,
				Queue:            dq,
				BodyStore:        bodyStore,
				Signer:           signer,
				PublicBaseURL:    cfg.PublicBaseURL,
				DevMode:          cfg.DevMode,
				EvictSourceCache: ih.InvalidateSource,
			}, mw.CSRF(cfg.CookieSecure))

			admin.Mount(r, admin.Deps{
				Log:     log,
				Queries: q,
				Redis:   rdb,
				Signer:  signer,
				Queue:   dq,
			})

			// TODO(phase-1.4 follow-up): mount /web/* dashboard.

			srv := &http.Server{
				Addr:              cfg.HTTPAddr,
				Handler:           otelhttp.NewHandler(r, "http.server"),
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
