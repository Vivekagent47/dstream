import { createFileRoute } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { toast } from 'sonner'
import { MoreHorizontal, Plus } from 'lucide-react'

import type { Endpoint } from '#/lib/api'
import { portalApi, portalQk } from '#/lib/portal-api'
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

export const Route = createFileRoute('/portal/')({ component: PortalEndpoints })

// Shared textarea styling — there is no ui/textarea component and JSON needs a
// multi-line, monospace field. Mirrors ui/input's border/focus treatment.
const textareaClass =
  'flex min-h-[120px] w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs shadow-sm transition-colors placeholder:text-muted-foreground focus-visible:ring-1 focus-visible:ring-ring focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

// Token pins the app, so no appId anywhere — portalApi/portalQk only.
const eventTypesQuery = queryOptions({
  queryKey: portalQk.eventTypes,
  queryFn: portalApi.listEventTypes,
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

function PortalEndpoints() {
  const qc = useQueryClient()
  const [addOpen, setAddOpen] = useState(false)
  const [revealSecret, setRevealSecret] = useState<string | null>(null)
  const [editTarget, setEditTarget] = useState<Endpoint | null>(null)
  const [testTarget, setTestTarget] = useState<Endpoint | null>(null)
  const [recoverTarget, setRecoverTarget] = useState<Endpoint | null>(null)
  const [rotateTarget, setRotateTarget] = useState<Endpoint | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<Endpoint | null>(null)

  const { data: endpoints, error } = useQuery({
    queryKey: portalQk.endpoints,
    queryFn: portalApi.listEndpoints,
  })
  const rows = endpoints ?? []

  const toggle = useMutation({
    mutationFn: (e: Endpoint) => portalApi.updateEndpoint(e.id, { disabled: !e.disabled }),
    onSuccess: (_r, e) => {
      qc.invalidateQueries({ queryKey: portalQk.endpoints })
      toast.success(e.disabled ? 'Endpoint enabled' : 'Endpoint disabled')
    },
    onError: (e) => toast.error((e as Error).message),
  })

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
              <TableHead>Failures</TableHead>
              <TableHead className="pr-6 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((e) => {
              const s = endpointStatus(e)
              return (
                <TableRow key={e.id}>
                  <TableCell className="pl-6 font-mono text-xs">{e.url}</TableCell>
                  <TableCell className="text-muted-foreground">
                    {e.filter_event_types?.join(', ') || 'all'}
                  </TableCell>
                  <TableCell>
                    <Badge variant={s.variant}>{s.label}</Badge>
                  </TableCell>
                  <TableCell>{e.consecutive_failures}</TableCell>
                  <TableCell className="pr-6 text-right">
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button
                          size="icon"
                          variant="ghost"
                          className="h-8 w-8"
                          aria-label="Endpoint actions"
                        >
                          <MoreHorizontal className="h-4 w-4" />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end" className="w-44">
                        <DropdownMenuItem onClick={() => setTestTarget(e)}>
                          Send test
                        </DropdownMenuItem>
                        <DropdownMenuItem onClick={() => setRecoverTarget(e)}>
                          Recover
                        </DropdownMenuItem>
                        <DropdownMenuItem onClick={() => setRotateTarget(e)}>
                          Rotate secret
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          onClick={() => toggle.mutate(e)}
                          disabled={toggle.isPending}
                        >
                          {e.disabled ? 'Enable' : 'Disable'}
                        </DropdownMenuItem>
                        <DropdownMenuItem onClick={() => setEditTarget(e)}>Edit</DropdownMenuItem>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem
                          onClick={() => setDeleteTarget(e)}
                          className="text-destructive"
                        >
                          Delete
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              )
            })}
            {rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={5} className="py-12 text-center text-sm text-muted-foreground">
                  No endpoints yet — add one to start delivering messages.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <AddEndpointDialog
        open={addOpen}
        onOpenChange={setAddOpen}
        onCreated={(secret) => setRevealSecret(secret)}
      />
      {editTarget && (
        <EditEndpointDialog endpoint={editTarget} onClose={() => setEditTarget(null)} />
      )}
      {testTarget && <TestDialog endpoint={testTarget} onClose={() => setTestTarget(null)} />}
      {recoverTarget && (
        <RecoverDialog endpoint={recoverTarget} onClose={() => setRecoverTarget(null)} />
      )}
      {rotateTarget && (
        <RotateDialog
          endpoint={rotateTarget}
          onClose={() => setRotateTarget(null)}
          onRotated={(secret) => setRevealSecret(secret)}
        />
      )}
      {deleteTarget && (
        <DeleteDialog endpoint={deleteTarget} onClose={() => setDeleteTarget(null)} />
      )}
      <RevealSecretDialog secret={revealSecret} onClose={() => setRevealSecret(null)} />
    </div>
  )
}

function EventTypeCheckboxes({
  filters,
  onToggle,
}: {
  filters: Set<string>
  onToggle: (name: string, checked: boolean) => void
}) {
  const { data: eventTypes } = useQuery(eventTypesQuery)
  const active = (eventTypes ?? []).filter((et) => !et.archived)
  if (active.length === 0) {
    return <p className="text-sm text-muted-foreground">No event types defined.</p>
  }
  return (
    <div className="max-h-40 space-y-1.5 overflow-y-auto rounded-md border border-border p-3">
      {active.map((et) => (
        <label key={et.id} className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 rounded border-input"
            checked={filters.has(et.name)}
            onChange={(e) => onToggle(et.name, e.target.checked)}
          />
          <span className="font-mono text-xs">{et.name}</span>
        </label>
      ))}
    </div>
  )
}

function AddEndpointDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  onCreated: (secret: string) => void
}) {
  const qc = useQueryClient()
  const [url, setUrl] = useState('')
  const [uid, setUid] = useState('')
  const [description, setDescription] = useState('')
  const [filters, setFilters] = useState<Set<string>>(new Set())

  const create = useMutation({
    mutationFn: () =>
      portalApi.createEndpoint({
        url,
        ...(uid.trim() ? { uid: uid.trim() } : {}),
        ...(description.trim() ? { description: description.trim() } : {}),
        ...(filters.size > 0 ? { filter_event_types: [...filters] } : {}),
      }),
    onSuccess: (result) => {
      qc.invalidateQueries({ queryKey: portalQk.endpoints })
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
            Where your messages are delivered. A signing secret is generated and shown once on
            creation.
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
            <EventTypeCheckboxes filters={filters} onToggle={toggle} />
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

// Set equality for the filter checkboxes' dirty check.
function sameFilters(a: Set<string>, b: string[] | null | undefined): boolean {
  const bSet = new Set(b ?? [])
  if (a.size !== bSet.size) return false
  for (const x of a) if (!bSet.has(x)) return false
  return true
}

function EditEndpointDialog({ endpoint: ep, onClose }: { endpoint: Endpoint; onClose: () => void }) {
  const qc = useQueryClient()
  const [url, setUrl] = useState(ep.url)
  const [description, setDescription] = useState(ep.description)
  const [filters, setFilters] = useState<Set<string>>(new Set(ep.filter_event_types ?? []))

  // Reseed if the dialog is reused for a different endpoint.
  const seededId = useRef<string | null>(null)
  useEffect(() => {
    if (seededId.current === ep.id) return
    seededId.current = ep.id
    setUrl(ep.url)
    setDescription(ep.description)
    setFilters(new Set(ep.filter_event_types ?? []))
  }, [ep])

  const save = useMutation({
    mutationFn: () =>
      portalApi.updateEndpoint(ep.id, {
        url,
        description,
        filter_event_types: [...filters],
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: portalQk.endpoints })
      toast.success('Endpoint saved')
      onClose()
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
    !sameFilters(filters, ep.filter_event_types)

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Edit endpoint</DialogTitle>
          <DialogDescription>Update where and which messages are delivered.</DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <div>
            <Label htmlFor="edit-url" className="mb-2 block">
              URL
            </Label>
            <Input
              id="edit-url"
              type="url"
              className="w-full font-mono"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="https://example.com/webhooks"
            />
          </div>
          <div>
            <Label htmlFor="edit-description" className="mb-2 block">
              Description <span className="text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="edit-description"
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
            <EventTypeCheckboxes filters={filters} onToggle={toggleFilter} />
          </div>
        </div>
        <DialogFooter>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button disabled={!dirty || save.isPending || !url.trim()} onClick={() => save.mutate()}>
            {save.isPending ? 'Saving…' : 'Save'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function TestDialog({ endpoint: ep, onClose }: { endpoint: Endpoint; onClose: () => void }) {
  const { data: eventTypes } = useQuery(eventTypesQuery)
  const [eventType, setEventType] = useState('')
  const [payload, setPayload] = useState('')

  const active = (eventTypes ?? []).filter((et) => !et.archived)

  const send = useMutation({
    mutationFn: (input: { event_type: string; payload?: unknown }) =>
      portalApi.testEndpoint(ep.id, input),
    onSuccess: () => {
      toast.success('Test sent')
      onClose()
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
    <Dialog open onOpenChange={(o) => !o && onClose()}>
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
                  <SelectValue>{(v: string | null) => v || 'Select an event type'}</SelectValue>
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
          <Button variant="ghost" onClick={onClose}>
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

function RecoverDialog({ endpoint: ep, onClose }: { endpoint: Endpoint; onClose: () => void }) {
  const [since, setSince] = useState('')

  const recover = useMutation({
    mutationFn: (iso: string) => portalApi.recoverEndpoint(ep.id, { since: iso }),
    onSuccess: (r) => {
      toast.success(`Recovered ${r.recovered}${r.truncated ? ' (truncated)' : ''}`)
      onClose()
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function onSubmit() {
    if (!since) return
    recover.mutate(new Date(since).toISOString())
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
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
          <Button variant="ghost" onClick={onClose}>
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

function RotateDialog({
  endpoint: ep,
  onClose,
  onRotated,
}: {
  endpoint: Endpoint
  onClose: () => void
  onRotated: (secret: string) => void
}) {
  const rotate = useMutation({
    mutationFn: () => portalApi.rotateEndpointSecret(ep.id),
    onSuccess: (result) => {
      onClose()
      onRotated(result.secret)
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Rotate signing secret?</DialogTitle>
          <DialogDescription>
            A new signing secret is generated and shown once. The current secret keeps working
            during a short overlap, then stops. Update your receiver before it expires.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button disabled={rotate.isPending} onClick={() => rotate.mutate()}>
            {rotate.isPending ? 'Rotating…' : 'Rotate secret'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function DeleteDialog({ endpoint: ep, onClose }: { endpoint: Endpoint; onClose: () => void }) {
  const qc = useQueryClient()
  const remove = useMutation({
    mutationFn: () => portalApi.deleteEndpoint(ep.id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: portalQk.endpoints })
      toast.success('Endpoint deleted')
      onClose()
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete this endpoint?</DialogTitle>
          <DialogDescription>
            Delivery to this URL stops and its history is removed. This cannot be undone.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
            {remove.isPending ? 'Deleting…' : 'Delete endpoint'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
