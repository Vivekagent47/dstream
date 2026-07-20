import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Bar, BarChart, CartesianGrid, XAxis } from 'recharts'

import {
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from '#/components/ui/chart'
import { Card, CardContent, CardHeader, CardTitle } from '#/components/ui/card'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '#/components/ui/select'
import { api, qk } from '#/lib/api'

// Range picker windows, identical semantics to the Events page: "Last hour" is
// rolling, the day/week/month windows are UTC calendar-aligned so the graph is
// exactly N date columns and lines up with the server's gap-filled buckets.
const RANGES = [
  { key: '1h', label: 'Last hour', mode: 'rolling', ms: 3_600_000, bucket: 'minute' },
  { key: '24h', label: 'Last day', mode: 'calendar', days: 1, bucket: 'hour' },
  { key: '7d', label: 'Last 7 days', mode: 'calendar', days: 7, bucket: 'day' },
  { key: '30d', label: 'Last 30 days', mode: 'calendar', days: 30, bucket: 'day' },
] as const
type RangeKey = (typeof RANGES)[number]['key']

function useMetricsWindow() {
  const [range, setRange] = useState<RangeKey>('7d')
  const active = RANGES.find((r) => r.key === range)!
  const after = useMemo(() => {
    if (active.mode === 'rolling') {
      return new Date(Date.now() - active.ms).toISOString()
    }
    const d = new Date()
    d.setUTCHours(0, 0, 0, 0)
    d.setUTCDate(d.getUTCDate() - (active.days - 1))
    return d.toISOString()
  }, [active])
  return { range, setRange, active, after }
}

function RangePicker({ range, setRange }: { range: RangeKey; setRange: (v: RangeKey) => void }) {
  return (
    <Select value={range} onValueChange={(v) => setRange((v as RangeKey) ?? '7d')}>
      <SelectTrigger className="h-8 w-36 text-xs">
        <SelectValue>
          {(v: string | null) => RANGES.find((r) => r.key === v)?.label ?? 'Last 7 days'}
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
  )
}

// UTC for day buckets (they are UTC-midnight timestamps), local for intraday.
function fmtTick(ts: string, bucket: string) {
  const d = new Date(ts)
  return bucket === 'day'
    ? d.toLocaleDateString([], { month: 'short', day: 'numeric', timeZone: 'UTC' })
    : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}
function fmtLabel(ts: string, bucket: string) {
  const d = new Date(ts)
  return bucket === 'day'
    ? d.toLocaleDateString([], { month: 'short', day: 'numeric', year: 'numeric', timeZone: 'UTC' })
    : d.toLocaleString()
}

const deliveryConfig = {
  delivered: { label: 'Delivered', color: '#10b981' },
  in_flight: { label: 'In flight', color: '#3b82f6' },
  queued: { label: 'Queued', color: '#9ca3af' },
  paused: { label: 'Paused', color: '#fbbf24' },
  discarded: { label: 'Discarded', color: '#f59e0b' },
  failed: { label: 'Failed', color: '#ef4444' },
  dead: { label: 'Dead', color: '#991b1b' },
} satisfies ChartConfig
const DELIVERY_STATUS_ORDER = Object.keys(deliveryConfig)

const requestConfig = { count: { label: 'Requests', color: '#3b82f6' } } satisfies ChartConfig

// ChartCard wraps a titled bordered card around a bar timeline. `empty` renders
// the "no data" message centred (the series is always present/gap-filled, so
// this only shows on a load error or a genuinely empty account).
function ChartCard({
  title,
  empty,
  children,
  className,
}: {
  title: string
  empty: boolean
  children: React.ReactNode
  className?: string
}) {
  return (
    <Card className={'flex flex-col ' + (className ?? '')}>
      <CardHeader className="px-5 py-3.5">
        <CardTitle className="text-sm font-medium">{title}</CardTitle>
      </CardHeader>
      <CardContent className="flex flex-1 items-center justify-center px-4 pb-3">
        {empty ? (
          <span className="text-sm text-muted-foreground">No data to display.</span>
        ) : (
          children
        )}
      </CardContent>
    </Card>
  )
}

// Stat renders a single big-number card; value null → "—".
function Stat({ title, value }: { title: string; value: string | null }) {
  return (
    <Card className="flex flex-col">
      <CardHeader className="px-5 py-3.5">
        <CardTitle className="text-sm font-medium">{title}</CardTitle>
      </CardHeader>
      <CardContent className="flex flex-1 items-center justify-center">
        <span className={value == null ? 'text-sm text-muted-foreground' : 'text-3xl font-semibold'}>
          {value ?? '—'}
        </span>
      </CardContent>
    </Card>
  )
}

function fmtLatency(ms: number | null): string | null {
  if (ms == null) return null
  return ms >= 1000 ? `${(ms / 1000).toFixed(2)} s` : `${Math.round(ms)} ms`
}

export function DestinationMetrics({ id }: { id: string }) {
  const { range, setRange, active, after } = useMetricsWindow()
  const { data } = useQuery({
    queryKey: qk.destinationMetrics(id, { bucket: active.bucket, after }),
    queryFn: () => api.destinationMetrics(id, { bucket: active.bucket, after }),
    refetchInterval: 5000,
  })

  const series = data?.series ?? []
  const present = DELIVERY_STATUS_ORDER.filter((k) =>
    series.some((b) => (b.counts[k] ?? 0) > 0),
  )
  const chartData = series.map((b) => ({ ts: b.ts, ...b.counts }))
  const rate = data?.delivery_rate
  const latency = data?.avg_latency_ms ?? null

  return (
    <div className="flex min-h-full flex-col gap-3">
      <div className="flex justify-end">
        <RangePicker range={range} setRange={setRange} />
      </div>
      <ChartCard title="Deliveries" empty={present.length === 0} className="min-h-40 flex-[3]">
        <ChartContainer config={deliveryConfig} className="aspect-auto h-40 w-full">
          <BarChart data={chartData} margin={{ top: 4, right: 4, bottom: 0, left: 4 }} barCategoryGap={2}>
            <CartesianGrid vertical={false} strokeDasharray="3 3" />
            <XAxis
              dataKey="ts"
              tickLine={false}
              axisLine={false}
              tickMargin={8}
              minTickGap={32}
              tickFormatter={(ts) => fmtTick(ts as string, active.bucket)}
            />
            <ChartTooltip
              content={<ChartTooltipContent labelFormatter={(v) => fmtLabel(v as string, active.bucket)} />}
            />
            <ChartLegend content={<ChartLegendContent />} />
            {present.map((k, i) => (
              <Bar
                key={k}
                dataKey={k}
                stackId="a"
                fill={`var(--color-${k})`}
                radius={i === present.length - 1 ? [3, 3, 0, 0] : 0}
              />
            ))}
          </BarChart>
        </ChartContainer>
      </ChartCard>
      <div className="grid min-h-32 flex-[2] gap-3 xl:grid-cols-2">
        <Stat title="Delivery rate" value={rate == null ? null : `${(rate * 100).toFixed(1)}%`} />
        <Stat title="Avg. latency" value={fmtLatency(latency)} />
      </div>
    </div>
  )
}

export function SourceMetrics({ id }: { id: string }) {
  const { range, setRange, active, after } = useMetricsWindow()
  const { data } = useQuery({
    queryKey: qk.sourceMetrics(id, { bucket: active.bucket, after }),
    queryFn: () => api.sourceMetrics(id, { bucket: active.bucket, after }),
    refetchInterval: 5000,
  })

  const series = data?.series ?? []
  const hasData = series.some((b) => b.count > 0)
  const rate = data?.requests_rate
  const fanout = data?.avg_events_per_request ?? null

  return (
    <div className="flex min-h-full flex-col gap-3">
      <div className="flex justify-end">
        <RangePicker range={range} setRange={setRange} />
      </div>
      <ChartCard title="Requests" empty={!hasData} className="min-h-40 flex-[3]">
        <ChartContainer config={requestConfig} className="aspect-auto h-40 w-full">
          <BarChart data={series} margin={{ top: 4, right: 4, bottom: 0, left: 4 }} barCategoryGap={2}>
            <CartesianGrid vertical={false} strokeDasharray="3 3" />
            <XAxis
              dataKey="ts"
              tickLine={false}
              axisLine={false}
              tickMargin={8}
              minTickGap={32}
              tickFormatter={(ts) => fmtTick(ts as string, active.bucket)}
            />
            <ChartTooltip
              content={<ChartTooltipContent labelFormatter={(v) => fmtLabel(v as string, active.bucket)} />}
            />
            <Bar dataKey="count" fill="var(--color-count)" radius={[3, 3, 0, 0]} />
          </BarChart>
        </ChartContainer>
      </ChartCard>
      <div className="grid min-h-32 flex-[2] gap-3 xl:grid-cols-2">
        <Stat title="Requests rate" value={rate == null ? null : `${rate.toFixed(1)}/day`} />
        <Stat
          title="Avg. events per request"
          value={fanout == null ? null : fanout.toFixed(2)}
        />
      </div>
    </div>
  )
}
