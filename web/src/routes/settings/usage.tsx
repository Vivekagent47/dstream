import { createFileRoute, Navigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'

import { api, qk } from '#/lib/api'
import { quotaState } from '#/lib/quota'
import { PageHeader } from '#/components/TopBar'
import { Badge } from '#/components/ui/badge'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '#/components/ui/card'

export const Route = createFileRoute('/settings/usage')({ component: UsagePage })

// The two gating metrics (design §3/§6) — shown as a meter against their
// soft/hard limits. requests and attempts are metered but never enforced, so
// they get a plain count instead.
const GATED_METRICS = [
  {
    key: 'events',
    label: 'Events',
    note: 'Inbound fan-out. The primary billable unit, and the one gate enforces.',
  },
  { key: 'messages', label: 'Messages', note: 'Outbound messages published to applications.' },
] as const

const COUNTED_ONLY_METRICS = [
  {
    key: 'requests',
    label: 'Requests',
    note: 'Inbound HTTP requests. Not separately limited — gating events already gates the request that produced them.',
  },
  {
    key: 'attempts',
    label: 'Delivery attempts',
    note: 'Every attempt including retries. Never enforced — rejecting a retry would punish you for your own endpoint being down.',
  },
] as const

function UsagePage() {
  const { data, error, isLoading } = useQuery({
    queryKey: qk.usage(),
    queryFn: api.getUsage,
    retry: false,
  })

  if (error) return <Navigate to="/" />

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title="Usage"
        help="What this org has used this period, against its plan limits. Limits are set by the platform operator, not here. Only the hard ceiling ever rejects a request — the soft limit accepts and warns, because a sender that doesn't retry loses the webhook for good."
      />
      <div className="flex-1 overflow-y-auto px-6 py-8">
        <div className="mx-auto max-w-3xl space-y-6">
          <Card>
            <CardHeader>
              <div className="flex items-start justify-between gap-3">
                <div>
                  <CardTitle>This period</CardTitle>
                  <CardDescription>
                    Plan <span className="font-medium text-foreground">{data?.plan ?? '…'}</span>
                    {' · resets every '}
                    {data?.period ?? '…'}
                  </CardDescription>
                </div>
                {data?.partial && (
                  <Badge
                    variant="outline"
                    title="This period is still accumulating; the count will keep rising until it ends."
                  >
                    Partial period
                  </Badge>
                )}
              </div>
            </CardHeader>
            <CardContent className="space-y-5">
              {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
              {data && (
                <>
                  {GATED_METRICS.map((m) => (
                    <UsageMeter
                      key={m.key}
                      label={m.label}
                      note={m.note}
                      count={data.usage[m.key]}
                      soft={data.limits[`${m.key}_soft`]}
                      hard={data.limits[`${m.key}_hard`]}
                    />
                  ))}
                  <div className="grid gap-4 border-t border-border pt-4 sm:grid-cols-2">
                    {COUNTED_ONLY_METRICS.map((m) => (
                      <div key={m.key}>
                        <div className="text-sm font-medium">{m.label}</div>
                        <div className="text-lg font-semibold tabular-nums">
                          {data.usage[m.key].toLocaleString()}
                        </div>
                        <p className="mt-1 text-xs text-muted-foreground">{m.note}</p>
                      </div>
                    ))}
                  </div>
                </>
              )}
            </CardContent>
          </Card>

          {data && <UsageTrend period={data.period} />}
        </div>
      </div>
    </div>
  )
}

// UsageTrend renders GET /api/usage/history for the two gated metrics.
//
// Deliberately hand-drawn bars rather than a chart library: twelve values per
// metric with no axes, tooltips or interaction does not justify pulling one
// in, and the page has no other chart to share it with.
function UsageTrend({ period }: { period: string }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Recent {period === 'day' ? 'days' : 'months'}</CardTitle>
        <CardDescription>
          The last 12 periods for the two metrics that are enforced. The final bar is the current
          period and is still filling.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-6">
        {GATED_METRICS.map((m) => (
          <MetricHistory key={m.key} metric={m.key} label={m.label} />
        ))}
      </CardContent>
    </Card>
  )
}

