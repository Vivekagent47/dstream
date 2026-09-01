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
import { Plus, Trash2 } from 'lucide-react'

import {
  api,
  qk,
  type Application,
  type Endpoint,
  type Page,
  type Message,
} from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { CopyValue, DetailRow } from '#/components/detail-page'
import { PageHeader } from '#/components/TopBar'
import { RevealSecretDialog } from '#/components/outbound/RevealSecretDialog'
import { SendMessageDialog } from '#/components/outbound/SendMessageDialog'
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
  { key: 'endpoints', label: 'Endpoints' },
  { key: 'messages', label: 'Messages' },
  { key: 'settings', label: 'Settings' },
] as const
type Tab = (typeof TABS)[number]['key']

// Shared textarea styling — there is no ui/textarea component and JSON needs a
// multi-line, monospace field. Mirrors ui/input's border/focus treatment.
const textareaClass =
  'flex min-h-[120px] w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs shadow-sm transition-colors placeholder:text-muted-foreground focus-visible:ring-1 focus-visible:ring-ring focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

const applicationQuery = (id: string) =>
  queryOptions({ queryKey: qk.application(id), queryFn: () => api.getApplication(id) })

const eventTypesQuery = queryOptions({
  queryKey: qk.eventTypes(),
  queryFn: () => api.listEventTypes(),
})

export const Route = createFileRoute('/applications/$id')({
  validateSearch: (search: Record<string, unknown>): { tab?: Tab } => {
    const t = search.tab as Tab
    return TABS.some((x) => x.key === t) && t !== 'overview' ? { tab: t } : {}
  },
  // Client-only prefetch — SSR can't forward the session cookie.
  loader: ({ context, params }) =>
    typeof window === 'undefined'
      ? undefined
      : context.queryClient.ensureQueryData(applicationQuery(params.id)),
  component: ApplicationDetail,
  errorComponent: AuthErrorBoundary,
})

// Metadata is an arbitrary JSON blob; pretty-print for display / editing.
function fmtMetadata(m: unknown): string {
  return m == null ? '' : JSON.stringify(m, null, 2)
}

function endpointStatus(e: Endpoint): {
  label: string
  variant: React.ComponentProps<typeof Badge>['variant']
} {
  if (!e.disabled) return { label: 'active', variant: 'success' }
  return e.disabled_at
    ? { label: 'auto-disabled', variant: 'warning' }
    : { label: 'disabled', variant: 'secondary' }
}

function ApplicationDetail() {
  const { id } = Route.useParams()
  const { tab = 'overview' } = Route.useSearch()
  const [sendOpen, setSendOpen] = useState(false)
  const { data: app } = useQuery(applicationQuery(id))

  if (!app) {
    return (
      <div className="flex flex-1 flex-col">
        <PageHeader title="Application" />
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
              to="/applications"
              className="font-normal text-muted-foreground hover:text-foreground"
            >
              Applications
            </Link>
            <span className="font-normal text-muted-foreground">/</span>
            <span className="truncate">{app.name}</span>
          </span>
        }
        actions={
          <Button size="sm" onClick={() => setSendOpen(true)}>
            Send message
          </Button>
        }
      />

      <SendMessageDialog appId={id} open={sendOpen} onOpenChange={setSendOpen} />

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
        {tab === 'overview' && <OverviewTab app={app} />}
        {tab === 'endpoints' && <EndpointsTab appId={id} />}
        {tab === 'messages' && <MessagesTab appId={id} />}
        {tab === 'settings' && <SettingsTab app={app} />}
      </div>
    </div>
  )
}

function OverviewTab({ app }: { app: Application }) {
  const { data: endpoints } = useQuery({
    queryKey: qk.endpoints(app.id),
    queryFn: () => api.listEndpoints(app.id),
  })
  return (
    <div className="max-w-2xl space-y-3">
      <h2 className="text-base font-semibold">Application details</h2>
      <div className="space-y-3">
        <DetailRow label="Application ID">
          <CopyValue value={app.id} what="Application ID" mono />
        </DetailRow>
        <DetailRow label="UID">
          {app.uid ? <span className="font-mono text-xs">{app.uid}</span> : '—'}
        </DetailRow>
        <DetailRow label="Name">{app.name}</DetailRow>
        <DetailRow label="Endpoints">{endpoints?.length ?? '—'}</DetailRow>
        <DetailRow label="Metadata">
          {app.metadata != null ? (
            <pre className="overflow-x-auto rounded border border-border bg-muted px-3 py-2 font-mono text-xs">
              {fmtMetadata(app.metadata)}
            </pre>
          ) : (
            '—'
          )}
        </DetailRow>
        <DetailRow label="Created at">{new Date(app.created_at).toLocaleString()}</DetailRow>
      </div>
    </div>
  )
}

