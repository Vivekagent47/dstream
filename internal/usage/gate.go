package usage

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/opevents"
	"github.com/Vivekagent47/dstream/internal/store"
)

// The two metrics that gate a request. The other two (requests, attempts) are
// metered only: requests is subsumed by events, and rejecting an attempt would
// punish a customer for their own endpoint's outage (design §3).
const (
	MetricEvents   = "events"
	MetricMessages = "messages"
)

const (
	// LimitsTTL is how long a loaded quota snapshot is served before a refresh
	// is kicked off. A plan change therefore takes effect within a minute,
	// matching ingest's SourceCacheTTL — the same trade the source cache
	// already makes on the same hot path.
	LimitsTTL = 60 * time.Second

	// RetryAfter is the Retry-After value on a quota 429, in seconds.
	//
	// Deliberately not "seconds until the period rolls over": on a 'month'
	// period that would tell Stripe or GitHub to come back in three weeks. An
	// hour is the honest answer instead, because the thing that most often
	// clears a 429 is not the period boundary — it is the next rollup sweep
	// reconciling an over-counted counter downward, or an operator raising the
	// ceiling.
	RetryAfter = "3600"

	// alertTimeout bounds the off-request-path alert publish, which does
	// several queries plus an enqueue.
	alertTimeout = 10 * time.Second
)

// Limits is one org's quota row: the two enforced pairs and the period they
// are counted over. Zero still means unlimited at every tier, independently —
// but it is no longer the column default; a new org lands on the free preset
// instead (plans.go). Also the shape of a plan preset — see Presets in
// plans.go, which fills this struct in per product tier instead of declaring
// its own.
type Limits struct {
	EventsSoft   int64
	EventsHard   int64
	MessagesSoft int64
	MessagesHard int64
	Period       string
}

// Gate decides and records one request's quota outcome.
//
// It holds the whole org→limits table in memory and refreshes it off the
// request path, so a check costs one Redis INCR and NO database query — which
// is the requirement at ingest, where the gate sits in front of a 5 MiB body
// read. A nil *Gate allows everything, so a handler wired without quotas (any
// test, any deployment that does not configure plans) needs no guard.
type Gate struct {
	Log     *slog.Logger
	Queries *store.Queries
	Redis   *redis.Client
	Queue   *dqueue.Client

	snap       atomic.Pointer[snapshot]
	refreshing atomic.Bool
	alerts     sync.WaitGroup
}

type snapshot struct {
	at     time.Time
	limits map[uuid.UUID]Limits
}

// CheckIngest meters one inbound request against the org's events quota.
func (g *Gate) CheckIngest(ctx context.Context, orgID uuid.UUID) Decision {
	return g.check(ctx, orgID, MetricEvents)
}

// CheckPublish meters one outbound publish against the org's messages quota.
func (g *Gate) CheckPublish(ctx context.Context, orgID uuid.UUID) Decision {
	return g.check(ctx, orgID, MetricMessages)
}

// check increments the live counter and returns the ladder's rung. It never
// rejects on an error of its own: a Redis failure, a missing snapshot and an
// unconfigured org all return Allow.
func (g *Gate) check(ctx context.Context, orgID uuid.UUID, metric string) Decision {
	if g == nil || g.Redis == nil {
		return Allow
	}
	lim := g.limits(orgID)
	soft, hard := lim.EventsSoft, lim.EventsHard
	if metric == MetricMessages {
		soft, hard = lim.MessagesSoft, lim.MessagesHard
	}
	// Zero means unlimited, but it is no longer the common case: free and pro
	// both carry nonzero limits, so most requests take the Incr+alert path
	// below instead. This branch now serves only genuinely-unlimited orgs —
	// enterprise (all four columns 0 by design) or a custom org a super-admin
	// left at 0 — plus a cache miss, which lands here too by returning the
	// zero Limits. It still must cost nothing when it does hit: no Redis
	// round-trip, no alert, no rejection. The rollup sweep still records this
	// org's usage in Postgres, which is where usage reporting reads from
	// anyway.
	if soft == 0 && hard == 0 {
		return Allow
	}
	// The org's OWN period, never a hardcoded "month": the sweep reconciles the
	// counter at PeriodStart(now, org.quota_period), so a gate that keyed by
	// month would read a key nobody reconciles for a 'day' org — and since the
	// increment below counts the requests it then rejects, that key drifts
	// upward only and surfaces as spurious 429s with nothing in the logs.
	periodStart := PeriodStart(time.Now(), lim.Period)
	// n=1 per request, not per resulting event. The gate runs before the body
	// read and before the connection fan-out is known, so one is the only count
	// available without a query; the sweep later replaces this approximation
	// with the real events-row count. Consequence: between sweeps a source with
	// several enabled connections crosses its limit later than its true event
	// volume implies (and one with no enabled connections, earlier).
	count, err := Incr(ctx, g.Redis, orgID, metric, periodStart, 1)
	if err != nil {
		// Fail open, like the ingest rate limiter: refusing webhooks because an
		// internal cache is down is a worse outage than the overage. Logged
		// rather than swallowed, so a Redis outage does not silently disable
		// quota enforcement with zero signal.
		g.Log.WarnContext(ctx, "usage: quota counter unavailable (fail-open)",
			"err", err, "org_id", orgID.String(), "metric", metric)
		return Allow
	}
	dec := Check(count, soft, hard)
	if dec != Allow {
		g.alert(orgID, metric, periodStart, dec, count, soft, hard)
	}
	return dec
}

