import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { toast } from 'sonner'
import {
  Globe,
  Inbox,
  Maximize2,
  MoreHorizontal,
  MoveRight,
  Pause,
  Play,
  Plus,
  RotateCcw,
  Search,
  Send,
  Webhook,
} from 'lucide-react'

import { api, qk, type Connection } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '#/components/ui/dropdown-menu'
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

const connectionsQuery = queryOptions({
  queryKey: qk.connections('all'),
  queryFn: () => api.listConnections(),
})

const sourcesQuery = queryOptions({
  queryKey: qk.sources(),
  queryFn: () => api.listSources(),
})

const destinationsQuery = queryOptions({
  queryKey: qk.destinations(),
  queryFn: () => api.listDestinations(),
})

type View = 'structured' | 'table'

export const Route = createFileRoute('/connections/')({
  validateSearch: (search: Record<string, unknown>): { view?: View } => {
    const v = search.view
    return v === 'table' ? { view: 'table' } : {}
  },
  // Client-only prefetch — same SSR-cookie caveat as /sources.
  loader: ({ context }) =>
    typeof window === 'undefined'
      ? undefined
      : context.queryClient.ensureQueryData(connectionsQuery),
  component: ConnectionsPage,
  errorComponent: AuthErrorBoundary,
})

// 24h delivery summary from the all-connections stats map.
function statText(s: { delivered: number; failed: number; total: number } | undefined): string {
  if (!s || s.total === 0) return 'NO DATA'
  return `${s.delivered} ok / ${s.failed} fail`
}