function EndpointsTab({ appId }: { appId: string }) {
  const [addOpen, setAddOpen] = useState(false)
  const [revealSecret, setRevealSecret] = useState<string | null>(null)
  const { data: endpoints, error } = useQuery({
    queryKey: qk.endpoints(appId),
    queryFn: () => api.listEndpoints(appId),
  })

  const rows = endpoints ?? []

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <h2 className="text-base font-semibold">Endpoints</h2>
        <Button size="sm" onClick={() => setAddOpen(true)}>
          <Plus className="h-4 w-4" /> Add endpoint
        </Button>
      </div>

      {error && <p className="py-3 text-sm text-destructive">{(error as Error).message}</p>}

      <div className="rounded-lg border border-border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">URL</TableHead>
              <TableHead>Filter</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="pr-6">Failures</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((e) => {
              const s = endpointStatus(e)
              return (
                <TableRow key={e.id}>
                  <TableCell className="pl-6 font-mono text-xs">
                    <Link
                      to="/applications/$id/endpoints/$endpointId"
                      params={{ id: appId, endpointId: e.id }}
                      className="text-primary hover:underline"
                    >
                      {e.url}
                    </Link>
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {e.filter_event_types?.join(', ') || 'all'}
                  </TableCell>
                  <TableCell>
                    <Badge variant={s.variant}>{s.label}</Badge>
                  </TableCell>
                  <TableCell className="pr-6">{e.consecutive_failures}</TableCell>
                </TableRow>
              )
            })}
            {rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={4} className="py-12 text-center text-sm text-muted-foreground">
                  No endpoints yet — add one to start delivering messages.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <AddEndpointDialog
        appId={appId}
        open={addOpen}
        onOpenChange={setAddOpen}
        onCreated={(secret) => setRevealSecret(secret)}
      />
      <RevealSecretDialog secret={revealSecret} onClose={() => setRevealSecret(null)} />
    </div>
  )
}

