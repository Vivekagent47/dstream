import { createFileRoute, Link } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { toast } from 'sonner'
import { Copy, Inbox, MoreHorizontal, Plus, Search } from 'lucide-react'

import { api, qk, type Source } from '#/lib/api'
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

const sourcesQuery = queryOptions({
  queryKey: qk.sources(),
  queryFn: () => api.listSources(),
})

export const Route = createFileRoute('/sources')({
  // Client-only prefetch. On SSR the Node server can't forward the browser's
  // session cookie to the API, so an authed fetch here 401s and 500s the page.
  // The component's useQuery loads the data (with the cookie) after hydration.
  loader: ({ context }) =>
    typeof window === 'undefined' ? undefined : context.queryClient.ensureQueryData(sourcesQuery),
  component: SourcesPage,
  errorComponent: AuthErrorBoundary,
})

function ingestUrl(token: string): string {
  const origin = typeof window === 'undefined' ? '' : window.location.origin
  return `${origin}/e/${token}`
}

function SourcesPage() {
  const qc = useQueryClient()
  const { data: sources } = useQuery(sourcesQuery)

  const [q, setQ] = useState('')
  const [status, setStatus] = useState('all')
  const [order, setOrder] = useState<'newest' | 'oldest'>('newest')
  const [createOpen, setCreateOpen] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<Source | null>(null)

  const rows = useMemo(() => {
    let list = sources ?? []
    const needle = q.trim().toLowerCase()
    if (needle) list = list.filter((s) => s.name.toLowerCase().includes(needle))
    if (status !== 'all') list = list.filter((s) => (status === 'active' ? s.enabled : !s.enabled))
    return [...list].sort((a, b) => {
      const d = new Date(a.created_at).getTime() - new Date(b.created_at).getTime()
      return order === 'newest' ? -d : d
    })
  }, [sources, q, status, order])

  const patch = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) => api.updateSource(id, { enabled }),
    onSuccess: (_r, v) => {
      qc.invalidateQueries({ queryKey: qk.sources() })
      toast.success(v.enabled ? 'Source enabled' : 'Source disabled')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.deleteSource(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.sources() })
      toast.success('Source deleted')
      setDeleteTarget(null)
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function copyUrl(token: string) {
    navigator.clipboard
      .writeText(ingestUrl(token))
      .then(() => toast.success('Ingest URL copied'))
      .catch(() => toast.error('Couldn’t copy — select the URL manually'))
  }

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title="Sources"
        actions={
          <Button size="sm" onClick={() => setCreateOpen(true)}>
            <Plus className="h-4 w-4" /> New source
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
        <Select value={status} onValueChange={(v) => setStatus(v ?? 'all')}>
          <SelectTrigger className="w-[140px]">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All statuses</SelectItem>
            <SelectItem value="active">Active</SelectItem>
            <SelectItem value="disabled">Disabled</SelectItem>
          </SelectContent>
        </Select>
        <Select value={order} onValueChange={(v) => setOrder(v as 'newest' | 'oldest')}>
          <SelectTrigger className="ml-auto w-[170px]">
            <SelectValue />
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
              <TableHead className="pl-6">Source</TableHead>
              <TableHead>Ingest URL</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="w-[52px] pr-6" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((s) => (
              <TableRow key={s.id}>
                <TableCell className="pl-6">
                  <Link
                    to="/sources/$id"
                    params={{ id: s.id }}
                    className="flex items-center gap-2.5 font-medium hover:underline"
                  >
                    <Inbox className="h-4 w-4 shrink-0 text-muted-foreground" />
                    {s.name}
                  </Link>
                </TableCell>
                <TableCell>
                  <button
                    type="button"
                    onClick={() => copyUrl(s.ingest_token)}
                    className="group inline-flex max-w-[280px] items-center gap-1.5 font-mono text-xs text-muted-foreground transition-colors hover:text-foreground"
                    title="Copy ingest URL"
                  >
                    <span className="truncate">/e/{s.ingest_token}</span>
                    <Copy className="h-3 w-3 shrink-0 opacity-0 transition-opacity group-hover:opacity-100" />
                  </button>
                </TableCell>
                <TableCell>
                  {s.enabled ? (
                    <Badge variant="success">Active</Badge>
                  ) : (
                    <Badge variant="secondary">Disabled</Badge>
                  )}
                </TableCell>
                <TableCell className="whitespace-nowrap text-muted-foreground">
                  {new Date(s.created_at).toLocaleDateString()}
                </TableCell>
                <TableCell className="pr-6 text-right">
                  <SourceRowMenu
                    source={s}
                    onCopy={() => copyUrl(s.ingest_token)}
                    onToggle={() => patch.mutate({ id: s.id, enabled: !s.enabled })}
                    onDelete={() => setDeleteTarget(s)}
                    pending={patch.isPending}
                  />
                </TableCell>
              </TableRow>
            ))}
            {rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={5} className="py-12 text-center text-sm text-muted-foreground">
                  {sources && sources.length > 0
                    ? 'No sources match these filters.'
                    : 'No sources yet — create one to get a webhook URL.'}
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <footer className="border-t border-border px-6 py-3 text-sm text-muted-foreground">
        Viewing {rows.length} {rows.length === 1 ? 'source' : 'sources'}
      </footer>

      <CreateSourceDialog open={createOpen} onOpenChange={setCreateOpen} />

      <Dialog open={!!deleteTarget} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {deleteTarget?.name}?</DialogTitle>
            <DialogDescription>
              This removes the source and its ingest URL. Incoming webhooks to it will fail.
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
              {remove.isPending ? 'Deleting…' : 'Delete source'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function SourceRowMenu({
  source,
  onCopy,
  onToggle,
  onDelete,
  pending,
}: {
  source: Source
  onCopy: () => void
  onToggle: () => void
  onDelete: () => void
  pending: boolean
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button size="icon" variant="ghost" className="h-8 w-8" aria-label="Source actions" disabled={pending}>
          <MoreHorizontal className="h-4 w-4" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-40">
        <DropdownMenuItem onClick={onCopy}>Copy ingest URL</DropdownMenuItem>
        <DropdownMenuItem onClick={onToggle}>
          {source.enabled ? 'Disable' : 'Enable'}
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem onClick={onDelete} className="text-destructive">
          Delete
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

function CreateSourceDialog({
  open,
  onOpenChange,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')

  const create = useMutation({
    mutationFn: () => api.createSource({ name, description }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.sources() })
      toast.success('Source created')
      onOpenChange(false)
      setName('')
      setDescription('')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New source</DialogTitle>
          <DialogDescription>
            Sources receive webhook traffic. Each gets a unique ingest URL.
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
            <Label htmlFor="src-name" className="mb-2 block">
              Name
            </Label>
            <Input
              id="src-name"
              className="w-full"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="stripe-prod"
              required
              autoFocus
            />
          </div>
          <div>
            <Label htmlFor="src-description" className="mb-2 block">
              Description <span className="text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="src-description"
              className="w-full"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Production Stripe webhooks"
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={create.isPending || !name.trim()}>
              {create.isPending ? 'Creating…' : 'Create source'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
