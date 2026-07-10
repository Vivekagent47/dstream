import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import {
  queryOptions,
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { toast } from 'sonner'
import { Pencil, Trash2 } from 'lucide-react'

import { api, qk, type Connection, type EventsPage as EventsPageData } from '#/lib/api'
import { capitalize } from '#/lib/utils'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { CopyValue, DetailRow } from '#/components/detail-page'
import { PageHeader } from '#/components/TopBar'
import { Badge } from '#/components/ui/badge'
import { Button } from '#/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '#/components/ui/dialog'
import { Input } from '#/components/ui/input'
import { Label } from '#/components/ui/label'
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

const TABS = [
  { key: 'overview', label: 'Overview' },
  { key: 'events', label: 'Events' },
  { key: 'settings', label: 'Settings' },
] as const
type Tab = (typeof TABS)[number]['key']

const EVENTS_PAGE_SIZE = 50

const connectionQuery = (id: string) =>
  queryOptions({ queryKey: qk.connection(id), queryFn: () => api.getConnection(id) })

const sourcesQuery = queryOptions({ queryKey: qk.sources(), queryFn: () => api.listSources() })
const destinationsQuery = queryOptions({
  queryKey: qk.destinations(),
  queryFn: () => api.listDestinations(),
})

export const Route = createFileRoute('/connections/$id')({
  validateSearch: (search: Record<string, unknown>): { tab?: Tab } => {
    const t = search.tab as Tab
    return TABS.some((x) => x.key === t) && t !== 'overview' ? { tab: t } : {}
  },
  loader: ({ context, params }) =>
    typeof window === 'undefined'
      ? undefined
      : context.queryClient.ensureQueryData(connectionQuery(params.id)),
  component: ConnectionDetail,
  errorComponent: AuthErrorBoundary,
})

const statusVariant: Record<string, React.ComponentProps<typeof Badge>['variant']> = {
  delivered: 'success',
  queued: 'secondary',
  in_flight: 'info',
  failed: 'destructive',
  paused: 'warning',
  dead: 'destructive',
}

// "30000 ms" reads worse than "30s" in policy summaries.
function fmtMs(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  if (ms < 60_000) return `${Math.round(ms / 100) / 10}s`
  if (ms < 3_600_000) return `${Math.round(ms / 6000) / 10}m`
  return `${Math.round(ms / 360_000) / 10}h`
}

function retrySummary(c: Connection): string {
  if (c.retry_strategy === 'custom') {
    const steps = (c.custom_retry_schedule ?? []).map(fmtMs).join(', ')
    return `custom · [${steps}] · jitter ±${c.retry_jitter_pct}%`
  }
  return `${c.retry_strategy} · ${c.max_retries} retries · base ${fmtMs(c.retry_base_ms)} · cap ${fmtMs(c.retry_cap_ms)} · jitter ±${c.retry_jitter_pct}%`
}

function ConnectionDetail() {
  const { id } = Route.useParams()
  const { tab = 'overview' } = Route.useSearch()
  const qc = useQueryClient()
  const { data: conn } = useQuery(connectionQuery(id))
  const { data: sources } = useQuery(sourcesQuery)
  const { data: destinations } = useQuery(destinationsQuery)

  const srcName = sources?.find((s) => s.id === conn?.source_id)?.name ?? conn?.source_id
  const destName =
    destinations?.find((d) => d.id === conn?.destination_id)?.name ?? conn?.destination_id

  const toggle = useMutation({
    mutationFn: () => api.patchConnection(id, { enabled: !conn!.enabled }),
    onSuccess: (_r) => {
      qc.invalidateQueries({ queryKey: qk.connection(id) })
      qc.invalidateQueries({ queryKey: ['connections'] })
      toast.success(conn!.enabled ? 'Connection disabled' : 'Connection enabled')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const sendTest = useMutation({
    mutationFn: () => api.testConnection(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.events({ limit: 50, connection_id: id }) })
      qc.invalidateQueries({ queryKey: qk.connectionStats(id) })
      toast.success('Test event sent — watch the Events tab')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  if (!conn) {
    return (
      <div className="flex flex-1 flex-col">
        <PageHeader title="Connection" />
        <p className="px-6 py-8 text-sm text-muted-foreground">Loading…</p>
      </div>
    )
  }

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title={
          <span className="flex min-w-0 items-center gap-1.5">
            <Link
              to="/connections"
              className="font-normal text-muted-foreground hover:text-foreground"
            >
              Connections
            </Link>
            <span className="font-normal text-muted-foreground">/</span>
            <span className="truncate">
              {srcName} → {destName}
            </span>
          </span>
        }
        actions={
          <div className="flex items-center gap-2">
            <Button
              size="sm"
              variant="outline"
              onClick={() => sendTest.mutate()}
              disabled={sendTest.isPending}
            >
              {sendTest.isPending ? 'Sending…' : 'Send test event'}
            </Button>
            <Button
              size="sm"
              variant="outline"
              onClick={() => toggle.mutate()}
              disabled={toggle.isPending}
            >
              {conn.enabled ? 'Disable' : 'Enable'}
            </Button>
          </div>
        }
      />

      <nav className="flex shrink-0 gap-6 border-b border-border px-6">
        {TABS.map((t) => (
          <Link
            key={t.key}
            from={Route.fullPath}
            search={t.key === 'overview' ? {} : { tab: t.key }}
            className={
              '-mb-px flex items-center gap-2 border-b-2 py-2.5 text-sm font-medium ' +
              (tab === t.key
                ? 'border-primary text-foreground'
                : 'border-transparent text-muted-foreground hover:text-foreground')
            }
          >
            {t.label}
          </Link>
        ))}
      </nav>

      <div className="flex-1 overflow-y-auto px-6 py-4">
        {tab === 'overview' && (
          <OverviewTab conn={conn} srcName={srcName ?? ''} destName={destName ?? ''} />
        )}
        {tab === 'events' && <EventsTab connectionId={conn.id} />}
        {tab === 'settings' && <SettingsTab conn={conn} />}
      </div>
    </div>
  )
}

function OverviewTab({
  conn,
  srcName,
  destName,
}: {
  conn: Connection
  srcName: string
  destName: string
}) {
  const navigate = useNavigate({ from: Route.fullPath })
  const { data: stats } = useQuery({
    queryKey: qk.connectionStats(conn.id),
    queryFn: () => api.getConnectionStats(conn.id),
  })
  return (
    <div className="grid min-h-full gap-6 lg:grid-cols-[380px_1fr]">
      <div className="space-y-3 lg:border-r lg:border-border lg:pr-6">
        <div className="flex items-center justify-between">
          <h2 className="text-base font-semibold">Connection details</h2>
          <Button
            size="sm"
            variant="outline"
            onClick={() => navigate({ search: { tab: 'settings' } })}
          >
            <Pencil className="h-3.5 w-3.5" /> Edit
          </Button>
        </div>
        <div className="space-y-3">
          <DetailRow label="Source">
            <Link
              to="/sources/$id"
              params={{ id: conn.source_id }}
              className="text-primary hover:underline"
            >
              {srcName}
            </Link>
          </DetailRow>
          <DetailRow label="Destination">
            <Link
              to="/destinations/$id"
              params={{ id: conn.destination_id }}
              className="text-primary hover:underline"
            >
              {destName}
            </Link>
          </DetailRow>
          <DetailRow label="Status">
            {conn.enabled ? (
              <Badge variant="success">Active</Badge>
            ) : (
              <Badge variant="secondary">Disabled</Badge>
            )}
          </DetailRow>

          <div className="border-t border-border pt-3 text-sm font-semibold">Retry policy</div>
          <DetailRow label="Policy">{retrySummary(conn)}</DetailRow>

          <div className="border-t border-border pt-3 text-sm font-semibold">Metadata</div>
          <DetailRow label="Connection ID">
            <CopyValue value={conn.id} what="Connection ID" mono />
          </DetailRow>
          <DetailRow label="Created at">{new Date(conn.created_at).toLocaleString()}</DetailRow>
        </div>
      </div>

      <div className="flex min-h-0 flex-col gap-3">
        <h2 className="text-base font-semibold">
          Delivery health{' '}
          <span className="text-sm font-normal text-muted-foreground">(last 24h)</span>
        </h2>
        <div className="grid gap-3 sm:grid-cols-3">
          <StatCard label="Delivered" value={stats?.delivered} />
          <StatCard label="Failed" value={stats?.failed} />
          <StatCard label="Pending" value={stats?.pending} />
        </div>
      </div>
    </div>
  )
}

function EventsTab({ connectionId }: { connectionId: string }) {
  const [status, setStatus] = useState('all')
  const qc = useQueryClient()
  const { data, error, fetchNextPage, hasNextPage, isFetchingNextPage } = useInfiniteQuery({
    queryKey: qk.events({
      limit: EVENTS_PAGE_SIZE,
      connection_id: connectionId,
      status: status === 'all' ? undefined : status,
    }),
    queryFn: ({ pageParam }) =>
      api.listEvents({
        limit: EVENTS_PAGE_SIZE,
        connection_id: connectionId,
        status: status === 'all' ? undefined : status,
        cursor: pageParam,
      }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage: EventsPageData) => lastPage.next_cursor,
    // Poll for live status churn, same cadence as the main events page.
    refetchInterval: 5000,
  })

  const retry = useMutation({
    mutationFn: (eventId: string) => api.retryEvent(eventId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['events'] })
      // Refresh the Overview cards too — a retry moves the event out of
      // Failed; the stats query has no poll, so invalidate it explicitly.
      qc.invalidateQueries({ queryKey: qk.connectionStats(connectionId) })
      toast.success('Event re-queued')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const events = data?.pages.flatMap((p) => p.events) ?? []

  if (error) {
    return <p className="py-3 text-sm text-destructive">{(error as Error).message}</p>
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3">
        <Select value={status} onValueChange={(v) => setStatus(v ?? 'all')}>
          <SelectTrigger className="w-[160px]">
            <SelectValue>
              {(v: string | null) => (v && v !== 'all' ? capitalize(v) : 'All statuses')}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All statuses</SelectItem>
            <SelectItem value="queued">Queued</SelectItem>
            <SelectItem value="in_flight">In flight</SelectItem>
            <SelectItem value="delivered">Delivered</SelectItem>
            <SelectItem value="failed">Failed</SelectItem>
            <SelectItem value="paused">Paused</SelectItem>
            <SelectItem value="dead">Dead</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <div className="rounded-lg border border-border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">ID</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Attempts</TableHead>
              <TableHead>Last attempt</TableHead>
              <TableHead>Next retry</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="pr-6" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {events.map((e) => (
              <TableRow key={e.id}>
                <TableCell className="pl-6 font-mono text-xs">
                  <Link
                    to="/events/$id"
                    params={{ id: e.id }}
                    className="text-primary hover:underline"
                  >
                    {e.id.slice(0, 8)}
                  </Link>
                </TableCell>
                <TableCell>
                  <Badge variant={statusVariant[e.status] || 'secondary'}>
                    {capitalize(e.status)}
                  </Badge>
                </TableCell>
                <TableCell>{e.attempt_count}</TableCell>
                <TableCell className="whitespace-nowrap text-muted-foreground">
                  {e.last_attempt_at ? new Date(e.last_attempt_at).toLocaleString() : '—'}
                </TableCell>
                <TableCell className="whitespace-nowrap text-muted-foreground">
                  {e.next_retry_at ? new Date(e.next_retry_at).toLocaleString() : '—'}
                </TableCell>
                <TableCell className="whitespace-nowrap text-muted-foreground">
                  {new Date(e.created_at).toLocaleString()}
                </TableCell>
                <TableCell className="pr-6 text-right">
                  {e.is_test ? (
                    <Badge variant="secondary" className="mr-2">
                      Test
                    </Badge>
                  ) : null}
                  {e.status === 'failed' || e.status === 'dead' ? (
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => retry.mutate(e.id)}
                      disabled={retry.isPending}
                    >
                      Retry
                    </Button>
                  ) : null}
                </TableCell>
              </TableRow>
            ))}
            {events.length === 0 && (
              <TableRow>
                <TableCell colSpan={7} className="py-12 text-center text-sm text-muted-foreground">
                  No events yet for this connection.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>
      {hasNextPage && (
        <Button
          variant="outline"
          size="sm"
          onClick={() => fetchNextPage()}
          disabled={isFetchingNextPage}
        >
          {isFetchingNextPage ? 'Loading…' : 'Load more'}
        </Button>
      )}
    </div>
  )
}

function StatCard({ label, value }: { label: string; value: number | undefined }) {
  return (
    <div className="rounded-lg border border-border p-4">
      <div className="text-sm text-muted-foreground">{label}</div>
      <div className="mt-1 text-2xl font-semibold tabular-nums">{value ?? '—'}</div>
    </div>
  )
}

// Parse "10000, 30000, 60000" into [10000, 30000, 60000]; null = invalid.
function parseSchedule(s: string): number[] | null {
  const parts = s.split(',').map((x) => x.trim()).filter((x) => x !== '')
  if (parts.length === 0) return null
  const nums = parts.map(Number)
  if (nums.some((n) => !Number.isInteger(n) || n <= 0)) return null
  return nums
}

function SettingsTab({ conn }: { conn: Connection }) {
  const qc = useQueryClient()
  const navigate = useNavigate()

  const [name, setName] = useState(conn.name ?? '')
  const [maxRetries, setMaxRetries] = useState(String(conn.max_retries))
  const [strategy, setStrategy] = useState<Connection['retry_strategy']>(conn.retry_strategy)
  const [baseMs, setBaseMs] = useState(String(conn.retry_base_ms))
  const [capMs, setCapMs] = useState(String(conn.retry_cap_ms))
  const [jitter, setJitter] = useState(String(conn.retry_jitter_pct))
  const [schedule, setSchedule] = useState((conn.custom_retry_schedule ?? []).join(', '))
  const [deleteOpen, setDeleteOpen] = useState(false)

  // Seed the form once per connection (on mount / navigation to another id),
  // NOT on every conn refetch — a background refetch (header toggle, poll,
  // concurrent edit) would otherwise wipe in-progress edits. After Save the
  // typed values already equal the server row, so skipping re-seed is a no-op;
  // the retry fields here don't reflect `enabled`, so a toggle refetch has
  // nothing to re-seed anyway.
  const seededId = useRef<string | null>(null)
  useEffect(() => {
    if (seededId.current === conn.id) return
    seededId.current = conn.id
    setName(conn.name ?? '')
    setMaxRetries(String(conn.max_retries))
    setStrategy(conn.retry_strategy)
    setBaseMs(String(conn.retry_base_ms))
    setCapMs(String(conn.retry_cap_ms))
    setJitter(String(conn.retry_jitter_pct))
    setSchedule((conn.custom_retry_schedule ?? []).join(', '))
  }, [conn])

  const save = useMutation({
    mutationFn: () =>
      api.patchConnection(conn.id, {
        name,
        max_retries: Number(maxRetries),
        retry_strategy: strategy,
        retry_base_ms: Number(baseMs),
        retry_cap_ms: Number(capMs),
        retry_jitter_pct: Number(jitter),
        ...(strategy === 'custom'
          ? { custom_retry_schedule: parseSchedule(schedule) ?? undefined }
          : {}),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.connection(conn.id) })
      qc.invalidateQueries({ queryKey: ['connections'] })
      toast.success('Connection saved')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: () => api.deleteConnection(conn.id),
    onSuccess: () => {
      // Drop the detail cache outright — the resource is gone, so a later
      // Back navigation must refetch (404) instead of rendering the stale row.
      qc.removeQueries({ queryKey: qk.connection(conn.id) })
      qc.invalidateQueries({ queryKey: ['connections'] })
      toast.success('Connection deleted')
      navigate({ to: '/connections' })
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function validate(): string | null {
    const mr = Number(maxRetries)
    if (!Number.isInteger(mr) || mr < 0 || mr > 20) return 'Max retries must be 0–20'
    const b = Number(baseMs)
    if (!Number.isInteger(b) || b <= 0) return 'Base delay must be a positive integer (ms)'
    const c = Number(capMs)
    if (!Number.isInteger(c) || c <= 0) return 'Cap must be a positive integer (ms)'
    if (c < b) return 'Cap must be ≥ base delay'
    const j = Number(jitter)
    if (!Number.isInteger(j) || j < 0 || j > 100) return 'Jitter must be 0–100 (%)'
    if (strategy === 'custom' && parseSchedule(schedule) === null)
      return 'Custom schedule must be comma-separated positive integers (ms)'
    return null
  }

  function onSave() {
    const err = validate()
    if (err) {
      toast.error(err)
      return
    }
    save.mutate()
  }

  const dirty =
    name !== (conn.name ?? '') ||
    Number(maxRetries) !== conn.max_retries ||
    strategy !== conn.retry_strategy ||
    Number(baseMs) !== conn.retry_base_ms ||
    Number(capMs) !== conn.retry_cap_ms ||
    Number(jitter) !== conn.retry_jitter_pct ||
    (strategy === 'custom' &&
      schedule !== (conn.custom_retry_schedule ?? []).join(', '))

  const strategyLabel = (v: string | null) => (v ? capitalize(v) : 'Strategy')

  return (
    <div className="max-w-3xl space-y-8">
      <section className="space-y-4">
        <h2 className="text-sm font-semibold">Retry policy</h2>
        <p className="text-sm text-muted-foreground">
          How failed deliveries are retried. Changes apply to future events only — already
          queued deliveries keep the policy they were enqueued with.
        </p>
        <div>
          <Label htmlFor="conn-name" className="mb-2 block">
            Name <span className="text-muted-foreground">(optional)</span>
          </Label>
          <Input
            id="conn-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. prod-billing"
            className="w-full"
          />
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <Label className="mb-2 block">Strategy</Label>
            <Select
              value={strategy}
              onValueChange={(v) => setStrategy((v ?? 'exponential') as Connection['retry_strategy'])}
            >
              <SelectTrigger className="w-full">
                <SelectValue>{strategyLabel}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="exponential">Exponential</SelectItem>
                <SelectItem value="linear">Linear</SelectItem>
                <SelectItem value="fixed">Fixed</SelectItem>
                <SelectItem value="custom">Custom</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div>
            <Label htmlFor="max-retries" className="mb-2 block">Max retries</Label>
            <Input
              id="max-retries"
              type="number"
              min="0"
              max="20"
              value={maxRetries}
              onChange={(e) => setMaxRetries(e.target.value)}
            />
          </div>
        </div>
        {strategy !== 'custom' ? (
          <div className="grid gap-4 sm:grid-cols-3">
            <div>
              <Label htmlFor="base-ms" className="mb-2 block">Base delay (ms)</Label>
              <Input
                id="base-ms"
                type="number"
                min="1"
                value={baseMs}
                onChange={(e) => setBaseMs(e.target.value)}
              />
            </div>
            <div>
              <Label htmlFor="cap-ms" className="mb-2 block">Cap (ms)</Label>
              <Input
                id="cap-ms"
                type="number"
                min="1"
                value={capMs}
                onChange={(e) => setCapMs(e.target.value)}
              />
            </div>
            <div>
              <Label htmlFor="jitter" className="mb-2 block">Jitter (±%)</Label>
              <Input
                id="jitter"
                type="number"
                min="0"
                max="100"
                value={jitter}
                onChange={(e) => setJitter(e.target.value)}
              />
            </div>
          </div>
        ) : (
          <div>
            <Label htmlFor="schedule" className="mb-2 block">
              Custom schedule <span className="text-muted-foreground">(ms, comma-separated)</span>
            </Label>
            <Input
              id="schedule"
              value={schedule}
              onChange={(e) => setSchedule(e.target.value)}
              placeholder="10000, 30000, 60000, 300000"
              className="w-full font-mono text-xs"
            />
          </div>
        )}
        <Button size="sm" onClick={onSave} disabled={!dirty || save.isPending}>
          {save.isPending ? 'Saving…' : 'Save'}
        </Button>
      </section>

      <section className="space-y-2 border-t border-border pt-6">
        <h2 className="text-sm font-semibold text-destructive">Delete connection</h2>
        <p className="text-sm text-muted-foreground">
          New events from this source stop routing to this destination. Already-queued
          deliveries are unaffected.
        </p>
        <Button variant="destructive" onClick={() => setDeleteOpen(true)}>
          <Trash2 className="h-4 w-4" /> Delete connection
        </Button>
      </section>

      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete this connection?</DialogTitle>
            <DialogDescription>
              Routing from its source to its destination stops. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setDeleteOpen(false)}>Cancel</Button>
            <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
              {remove.isPending ? 'Deleting…' : 'Delete connection'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
