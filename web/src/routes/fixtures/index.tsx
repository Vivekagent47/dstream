import { createFileRoute } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { toast } from 'sonner'

import { api, qk, type Bookmark } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { ConfirmDialog } from '#/components/ConfirmDialog'
import { PageHeader } from '#/components/TopBar'
import { Badge } from '#/components/ui/badge'
import { Button, buttonVariants } from '#/components/ui/button'
import { Input } from '#/components/ui/input'
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

const sourcesQuery = queryOptions({ queryKey: qk.sources(), queryFn: () => api.listSources() })

function bookmarksQuery(params: { source_id?: string; tag?: string }) {
  return queryOptions({
    queryKey: qk.bookmarks(params),
    queryFn: () => api.listBookmarks(params),
  })
}

export const Route = createFileRoute('/fixtures/')({
  // Client-only prefetch — same SSR-cookie caveat as /sources.
  loader: ({ context }) =>
    typeof window === 'undefined'
      ? undefined
      : context.queryClient.ensureQueryData(bookmarksQuery({})),
  component: FixturesPage,
  errorComponent: AuthErrorBoundary,
})

function FixturesPage() {
  const qc = useQueryClient()
  const { data: sources } = useQuery(sourcesQuery)

  const [sourceId, setSourceId] = useState('all')
  const [tag, setTag] = useState('')

  const params = {
    source_id: sourceId === 'all' ? undefined : sourceId,
    tag: tag.trim() || undefined,
  }
  const { data: bookmarks, error } = useQuery(bookmarksQuery(params))

  const srcName = useMemo(
    () => new Map((sources ?? []).map((s) => [s.id, s.name])),
    [sources],
  )

  // Every mutation below only touches one row's cache-invisible server state;
  // invalidating the whole 'bookmarks' prefix refreshes every filtered view
  // that's cached under qk.bookmarks(<their params>).
  const invalidate = () => qc.invalidateQueries({ queryKey: ['bookmarks'] })

  const replay = useMutation({
    mutationFn: (id: string) => api.replayBookmark(id),
    onSuccess: (r) => {
      const n = r.event_ids.length
      toast.success(`Replayed — ${n} event${n === 1 ? '' : 's'} injected`)
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const replayTo = useMutation({
    mutationFn: ({ id, url }: { id: string; url: string }) => api.replayBookmarkTo(id, url),
    onSuccess: (r) => toast.success(`${r.status} in ${r.duration_ms}ms`),
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.deleteBookmark(id),
    onSuccess: () => {
      invalidate()
      toast.success('Fixture deleted')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function handleReplayTo(b: Bookmark) {
    const url = window.prompt('Replay to URL:')
    if (!url) return
    replayTo.mutate({ id: b.id, url })
  }

  const rows = bookmarks ?? []

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader title="Fixtures" />

      <div className="flex flex-wrap items-center gap-3 border-b border-border px-6 py-3">
        <Select value={sourceId} onValueChange={(v) => setSourceId(v ?? 'all')}>
          <SelectTrigger className="h-8 w-48 text-xs">
            <SelectValue>
              {(v: string | null) => {
                if (!v || v === 'all') return 'All sources'
                return srcName.get(v) ?? v.slice(0, 8)
              }}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All sources</SelectItem>
            {(sources ?? []).map((s) => (
              <SelectItem key={s.id} value={s.id}>
                {s.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input
          className="h-8 w-48 text-xs"
          placeholder="Filter by tag…"
          value={tag}
          onChange={(e) => setTag(e.target.value)}
        />
      </div>

      <div className="flex-1 overflow-x-auto">
        {error && <p className="px-6 py-3 text-sm text-destructive">{(error as Error).message}</p>}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Name</TableHead>
              <TableHead>Source</TableHead>
              <TableHead>Method</TableHead>
              <TableHead>Tags</TableHead>
              <TableHead>Captured</TableHead>
              <TableHead className="pr-6 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((b) => (
              <TableRow key={b.id}>
                <TableCell className="pl-6 font-medium">{b.name}</TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">
                  {srcName.get(b.source_id) ?? b.source_id.slice(0, 8)}
                </TableCell>
                <TableCell>
                  <span className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs font-semibold">
                    {b.http_method}
                  </span>
                </TableCell>
                <TableCell>
                  <div className="flex flex-wrap gap-1">
                    {b.tags.map((t) => (
                      <Badge key={t} variant="secondary">
                        {t}
                      </Badge>
                    ))}
                  </div>
                </TableCell>
                <TableCell className="whitespace-nowrap text-muted-foreground">
                  {new Date(b.captured_at).toLocaleString()}
                </TableCell>
                <TableCell className="pr-6 text-right">
                  <div className="flex justify-end gap-1.5">
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => replay.mutate(b.id)}
                      disabled={replay.isPending}
                    >
                      Replay
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => handleReplayTo(b)}
                      disabled={replayTo.isPending}
                    >
                      Replay to URL
                    </Button>
                    <a
                      href={api.exportBookmarkUrl(b.id)}
                      download
                      className={buttonVariants({ variant: 'outline', size: 'sm' })}
                    >
                      Export
                    </a>
                    <ConfirmDialog
                      title={`Delete ${b.name}?`}
                      description="This removes the saved fixture. It doesn't affect past events."
                      confirmLabel="Delete"
                      destructive
                      pending={remove.isPending}
                      onConfirm={() => remove.mutate(b.id)}
                    >
                      {(open) => (
                        <Button size="sm" variant="ghost" onClick={open}>
                          Delete
                        </Button>
                      )}
                    </ConfirmDialog>
                  </div>
                </TableCell>
              </TableRow>
            ))}
            {rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={6} className="py-12 text-center text-sm text-muted-foreground">
                  {sourceId !== 'all' || tag.trim()
                    ? 'No fixtures match these filters.'
                    : 'No fixtures yet — save one from an event’s detail page.'}
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <footer className="border-t border-border px-6 py-3 text-sm text-muted-foreground">
        Viewing {rows.length} {rows.length === 1 ? 'fixture' : 'fixtures'}
      </footer>
    </div>
  )
}
