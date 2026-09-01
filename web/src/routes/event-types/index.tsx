import { createFileRoute } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { toast } from 'sonner'
import { MoreHorizontal, Plus, Search } from 'lucide-react'

import { api, qk, type EventType } from '#/lib/api'
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
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'

const eventTypesQuery = queryOptions({
  queryKey: qk.eventTypes(),
  queryFn: () => api.listEventTypes(),
})

export const Route = createFileRoute('/event-types/')({
  // Client-only prefetch — SSR can't forward the session cookie (see sources).
  loader: ({ context }) =>
    typeof window === 'undefined'
      ? undefined
      : context.queryClient.ensureQueryData(eventTypesQuery),
  component: EventTypesPage,
  errorComponent: AuthErrorBoundary,
})

// Shared textarea styling — there is no ui/textarea component and JSON needs a
// multi-line, monospace field. Mirrors ui/input's border/focus treatment.
const textareaClass =
  'flex min-h-[120px] w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs shadow-sm transition-colors placeholder:text-muted-foreground focus-visible:ring-1 focus-visible:ring-ring focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

function EventTypesPage() {
  const qc = useQueryClient()
  const { data: eventTypes } = useQuery(eventTypesQuery)

  const [q, setQ] = useState('')
  const [createOpen, setCreateOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<EventType | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<EventType | null>(null)

  const rows = useMemo(() => {
    const list = eventTypes ?? []
    const needle = q.trim().toLowerCase()
    return needle ? list.filter((e) => e.name.toLowerCase().includes(needle)) : list
  }, [eventTypes, q])

  const remove = useMutation({
    mutationFn: (name: string) => api.deleteEventType(name),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.eventTypes() })
      toast.success('Event type deleted')
      setDeleteTarget(null)
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title="Event Types"
        actions={
          <Button size="sm" onClick={() => setCreateOpen(true)}>
            <Plus className="h-4 w-4" /> New event type
          </Button>
        }
      />

      <div className="flex flex-wrap items-center gap-3 border-b border-border px-6 py-3">
        <div className="relative min-w-[200px] flex-1 sm:max-w-xs">
          <Search className="pointer-events-none absolute top-1/2 left-3 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            className="pl-9"
            placeholder="Filter by name…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
        </div>
      </div>

      <div className="flex-1 overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Name</TableHead>
              <TableHead>Description</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="w-[52px] pr-6" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((et) => (
              <TableRow key={et.id}>
                <TableCell className="pl-6 font-mono font-medium">{et.name}</TableCell>
                <TableCell className="max-w-[420px] truncate text-muted-foreground">
                  {et.description || '—'}
                </TableCell>
                <TableCell>
                  <Badge variant={et.archived ? 'secondary' : 'success'}>
                    {et.archived ? 'archived' : 'active'}
                  </Badge>
                </TableCell>
                <TableCell className="pr-6 text-right">
                  <DropdownMenu>
                    <DropdownMenuTrigger asChild>
                      <Button
                        size="icon"
                        variant="ghost"
                        className="h-8 w-8"
                        aria-label="Event type actions"
                      >
                        <MoreHorizontal className="h-4 w-4" />
                      </Button>
                    </DropdownMenuTrigger>
                    <DropdownMenuContent align="end" className="w-40">
                      <DropdownMenuItem onClick={() => setEditTarget(et)}>Edit</DropdownMenuItem>
                      <DropdownMenuSeparator />
                      <DropdownMenuItem
                        onClick={() => setDeleteTarget(et)}
                        className="text-destructive"
                      >
                        Delete
                      </DropdownMenuItem>
                    </DropdownMenuContent>
                  </DropdownMenu>
                </TableCell>
              </TableRow>
            ))}
            {rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={4} className="py-12 text-center text-sm text-muted-foreground">
                  {eventTypes && eventTypes.length > 0
                    ? 'No event types match this filter.'
                    : 'No event types yet — create one to classify your messages.'}
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <footer className="border-t border-border px-6 py-3 text-sm text-muted-foreground">
        Viewing {rows.length} {rows.length === 1 ? 'event type' : 'event types'}
      </footer>

      <CreateEventTypeDialog open={createOpen} onOpenChange={setCreateOpen} />

      {editTarget && (
        <EditEventTypeDialog
          key={editTarget.id}
          target={editTarget}
          onClose={() => setEditTarget(null)}
        />
      )}

      <Dialog open={!!deleteTarget} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {deleteTarget?.name}?</DialogTitle>
            <DialogDescription>
              This removes the event type. Messages already sent keep their recorded type.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setDeleteTarget(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={remove.isPending}
              onClick={() => deleteTarget && remove.mutate(deleteTarget.name)}
            >
              {remove.isPending ? 'Deleting…' : 'Delete event type'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// Parse a JSON textarea value. Returns { ok: true, value } where value is the
// parsed object or undefined when blank; { ok: false } after toasting on a
// syntax error so callers just abort.
function parseSchema(raw: string): { ok: true; value: unknown } | { ok: false } {
  const trimmed = raw.trim()
  if (!trimmed) return { ok: true, value: undefined }
  try {
    return { ok: true, value: JSON.parse(trimmed) }
  } catch {
    toast.error('invalid schema JSON')
    return { ok: false }
  }
}

function CreateEventTypeDialog({
  open,
  onOpenChange,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [schema, setSchema] = useState('')

  const create = useMutation({
    mutationFn: (parsedSchema: unknown) =>
      api.createEventType({
        name,
        description: description || undefined,
        ...(parsedSchema !== undefined ? { schema: parsedSchema } : {}),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.eventTypes() })
      toast.success('Event type created')
      onOpenChange(false)
      setName('')
      setDescription('')
      setSchema('')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New event type</DialogTitle>
          <DialogDescription>
            Event types classify the messages your applications send.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            const parsed = parseSchema(schema)
            if (!parsed.ok) return
            create.mutate(parsed.value)
          }}
          className="space-y-4"
        >
          <div>
            <Label htmlFor="et-name" className="mb-2 block">
              Name
            </Label>
            <Input
              id="et-name"
              className="w-full font-mono"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="order.created"
              required
              autoFocus
            />
          </div>
          <div>
            <Label htmlFor="et-description" className="mb-2 block">
              Description <span className="text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="et-description"
              className="w-full"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Fired when an order is created"
            />
          </div>
          <div>
            <Label htmlFor="et-schema" className="mb-2 block">
              Schema JSON <span className="text-muted-foreground">(optional)</span>
            </Label>
            <textarea
              id="et-schema"
              className={textareaClass}
              value={schema}
              onChange={(e) => setSchema(e.target.value)}
              placeholder='{ "type": "object" }'
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={create.isPending || !name.trim()}>
              {create.isPending ? 'Creating…' : 'Create event type'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function EditEventTypeDialog({ target, onClose }: { target: EventType; onClose: () => void }) {
  const qc = useQueryClient()
  const [description, setDescription] = useState(target.description)
  const [schema, setSchema] = useState(
    target.schema != null ? JSON.stringify(target.schema, null, 2) : '',
  )
  const [archived, setArchived] = useState(target.archived)

  const update = useMutation({
    mutationFn: (parsedSchema: unknown) =>
      api.updateEventType(target.name, {
        description,
        // undefined here means blank → clear the stored schema.
        schema: parsedSchema === undefined ? null : parsedSchema,
        archived,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.eventTypes() })
      toast.success('Event type updated')
      onClose()
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle className="font-mono">{target.name}</DialogTitle>
          <DialogDescription>Update this event type.</DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            const parsed = parseSchema(schema)
            if (!parsed.ok) return
            update.mutate(parsed.value)
          }}
          className="space-y-4"
        >
          <div>
            <Label htmlFor="et-edit-description" className="mb-2 block">
              Description
            </Label>
            <Input
              id="et-edit-description"
              className="w-full"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Fired when an order is created"
            />
          </div>
          <div>
            <Label htmlFor="et-edit-schema" className="mb-2 block">
              Schema JSON <span className="text-muted-foreground">(optional)</span>
            </Label>
            <textarea
              id="et-edit-schema"
              className={textareaClass}
              value={schema}
              onChange={(e) => setSchema(e.target.value)}
              placeholder='{ "type": "object" }'
            />
          </div>
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              className="h-4 w-4 rounded border-input"
              checked={archived}
              onChange={(e) => setArchived(e.target.checked)}
            />
            Archived
          </label>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={update.isPending}>
              {update.isPending ? 'Saving…' : 'Save changes'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