function MetricHistory({ metric, label }: { metric: 'events' | 'messages'; label: string }) {
  const { data, isLoading } = useQuery({
    queryKey: qk.usageHistory(metric, 12),
    queryFn: () => api.getUsageHistory(metric, 12),
    retry: false,
  })

  if (isLoading) return <p className="text-sm text-muted-foreground">Loading {label}…</p>
  const rows = data?.periods ?? []
  if (rows.length === 0) {
    return (
      <div>
        <div className="text-sm font-medium">{label}</div>
        <p className="mt-1 text-xs text-muted-foreground">
          No history yet — rollups start accumulating from the first sweep after deploy.
        </p>
      </div>
    )
  }

  // Scale against the tallest bar, not the limit: the limit can be 0
  // (unlimited), and a trend is about shape rather than headroom — the meters
  // above already show headroom. max||1 avoids dividing by zero on an
  // all-empty history.
  const max = Math.max(...rows.map((r) => r.count), 1)

  return (
    <div>
      <div className="flex items-baseline justify-between">
        <span className="text-sm font-medium">{label}</span>
        <span className="text-xs text-muted-foreground tabular-nums">
          peak {max.toLocaleString()}
        </span>
      </div>
      <div className="mt-2 flex h-16 items-end gap-1">
        {rows.map((r) => (
          <div
            key={r.period_start}
            className="flex-1"
            title={`${new Date(r.period_start).toLocaleDateString()} — ${r.count.toLocaleString()}${
              r.partial ? ' (still filling)' : ''
            }`}
          >
            <div
              // A zero-count period still gets a hairline, so the bar is
              // visibly "there and empty" rather than missing entirely.
              className={`w-full rounded-sm ${r.partial ? 'bg-primary/40' : 'bg-primary'}`}
              style={{ height: `${Math.max(2, (r.count / max) * 56)}px` }}
            />
          </div>
        ))}
      </div>
    </div>
  )
}

function UsageMeter({
  label,
  note,
  count,
  soft,
  hard,
}: {
  label: string
  note: string
  count: number
  soft: number
  hard: number
}) {
  // The 0-means-unlimited rules live in #/lib/quota, shared with the
  // operator console so the two pages cannot drift, and covered by
  // src/lib/quota.test.ts.
  const { noCeiling, fullyUnlimited, overSoft, overHard, pct } = quotaState(count, soft, hard)
  const barColor = overHard ? 'bg-destructive' : overSoft ? 'bg-amber-800' : 'bg-primary'

  return (
    <div>
      <div className="flex items-center justify-between text-sm">
        <span className="font-medium">{label}</span>
        <div className="flex items-center gap-2">
          <span className="text-muted-foreground tabular-nums">
            {count.toLocaleString()}
            {!noCeiling && ` / ${hard.toLocaleString()}`}
          </span>
          {fullyUnlimited ? (
            <Badge variant="outline">Unlimited</Badge>
          ) : overHard ? (
            <Badge variant="destructive">At hard ceiling</Badge>
          ) : overSoft ? (
            <Badge variant="warning">Over soft limit</Badge>
          ) : noCeiling ? (
            <Badge variant="outline">No hard ceiling</Badge>
          ) : null}
        </div>
      </div>
      {/* No bar when there's no hard ceiling — nothing to scale it against,
          but the badge above still reports over-soft regardless. */}
      {!noCeiling && (
        <div className="mt-1.5 h-2 w-full overflow-hidden rounded-full bg-muted">
          <div className={`h-full rounded-full ${barColor}`} style={{ width: `${pct}%` }} />
        </div>
      )}
      <p className="mt-1 text-xs text-muted-foreground">{note}</p>
    </div>
  )
}
