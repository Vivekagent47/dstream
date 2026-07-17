import { createFileRoute, Link } from '@tanstack/react-router'
import { queryOptions, useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { MoveRight } from 'lucide-react'
import { Bar, BarChart, CartesianGrid, XAxis } from 'recharts'
import {
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from '#/components/ui/chart'

import {
  api,
  qk,
  type EventsPage as EventsPageData,
  type EventHistogramBucket,
} from '#/lib/api'
import { capitalize } from '#/lib/utils'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { PageHeader } from '#/components/TopBar'
import { Badge } from '#/components/ui/badge'
import { Button } from '#/components/ui/button'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '#/components/ui/select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'

const PAGE_SIZE = 100

// Time windows for the range picker. `bucket` is the date_trunc unit the graph
// endpoint groups by — kept coarse enough that a window never yields a wall of
// hairline bars.
const RANGES = [
  { key: '1h', label: 'Last hour', ms: 3_600_000, bucket: 'minute' },
  { key: '24h', label: 'Last day', ms: 86_400_000, bucket: 'hour' },
  { key: '7d', label: 'Last 7 days', ms: 604_800_000, bucket: 'hour' },
  { key: '30d', label: 'Last 30 days', ms: 2_592_000_000, bucket: 'day' },
] as const
type RangeKey = (typeof RANGES)[number]['key']

const STATUSES = ['queued', 'in_flight', 'delivered', 'failed', 'paused', 'dead', 'discarded'] as const

const statusVariant: Record<string, React.ComponentProps<typeof Badge>['variant']> = {
  delivered: 'success',
  queued: 'secondary',
  in_flight: 'info',
  failed: 'destructive',
  paused: 'warning',
  dead: 'destructive',
  discarded: 'warning',
}

const connectionsQuery = queryOptions({
  queryKey: qk.connections('all'),
  queryFn: () => api.listConnections(),
})
const sourcesQuery = queryOptions({ queryKey: qk.sources(), queryFn: () => api.listSources() })
const destinationsQuery = queryOptions({
  queryKey: qk.destinations(),
  queryFn: () => api.listDestinations(),
})

export const Route = createFileRoute('/events/')({
  component: EventsPage,
  errorComponent: AuthErrorBoundary,
})

function EventsPage() {
  const [range, setRange] = useState<RangeKey>('24h')
  const [status, setStatus] = useState<string>('all')
  const [connId, setConnId] = useState<string>('all')

  const active = RANGES.find((r) => r.key === range)!
  // `after` is frozen per range selection (recomputed only when the deps
  // change), so polling reuses one query key instead of sliding every tick.
  // ponytail: window doesn't auto-slide; re-pick the range to advance it.
  const after = useMemo(
    () => new Date(Date.now() - active.ms).toISOString(),
    [active.ms],
  )

  const filters = {
    after,
    connection_id: connId === 'all' ? undefined : connId,
    status: status === 'all' ? undefined : status,
  }

  const { data, error, fetchNextPage, hasNextPage, isFetchingNextPage } = useInfiniteQuery({
    queryKey: qk.events({ limit: PAGE_SIZE, ...filters }),
    queryFn: ({ pageParam }) =>
      api.listEvents({ limit: PAGE_SIZE, cursor: pageParam, ...filters }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage: EventsPageData) => lastPage.next_cursor,
    refetchInterval: 5000,
  })

  const { data: histogram } = useQuery({
    queryKey: qk.eventsHistogram({ bucket: active.bucket, ...filters }),
    queryFn: () => api.eventsHistogram({ bucket: active.bucket, ...filters }),
    refetchInterval: 5000,
  })

  const { data: connections } = useQuery(connectionsQuery)
  const { data: sources } = useQuery(sourcesQuery)
  const { data: destinations } = useQuery(destinationsQuery)

  // connection_id -> "source → destination" label.
  const connLabel = useMemo(() => {
    const srcName = new Map((sources ?? []).map((s) => [s.id, s.name]))
    const dstName = new Map((destinations ?? []).map((d) => [d.id, d.name]))
    return new Map(
      (connections ?? []).map((c) => [
        c.id,
        {
          source: srcName.get(c.source_id) ?? c.source_id.slice(0, 8),
          dest: dstName.get(c.destination_id) ?? c.destination_id.slice(0, 8),
        },
      ]),
    )
  }, [connections, sources, destinations])

  const events = data?.pages.flatMap((p) => p.events) ?? []

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader title="Events" />

      {/* filter toolbar */}
      <div className="flex flex-wrap items-center gap-2 border-b border-border px-6 py-3">
        <Select value={range} onValueChange={(v) => setRange((v as RangeKey) ?? '24h')}>
          <SelectTrigger className="h-8 w-36 text-xs">
            <SelectValue>
              {(v: string | null) => RANGES.find((r) => r.key === v)?.label ?? 'Last day'}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            {RANGES.map((r) => (
              <SelectItem key={r.key} value={r.key}>
                {r.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        <Select value={status} onValueChange={(v) => setStatus(v ?? 'all')}>
          <SelectTrigger className="h-8 w-36 text-xs">
            <SelectValue>
              {(v: string | null) =>
                v && v !== 'all' ? capitalize(v.replace('_', ' ')) : 'All statuses'
              }
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All statuses</SelectItem>
            {STATUSES.map((s) => (
              <SelectItem key={s} value={s}>
                {capitalize(s.replace('_', ' '))}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        <Select value={connId} onValueChange={(v) => setConnId(v ?? 'all')}>
          <SelectTrigger className="h-8 w-52 text-xs">
            <SelectValue>
              {(v: string | null) => {
                if (!v || v === 'all') return 'All connections'
                const l = connLabel.get(v)
                return l ? `${l.source} → ${l.dest}` : v.slice(0, 8)
              }}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All connections</SelectItem>
            {(connections ?? []).map((c) => {
              const l = connLabel.get(c.id)
              return (
                <SelectItem key={c.id} value={c.id}>
                  {l ? `${l.source} → ${l.dest}` : c.id.slice(0, 8)}
                </SelectItem>
              )
            })}
          </SelectContent>
        </Select>
      </div>

      {/* timeline graph */}
      <Histogram buckets={histogram?.buckets ?? []} />

      <div className="flex-1 overflow-x-auto">
        {error && (
          <p className="px-6 py-3 text-sm text-destructive">{(error as Error).message}</p>
        )}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Event Date</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Connection</TableHead>
              <TableHead>Attempts</TableHead>
              <TableHead className="pr-6">Next Scheduled Attempt</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {events.map((e) => {
              const l = connLabel.get(e.connection_id)
              return (
                <TableRow key={e.id}>
                  <TableCell className="pl-6 whitespace-nowrap font-mono text-xs">
                    <Link
                      to="/events/$id"
                      params={{ id: e.id }}
                      className="text-foreground hover:underline"
                    >
                      {new Date(e.created_at).toLocaleString()}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <Badge variant={statusVariant[e.status] || 'secondary'}>
                      {capitalize(e.status.replace('_', ' '))}
                    </Badge>
                  </TableCell>
                  <TableCell className="whitespace-nowrap font-mono text-xs">
                    {l ? (
                      <span className="inline-flex items-center gap-1.5">
                        {l.source}
                        <MoveRight className="h-3 w-3 text-muted-foreground" />
                        {l.dest}
                      </span>
                    ) : (
                      e.connection_id.slice(0, 8)
                    )}
                  </TableCell>
                  <TableCell>{e.attempt_count}</TableCell>
                  <TableCell className="pr-6 whitespace-nowrap text-muted-foreground">
                    {e.next_retry_at ? new Date(e.next_retry_at).toLocaleString() : '—'}
                  </TableCell>
                </TableRow>
              )
            })}
            {events.length === 0 && (
              <TableRow>
                <TableCell colSpan={5} className="py-12 text-center text-sm text-muted-foreground">
                  No events in this window.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <footer className="flex items-center gap-3 border-t border-border px-6 py-3 text-sm text-muted-foreground">
        <span>
          Viewing {events.length} {events.length === 1 ? 'event' : 'events'}
        </span>
        {hasNextPage && (
          <Button
            variant="outline"
            size="sm"
            className="ml-auto"
            onClick={() => fetchNextPage()}
            disabled={isFetchingNextPage}
          >
            {isFetchingNextPage ? 'Loading…' : 'Load more'}
          </Button>
        )}
      </footer>
    </div>
  )
}

// Status → label + color, in stack order (bottom→top). Colors are semantic
// (green delivered … red failed) so a glance reads health; the theme's blue
// --chart-N ramp would make every status look the same. ChartContainer turns
// this config into --color-<key> CSS vars the Bars fill with.
const eventChartConfig = {
  delivered: { label: 'Delivered', color: '#10b981' },
  in_flight: { label: 'In flight', color: '#3b82f6' },
  queued: { label: 'Queued', color: '#9ca3af' },
  paused: { label: 'Paused', color: '#fbbf24' },
  discarded: { label: 'Discarded', color: '#f59e0b' },
  failed: { label: 'Failed', color: '#ef4444' },
  dead: { label: 'Dead', color: '#991b1b' },
} satisfies ChartConfig

const STATUS_ORDER = Object.keys(eventChartConfig)

// Stacked bar timeline built on the shadcn chart primitives. One bar per
// bucket, segmented by status; only statuses present in the window render.
// ponytail: empty buckets aren't back-filled, so a sparse window packs its bars
// together; back-fill from `after`→now stepped by bucket if precise spacing matters.
function Histogram({ buckets }: { buckets: EventHistogramBucket[] }) {
  const present = STATUS_ORDER.filter((k) => buckets.some((b) => (b.counts[k] ?? 0) > 0))
  const data = buckets.map((b) => ({ ts: b.ts, ...b.counts }))

  if (buckets.length === 0) {
    return (
      <div className="flex h-28 items-center justify-center border-b border-border px-6 text-xs text-muted-foreground">
        No events in this window
      </div>
    )
  }

  return (
    <div className="border-b border-border px-6 py-3">
      <ChartContainer config={eventChartConfig} className="aspect-auto h-28 w-full">
        <BarChart data={data} margin={{ top: 4, right: 4, bottom: 0, left: 4 }} barCategoryGap={2}>
          <CartesianGrid vertical={false} strokeDasharray="3 3" />
          <XAxis
            dataKey="ts"
            tickLine={false}
            axisLine={false}
            tickMargin={8}
            minTickGap={48}
            tickFormatter={(ts) =>
              new Date(ts as string).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
            }
          />
          <ChartTooltip
            content={
              <ChartTooltipContent labelFormatter={(v) => new Date(v as string).toLocaleString()} />
            }
          />
          <ChartLegend content={<ChartLegendContent />} />
          {present.map((k, i) => (
            <Bar
              key={k}
              dataKey={k}
              stackId="a"
              fill={`var(--color-${k})`}
              // round only the topmost segment of the stack
              radius={i === present.length - 1 ? [3, 3, 0, 0] : 0}
            />
          ))}
        </BarChart>
      </ChartContainer>
    </div>
  )
}
