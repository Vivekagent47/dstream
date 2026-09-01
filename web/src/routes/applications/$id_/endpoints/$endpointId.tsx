import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import {
  queryOptions,
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { toast } from 'sonner'
import { Trash2 } from 'lucide-react'

import {
  api,
  qk,
  type Endpoint,
  type MessageDeliveryAttempt,
} from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { CopyValue, DetailRow } from '#/components/detail-page'
import { PageHeader } from '#/components/TopBar'
import { RevealSecretDialog } from '#/components/outbound/RevealSecretDialog'
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
  { key: 'deliveries', label: 'Deliveries' },
  { key: 'settings', label: 'Settings' },
] as const
type Tab = (typeof TABS)[number]['key']

// Shared textarea styling — there is no ui/textarea component and JSON needs a
// multi-line, monospace field. Mirrors ui/input's border/focus treatment.
const textareaClass =
  'flex min-h-[120px] w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs shadow-sm transition-colors placeholder:text-muted-foreground focus-visible:ring-1 focus-visible:ring-ring focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

const endpointQuery = (id: string, endpointId: string) =>
  queryOptions({
    queryKey: qk.endpoint(id, endpointId),
    queryFn: () => api.getEndpoint(id, endpointId),
  })

const eventTypesQuery = queryOptions({
  queryKey: qk.eventTypes(),
  queryFn: () => api.listEventTypes(),
})

export const Route = createFileRoute('/applications/$id_/endpoints/$endpointId')({
  validateSearch: (search: Record<string, unknown>): { tab?: Tab } => {
    const t = search.tab as Tab
    return TABS.some((x) => x.key === t) && t !== 'overview' ? { tab: t } : {}
  },
  // Client-only prefetch — SSR can't forward the session cookie.
  loader: ({ context, params }) =>
    typeof window === 'undefined'
      ? undefined
      : context.queryClient.ensureQueryData(endpointQuery(params.id, params.endpointId)),
  component: EndpointDetail,
  errorComponent: AuthErrorBoundary,
})

function endpointStatus(e: Endpoint): {
  label: string
  variant: React.ComponentProps<typeof Badge>['variant']
} {
  if (!e.disabled) return { label: 'active', variant: 'success' }
  return e.disabled_at
    ? { label: 'auto-disabled', variant: 'warning' }
    : { label: 'disabled', variant: 'secondary' }
}

function EndpointDetail() {
  const { id, endpointId } = Route.useParams()
  const { tab = 'overview' } = Route.useSearch()
  const { data: ep } = useQuery(endpointQuery(id, endpointId))

  if (!ep) {
    return (
      <div className="flex flex-1 flex-col">
        <PageHeader title="Endpoint" />
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
              to="/applications/$id"
              params={{ id }}
              search={{ tab: 'endpoints' }}
              className="font-normal text-muted-foreground hover:text-foreground"
            >
              Endpoints
            </Link>
            <span className="font-normal text-muted-foreground">/</span>
            <span className="truncate font-mono text-sm">{ep.url}</span>
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
          </Link>
        ))}
      </nav>

      <div className="flex-1 overflow-y-auto px-6 py-4">
        {tab === 'overview' && <OverviewTab ep={ep} appId={id} />}
        {tab === 'deliveries' && <DeliveriesTab appId={id} endpointId={endpointId} />}
        {tab === 'settings' && <SettingsTab ep={ep} appId={id} />}
      </div>
    </div>
  )
}

