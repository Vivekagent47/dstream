import { createFileRoute, Link } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { toast } from 'sonner'
import { Copy, MoreHorizontal, Plus, Search, Send } from 'lucide-react'

import { api, qk, type Destination } from '#/lib/api'
import { capitalize } from '#/lib/utils'
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

const destinationsQuery = queryOptions({
  queryKey: qk.destinations(),
  queryFn: () => api.listDestinations(),
})

export const Route = createFileRoute('/destinations/')({
  // Client-only prefetch — SSR can't forward the session cookie (see sources).
  loader: ({ context }) =>
    typeof window === 'undefined'
      ? undefined
      : context.queryClient.ensureQueryData(destinationsQuery),
  component: DestinationsPage,
  errorComponent: AuthErrorBoundary,
})

const DEST_TYPES = ['http', 'cli'] as const

// Labels: 'http' → 'HTTP', 'cli' → 'CLI' (capitalize() would give "Http").
const typeLabel = (t: string) => (t === 'http' ? 'HTTP' : t === 'cli' ? 'CLI' : capitalize(t))

function DestinationsPage() {
  const qc = useQueryClient()
  const { data: destinations } = useQuery(destinationsQuery)

  const [q, setQ] = useState('')
  const [type, setType] = useState('all')
  const [order, setOrder] = useState<'newest' | 'oldest'>('newest')
  const [createOpen, setCreateOpen] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<Destination | null>(null)

  const rows = useMemo(() => {
    let list = destinations ?? []
    const needle = q.trim().toLowerCase()
    if (needle) list = list.filter((d) => d.name.toLowerCase().includes(needle))
    if (type !== 'all') list = list.filter((d) => d.type === type)
    return [...list].sort((a, b) => {
      const diff = new Date(a.created_at).getTime() - new Date(b.created_at).getTime()
      return order === 'newest' ? -diff : diff
    })
  }, [destinations, q, type, order])

  const remove = useMutation({
    mutationFn: (id: string) => api.deleteDestination(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.destinations() })
      toast.success('Destination deleted')
      setDeleteTarget(null)
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function copyUrl(url: string) {
    navigator.clipboard
      .writeText(url)
      .then(() => toast.success('URL copied'))
      .catch(() => toast.error('Couldn’t copy — select the URL manually'))
  }

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title="Destinations"
        actions={
          <Button size="sm" onClick={() => setCreateOpen(true)}>
            <Plus className="h-4 w-4" /> New destination
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
        <Select value={type} onValueChange={(v) => setType(v ?? 'all')}>
          <SelectTrigger className="w-[150px]">
            <SelectValue>
              {(v: string | null) => (!v || v === 'all' ? 'All types' : typeLabel(v))}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All types</SelectItem>
            {DEST_TYPES.map((t) => (
              <SelectItem key={t} value={t}>
                {typeLabel(t)}
              </SelectItem>
            ))}
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

      <div className="flex-1 overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Destination</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>URL</TableHead>
              <TableHead>Auth</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="w-[52px] pr-6" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((d) => (
              <TableRow key={d.id}>
                <TableCell className="pl-6">
                  <Link
                    to="/destinations/$id"
                    params={{ id: d.id }}
                    className="flex items-center gap-2.5 font-medium hover:underline"
                  >
                    <Send className="h-4 w-4 shrink-0 text-muted-foreground" />
                    {d.name}
                  </Link>
                </TableCell>
                <TableCell>
                  <Badge variant="secondary">{typeLabel(d.type)}</Badge>
                </TableCell>
                <TableCell>
                  {d.type === 'http' && d.url ? (
                    <button
                      type="button"
                      onClick={() => copyUrl(d.url as string)}
                      className="group inline-flex max-w-[320px] items-center gap-1.5 font-mono text-xs text-muted-foreground transition-colors hover:text-foreground"
                      title="Copy URL"
                    >
                      <span className="truncate">{d.url}</span>
                      <Copy className="h-3 w-3 shrink-0 opacity-0 transition-opacity group-hover:opacity-100" />
                    </button>
                  ) : (
                    <span className="text-muted-foreground">CLI tunnel</span>
                  )}
                </TableCell>
                <TableCell>
                  {d.auth_configured ? (
                    <Badge variant="success">Configured</Badge>
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </TableCell>
                <TableCell className="whitespace-nowrap text-muted-foreground">
                  {new Date(d.created_at).toLocaleDateString()}
                </TableCell>
                <TableCell className="pr-6 text-right">
                  <DropdownMenu>
                    <DropdownMenuTrigger asChild>
                      <Button
                        size="icon"
                        variant="ghost"
                        className="h-8 w-8"
                        aria-label="Destination actions"
                      >
                        <MoreHorizontal className="h-4 w-4" />
                      </Button>
                    </DropdownMenuTrigger>
                    <DropdownMenuContent align="end" className="w-40">
                      {d.type === 'http' && d.url && (
                        <DropdownMenuItem onClick={() => copyUrl(d.url as string)}>
                          Copy URL
                        </DropdownMenuItem>
                      )}
                      <DropdownMenuSeparator />
                      <DropdownMenuItem onClick={() => setDeleteTarget(d)} className="text-destructive">
                        Delete
                      </DropdownMenuItem>
                    </DropdownMenuContent>
                  </DropdownMenu>
                </TableCell>
              </TableRow>
            ))}
            {rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={6} className="py-12 text-center text-sm text-muted-foreground">
                  {destinations && destinations.length > 0
                    ? 'No destinations match these filters.'
                    : 'No destinations yet — create one to deliver events.'}
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <footer className="border-t border-border px-6 py-3 text-sm text-muted-foreground">
        Viewing {rows.length} {rows.length === 1 ? 'destination' : 'destinations'}
      </footer>

      <CreateDestinationDialog open={createOpen} onOpenChange={setCreateOpen} />

      <Dialog open={!!deleteTarget} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {deleteTarget?.name}?</DialogTitle>
            <DialogDescription>
              This removes the destination. Connections routing to it will stop delivering.
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
              {remove.isPending ? 'Deleting…' : 'Delete destination'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function CreateDestinationDialog({
  open,
  onOpenChange,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [type, setType] = useState<'http' | 'cli'>('http')
  const [url, setUrl] = useState('')

  const create = useMutation({
    mutationFn: () =>
      api.createDestination({
        name,
        description,
        type,
        ...(type === 'http' ? { url } : {}),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.destinations() })
      toast.success('Destination created')
      onOpenChange(false)
      setName('')
      setDescription('')
      setType('http')
      setUrl('')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New destination</DialogTitle>
          <DialogDescription>
            Destinations receive delivered events — an HTTP endpoint or a local CLI tunnel.
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
            <Label htmlFor="dest-name" className="mb-2 block">
              Name
            </Label>
            <Input
              id="dest-name"
              className="w-full"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="orders-service"
              required
              autoFocus
            />
          </div>
          <div>
            <Label htmlFor="dest-description" className="mb-2 block">
              Description <span className="text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="dest-description"
              className="w-full"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Production orders webhook consumer"
            />
          </div>
          <div>
            <Label className="mb-2 block">Type</Label>
            <Select value={type} onValueChange={(v) => setType((v as 'http' | 'cli') ?? 'http')}>
              <SelectTrigger className="w-full">
                <SelectValue>{(v: string | null) => (v ? typeLabel(v) : 'Select a type')}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {DEST_TYPES.map((t) => (
                  <SelectItem key={t} value={t}>
                    {typeLabel(t)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          {type === 'http' && (
            <div>
              <Label htmlFor="dest-url" className="mb-2 block">
                URL
              </Label>
              <Input
                id="dest-url"
                type="url"
                className="w-full"
                value={url}
                onChange={(e) => setUrl(e.target.value)}
                placeholder="https://api.example.com/webhooks"
                required
              />
            </div>
          )}
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={create.isPending || !name.trim() || (type === 'http' && !url.trim())}
            >
              {create.isPending ? 'Creating…' : 'Create destination'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
