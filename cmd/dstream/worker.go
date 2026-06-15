package main

import (
	"context"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/streamingo/dstream/internal/config"
	"github.com/streamingo/dstream/internal/deliver"
	"github.com/streamingo/dstream/internal/ingest"
	"github.com/streamingo/dstream/internal/logging"
	"github.com/streamingo/dstream/internal/queue"
	"github.com/streamingo/dstream/internal/store"
)

func workerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "worker",
		Short: "Run the delivery worker (asynq task processor)",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			log := logging.New(cfg.LogLevel, cfg.LogFormat)
			log.Info("starting worker", "concurrency", cfg.Worker.Concurrency, "version", version)

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

			bs := ingest.NewPostgresBodyStore(q)
			h := deliver.New(log, q, rdb, bs, qc)

			redisOpt := asynq.RedisClientOpt{
				Addr:     cfg.Redis.Addr,
				Password: cfg.Redis.Password,
				DB:       cfg.Redis.DB,
			}

			srv := asynq.NewServer(redisOpt, asynq.Config{
				Concurrency:    cfg.Worker.Concurrency,
				RetryDelayFunc: h.RetryDelayFunc(),
				Queues: map[string]int{
					queue.QueueDeliveries: 10,
					queue.QueueDefault:    1,
				},
			})

			mux := asynq.NewServeMux()
			h.Register(mux)

			return srv.Run(mux)
		},
	}
}
