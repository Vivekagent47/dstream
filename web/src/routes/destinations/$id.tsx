import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { Pencil, Send, TerminalSquare, Trash2 } from 'lucide-react'

import { api, qk, type Connection, type Destination } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { CopyValue, DetailRow } from '#/components/detail-page'
import { DestinationMetrics } from '#/components/entity-metrics'
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
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'

const TABS = [
  { key: 'overview', label: 'Overview' },
  { key: 'metrics', label: 'Metrics' },
  { key: 'connections', label: 'Connections' },
  { key: 'settings', label: 'Settings' },
] as const
type Tab = (typeof TABS)[number]['key']

const destinationQuery = (id: string) =>
  queryOptions({ queryKey: qk.destination(id), queryFn: () => api.getDestination(id) })

// Server ignores connection filter params and returns all org connections —
// filter client-side until a server-side filter exists.
const connectionsQuery = (id: string) =>
  queryOptions({
    queryKey: qk.connections(id),
    queryFn: () => api.listConnections(id),
    select: (rows: Connection[]) => rows.filter((c) => c.destination_id === id),
  })

export const Route = createFileRoute('/destinations/$id')({
  validateSearch: (search: Record<string, unknown>): { tab?: Tab } => {
    const t = search.tab as Tab
    return TABS.some((x) => x.key === t) && t !== 'overview' ? { tab: t } : {}
  },
  loader: ({ context, params }) =>
    typeof window === 'undefined'
      ? undefined
      : context.queryClient.ensureQueryData(destinationQuery(params.id)),
  component: DestinationDetail,
  errorComponent: AuthErrorBoundary,
})

function DestinationDetail() {
  const { id } = Route.useParams()
  const { tab = 'overview' } = Route.useSearch()
  const { data: dest } = useQuery(destinationQuery(id))
  const { data: connections } = useQuery(connectionsQuery(id))

  if (!dest) {
    return (
      <div className="flex flex-1 flex-col">
        <PageHeader title="Destination" />
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
              to="/destinations"
              className="font-normal text-muted-foreground hover:text-foreground"
            >
              Destinations
            </Link>
            <span className="font-normal text-muted-foreground">/</span>
            <span className="truncate">{dest.name}</span>
          </span>
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
            {t.key === 'connections' && connections && connections.length > 0 ? (
              <Badge variant="secondary" className="px-1.5">{connections.length}</Badge>
            ) : null}
          </Link>
        ))}
      </nav>

      <div className="flex-1 overflow-y-auto px-6 py-4">
        {tab === 'overview' && <OverviewTab dest={dest} />}
        {tab === 'metrics' && <DestinationMetrics id={dest.id} />}
        {tab === 'connections' && <ConnectionsTab connections={connections} />}
        {tab === 'settings' && <SettingsTab dest={dest} />}
      </div>
    </div>
  )
}

function limitText(v: number | null, unit?: string) {
  return v == null ? '—' : unit ? `${v} ${unit}` : String(v)
}