function ConnectionsPage() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { view = 'structured' } = Route.useSearch()
  const { data: connections } = useQuery(connectionsQuery)
  const { data: sources } = useQuery(sourcesQuery)
  const { data: destinations } = useQuery(destinationsQuery)
  const { data: stats } = useQuery({
    queryKey: qk.connectionStatsAll(),
    queryFn: () => api.getAllConnectionStats(),
  })

  const [q, setQ] = useState('')
  const [status, setStatus] = useState('all')
  const [order, setOrder] = useState<'newest' | 'oldest'>('newest')
  const [createOpen, setCreateOpen] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<Connection | null>(null)

  // Server returns IDs only — join names from the lists the create dialog
  // needs anyway. Fall back to the raw ID if a lookup misses.
  const srcName = useMemo(
    () => new Map((sources ?? []).map((s) => [s.id, s.name])),
    [sources],
  )
  const destName = useMemo(
    () => new Map((destinations ?? []).map((d) => [d.id, d.name])),
    [destinations],
  )

  // Prefix key: also refreshes the per-source/per-destination detail tabs,
  // which cache under qk.connections(<their id>).
  const invalidate = () => qc.invalidateQueries({ queryKey: ['connections'] })

  const patch = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      api.patchConnection(id, { enabled }),
    onSuccess: (_r, v) => {
      invalidate()
      toast.success(v.enabled ? 'Connection enabled' : 'Connection disabled')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.deleteConnection(id),
    onSuccess: () => {
      invalidate()
      toast.success('Connection deleted')
      setDeleteTarget(null)
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const filtered = useMemo(() => {
    let list = connections ?? []
    const needle = q.trim().toLowerCase()
    if (needle) {
      list = list.filter((c) => {
        const hay = [
          c.id,
          c.name ?? '',
          srcName.get(c.source_id) ?? '',
          destName.get(c.destination_id) ?? '',
        ]
          .join(' ')
          .toLowerCase()
        return hay.includes(needle)
      })
    }
    if (status !== 'all') list = list.filter((c) => (status === 'active' ? c.enabled : !c.enabled))
    return [...list].sort((a, b) => {
      const d = new Date(a.created_at).getTime() - new Date(b.created_at).getTime()
      return order === 'newest' ? -d : d
    })
  }, [connections, q, status, order, srcName, destName])

  const groups = useMemo(() => {
    const m = new Map<string, Connection[]>()
    for (const c of filtered) {
      const arr = m.get(c.source_id) ?? []
      arr.push(c)
      m.set(c.source_id, arr)
    }
    return [...m.entries()]
  }, [filtered])

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title="Connections"
        actions={
          <div className="flex items-center gap-2">
            <div className="flex rounded-md border border-border p-0.5">
              <Link
                from={Route.fullPath}
                search={{}}
                className={
                  'rounded px-2.5 py-1 text-sm ' +
                  (view === 'structured' ? 'bg-muted font-medium' : 'text-muted-foreground')
                }
              >
                Structured
              </Link>
              <Link
                from={Route.fullPath}
                search={{ view: 'table' }}
                className={
                  'rounded px-2.5 py-1 text-sm ' +
                  (view === 'table' ? 'bg-muted font-medium' : 'text-muted-foreground')
                }
              >
                Table
              </Link>
            </div>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="h-4 w-4" /> New connection
            </Button>
          </div>
        }
      />

      <div className="flex flex-wrap items-center gap-3 border-b border-border px-6 py-3">
        <div className="relative min-w-[200px] flex-1 sm:max-w-xs">
          <Search className="pointer-events-none absolute top-1/2 left-3 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            className="pl-9"
            placeholder="Filter by name, source, destination…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
        </div>
        <Select value={status} onValueChange={(v) => setStatus(v ?? 'all')}>
          <SelectTrigger className="w-[140px]">
            <SelectValue>
              {(v: string | null) =>
                v === 'active' ? 'Active' : v === 'disabled' ? 'Disabled' : 'All statuses'
              }
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All statuses</SelectItem>
            <SelectItem value="active">Active</SelectItem>
            <SelectItem value="disabled">Disabled</SelectItem>
          </SelectContent>
        </Select>
        <Select value="source" onValueChange={() => {}}>
          <SelectTrigger className="w-[160px]">
            <SelectValue>{() => 'Group by Source'}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="source">Group by Source</SelectItem>
          </SelectContent>
        </Select>
        <Select value={order} onValueChange={(v) => setOrder(v as 'newest' | 'oldest')}>
          <SelectTrigger className="ml-auto w-[170px]">
            <SelectValue>
              {(v: string | null) => (v === 'oldest' ? 'Oldest → Newest' : 'Newest → Oldest')}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="newest">Newest → Oldest</SelectItem>
            <SelectItem value="oldest">Oldest → Newest</SelectItem>
          </SelectContent>
        </Select>
      </div>

      {view === 'structured' && (
        <div className="flex-1 space-y-8 overflow-x-auto px-6 py-6">
          {groups.map(([sourceId, conns]) => (
            <div key={sourceId} className="flex items-stretch">
              {/* Source node — vertically centered against its branches, with a
                  stub connector into the bus. */}
              <div className="flex shrink-0 items-center">
                <Link
                  to="/sources/$id"
                  params={{ id: sourceId }}
                  className="flex w-64 items-center gap-2.5 rounded-lg border border-border bg-card px-3.5 py-3 text-sm font-medium transition-colors hover:border-foreground/30"
                >
                  <Webhook className="h-4 w-4 shrink-0 text-muted-foreground" />
                  <span className="truncate">{srcName.get(sourceId) ?? sourceId}</span>
                </Link>
                <span className="h-px w-6 bg-border" />
              </div>

              {/* Bus column. The vertical trunk is drawn per-row so it spans
                  only from the first branch to the last (no overhang past the
                  ends); justify-center keeps the branch block aligned with the
                  centered source card so the stub meets the trunk. */}
              <div className="relative flex flex-1 flex-col justify-center">
                {conns.map((c, i) => (
                  <div key={c.id} className="relative flex items-center gap-2 py-2.5 pl-6">
                    {/* vertical trunk segment (omitted when there's one branch) */}
                    {conns.length > 1 ? (
                      <span
                        className={
                          'absolute left-0 w-px bg-border ' +
                          (i === 0
                            ? 'top-1/2 bottom-0'
                            : i === conns.length - 1
                              ? 'top-0 bottom-1/2'
                              : 'inset-y-0')
                        }
                      />
                    ) : null}
                    {/* branch from the trunk to this row */}
                    <span
                      className={
                        'absolute left-0 top-1/2 w-6 border-t ' +
                        (c.enabled ? 'border-border' : 'border-dashed border-border')
                      }
                    />
                    {/* connection name pill on the edge */}
                    {c.name ? (
                      <span className="shrink-0 rounded border border-border bg-card px-2 py-0.5 font-mono text-xs uppercase tracking-wide text-muted-foreground">
                        {c.name}
                      </span>
                    ) : null}
                    {/* edge line running to the destination */}
                    <span
                      className={
                        'min-w-6 flex-1 border-t ' +
                        (c.enabled ? 'border-border' : 'border-dashed border-border')
                      }
                    />
                    {/* edge controls: enable/disable + go-to-retries */}
                    <button
                      type="button"
                      onClick={() => patch.mutate({ id: c.id, enabled: !c.enabled })}
                      disabled={patch.isPending}
                      title={c.enabled ? 'Disable connection' : 'Enable connection'}
                      aria-label={c.enabled ? 'Disable connection' : 'Enable connection'}
                      className="grid h-7 w-7 shrink-0 place-items-center rounded border border-border text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50"
                    >
                      {c.enabled ? <Pause className="h-3.5 w-3.5" /> : <Play className="h-3.5 w-3.5" />}
                    </button>
                    <Link
                      to="/connections/$id"
                      params={{ id: c.id }}
                      search={{ tab: 'events' }}
                      title="View deliveries & retry"
                      aria-label="View deliveries and retry"
                      className="grid h-7 w-7 shrink-0 place-items-center rounded border border-border text-muted-foreground transition-colors hover:text-foreground"
                    >
                      <RotateCcw className="h-3.5 w-3.5" />
                    </Link>
                    {/* destination node */}
                    <Link
                      to="/destinations/$id"
                      params={{ id: c.destination_id }}
                      className={
                        'flex w-64 shrink-0 items-center gap-2.5 rounded-lg border px-3.5 py-3 text-sm transition-colors hover:border-foreground/30 ' +
                        (c.enabled ? 'border-border bg-card' : 'border-dashed border-border bg-card/50 text-muted-foreground')
                      }
                    >
                      <Globe className="h-4 w-4 shrink-0 text-muted-foreground" />
                      <span className="truncate font-medium">
                        {destName.get(c.destination_id) ?? c.destination_id}
                      </span>
                    </Link>
                    {/* 24h delivery stat */}
                    <span className="w-24 shrink-0 text-right font-mono text-[11px] uppercase tracking-wide text-muted-foreground">
                      {statText(stats?.[c.id])}
                    </span>
                    {/* expand to detail + overflow menu */}
                    <Link
                      to="/connections/$id"
                      params={{ id: c.id }}
                      title="Open connection"
                      aria-label="Open connection"
                      className="grid h-7 w-7 shrink-0 place-items-center rounded text-muted-foreground transition-colors hover:text-foreground"
                    >
                      <Maximize2 className="h-3.5 w-3.5" />
                    </Link>
                    <ConnectionRowMenu
                      connection={c}
                      onView={() => navigate({ to: '/connections/$id', params: { id: c.id } })}
                      onToggle={() => patch.mutate({ id: c.id, enabled: !c.enabled })}
                      onDelete={() => setDeleteTarget(c)}
                      pending={patch.isPending}
                    />
                  </div>
                ))}
              </div>
            </div>
          ))}
          {groups.length === 0 && (
            <p className="py-12 text-center text-sm text-muted-foreground">
              {connections && connections.length > 0
                ? 'No connections match these filters.'
                : 'No connections yet — connect a source to a destination to start routing events.'}
            </p>
          )}
        </div>
      )}

      {view === 'table' && (
      <div className="flex-1 overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Name</TableHead>
              <TableHead>Source</TableHead>
              <TableHead className="w-[40px]" />
              <TableHead>Destination</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Retry</TableHead>
              <TableHead>24h</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="w-[52px] pr-6" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {filtered.map((c) => (
              <TableRow key={c.id}>
                <TableCell className="pl-6 font-medium">
                  {c.name || <span className="text-muted-foreground">(unnamed)</span>}
                </TableCell>
                <TableCell>
                  <Link
                    to="/sources/$id"
                    params={{ id: c.source_id }}
                    className="flex items-center gap-2.5 font-medium hover:underline"
                  >
                    <Inbox className="h-4 w-4 shrink-0 text-muted-foreground" />
                    {srcName.get(c.source_id) ?? c.source_id}
                  </Link>
                </TableCell>
                <TableCell>
                  <MoveRight className="h-4 w-4 text-muted-foreground" />
                </TableCell>
                <TableCell>
                  <Link
                    to="/destinations/$id"
                    params={{ id: c.destination_id }}
                    className="flex items-center gap-2.5 font-medium hover:underline"
                  >
                    <Send className="h-4 w-4 shrink-0 text-muted-foreground" />
                    {destName.get(c.destination_id) ?? c.destination_id}
                  </Link>
                </TableCell>
                <TableCell>
                  {c.enabled ? (
                    <Badge variant="success">Active</Badge>
                  ) : (
                    <Badge variant="secondary">Disabled</Badge>
                  )}
                </TableCell>
                <TableCell className="whitespace-nowrap text-muted-foreground">
                  {c.retry_strategy} · {c.max_retries} retries
                </TableCell>
                <TableCell className="whitespace-nowrap font-mono text-xs text-muted-foreground">
                  {statText(stats?.[c.id])}
                </TableCell>
                <TableCell className="whitespace-nowrap text-muted-foreground">
                  {new Date(c.created_at).toLocaleDateString()}
                </TableCell>
                <TableCell className="pr-6 text-right">
                  <ConnectionRowMenu
                    connection={c}
                    onView={() => navigate({ to: '/connections/$id', params: { id: c.id } })}
                    onToggle={() => patch.mutate({ id: c.id, enabled: !c.enabled })}
                    onDelete={() => setDeleteTarget(c)}
                    pending={patch.isPending}
                  />
                </TableCell>
              </TableRow>
            ))}
            {filtered.length === 0 && (
              <TableRow>
                <TableCell colSpan={9} className="py-12 text-center text-sm text-muted-foreground">
                  {connections && connections.length > 0
                    ? 'No connections match these filters.'
                    : 'No connections yet — connect a source to a destination to start routing events.'}
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>
      )}

      <footer className="border-t border-border px-6 py-3 text-sm text-muted-foreground">
        Viewing {filtered.length} {filtered.length === 1 ? 'connection' : 'connections'}
      </footer>

      <CreateConnectionDialog open={createOpen} onOpenChange={setCreateOpen} />

      <Dialog open={!!deleteTarget} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              Delete connection{' '}
              {deleteTarget
                ? `${srcName.get(deleteTarget.source_id) ?? '…'} → ${destName.get(deleteTarget.destination_id) ?? '…'}`
                : ''}
              ?
            </DialogTitle>
            <DialogDescription>
              New events from this source stop routing to this destination. Already-queued
              deliveries are unaffected.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setDeleteTarget(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={remove.isPending}
              onClick={() => deleteTarget && remove.mutate(deleteTarget.id)}
            >
              {remove.isPending ? 'Deleting…' : 'Delete connection'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function ConnectionRowMenu({
  connection,
  onView,
  onToggle,
  onDelete,
  pending,
}: {
  connection: Connection
  onView: () => void
  onToggle: () => void
  onDelete: () => void
  pending: boolean
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          size="icon"
          variant="ghost"
          className="h-8 w-8"
          aria-label="Connection actions"
          disabled={pending}
        >
          <MoreHorizontal className="h-4 w-4" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-40">
        <DropdownMenuItem onClick={onView}>View details</DropdownMenuItem>
        <DropdownMenuItem onClick={onToggle}>
          {connection.enabled ? 'Disable' : 'Enable'}
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem onClick={onDelete} className="text-destructive">
          Delete
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

function CreateConnectionDialog({
  open,
  onOpenChange,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const qc = useQueryClient()
  const { data: sources } = useQuery(sourcesQuery)
  const { data: destinations } = useQuery(destinationsQuery)
  const [name, setName] = useState('')
  const [sourceId, setSourceId] = useState('')
  const [destinationId, setDestinationId] = useState('')

  const create = useMutation({
    mutationFn: () =>
      api.createConnection({
        source_id: sourceId,
        destination_id: destinationId,
        name: name || undefined,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['connections'] })
      toast.success('Connection created')
      onOpenChange(false)
      setName('')
      setSourceId('')
      setDestinationId('')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const srcLabel = (id: string | null) =>
    (sources ?? []).find((s) => s.id === id)?.name ?? 'Select a source'
  const destLabel = (id: string | null) =>
    (destinations ?? []).find((d) => d.id === id)?.name ?? 'Select a destination'

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New connection</DialogTitle>
          <DialogDescription>
            Route events from a source to a destination. Retry policy defaults apply; edit via
            API for now.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            create.mutate()
          }}
          className="space-y-4"
        >
          <div>
            <Label htmlFor="conn-name" className="mb-2 block">
              Name <span className="text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="conn-name"
              className="w-full"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="stripe → slack alerts"
            />
          </div>
          <div>
            <Label className="mb-2 block">Source</Label>
            <Select value={sourceId} onValueChange={(v) => setSourceId(v ?? '')}>
              <SelectTrigger className="w-full">
                <SelectValue>{srcLabel}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {(sources ?? []).map((s) => (
                  <SelectItem key={s.id} value={s.id}>
                    {s.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div>
            <Label className="mb-2 block">Destination</Label>
            <Select value={destinationId} onValueChange={(v) => setDestinationId(v ?? '')}>
              <SelectTrigger className="w-full">
                <SelectValue>{destLabel}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {(destinations ?? []).map((d) => (
                  <SelectItem key={d.id} value={d.id}>
                    {d.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={create.isPending || !sourceId || !destinationId}>
              {create.isPending ? 'Creating…' : 'Create connection'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
