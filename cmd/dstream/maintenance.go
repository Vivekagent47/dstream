package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"
)

const (
	maintenanceInterval = 1 * time.Hour
	// Keep expired rows this long before purging — a small debug window — then
	// reclaim. Magic-link tokens expire in minutes; invites in a few days.
	expiredRetention = 24 * time.Hour
)

// runMaintenance periodically purges expired magic-link tokens and org invites
// so those tables don't grow without bound (they are insert-per-login /
// insert-per-invite and were never cleaned up), and rolls per-org usage up into
// usage_rollups. Runs in the worker; the DELETEs are safe across replicas, and
// the rollup converges rather than accumulating, so running it on several
// replicas at once is safe too. Sweeps every `every` (maintenanceInterval in
// production). Stops when ctx is cancelled.
//
// rdb may be nil (no Redis configured); the Postgres rollup still runs.
func runMaintenance(ctx context.Context, q *store.Queries, rdb *redis.Client, log *slog.Logger, retention, every time.Duration) {
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
		// Payload retention: null out message payloads + attempt response bodies
		// past the window. 0 = keep forever.
		if retention > 0 {
			cut := pgtype.Timestamptz{Time: time.Now().Add(-retention), Valid: true}
			if n, err := q.ExpireOldMessagePayloads(ctx, cut); err != nil {
				log.Error("maintenance: expire payloads", "err", err)
			} else if n > 0 {
				log.Info("maintenance: expired message payloads", "count", n)
			}
			if n, err := q.ExpireOldAttemptBodies(ctx, cut); err != nil {
				log.Error("maintenance: expire attempt bodies", "err", err)
			} else if n > 0 {
				log.Info("maintenance: expired attempt bodies", "count", n)
			}
			// Inbound: request payloads + inbound delivery attempt bodies (the bulk
			// of ingress storage). An expunged request body reads back as NULL →
			// GetRequestBody yields no row → the delivery worker's missing-body path.
			if n, err := q.ExpireOldRequestBodies(ctx, cut); err != nil {
				log.Error("maintenance: expire request bodies", "err", err)
			} else if n > 0 {
				log.Info("maintenance: expired request bodies", "count", n)
			}
			if n, err := q.ExpireOldInboundAttemptBodies(ctx, cut); err != nil {
				log.Error("maintenance: expire inbound attempt bodies", "err", err)
			} else if n > 0 {
				log.Info("maintenance: expired inbound attempt bodies", "count", n)
			}
		}
		rollupUsage(ctx, q, rdb, log)
	}
	sweep() // once at startup, then on the interval
	t := time.NewTicker(every)
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

// usageRow is the one shape the four Rollup* result types are normalised to.
// They are structurally identical but distinct Go types, and Go has no
// structural typing, so each query gets a three-line adapter below.
type usageRow struct {
	OrgID       pgtype.UUID
	PeriodStart pgtype.Timestamptz
	Count       int64
}

// rollupUsage recomputes the CURRENT period's usage for every org from the
// authoritative tables and upserts it into usage_rollups, then writes each
// total back into the Redis counter the hot path reads.
//
// Recompute-and-upsert rather than increment: the sweep re-runs the current
// period on every worker restart, and across replicas, so the only safe shape
// is one that converges on the true count. That also makes the Redis counter
// self-healing — an eviction or a drift is corrected within one interval,
// which is why Redis can be a cache of the decision without ever being the
// billing record.
//
// Orgs are grouped by organizations.quota_period because the counter the hot
// path reads is keyed by PeriodStart(now, that org's period). A sweep that
// always bucketed by month would write a key a 'day' org never reads, so its
// counter would never be reconciled — and since Incr counts the requests it
// then rejects, that drifts upward only and surfaces as spurious 429s. In the
// common case every org is on the default 'month' and this is a single pass.
//
// rdb may be nil (no Redis configured); the Postgres rollup still runs.
func rollupUsage(ctx context.Context, q *store.Queries, rdb *redis.Client, log *slog.Logger) {
	orgs, err := q.ListOrgQuotas(ctx)
	if err != nil {
		log.Error("maintenance: list org quotas", "err", err)
		return
	}
	byPeriod := map[string]map[uuid.UUID]struct{}{}
	for _, o := range orgs {
		p := o.QuotaPeriod
		if p != "day" {
			p = "month" // the column default, and the CHECK allows nothing else
		}
		if byPeriod[p] == nil {
			byPeriod[p] = map[uuid.UUID]struct{}{}
		}
		byPeriod[p][store.GoUUID(o.ID)] = struct{}{}
	}
	now := time.Now()
	for period, members := range byPeriod {
		rollupPeriod(ctx, q, rdb, log, period, usage.PeriodStart(now, period), members)
	}
}