function OverviewTab({ dest }: { dest: Destination }) {
  const navigate = useNavigate({ from: Route.fullPath })
  return (
    <div className="grid min-h-full gap-6 lg:grid-cols-[380px_1fr]">
      {/* Destination details */}
      <div className="space-y-3 lg:border-r lg:border-border lg:pr-6">
        <div className="flex items-center justify-between">
          <h2 className="text-base font-semibold">Destination details</h2>
          <Button
            size="sm"
            variant="outline"
            onClick={() => navigate({ search: { tab: 'settings' } })}
          >
            <Pencil className="h-3.5 w-3.5" /> Edit
          </Button>
        </div>
        <div className="space-y-3">
          <DetailRow label="Name">
            <CopyValue value={dest.name} what="Name" />
          </DetailRow>
          {dest.description ? (
            <DetailRow label="Description">{dest.description}</DetailRow>
          ) : null}
          <DetailRow label="Type">
            {dest.type === 'http' ? (
              <Badge variant="secondary" className="gap-1">
                <Send className="h-3 w-3" /> HTTP
              </Badge>
            ) : (
              <Badge variant="secondary" className="gap-1">
                <TerminalSquare className="h-3 w-3" /> CLI
              </Badge>
            )}
          </DetailRow>
          {dest.type === 'http' && dest.url ? (
            <DetailRow label="URL">
              <CopyValue value={dest.url} what="URL" mono />
            </DetailRow>
          ) : null}

          <div className="border-t border-border pt-3 text-sm font-semibold">Delivery limits</div>
          <DetailRow label="Rate limit">{limitText(dest.rate_limit_rps, 'req/s')}</DetailRow>
          <DetailRow label="Burst">{limitText(dest.rate_limit_burst)}</DetailRow>
          <DetailRow label="Max in-flight">{limitText(dest.max_inflight)}</DetailRow>

          <div className="border-t border-border pt-3 text-sm font-semibold">Metadata</div>
          <DetailRow label="Destination ID">
            <CopyValue value={dest.id} what="Destination ID" mono />
          </DetailRow>
          <DetailRow label="Created at">{new Date(dest.created_at).toLocaleString()}</DetailRow>
          <DetailRow label="Last updated">{new Date(dest.updated_at).toLocaleString()}</DetailRow>
        </div>
      </div>

      {/* Metrics overview */}
      <div className="flex min-h-0 flex-col gap-3">
        <h2 className="text-base font-semibold">Metrics overview</h2>
        <DestinationMetrics id={dest.id} />
      </div>
    </div>
  )
}

