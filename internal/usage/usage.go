// Package usage holds the live quota counter and the quota decision.
//
// Two mechanisms meter usage and they are not interchangeable. Postgres
// (usage_rollups) is the authoritative, lagging record — it is what the API,
// the dashboard and any future biller read. Redis, which is all this package
// touches, is a cache of the *quota decision* for the hot path, where a
// COUNT(*) per request is not affordable. The maintenance sweep writes the
// Postgres count back over the Redis counter each interval, so an eviction or
// drift self-heals; nothing here is ever a billing record.
package usage

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// counterTTL bounds an abandoned counter instead of letting dead org/period
// keys accumulate forever. It is two of the longest quota period ("month"),
// since Incr is given a period start, not the period unit; for a "day" org
// that is simply more slack than the two days the design calls for, which is
// harmless because the sweep rewrites the live key every interval anyway.
const counterTTL = 62 * 24 * time.Hour

// Decision is the outcome of a quota check for one metric in one period.
type Decision int

const (
	// Allow — under every configured limit, or no limit configured.
	Allow Decision = iota
	// OverSoft — at or above the plan limit. The request is still processed;
	// the caller warns once per period.
	OverSoft
	// OverHard — at or above the ceiling. The caller rejects with 429.
	OverHard
)

func (d Decision) String() string {
	switch d {
	case OverSoft:
		return "over_soft"
	case OverHard:
		return "over_hard"
	case Allow:
		return "allow"
	default:
		// Never render an unknown tier as "allow" — a missed switch arm in a
		// later tier would make the log actively lie about a rejection.
		return "decision(" + strconv.Itoa(int(d)) + ")"
	}
}

// Check turns a count and the org's two limits into a decision.
//
// A limit of 0 means unlimited, at both tiers independently — but 0 is no
// longer the default for every quota column; a new org lands on the free
// preset instead (internal/usage/plans.go). Each comparison is still guarded
// by "limit > 0" first: a bare `count >= limit` would otherwise reject every
// request for a plan left at zero on purpose (enterprise, or custom).
// Consequently an org with a soft limit and no ceiling can never escalate to
// OverHard however far over it runs.
func Check(count, soft, hard int64) Decision {
	if hard > 0 && count >= hard {
		return OverHard
	}
	if soft > 0 && count >= soft {
		return OverSoft
	}
	return Allow
}

// PeriodStart truncates now to the start of its quota period, in UTC.
//
// UTC is load-bearing, not cosmetic: the rollup sweep buckets rows with SQL
// date_trunc over a pool that pins timezone=UTC on every connection
// (internal/store/pool.go), and reconciliation writes that bucket's count
// into the key this function dates. If the two disagreed by even an hour,
// enforcement would read a counter nobody reconciles.
//
// period comes from organizations.quota_period, CHECK-constrained to
// 'day'|'month'; anything else falls back to the column default, 'month'.
func PeriodStart(now time.Time, period string) time.Time {
	t := now.UTC()
	if period == "day" {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// CounterKey is usage:{org}:{metric}:{unix of periodStart}. The shape is
// fixed: the sweep reconciles against exactly this key, and a mismatch would
// silently mean enforcement never sees the authoritative count.
func CounterKey(orgID uuid.UUID, metric string, periodStart time.Time) string {
	return "usage:" + orgID.String() + ":" + metric + ":" + strconv.FormatInt(periodStart.Unix(), 10)
}

// Incr adds n to the counter and returns the new count.
//
// On any Redis error it returns a count of 0 so a caller that cannot read the
// counter allows the request: Check(0, …) is always Allow. That mirrors the
// ingest rate limiter, which already fails open on the grounds that refusing
// webhooks because an internal cache is down is a worse outage than the
// overage. Callers log the error; they must not reject on it.
func Incr(ctx context.Context, rdb *redis.Client, orgID uuid.UUID, metric string, periodStart time.Time, n int64) (int64, error) {
	key := CounterKey(orgID, metric, periodStart)
	pipe := rdb.TxPipeline()
	incr := pipe.IncrBy(ctx, key, n)
	pipe.Expire(ctx, key, counterTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}

// SetCounter overwrites the counter with the authoritative Postgres count.
//
// It returns the error rather than swallowing it: a persistently failing SET
// (maxmemory with noeviction, a read-only replica, a tight context deadline)
// means reconciliation silently never happens while Incr keeps counting on
// the hot path — and since Incr counts requests it then rejects, the
// unreconciled counter drifts upward only. The symptom is spurious 429s with
// nothing in the logs. The caller logs; it must not abort the sweep, since
// the next interval reconciles again.
func SetCounter(ctx context.Context, rdb *redis.Client, orgID uuid.UUID, metric string, periodStart time.Time, v int64) error {
	return rdb.Set(ctx, CounterKey(orgID, metric, periodStart), v, counterTTL).Err()
}