// rollupPeriod rolls one quota period's orgs up for every metric.
//
// after is the START of the current period and nothing earlier, deliberately.
// The Rollup* queries filter `created_at >= after` with no upper bound and
// UpsertUsageRollup REPLACES count rather than adding to it, so any already
// closed period this sweep re-read would be rewritten from whatever live rows
// still exist — and events.connection_id and requests.source_id are both
// ON DELETE CASCADE. Deleting one connection or source would therefore
// silently revise a closed period's billing record downward. (§8 of the design
// makes the same argument against a historical backfill: payload retention and
// row deletion mean the past cannot be reconstructed, only recorded as it
// happens.) A multi-period catch-up would need an explicit "do not touch
// periods older than X" guard; there is no such need today, so there is no
// such guard.
func rollupPeriod(ctx context.Context, q *store.Queries, rdb *redis.Client, log *slog.Logger,
	period string, after time.Time, members map[uuid.UUID]struct{}) {

	afterTS := pgtype.Timestamptz{Time: after, Valid: true}
	rollups := []struct {
		metric string
		rows   func() ([]usageRow, error)
	}{
		{"events", func() ([]usageRow, error) {
			rs, err := q.RollupEvents(ctx, store.RollupEventsParams{Bucket: period, After: afterTS})
			out := make([]usageRow, len(rs))
			for i, r := range rs {
				out[i] = usageRow{r.OrgID, r.PeriodStart, r.Count}
			}
			return out, err
		}},
		{"requests", func() ([]usageRow, error) {
			rs, err := q.RollupRequests(ctx, store.RollupRequestsParams{Bucket: period, After: afterTS})
			out := make([]usageRow, len(rs))
			for i, r := range rs {
				out[i] = usageRow{r.OrgID, r.PeriodStart, r.Count}
			}
			return out, err
		}},
		{"messages", func() ([]usageRow, error) {
			rs, err := q.RollupMessages(ctx, store.RollupMessagesParams{Bucket: period, After: afterTS})
			out := make([]usageRow, len(rs))
			for i, r := range rs {
				out[i] = usageRow{r.OrgID, r.PeriodStart, r.Count}
			}
			return out, err
		}},
		{"attempts", func() ([]usageRow, error) {
			rs, err := q.RollupAttempts(ctx, store.RollupAttemptsParams{Bucket: period, After: afterTS})
			out := make([]usageRow, len(rs))
			for i, r := range rs {
				out[i] = usageRow{r.OrgID, r.PeriodStart, r.Count}
			}
			return out, err
		}},
	}

	// Convergence here is downward-only to a NON-ZERO value, which is the one
	// way this sweep is not a full recompute. GROUP BY returns no row at all for
	// an org with no qualifying rows, so a count that falls to zero inside a
	// period — every connection cascade-deleted, or a period whose remaining
	// events are all is_test — produces nothing to upsert, and the previous
	// usage_rollups value and Redis counter both stay as they were until the
	// period rolls over and the org simply starts fresh in a new bucket. Bounded
	// and self-correcting, so it is left alone; fixing it would mean enumerating
	// every (org, metric) pair rather than only the ones with traffic.
	for _, r := range rollups {
		rows, err := r.rows()
		if err != nil {
			log.Error("maintenance: rollup query", "metric", r.metric, "period", period, "err", err)
			continue // one bad metric must not abort the others
		}
		var considered, bucketSkipped int
		for _, row := range rows {
			orgID := store.GoUUID(row.OrgID)
			// The queries are not org-scoped, so skip orgs on a different quota
			// period (their rows are handled by their own pass) and orgs that
			// vanished since ListOrgQuotas — upserting those would fail the FK.
			if _, ok := members[orgID]; !ok {
				continue
			}
			considered++
			// Belt and braces over the `>= after` filter: only the bucket this
			// pass is recomputing is ever written. If the month rolled over
			// mid-sweep, the newer bucket is dropped and the next sweep picks it
			// up — recompute converges, so nothing is lost.
			if !row.PeriodStart.Time.Equal(after) {
				bucketSkipped++
				continue
			}
			if err := q.UpsertUsageRollup(ctx, store.UpsertUsageRollupParams{
				OrgID:       row.OrgID,
				PeriodStart: row.PeriodStart,
				Metric:      r.metric,
				Count:       row.Count,
			}); err != nil {
				log.Error("maintenance: upsert rollup", "metric", r.metric, "org_id", orgID, "err", err)
				continue
			}
			if rdb == nil {
				continue
			}
			// Reconcile the hot-path counter to the authoritative value. A
			// failure is logged and the sweep continues: the next interval
			// reconciles again, and silence here would mean the counter drifts
			// upward forever with nothing in the logs behind the 429s.
			if err := usage.SetCounter(ctx, rdb, orgID, r.metric, after, row.Count); err != nil {
				log.Warn("maintenance: reconcile usage counter", "metric", r.metric, "org_id", orgID, "err", err)
			}
		}
		// Tripwire. Dropping SOME rows is normal (a bucket rolled over
		// mid-sweep). Dropping EVERY row means Go and SQL disagree about where
		// the period starts — and then the sweep writes nothing, which without
		// this line it would also say nothing about. A silent total no-op is the
		// same unobservable failure that made SetCounter return its error:
		// usage would quietly stop being recorded with no symptom until someone
		// was wrongly billed. Unreachable while internal/store/pool.go pins
		// timezone=UTC on every connection, which is precisely why it is a
		// tripwire and not an assumption.
		if considered > 0 && bucketSkipped == considered {
			log.Warn("maintenance: rollup wrote nothing; every bucket missed the period start",
				"metric", r.metric, "period", period, "after", after, "rows", considered)
		}
	}
}