function ConnectionsTab({ connections }: { connections: Connection[] | undefined }) {
  const { data: sources } = useQuery({
    queryKey: qk.sources(),
    queryFn: () => api.listSources(),
  })
  const sourceName = (id: string) => sources?.find((s) => s.id === id)?.name ?? id

  if (!connections) return <p className="text-sm text-muted-foreground">Loading…</p>
  if (connections.length === 0) {
    return (
      <p className="py-8 text-center text-sm text-muted-foreground">
        No connections yet. Connect a source to this destination to start routing events.
      </p>
    )
  }
  return (
    <div className="rounded-lg border border-border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="pl-6">Source</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Retry strategy</TableHead>
            <TableHead>Created</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {connections.map((c) => (
            <TableRow key={c.id}>
              <TableCell className="pl-6 font-medium">
                <Link to="/sources/$id" params={{ id: c.source_id }} className="hover:underline">
                  {sourceName(c.source_id)}
                </Link>
              </TableCell>
              <TableCell>
                {c.enabled ? (
                  <Badge className="bg-emerald-500/15 text-emerald-600 hover:bg-emerald-500/15 dark:text-emerald-400">
                    Active
                  </Badge>
                ) : (
                  <Badge variant="secondary">Disabled</Badge>
                )}
              </TableCell>
              <TableCell className="text-muted-foreground">{c.retry_strategy}</TableCell>
              <TableCell className="text-muted-foreground">
                {new Date(c.created_at).toLocaleDateString()}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

// Number input helper: '' ⇄ null round-trip for the nullable limit fields.
function numOrNull(s: string): number | null {
  if (s.trim() === '') return null
  const n = Number(s)
  return Number.isFinite(n) && n >= 0 ? Math.floor(n) : null
}

function SettingsTab({ dest }: { dest: Destination }) {
  const qc = useQueryClient()
  const navigate = useNavigate()

  const [name, setName] = useState(dest.name)
  const [description, setDescription] = useState(dest.description)
  const [url, setUrl] = useState(dest.url ?? '')
  const [rps, setRps] = useState(dest.rate_limit_rps?.toString() ?? '')
  const [burst, setBurst] = useState(dest.rate_limit_burst?.toString() ?? '')
  const [inflight, setInflight] = useState(dest.max_inflight?.toString() ?? '')
  const [deleteOpen, setDeleteOpen] = useState(false)

  // Re-seed local form state when the server row changes (e.g. after save).
  useEffect(() => {
    setName(dest.name)
    setDescription(dest.description)
    setUrl(dest.url ?? '')
    setRps(dest.rate_limit_rps?.toString() ?? '')
    setBurst(dest.rate_limit_burst?.toString() ?? '')
    setInflight(dest.max_inflight?.toString() ?? '')
  }, [dest])

  const save = useMutation({
    mutationFn: () =>
      api.patchDestination(dest.id, {
        name,
        description,
        ...(dest.type === 'http' ? { url } : {}),
        rate_limit_rps: numOrNull(rps),
        rate_limit_burst: numOrNull(burst),
        max_inflight: numOrNull(inflight),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.destination(dest.id) })
      qc.invalidateQueries({ queryKey: qk.destinations() })
      toast.success('Destination saved')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: () => api.deleteDestination(dest.id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.destinations() })
      toast.success('Destination deleted')
      navigate({ to: '/destinations' })
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const dirty =
    name !== dest.name ||
    description !== dest.description ||
    (dest.type === 'http' && url !== (dest.url ?? '')) ||
    numOrNull(rps) !== dest.rate_limit_rps ||
    numOrNull(burst) !== dest.rate_limit_burst ||
    numOrNull(inflight) !== dest.max_inflight

  return (
    <div className="max-w-3xl space-y-8">
      {/* General */}
      <section className="space-y-4">
        <div>
          <Label htmlFor="name" className="mb-2 block">Name</Label>
          <Input id="name" value={name} onChange={(e) => setName(e.target.value)} className="w-full" />
        </div>
        <div>
          <Label htmlFor="description" className="mb-2 block">
            Description <span className="text-muted-foreground">(optional)</span>
          </Label>
          <Input
            id="description"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            className="w-full"
          />
        </div>
        {dest.type === 'http' ? (
          <div>
            <Label htmlFor="url" className="mb-2 block">URL</Label>
            <Input
              id="url"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="https://example.com/webhooks"
              className="w-full font-mono text-xs"
            />
          </div>
        ) : null}
        <Button size="sm" onClick={() => save.mutate()} disabled={!dirty || save.isPending}>
          {save.isPending ? 'Saving…' : 'Save'}
        </Button>
      </section>

      {/* Delivery limits */}
      <section className="space-y-3 border-t border-border pt-6">
        <h2 className="text-sm font-semibold">Delivery limits</h2>
        <p className="text-sm text-muted-foreground">
          Empty means unlimited. Rate limit caps deliveries per second; burst allows short
          spikes above it; max in-flight caps concurrent deliveries.
        </p>
        <div className="grid gap-4 sm:grid-cols-3">
          <div>
            <Label htmlFor="rps" className="mb-2 block">Rate limit (req/s)</Label>
            <Input id="rps" type="number" min="0" value={rps} onChange={(e) => setRps(e.target.value)} />
          </div>
          <div>
            <Label htmlFor="burst" className="mb-2 block">Burst</Label>
            <Input id="burst" type="number" min="0" value={burst} onChange={(e) => setBurst(e.target.value)} />
          </div>
          <div>
            <Label htmlFor="inflight" className="mb-2 block">Max in-flight</Label>
            <Input id="inflight" type="number" min="0" value={inflight} onChange={(e) => setInflight(e.target.value)} />
          </div>
        </div>
      </section>

      {/* Delete */}
      <section className="space-y-2 border-t border-border pt-6">
        <h2 className="text-sm font-semibold text-destructive">Delete destination</h2>
        <p className="text-sm text-muted-foreground">
          Deletes this destination and all associated connections. Deliveries to it will stop.
        </p>
        <Button variant="destructive" onClick={() => setDeleteOpen(true)}>
          <Trash2 className="h-4 w-4" /> Delete destination
        </Button>
      </section>

      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {dest.name}?</DialogTitle>
            <DialogDescription>
              This removes the destination and its connections. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setDeleteOpen(false)}>Cancel</Button>
            <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
              {remove.isPending ? 'Deleting…' : 'Delete destination'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