function OverviewTab({ ep, appId }: { ep: Endpoint; appId: string }) {
  const qc = useQueryClient()
  const [revealSecret, setRevealSecret] = useState<string | null>(null)
  const [testOpen, setTestOpen] = useState(false)
  const [recoverOpen, setRecoverOpen] = useState(false)
  const [rotateOpen, setRotateOpen] = useState(false)
  const status = endpointStatus(ep)

  const toggle = useMutation({
    mutationFn: () => api.updateEndpoint(appId, ep.id, { disabled: !ep.disabled }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.endpoint(appId, ep.id) })
      qc.invalidateQueries({ queryKey: qk.endpoints(appId) })
      toast.success(ep.disabled ? 'Endpoint enabled' : 'Endpoint disabled')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const rotate = useMutation({
    mutationFn: () => api.rotateEndpointSecret(appId, ep.id),
    onSuccess: (result) => {
      qc.invalidateQueries({ queryKey: qk.endpoint(appId, ep.id) })
      setRotateOpen(false)
      setRevealSecret(result.secret)
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const reveal = useMutation({
    mutationFn: () => api.getEndpointSecret(appId, ep.id),
    onSuccess: (res) => setRevealSecret(res.secret),
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <div className="max-w-2xl space-y-6">
      <div className="space-y-3">
        <h2 className="text-base font-semibold">Endpoint details</h2>
        <div className="space-y-3">
          <DetailRow label="URL">
            <CopyValue value={ep.url} what="URL" mono />
          </DetailRow>
          <DetailRow label="Filter event types">
            {ep.filter_event_types?.join(', ') || 'all'}
          </DetailRow>
          <DetailRow label="Status">
            <span className="inline-flex items-center gap-2">
              <Badge variant={status.variant}>{status.label}</Badge>
              {ep.disabled_at && (
                <span className="text-xs text-muted-foreground">
                  since {new Date(ep.disabled_at).toLocaleString()}
                </span>
              )}
            </span>
          </DetailRow>
          <DetailRow label="Consecutive failures">{ep.consecutive_failures}</DetailRow>
          <DetailRow label="Created at">{new Date(ep.created_at).toLocaleString()}</DetailRow>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-2 border-t border-border pt-4">
        <Button size="sm" variant="outline" onClick={() => setTestOpen(true)}>
          Send test
        </Button>
        <Button size="sm" variant="outline" onClick={() => setRecoverOpen(true)}>
          Recover
        </Button>
        <Button size="sm" variant="outline" onClick={() => setRotateOpen(true)}>
          Rotate secret
        </Button>
        <Button
          size="sm"
          variant="outline"
          onClick={() => reveal.mutate()}
          disabled={reveal.isPending}
        >
          {reveal.isPending ? 'Revealing…' : 'Reveal secret'}
        </Button>
        <Button
          size="sm"
          variant="outline"
          onClick={() => toggle.mutate()}
          disabled={toggle.isPending}
        >
          {ep.disabled ? 'Enable' : 'Disable'}
        </Button>
      </div>

      <TestDialog appId={appId} endpointId={ep.id} open={testOpen} onOpenChange={setTestOpen} />
      <RecoverDialog
        appId={appId}
        endpointId={ep.id}
        open={recoverOpen}
        onOpenChange={setRecoverOpen}
      />

      <Dialog open={rotateOpen} onOpenChange={setRotateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Rotate signing secret?</DialogTitle>
            <DialogDescription>
              A new signing secret is generated and shown once. The current secret keeps working
              during a short overlap, then stops. Update your receiver before it expires.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setRotateOpen(false)}>
              Cancel
            </Button>
            <Button disabled={rotate.isPending} onClick={() => rotate.mutate()}>
              {rotate.isPending ? 'Rotating…' : 'Rotate secret'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <RevealSecretDialog secret={revealSecret} onClose={() => setRevealSecret(null)} />
    </div>
  )
}

function TestDialog({
  appId,
  endpointId,
  open,
  onOpenChange,
}: {
  appId: string
  endpointId: string
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const qc = useQueryClient()
  const { data: eventTypes } = useQuery(eventTypesQuery)
  const [eventType, setEventType] = useState('')
  const [payload, setPayload] = useState('')

  const active = (eventTypes ?? []).filter((et) => !et.archived)

  const send = useMutation({
    mutationFn: (input: { event_type: string; payload?: unknown }) =>
      api.testEndpoint(appId, endpointId, input),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.endpointAttempts(appId, endpointId) })
      onOpenChange(false)
      setEventType('')
      setPayload('')
      toast.success('Test sent')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function onSubmit() {
    const trimmed = payload.trim()
    let parsed: unknown = undefined
    if (trimmed) {
      try {
        parsed = JSON.parse(trimmed)
      } catch {
        toast.error('invalid payload JSON')
        return
      }
    }
    send.mutate({ event_type: eventType, ...(parsed !== undefined ? { payload: parsed } : {}) })
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Send test message</DialogTitle>
          <DialogDescription>
            Deliver a one-off message to this endpoint. Leave the payload blank for a default test
            body.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <div>
            <Label className="mb-2 block">Event type</Label>
            {active.length === 0 ? (
              <p className="text-sm text-muted-foreground">No event types defined.</p>
            ) : (
              <Select value={eventType} onValueChange={(v) => setEventType(v ?? '')}>
                <SelectTrigger className="w-full">
                  <SelectValue>
                    {(v: string | null) => v || 'Select an event type'}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {active.map((et) => (
                    <SelectItem key={et.id} value={et.name}>
                      {et.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          </div>
          <div>
            <Label htmlFor="test-payload" className="mb-2 block">
              Payload JSON <span className="text-muted-foreground">(optional)</span>
            </Label>
            <textarea
              id="test-payload"
              className={textareaClass}
              value={payload}
              onChange={(e) => setPayload(e.target.value)}
              placeholder='{ "hello": "world" }'
            />
          </div>
        </div>
        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button disabled={send.isPending || !eventType} onClick={onSubmit}>
            {send.isPending ? 'Sending…' : 'Send test'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function RecoverDialog({
  appId,
  endpointId,
  open,
  onOpenChange,
}: {
  appId: string
  endpointId: string
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const qc = useQueryClient()
  const [since, setSince] = useState('')

  const recover = useMutation({
    mutationFn: (iso: string) => api.recoverEndpoint(appId, endpointId, { since: iso }),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: qk.endpointAttempts(appId, endpointId) })
      onOpenChange(false)
      setSince('')
      toast.success(`Recovered ${r.recovered}${r.truncated ? ' (truncated)' : ''}`)
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function onSubmit() {
    if (!since) return
    recover.mutate(new Date(since).toISOString())
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Recover failed messages</DialogTitle>
          <DialogDescription>
            Re-queue delivery of every message for this endpoint created since the chosen time.
          </DialogDescription>
        </DialogHeader>
        <div>
          <Label htmlFor="recover-since" className="mb-2 block">
            Since
          </Label>
          <Input
            id="recover-since"
            type="datetime-local"
            className="w-full"
            value={since}
            onChange={(e) => setSince(e.target.value)}
          />
        </div>
        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button disabled={recover.isPending || !since} onClick={onSubmit}>
            {recover.isPending ? 'Recovering…' : 'Recover'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function statusBadge(status: number | null | undefined) {
  if (status == null) return <span className="text-muted-foreground">—</span>
  return (
    <Badge variant={status >= 200 && status < 300 ? 'success' : 'destructive'}>{status}</Badge>
  )
}

function DeliveriesTab({ appId, endpointId }: { appId: string; endpointId: string }) {
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const { data: attempts, error } = useQuery({
    queryKey: qk.endpointAttempts(appId, endpointId),
    queryFn: () => api.listEndpointAttempts(appId, endpointId),
  })

  const rows = attempts ?? []

  function toggle(id: string) {
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  if (error) {
    return <p className="py-3 text-sm text-destructive">{(error as Error).message}</p>
  }

  return (
    <div className="space-y-3">
      <p className="text-sm text-muted-foreground">
        Most recent delivery attempts for this endpoint (server-capped). Replay lives on the
        message detail page.
      </p>
      <div className="rounded-lg border border-border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Attempt</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Duration</TableHead>
              <TableHead>Attempted</TableHead>
              <TableHead className="pr-6">Error</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((a) => (
              <AttemptRow
                key={a.id}
                attempt={a}
                open={expanded.has(a.id)}
                onToggle={() => toggle(a.id)}
              />
            ))}
            {rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={5} className="py-12 text-center text-sm text-muted-foreground">
                  No delivery attempts yet.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>
    </div>
  )
}

function AttemptRow({
  attempt: a,
  open,
  onToggle,
}: {
  attempt: MessageDeliveryAttempt
  open: boolean
  onToggle: () => void
}) {
  return (
    <>
      <TableRow className="cursor-pointer" onClick={onToggle}>
        <TableCell className="pl-6">{a.attempt_num}</TableCell>
        <TableCell>{statusBadge(a.response_status)}</TableCell>
        <TableCell className="text-muted-foreground">
          {a.duration_ms == null ? '—' : `${a.duration_ms}ms`}
        </TableCell>
        <TableCell className="whitespace-nowrap text-muted-foreground">
          {new Date(a.attempted_at).toLocaleString()}
        </TableCell>
        <TableCell className="pr-6 text-muted-foreground">{a.error_message || '—'}</TableCell>
      </TableRow>
      {open && (
        <TableRow>
          <TableCell colSpan={5} className="bg-muted/40 pl-6">
            <div className="space-y-3 py-2">
              <div>
                <div className="mb-1 text-xs font-medium text-muted-foreground">
                  Response headers
                </div>
                <pre className="overflow-x-auto rounded border border-border bg-muted px-3 py-2 font-mono text-xs">
                  {a.response_headers != null
                    ? JSON.stringify(a.response_headers, null, 2)
                    : '—'}
                </pre>
              </div>
              <div>
                <div className="mb-1 text-xs font-medium text-muted-foreground">Response body</div>
                <pre className="overflow-x-auto rounded border border-border bg-muted px-3 py-2 font-mono text-xs">
                  {a.response_body || '—'}
                </pre>
              </div>
            </div>
          </TableCell>
        </TableRow>
      )}
    </>
  )
}

// Set equality for the filter checkboxes' dirty check.
function sameFilters(a: Set<string>, b: string[] | null | undefined): boolean {
  const bSet = new Set(b ?? [])
  if (a.size !== bSet.size) return false
  for (const x of a) if (!bSet.has(x)) return false
  return true
}

function SettingsTab({ ep, appId }: { ep: Endpoint; appId: string }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { data: eventTypes } = useQuery(eventTypesQuery)

  const [url, setUrl] = useState(ep.url)
  const [description, setDescription] = useState(ep.description)
  const [filters, setFilters] = useState<Set<string>>(new Set(ep.filter_event_types ?? []))
  const [disabled, setDisabled] = useState(ep.disabled)
  const [deleteOpen, setDeleteOpen] = useState(false)

  const active = (eventTypes ?? []).filter((et) => !et.archived)

  // Seed the form once per endpoint (mount / navigation to another id), NOT on
  // every refetch — a background refetch (poll, header toggle, concurrent edit)
  // would otherwise wipe in-progress edits.
  const seededId = useRef<string | null>(null)
  useEffect(() => {
    if (seededId.current === ep.id) return
    seededId.current = ep.id
    setUrl(ep.url)
    setDescription(ep.description)
    setFilters(new Set(ep.filter_event_types ?? []))
    setDisabled(ep.disabled)
  }, [ep])

  const save = useMutation({
    mutationFn: () =>
      api.updateEndpoint(appId, ep.id, {
        url,
        description,
        filter_event_types: [...filters],
        disabled,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.endpoint(appId, ep.id) })
      qc.invalidateQueries({ queryKey: qk.endpoints(appId) })
      toast.success('Endpoint saved')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: () => api.deleteEndpoint(appId, ep.id),
    onSuccess: () => {
      // Drop the detail cache — the resource is gone, so a later Back
      // navigation must refetch (404) instead of rendering the stale row.
      qc.removeQueries({ queryKey: qk.endpoint(appId, ep.id) })
      qc.invalidateQueries({ queryKey: qk.endpoints(appId) })
      toast.success('Endpoint deleted')
      navigate({ to: '/applications/$id', params: { id: appId }, search: { tab: 'endpoints' } })
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function toggleFilter(name: string, checked: boolean) {
    setFilters((prev) => {
      const next = new Set(prev)
      if (checked) next.add(name)
      else next.delete(name)
      return next
    })
  }

  const dirty =
    url !== ep.url ||
    description !== ep.description ||
    disabled !== ep.disabled ||
    !sameFilters(filters, ep.filter_event_types)

  return (
    <div className="max-w-3xl space-y-8">
      <section className="space-y-4">
        <h2 className="text-sm font-semibold">Endpoint settings</h2>
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
                    onChange={(e) => toggleFilter(et.name, e.target.checked)}
                  />
                  <span className="font-mono text-xs">{et.name}</span>
                </label>
              ))}
            </div>
          )}
        </div>
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 rounded border-input"
            checked={disabled}
            onChange={(e) => setDisabled(e.target.checked)}
          />
          <span>Disabled</span>
        </label>
        <Button size="sm" onClick={() => save.mutate()} disabled={!dirty || save.isPending || !url.trim()}>
          {save.isPending ? 'Saving…' : 'Save'}
        </Button>
      </section>

      <section className="space-y-2 border-t border-border pt-6">
        <h2 className="text-sm font-semibold text-destructive">Delete endpoint</h2>
        <p className="text-sm text-muted-foreground">
          Stops all delivery to this URL and removes its delivery history. This cannot be undone.
        </p>
        <Button variant="destructive" onClick={() => setDeleteOpen(true)}>
          <Trash2 className="h-4 w-4" /> Delete endpoint
        </Button>
      </section>

      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete this endpoint?</DialogTitle>
            <DialogDescription>
              Delivery to this URL stops and its history is removed. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setDeleteOpen(false)}>
              Cancel
            </Button>
            <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
              {remove.isPending ? 'Deleting…' : 'Delete endpoint'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