function AddEndpointDialog({
  appId,
  open,
  onOpenChange,
  onCreated,
}: {
  appId: string
  open: boolean
  onOpenChange: (o: boolean) => void
  onCreated: (secret: string) => void
}) {
  const qc = useQueryClient()
  const { data: eventTypes } = useQuery(eventTypesQuery)
  const [url, setUrl] = useState('')
  const [uid, setUid] = useState('')
  const [description, setDescription] = useState('')
  const [filters, setFilters] = useState<Set<string>>(new Set())

  const active = (eventTypes ?? []).filter((et) => !et.archived)

  const create = useMutation({
    mutationFn: () =>
      api.createEndpoint(appId, {
        url,
        ...(uid.trim() ? { uid: uid.trim() } : {}),
        ...(description.trim() ? { description: description.trim() } : {}),
        ...(filters.size > 0 ? { filter_event_types: [...filters] } : {}),
      }),
    onSuccess: (result) => {
      qc.invalidateQueries({ queryKey: qk.endpoints(appId) })
      onOpenChange(false)
      setUrl('')
      setUid('')
      setDescription('')
      setFilters(new Set())
      onCreated(result.secret)
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function toggle(name: string, checked: boolean) {
    setFilters((prev) => {
      const next = new Set(prev)
      if (checked) next.add(name)
      else next.delete(name)
      return next
    })
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Add endpoint</DialogTitle>
          <DialogDescription>
            Where this application&rsquo;s messages are delivered. A signing secret is generated
            and shown once on creation.
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
            <Label htmlFor="ep-url" className="mb-2 block">
              URL
            </Label>
            <Input
              id="ep-url"
              type="url"
              className="w-full font-mono"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="https://example.com/webhooks"
              required
              autoFocus
            />
          </div>
          <div>
            <Label htmlFor="ep-uid" className="mb-2 block">
              UID <span className="text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="ep-uid"
              className="w-full font-mono"
              value={uid}
              onChange={(e) => setUid(e.target.value)}
              placeholder="prod-endpoint"
            />
          </div>
          <div>
            <Label htmlFor="ep-description" className="mb-2 block">
              Description <span className="text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="ep-description"
              className="w-full"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Production receiver"
            />
          </div>
          <div>
            <Label className="mb-2 block">
              Event types <span className="text-muted-foreground">(none = all)</span>
            </Label>
            {active.length === 0 ? (
              <p className="text-sm text-muted-foreground">No event types defined.</p>
            ) : (
              <div className="max-h-40 space-y-1.5 overflow-y-auto rounded-md border border-border p-3">
                {active.map((et) => (
                  <label key={et.id} className="flex items-center gap-2 text-sm">
                    <input
                      type="checkbox"
                      className="h-4 w-4 rounded border-input"
                      checked={filters.has(et.name)}
                      onChange={(e) => toggle(et.name, e.target.checked)}
                    />
                    <span className="font-mono text-xs">{et.name}</span>
                  </label>
                ))}
              </div>
            )}
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={create.isPending || !url.trim()}>
              {create.isPending ? 'Adding…' : 'Add endpoint'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function MessagesTab({ appId }: { appId: string }) {
  const { data, error, fetchNextPage, hasNextPage, isFetchingNextPage } = useInfiniteQuery({
    queryKey: qk.messages(appId),
    queryFn: ({ pageParam }) => api.listMessages(appId, pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last: Page<Message>) => last.next_cursor ?? undefined,
    refetchInterval: 5000,
  })

  const messages = data?.pages.flatMap((p) => p.data) ?? []

  return (
    <div className="space-y-3">
      {error && <p className="py-3 text-sm text-destructive">{(error as Error).message}</p>}
      <div className="rounded-lg border border-border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Event type</TableHead>
              <TableHead>Event ID</TableHead>
              <TableHead className="pr-6">Created</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {messages.map((m) => (
              <TableRow key={m.id}>
                <TableCell className="pl-6 font-mono text-xs">
                  <Link
                    to="/applications/$id/messages/$messageId"
                    params={{ id: appId, messageId: m.id }}
                    className="text-primary hover:underline"
                  >
                    {m.event_type}
                  </Link>
                </TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">
                  {m.event_id || '—'}
                </TableCell>
                <TableCell className="pr-6 whitespace-nowrap text-muted-foreground">
                  {new Date(m.created_at).toLocaleString()}
                </TableCell>
              </TableRow>
            ))}
            {messages.length === 0 && (
              <TableRow>
                <TableCell colSpan={3} className="py-12 text-center text-sm text-muted-foreground">
                  No messages yet — send one to see it here.
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

function SettingsTab({ app }: { app: Application }) {
  const qc = useQueryClient()
  const navigate = useNavigate()

  const [name, setName] = useState(app.name)
  const [uid, setUid] = useState(app.uid ?? '')
  const [metadata, setMetadata] = useState(fmtMetadata(app.metadata))
  const [deleteOpen, setDeleteOpen] = useState(false)

  // Seed the form once per application (mount / navigation to another id), NOT
  // on every refetch — a background refetch (poll, concurrent edit) would
  // otherwise wipe in-progress edits.
  const seededId = useRef<string | null>(null)
  useEffect(() => {
    if (seededId.current === app.id) return
    seededId.current = app.id
    setName(app.name)
    setUid(app.uid ?? '')
    setMetadata(fmtMetadata(app.metadata))
  }, [app])

  const save = useMutation({
    mutationFn: (parsedMetadata: unknown) =>
      api.updateApplication(app.id, {
        name,
        uid: uid.trim() ? uid.trim() : undefined,
        ...(parsedMetadata !== undefined ? { metadata: parsedMetadata } : {}),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.application(app.id) })
      qc.invalidateQueries({ queryKey: qk.applications() })
      toast.success('Application saved')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: () => api.deleteApplication(app.id),
    onSuccess: () => {
      // Drop the detail cache — the resource is gone, so a later Back
      // navigation must refetch (404) instead of rendering the stale row.
      qc.removeQueries({ queryKey: qk.application(app.id) })
      qc.invalidateQueries({ queryKey: qk.applications() })
      toast.success('Application deleted')
      navigate({ to: '/applications' })
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function onSave() {
    const trimmed = metadata.trim()
    let parsed: unknown = undefined
    if (trimmed) {
      try {
        parsed = JSON.parse(trimmed)
      } catch {
        toast.error('invalid metadata JSON')
        return
      }
    }
    save.mutate(parsed)
  }

  const dirty =
    name !== app.name ||
    uid !== (app.uid ?? '') ||
    metadata !== fmtMetadata(app.metadata)

  return (
    <div className="max-w-3xl space-y-8">
      <section className="space-y-4">
        <h2 className="text-sm font-semibold">Application settings</h2>
        <div>
          <Label htmlFor="app-name" className="mb-2 block">
            Name
          </Label>
          <Input
            id="app-name"
            className="w-full"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Acme Inc"
          />
        </div>
        <div>
          <Label htmlFor="app-uid" className="mb-2 block">
            UID <span className="text-muted-foreground">(optional)</span>
          </Label>
          <Input
            id="app-uid"
            className="w-full font-mono"
            value={uid}
            onChange={(e) => setUid(e.target.value)}
            placeholder="acme-inc"
          />
        </div>
        <div>
          <Label htmlFor="app-metadata" className="mb-2 block">
            Metadata JSON <span className="text-muted-foreground">(optional)</span>
          </Label>
          <textarea
            id="app-metadata"
            className={textareaClass}
            value={metadata}
            onChange={(e) => setMetadata(e.target.value)}
            placeholder='{ "plan": "pro" }'
          />
        </div>
        <Button size="sm" onClick={onSave} disabled={!dirty || save.isPending || !name.trim()}>
          {save.isPending ? 'Saving…' : 'Save'}
        </Button>
      </section>

      <section className="space-y-2 border-t border-border pt-6">
        <h2 className="text-sm font-semibold text-destructive">Delete application</h2>
        <p className="text-sm text-muted-foreground">
          Removes the application, its endpoints, and message history. This cannot be undone.
        </p>
        <Button variant="destructive" onClick={() => setDeleteOpen(true)}>
          <Trash2 className="h-4 w-4" /> Delete application
        </Button>
      </section>

      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete this application?</DialogTitle>
            <DialogDescription>
              Its endpoints and message history are removed. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setDeleteOpen(false)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={remove.isPending}
              onClick={() => remove.mutate()}
            >
              {remove.isPending ? 'Deleting…' : 'Delete application'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
