import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { Copy, MinusCircle, Pencil, Trash2, Webhook } from 'lucide-react'

import { api, qk, type Connection, type Source } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { CopyValue, DetailRow, copyText } from '#/components/detail-page'
import { SourceMetrics } from '#/components/entity-metrics'
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

const SUPPORTED = ['POST', 'PUT', 'PATCH', 'DELETE'] as const

const TABS = [
  { key: 'overview', label: 'Overview' },
  { key: 'metrics', label: 'Metrics' },
  { key: 'connections', label: 'Connections' },
  { key: 'settings', label: 'Settings' },
] as const
type Tab = (typeof TABS)[number]['key']

const sourceQuery = (id: string) =>
  queryOptions({ queryKey: qk.source(id), queryFn: () => api.getSource(id) })

// Server ignores the source_id param and returns all org connections —
// filter client-side until a server-side filter exists.
const connectionsQuery = (id: string) =>
  queryOptions({
    queryKey: qk.connections(id),
    queryFn: () => api.listConnections(id),
    select: (rows: Connection[]) => rows.filter((c) => c.source_id === id),
  })

export const Route = createFileRoute('/sources/$id')({
  validateSearch: (search: Record<string, unknown>): { tab?: Tab } => {
    const t = search.tab as Tab
    return TABS.some((x) => x.key === t) && t !== 'overview' ? { tab: t } : {}
  },
  loader: ({ context, params }) =>
    typeof window === 'undefined'
      ? undefined
      : context.queryClient.ensureQueryData(sourceQuery(params.id)),
  component: SourceDetail,
  errorComponent: AuthErrorBoundary,
})

function ingestUrl(token: string): string {
  const origin = typeof window === 'undefined' ? '' : window.location.origin
  return `${origin}/e/${token}`
}

function SourceDetail() {
  const { id } = Route.useParams()
  const { tab = 'overview' } = Route.useSearch()
  const { data: src } = useQuery(sourceQuery(id))
  const { data: connections } = useQuery(connectionsQuery(id))

  if (!src) {
    return (
      <div className="flex flex-1 flex-col">
        <PageHeader title="Source" />
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
              to="/sources"
              className="font-normal text-muted-foreground hover:text-foreground"
            >
              Sources
            </Link>
            <span className="font-normal text-muted-foreground">/</span>
            <span className="truncate">{src.name}</span>
          </span>
        }
        actions={<ToggleEnabledButton src={src} />}
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
        {tab === 'overview' && <OverviewTab src={src} />}
        {tab === 'metrics' && <SourceMetrics id={src.id} />}
        {tab === 'connections' && <ConnectionsTab connections={connections} />}
        {tab === 'settings' && <SettingsTab src={src} />}
      </div>
    </div>
  )
}

function ToggleEnabledButton({ src }: { src: { id: string; enabled: boolean } }) {
  const qc = useQueryClient()
  const toggle = useMutation({
    mutationFn: () => api.updateSource(src.id, { enabled: !src.enabled }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.source(src.id) })
      qc.invalidateQueries({ queryKey: qk.sources() })
      toast.success(src.enabled ? 'Source disabled' : 'Source enabled')
    },
    onError: (e) => toast.error((e as Error).message),
  })
  return (
    <Button size="sm" variant="outline" onClick={() => toggle.mutate()} disabled={toggle.isPending}>
      <MinusCircle className="h-4 w-4" />
      {src.enabled ? 'Disable source' : 'Enable source'}
    </Button>
  )
}

function OverviewTab({ src }: { src: Source }) {
  const navigate = useNavigate({ from: Route.fullPath })
  return (
    <div className="grid min-h-full gap-6 lg:grid-cols-[380px_1fr]">
      {/* Source details */}
      <div className="space-y-3 lg:border-r lg:border-border lg:pr-6">
        <div className="flex items-center justify-between">
          <h2 className="text-base font-semibold">Source details</h2>
          <Button
            size="sm"
            variant="outline"
            onClick={() => navigate({ search: { tab: 'settings' } })}
          >
            <Pencil className="h-3.5 w-3.5" /> Edit
          </Button>
        </div>
        <div className="space-y-3">
          <DetailRow label="Status">
            {src.enabled ? (
              <Badge className="bg-emerald-500/15 text-emerald-600 hover:bg-emerald-500/15 dark:text-emerald-400">
                Active
              </Badge>
            ) : (
              <Badge variant="secondary">Disabled</Badge>
            )}
          </DetailRow>
          <DetailRow label="Name">
            <CopyValue value={src.name} what="Name" />
          </DetailRow>
          {src.description ? (
            <DetailRow label="Description">{src.description}</DetailRow>
          ) : null}

          <div className="border-t border-border pt-3 text-sm font-semibold">Configuration</div>
          <DetailRow label="Type">
            <Badge variant="secondary" className="gap-1">
              <Webhook className="h-3 w-3" /> Webhook
            </Badge>
          </DetailRow>
          <DetailRow label="URL">
            <CopyValue value={ingestUrl(src.ingest_token)} what="Ingest URL" mono />
          </DetailRow>
          <DetailRow label="HTTP methods">
            <span className="flex flex-wrap gap-1.5">
              {src.allowed_methods.map((m) => (
                <Badge key={m} variant="outline" className="font-mono text-xs">{m}</Badge>
              ))}
            </span>
          </DetailRow>

          <div className="border-t border-border pt-3 text-sm font-semibold">Metadata</div>
          <DetailRow label="Source ID">
            <CopyValue value={src.id} what="Source ID" mono />
          </DetailRow>
          <DetailRow label="Created at">
            {new Date(src.created_at).toLocaleString()}
          </DetailRow>
          <DetailRow label="Last updated">
            {new Date(src.updated_at).toLocaleString()}
          </DetailRow>
        </div>
      </div>

      {/* Metrics overview */}
      <div className="flex min-h-0 flex-col gap-3">
        <h2 className="text-base font-semibold">Metrics overview</h2>
        <SourceMetrics id={src.id} />
      </div>
    </div>
  )
}