// alert fires the operational webhook for an over-limit decision, at most once
// per org+metric+period+tier, OFF the request path.
//
// Off-path matters: a publish does several queries and an enqueue, and the
// soft-limit rung still accepts the request — so paying for the alert
// synchronously would add that latency to a request that is otherwise normal.
// Best-effort throughout (panic recovered, every error logged and dropped):
// the response has already been decided by the caller.
func (g *Gate) alert(orgID uuid.UUID, metric string, periodStart time.Time, dec Decision, count, soft, hard int64) {
	if g.Queries == nil || g.Queue == nil {
		return
	}
	eventType, limit := "usage.quota_warning", soft
	if dec == OverHard {
		eventType, limit = "usage.quota_exceeded", hard
	}
	g.alerts.Add(1)
	go func() {
		defer g.alerts.Done()
		defer func() {
			if rec := recover(); rec != nil {
				g.Log.Error("usage: quota alert panic (ignored)", "panic", rec, "org_id", orgID.String())
			}
		}()
		// Background context: the request this came from has already been
		// answered, so its context is cancelled.
		ctx, cancel := context.WithTimeout(context.Background(), alertTimeout)
		defer cancel()

		// Latch per TIER as well as per period: one latch for both would mean
		// an org that passed its soft limit early in the period never got the
		// quota_exceeded alert when it later hit the ceiling — the one alert
		// that explains why its webhooks are being refused.
		latch := "usage:alerted:" + orgID.String() + ":" + metric + ":" +
			strconv.FormatInt(periodStart.Unix(), 10) + ":" + eventType
		first, err := g.Redis.SetNX(ctx, latch, 1, counterTTL).Result()
		if err != nil {
			// No latch, no alert: without it a sustained overage would publish
			// one operational webhook per request and bury the org's own op app.
			g.Log.Warn("usage: quota alert latch unavailable (alert skipped)",
				"err", err, "org_id", orgID.String(), "metric", metric)
			return
		}
		if !first {
			return
		}
		if err := opevents.Publish(ctx, g.Queries, g.Queue, orgID, eventType, map[string]any{
			"metric":       metric,
			"count":        count,
			"limit":        limit,
			"period_start": periodStart.Format(time.RFC3339),
		}); err != nil {
			g.Log.Warn("usage: publish quota alert (ignored)",
				"err", err, "org_id", orgID.String(), "event_type", eventType)
		}
	}()
}

// WaitAlerts blocks until every in-flight alert goroutine has finished. For
// tests, which must observe the alert deterministically; the request path
// never calls it.
func (g *Gate) WaitAlerts() { g.alerts.Wait() }

// limits returns the org's cached quota row, or the zero Limits when no
// snapshot has loaded yet or the org is not in it. Zero means unlimited, so a
// cache miss allows: guessing a limit from a miss is how a deployment that
// configured nothing would start rejecting traffic.
func (g *Gate) limits(orgID uuid.UUID) Limits {
	s := g.snap.Load()
	if s == nil || time.Since(s.at) > LimitsTTL {
		// Asynchronous on purpose: the stale snapshot is served while the
		// reload runs, so no request ever waits on Postgres here.
		g.refresh()
	}
	if s == nil {
		return Limits{}
	}
	return s.limits[orgID]
}

// refresh reloads the snapshot in the background, one reload at a time.
func (g *Gate) refresh() {
	if g.Queries == nil || !g.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer g.refreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := g.Reload(ctx); err != nil {
			// Keep serving the previous snapshot. Enforcement goes stale, never
			// wrong, and the next request retries the reload.
			g.Log.Warn("usage: reload quota limits (serving stale)", "err", err)
		}
	}()
}

// Reload loads every org's limits into the snapshot synchronously. Called by
// the background refresh, once at boot so the first requests are not treated
// as unlimited, and by tests that need the snapshot to be current.
//
// ponytail: one full-table read per TTL per process, because ListOrgQuotas
// already exists and the rows are nine small columns. If org count ever makes
// that snapshot expensive, swap it for a per-org query behind a per-org TTL —
// but keep the load off the request path.
func (g *Gate) Reload(ctx context.Context) error {
	rows, err := g.Queries.ListOrgQuotas(ctx)
	if err != nil {
		return err
	}
	limits := make(map[uuid.UUID]Limits, len(rows))
	for _, r := range rows {
		// An org with no limits at all is left out of the map entirely: the
		// lookup's miss and "all zeros" mean the same thing (unlimited), and
		// most rows are this case.
		if r.QuotaEventsSoft == 0 && r.QuotaEventsHard == 0 &&
			r.QuotaMessagesSoft == 0 && r.QuotaMessagesHard == 0 {
			continue
		}
		limits[store.GoUUID(r.ID)] = Limits{
			EventsSoft:   r.QuotaEventsSoft,
			EventsHard:   r.QuotaEventsHard,
			MessagesSoft: r.QuotaMessagesSoft,
			MessagesHard: r.QuotaMessagesHard,
			Period:       r.QuotaPeriod,
		}
	}
	g.snap.Store(&snapshot{at: time.Now(), limits: limits})
	return nil
}
