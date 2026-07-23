package main

import (
	"context"
	"fmt"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/Vivekagent47/dstream/internal/config"
	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/logging"
	"github.com/Vivekagent47/dstream/internal/mailer"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/tracing"
)

func workerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "worker",
		Short: "Run the delivery worker (fair-queue processor)",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			log := logging.New(cfg.LogLevel, cfg.LogFormat)
			log.Info("starting worker", "concurrency", cfg.Worker.Concurrency, "per_org_max_inflight", cfg.Worker.PerOrgMaxInflight, "version", version)

			// ctx cancels on SIGINT/SIGTERM; every loop below watches it and drains.
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

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

			rdb := redis.NewClient(&redis.Options{
				Addr:     cfg.Redis.Addr,
				Password: cfg.Redis.Password,
				DB:       cfg.Redis.DB,
			})
			defer rdb.Close()

			bs := ingest.NewPostgresBodyStore(q)
			dq := dqueue.NewClient(rdb)
			h := deliver.New(log, q, rdb, bs, dq, cfg.AllowPrivateDestinations)
			h.PerOrgMaxInflight = cfg.Worker.PerOrgMaxInflight

			sender, err := mailer.NewSender(cfg.SMTP)
			if err != nil {
				return fmt.Errorf("init mailer: %w", err)
			}
			emailHandler := mailer.EmailHandler{Sender: sender, Log: log, DevMode: cfg.DevMode}

			// 5× the delivery timeout, matching the in-flight lease: long enough
			// that a live delivery never has its lease reclaimed mid-flight, short
			// enough that a crashed worker's events are recovered promptly.
			const leaseMs = int64(150000)

			var wg sync.WaitGroup

			// Worker pool: each goroutine fair-picks one event round-robin across
			// orgs and processes it. On an empty ring it blocks on WaitNotify so it
			// wakes the moment an event is enqueued/promoted rather than busy-polling.
			for i := 0; i < cfg.Worker.Concurrency; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for ctx.Err() == nil {
						raw, p, ok, err := dq.FairPick(ctx, leaseMs)
						if err != nil {
							if ctx.Err() != nil {
								return
							}
							log.Error("fairpick", "err", err)
							time.Sleep(200 * time.Millisecond)
							continue
						}
						if !ok {
							_ = dq.WaitNotify(ctx, 2*time.Second)
							continue
						}
						// Process one event in its own func so span.End runs on every
						// path and a panic in delivery is contained to this event
						// instead of killing the whole worker process.
						func() {
							if p.Kind == "email" {
								defer func() {
									if rec := recover(); rec != nil {
										log.Error("email process panic", "panic", rec)
										// No events row for email; just terminate the task.
										_ = dq.DeadLetter(context.Background(), raw)
									}
								}()
								if err := emailHandler.Process(ctx, p, raw, dq); err != nil {
									log.Error("email process", "err", err)
								}
								return
							}
							dctx := otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(p.Trace))
							dctx, span := otel.Tracer("dstream/deliver").Start(dctx, "deliver")
							defer span.End()
							defer func() {
								if rec := recover(); rec != nil {
									log.Error("process panic", "event_id", p.EventID, "panic", rec)
									// Terminate the poisoned event so neither the queue
									// recoverer nor the DB reaper re-injects it into an
									// endless panic loop (audit #5). Background ctx: the
									// delivery ctx may already be cancelled.
									bg := context.Background()
									_ = dq.DeadLetter(bg, raw)
									_ = q.MarkEventFailed(bg, store.UUID(p.EventID))
								}
							}()
							if perr := h.Process(dctx, p, raw); perr != nil {
								log.Error("process", "event_id", p.EventID, "err", perr)
							}
						}()
					}
				}()
			}

			// Scheduler mover: promote due scheduled retries/deferrals into the
			// pending ring. Recoverer: reinject events whose lease expired (crashed
			// worker) so at-least-once holds.
			wg.Add(2)
			go func() {
				defer wg.Done()
				tick(ctx, time.Second, func() { _, _ = dq.PromoteDue(ctx, time.Now().UnixMilli(), 500) })
			}()
			go func() {
				defer wg.Done()
				tick(ctx, 30*time.Second, func() { _, _ = dq.Recover(ctx, time.Now().UnixMilli()) })
			}()

			// DB-level safety net: re-queue events stuck with NO queue entry — an
			// ingest enqueue that failed after the row was written, or a CLI tunnel
			// that died mid-handoff. The queue recoverer only sees events already in
			// dq:processing, so it cannot cover these; the reaper claims them from
			// Postgres and re-enqueues.
			wg.Add(1)
			go func() { defer wg.Done(); h.RunReaper(ctx) }()

			// Background maintenance: purge expired magic-link tokens + invites.
			wg.Add(1)
			go func() { defer wg.Done(); runMaintenance(ctx, q, log) }()

			<-ctx.Done()
			log.Info("shutting down worker")
			wg.Wait()
			return nil
		},
	}
}

// tick runs fn every d until ctx is cancelled.
func tick(ctx context.Context, d time.Duration, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}
