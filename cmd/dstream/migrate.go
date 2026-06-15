package main

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spf13/cobra"

	"github.com/streamingo/dstream/internal/config"
	"github.com/streamingo/dstream/internal/logging"
	"github.com/streamingo/dstream/internal/migrations"
)

func migrateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "migrate",
		Short: "Database migrations (up/down/status)",
	}

	c.AddCommand(
		&cobra.Command{Use: "up", Short: "Apply all pending migrations", RunE: runMigrate("up")},
		&cobra.Command{Use: "down", Short: "Rollback last migration", RunE: runMigrate("down")},
		&cobra.Command{Use: "status", Short: "Show migration status", RunE: runMigrate("status")},
		&cobra.Command{Use: "reset", Short: "Rollback all migrations (DANGER)", RunE: runMigrate("reset")},
	)

	c.RunE = runMigrate("up")
	return c
}

func runMigrate(direction string) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, _ []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		log := logging.New(cfg.LogLevel, cfg.LogFormat)

		db, err := sql.Open("pgx", cfg.DB.URL)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer db.Close()

		if err := db.PingContext(context.Background()); err != nil {
			return fmt.Errorf("ping db: %w", err)
		}

		goose.SetBaseFS(migrations.FS)
		goose.SetLogger(gooseLogger{log: log})
		if err := goose.SetDialect("postgres"); err != nil {
			return fmt.Errorf("set dialect: %w", err)
		}

		switch direction {
		case "up":
			return goose.Up(db, ".")
		case "down":
			return goose.Down(db, ".")
		case "status":
			return goose.Status(db, ".")
		case "reset":
			return goose.Reset(db, ".")
		default:
			return fmt.Errorf("unknown direction: %s", direction)
		}
	}
}

type gooseLogger struct {
	log interface {
		Info(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

func (g gooseLogger) Fatalf(format string, v ...interface{}) {
	g.log.Error(fmt.Sprintf(format, v...))
}
func (g gooseLogger) Printf(format string, v ...interface{}) {
	g.log.Info(fmt.Sprintf(format, v...))
}
