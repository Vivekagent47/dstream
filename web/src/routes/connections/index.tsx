import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { toast } from 'sonner'
import { Inbox, MoreHorizontal, MoveRight, Plus, Send } from 'lucide-react'

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

export const Route = createFileRoute('/connections/')({
  // Client-only prefetch — same SSR-cookie caveat as /sources.
  loader: ({ context }) =>
    typeof window === 'undefined'
      ? undefined
      : context.queryClient.ensureQueryData(connectionsQuery),
  component: ConnectionsPage,
  errorComponent: AuthErrorBoundary,
})

function ConnectionsPage() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { data: connections } = useQuery(connectionsQuery)
  const { data: sources } = useQuery(sourcesQuery)
  const { data: destinations } = useQuery(destinationsQuery)

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

  const rows = connections ?? []

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title="Connections"
        actions={
          <Button size="sm" onClick={() => setCreateOpen(true)}>
            <Plus className="h-4 w-4" /> New connection
          </Button>
        }
      />

      <div className="flex-1 overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Source</TableHead>
              <TableHead className="w-[40px]" />
              <TableHead>Destination</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Retry</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="w-[52px] pr-6" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((c) => (
              <TableRow key={c.id}>
                <TableCell className="pl-6">
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
            {rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={7} className="py-12 text-center text-sm text-muted-foreground">
                  No connections yet — connect a source to a destination to start routing events.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <footer className="border-t border-border px-6 py-3 text-sm text-muted-foreground">
        Viewing {rows.length} {rows.length === 1 ? 'connection' : 'connections'}
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
  const [sourceId, setSourceId] = useState('')
  const [destinationId, setDestinationId] = useState('')

  const create = useMutation({
    mutationFn: () => api.createConnection({ source_id: sourceId, destination_id: destinationId }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['connections'] })
      toast.success('Connection created')
      onOpenChange(false)
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
