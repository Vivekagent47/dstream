package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/store"
)

const (
	maintenanceInterval = 1 * time.Hour
	// Keep expired rows this long before purging — a small debug window — then
	// reclaim. Magic-link tokens expire in minutes; invites in a few days.
	expiredRetention = 24 * time.Hour
)

// runMaintenance periodically purges expired magic-link tokens and org invites
// so those tables don't grow without bound (they are insert-per-login /
// insert-per-invite and were never cleaned up). Runs in the worker; the DELETEs
// are safe across replicas. Stops when ctx is cancelled.
func runMaintenance(ctx context.Context, q *store.Queries, log *slog.Logger) {
	sweep := func() {
		cutoff := pgtype.Timestamptz{Time: time.Now().Add(-expiredRetention), Valid: true}
		if n, err := q.DeleteExpiredMagicLinkTokens(ctx, cutoff); err != nil {
			log.Error("maintenance: purge magic-link tokens", "err", err)
		} else if n > 0 {
			log.Info("maintenance: purged expired magic-link tokens", "count", n)
		}
		if n, err := q.DeleteExpiredOrgInvites(ctx, cutoff); err != nil {
			log.Error("maintenance: purge org invites", "err", err)
		} else if n > 0 {
			log.Info("maintenance: purged expired org invites", "count", n)
		}
	}
	sweep() // once at startup, then on the interval
	t := time.NewTicker(maintenanceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}
