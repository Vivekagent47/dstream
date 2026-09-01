import { createFileRoute, Link } from '@tanstack/react-router'
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'
import { AppWindow, Plus } from 'lucide-react'

import { api, qk, type Page, type Application } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { PageHeader } from '#/components/TopBar'
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

export const Route = createFileRoute('/applications/')({
  component: ApplicationsPage,
  errorComponent: AuthErrorBoundary,
})

// Shared textarea styling — there is no ui/textarea component and JSON needs a
// multi-line, monospace field. Mirrors ui/input's border/focus treatment.
const textareaClass =
  'flex min-h-[120px] w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs shadow-sm transition-colors placeholder:text-muted-foreground focus-visible:ring-1 focus-visible:ring-ring focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

function ApplicationsPage() {
  const [createOpen, setCreateOpen] = useState(false)

  const { data, error, fetchNextPage, hasNextPage, isFetchingNextPage } = useInfiniteQuery({
    queryKey: qk.applications(),
    queryFn: ({ pageParam }) => api.listApplications(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last: Page<Application>) => last.next_cursor ?? undefined,
  })

  const apps = data?.pages.flatMap((p) => p.data) ?? []

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title="Applications"
        actions={
          <Button size="sm" onClick={() => setCreateOpen(true)}>
            <Plus className="h-4 w-4" /> New application
          </Button>
        }
      />

      <div className="flex-1 overflow-x-auto">
        {error && (
          <p className="px-6 py-3 text-sm text-destructive">{(error as Error).message}</p>
        )}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Application</TableHead>
              <TableHead>UID</TableHead>
              <TableHead className="pr-6">Created</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {apps.map((a) => (
              <TableRow key={a.id}>
                <TableCell className="pl-6">
                  <Link
                    to="/applications/$id"
                    params={{ id: a.id }}
                    className="flex items-center gap-2.5 font-medium hover:underline"
                  >
                    <AppWindow className="h-4 w-4 shrink-0 text-muted-foreground" />
                    {a.name}
                  </Link>
                </TableCell>
                <TableCell>
                  {a.uid ? (
                    <span className="font-mono text-xs">{a.uid}</span>
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </TableCell>
                <TableCell className="pr-6 whitespace-nowrap text-muted-foreground">
                  {new Date(a.created_at).toLocaleDateString()}
                </TableCell>
              </TableRow>
            ))}
            {apps.length === 0 && (
              <TableRow>
                <TableCell colSpan={3} className="py-12 text-center text-sm text-muted-foreground">
                  No applications yet.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <footer className="flex items-center gap-3 border-t border-border px-6 py-3 text-sm text-muted-foreground">
        <span>
          Viewing {apps.length} {apps.length === 1 ? 'application' : 'applications'}
        </span>
        {hasNextPage && (
          <Button
            variant="outline"
            size="sm"
            className="ml-auto"
            onClick={() => fetchNextPage()}
            disabled={isFetchingNextPage}
          >
            {isFetchingNextPage ? 'Loading…' : 'Load more'}
          </Button>
        )}
      </footer>

      <CreateApplicationDialog open={createOpen} onOpenChange={setCreateOpen} />
    </div>
  )
}

// Parse a JSON textarea value. Returns { ok: true, value } where value is the
// parsed object or undefined when blank; { ok: false } after toasting on a
// syntax error so callers just abort.
function parseMetadata(raw: string): { ok: true; value: unknown } | { ok: false } {
  const trimmed = raw.trim()
  if (!trimmed) return { ok: true, value: undefined }
  try {
    return { ok: true, value: JSON.parse(trimmed) }
  } catch {
    toast.error('invalid metadata JSON')
    return { ok: false }
  }
}

function CreateApplicationDialog({
  open,
  onOpenChange,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [uid, setUid] = useState('')
  const [metadata, setMetadata] = useState('')

  const create = useMutation({
    mutationFn: (parsedMetadata: unknown) =>
      api.createApplication({
        name,
        ...(uid.trim() ? { uid: uid.trim() } : {}),
        ...(parsedMetadata !== undefined ? { metadata: parsedMetadata } : {}),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.applications() })
      toast.success('Application created')
      onOpenChange(false)
      setName('')
      setUid('')
      setMetadata('')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New application</DialogTitle>
          <DialogDescription>
            Applications group the endpoints that receive your outbound webhooks.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            const parsed = parseMetadata(metadata)
            if (!parsed.ok) return
            create.mutate(parsed.value)
          }}
          className="space-y-4"
        >
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
              required
              autoFocus
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
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={create.isPending || !name.trim()}>
              {create.isPending ? 'Creating…' : 'Create application'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