function ConnectionsTab({ connections }: { connections: Connection[] | undefined }) {
  const { data: destinations } = useQuery({
    queryKey: qk.destinations(),
    queryFn: () => api.listDestinations(),
  })
  const destName = (id: string) => destinations?.find((d) => d.id === id)?.name ?? id

  if (!connections) return <p className="text-sm text-muted-foreground">Loading…</p>
  if (connections.length === 0) {
    return (
      <p className="py-8 text-center text-sm text-muted-foreground">
        No connections yet. Connect this source to a destination to start routing events.
      </p>
    )
  }
  return (
    <div className="rounded-lg border border-border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="pl-6">Destination</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Retry strategy</TableHead>
            <TableHead>Created</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {connections.map((c) => (
            <TableRow key={c.id}>
              <TableCell className="pl-6 font-medium">{destName(c.destination_id)}</TableCell>
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

function SettingsTab({ src }: { src: Source }) {
  const qc = useQueryClient()
  const navigate = useNavigate()

  const [name, setName] = useState(src.name)
  const [description, setDescription] = useState(src.description)
  const [methods, setMethods] = useState<string[]>(src.allowed_methods)
  const [deleteOpen, setDeleteOpen] = useState(false)

  // Re-seed local form state when the server row changes (e.g. after save).
  useEffect(() => {
    setName(src.name)
    setDescription(src.description)
    setMethods(src.allowed_methods)
  }, [src])

  const save = useMutation({
    mutationFn: () => api.updateSource(src.id, { name, description, allowed_methods: methods }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.source(src.id) })
      qc.invalidateQueries({ queryKey: qk.sources() })
      toast.success('Source saved')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: () => api.deleteSource(src.id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.sources() })
      toast.success('Source deleted')
      navigate({ to: '/sources' })
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const dirty =
    name !== src.name ||
    description !== src.description ||
    methods.slice().sort().join(',') !== src.allowed_methods.slice().sort().join(',')

  function toggleMethod(m: string) {
    setMethods((cur) => (cur.includes(m) ? cur.filter((x) => x !== m) : [...cur, m]))
  }

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
        <div>
          <Label className="mb-2 block">Ingest URL</Label>
          <button
            type="button"
            onClick={() => copyText(ingestUrl(src.ingest_token), 'Ingest URL')}
            className="group flex w-full items-center justify-between gap-2 rounded-md border border-border px-3 py-2 font-mono text-xs text-muted-foreground hover:text-foreground"
            title="Copy ingest URL"
          >
            <span className="truncate">{ingestUrl(src.ingest_token)}</span>
            <Copy className="h-3.5 w-3.5 shrink-0 opacity-60 group-hover:opacity-100" />
          </button>
        </div>
        <Button size="sm" onClick={() => save.mutate()} disabled={!dirty || save.isPending}>
          {save.isPending ? 'Saving…' : 'Save'}
        </Button>
      </section>

      {/* Advanced configuration */}
      <section className="space-y-3 border-t border-border pt-6">
        <h2 className="text-sm font-semibold">Advanced configuration</h2>
        <div>
          <Label className="mb-2 block">HTTP methods</Label>
          <div className="flex flex-wrap items-center gap-4">
            <label className="flex cursor-not-allowed items-center gap-2 text-sm text-muted-foreground">
              <input type="checkbox" disabled className="size-4 accent-primary" />
              GET <span className="text-xs">(not supported)</span>
            </label>
            {SUPPORTED.map((m) => (
              <label key={m} className="flex cursor-pointer items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  className="size-4 accent-primary"
                  checked={methods.includes(m)}
                  onChange={() => toggleMethod(m)}
                />
                {m}
              </label>
            ))}
          </div>
        </div>
      </section>

      {/* Delete */}
      <section className="space-y-2 border-t border-border pt-6">
        <h2 className="text-sm font-semibold text-destructive">Delete source</h2>
        <p className="text-sm text-muted-foreground">
          Deletes this source and all associated connections. Incoming webhooks to its URL will fail.
        </p>
        <Button variant="destructive" onClick={() => setDeleteOpen(true)}>
          <Trash2 className="h-4 w-4" /> Delete source
        </Button>
      </section>

      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {src.name}?</DialogTitle>
            <DialogDescription>
              This removes the source and its ingest URL. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setDeleteOpen(false)}>Cancel</Button>
            <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
              {remove.isPending ? 'Deleting…' : 'Delete source'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
