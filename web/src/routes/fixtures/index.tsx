import { createFileRoute } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useState } from 'react'
import { toast } from 'sonner'
import { Plus } from 'lucide-react'

import { api, qk, type Bookmark, type CaptureRule, type Source } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { ConfirmDialog } from '#/components/ConfirmDialog'
import { PageHeader } from '#/components/TopBar'
import { Badge } from '#/components/ui/badge'
import { Button, buttonVariants } from '#/components/ui/button'
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

// Shared textarea styling — there is no ui/textarea component and the pasted
// fixture is JSON. Mirrors ui/input's border/focus treatment (same class used
// by SendMessageDialog / applications' create dialog).
const textareaClass =
  'flex min-h-[160px] w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs shadow-sm transition-colors placeholder:text-muted-foreground focus-visible:ring-1 focus-visible:ring-ring focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

// The subset of api.importBookmark's body that comes straight from the pasted
// export blob — derived from the real client fn so this can't drift from it.
type ImportBody = Parameters<typeof api.importBookmark>[0]
type ExportedFixture = Pick<
  ImportBody,
  'method' | 'path' | 'headers' | 'content_type' | 'body_base64'
>

const sourcesQuery = queryOptions({ queryKey: qk.sources(), queryFn: () => api.listSources() })

function bookmarksQuery(params: { source_id?: string; tag?: string }) {
  return queryOptions({
    queryKey: qk.bookmarks(params),
    queryFn: () => api.listBookmarks(params),
  })
}

const captureRulesQuery = queryOptions({
  queryKey: qk.captureRules(),
  queryFn: () => api.listCaptureRules(),
})

export const Route = createFileRoute('/fixtures/')({
  // Client-only prefetch — same SSR-cookie caveat as /sources.
  loader: ({ context }) =>
    typeof window === 'undefined'
      ? undefined
      : Promise.all([
          context.queryClient.ensureQueryData(bookmarksQuery({})),
          context.queryClient.ensureQueryData(captureRulesQuery),
        ]),
  component: FixturesPage,
  errorComponent: AuthErrorBoundary,
})

function FixturesPage() {
  const qc = useQueryClient()
  const { data: sources } = useQuery(sourcesQuery)

  const [sourceId, setSourceId] = useState('all')
  const [tag, setTag] = useState('')
  const [importOpen, setImportOpen] = useState(false)

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
      <PageHeader
        title="Fixtures"
        help="Saved request captures you can replay on demand, plus rules that auto-capture matching events."
        actions={
          <Button size="sm" onClick={() => setImportOpen(true)}>
            <Plus className="h-4 w-4" /> Import
          </Button>
        }
      />

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

      <CaptureRulesSection sources={sources ?? []} />

      <ImportFixtureDialog sources={sources ?? []} open={importOpen} onOpenChange={setImportOpen} />
    </div>
  )
}

function CaptureRulesSection({ sources }: { sources: Source[] }) {
  const qc = useQueryClient()
  const { data: rules, error } = useQuery(captureRulesQuery)
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<CaptureRule | null>(null)

  const srcName = useMemo(() => new Map(sources.map((s) => [s.id, s.name])), [sources])

  const invalidate = () => qc.invalidateQueries({ queryKey: qk.captureRules() })

  const toggleEnabled = useMutation({
    mutationFn: (rule: CaptureRule) => api.updateCaptureRule(rule.id, { enabled: !rule.enabled }),
    onSuccess: () => {
      invalidate()
      toast.success('Capture rule updated')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.deleteCaptureRule(id),
    onSuccess: () => {
      invalidate()
      toast.success('Capture rule deleted')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function openCreate() {
    setEditing(null)
    setDialogOpen(true)
  }
  function openEdit(rule: CaptureRule) {
    setEditing(rule)
    setDialogOpen(true)
  }

  const rows = rules ?? []

  return (
    <div className="border-t border-border">
      <div className="flex items-center justify-between px-6 py-3">
        <h2 className="text-sm font-semibold">Capture rules</h2>
        <Button size="sm" onClick={openCreate}>
          <Plus className="h-4 w-4" /> New rule
        </Button>
      </div>

      {error && <p className="px-6 pb-3 text-sm text-destructive">{(error as Error).message}</p>}

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="pl-6">Name</TableHead>
            <TableHead>Source</TableHead>
            <TableHead>Filter</TableHead>
            <TableHead>Cap</TableHead>
            <TableHead>Enabled</TableHead>
            <TableHead className="pr-6 text-right">Actions</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((rule) => (
            <TableRow key={rule.id}>
              <TableCell className="pl-6 font-medium">{rule.name}</TableCell>
              <TableCell className="font-mono text-xs text-muted-foreground">
                {srcName.get(rule.source_id) ?? rule.source_id.slice(0, 8)}
              </TableCell>
              <TableCell className="max-w-xs truncate font-mono text-xs text-muted-foreground">
                {rule.filter_expr || '—'}
              </TableCell>
              <TableCell>{rule.cap}</TableCell>
              <TableCell>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => toggleEnabled.mutate(rule)}
                  disabled={toggleEnabled.isPending}
                >
                  {rule.enabled ? 'Enabled' : 'Disabled'}
                </Button>
              </TableCell>
              <TableCell className="pr-6 text-right">
                <div className="flex justify-end gap-1.5">
                  <Button size="sm" variant="outline" onClick={() => openEdit(rule)}>
                    Edit
                  </Button>
                  <ConfirmDialog
                    title={`Delete ${rule.name}?`}
                    description="This stops auto-capturing new fixtures for this rule. It doesn't remove fixtures already saved."
                    confirmLabel="Delete"
                    destructive
                    pending={remove.isPending}
                    onConfirm={() => remove.mutate(rule.id)}
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
                No capture rules yet — matching requests aren't auto-saved as fixtures.
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>

      <CaptureRuleDialog
        sources={sources}
        rule={editing}
        open={dialogOpen}
        onOpenChange={setDialogOpen}
      />
    </div>
  )
}

function CaptureRuleDialog({
  sources,
  rule,
  open,
  onOpenChange,
}: {
  sources: Source[]
  rule: CaptureRule | null
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [sourceId, setSourceId] = useState('')
  const [filterExpr, setFilterExpr] = useState('')
  const [cap, setCap] = useState('50')
  const [enabled, setEnabled] = useState(true)

  // (Re)populate the form whenever the dialog opens, from `rule` when editing
  // or blank defaults when creating — mirrors ImportFixtureDialog's reset.
  useEffect(() => {
    if (!open) return
    setName(rule?.name ?? '')
    setSourceId(rule?.source_id ?? '')
    setFilterExpr(rule?.filter_expr ?? '')
    setCap(String(rule?.cap ?? 50))
    setEnabled(rule?.enabled ?? true)
  }, [open, rule])

  const save = useMutation({
    mutationFn: () => {
      const capNum = Number(cap)
      if (rule) {
        return api.updateCaptureRule(rule.id, {
          name: name.trim(),
          filter_expr: filterExpr.trim() || null,
          cap: capNum,
          enabled,
        })
      }
      return api.createCaptureRule({
        source_id: sourceId,
        name: name.trim(),
        filter_expr: filterExpr.trim() || undefined,
        cap: capNum,
      })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.captureRules() })
      toast.success(rule ? 'Capture rule updated' : 'Capture rule created')
      onOpenChange(false)
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const capNum = Number(cap)
  const invalid =
    !name.trim() || !sourceId || !Number.isFinite(capNum) || capNum < 1 || capNum > 1000

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{rule ? 'Edit capture rule' : 'New capture rule'}</DialogTitle>
          <DialogDescription>
            Auto-save matching requests to this source as fixtures, up to the cap.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            save.mutate()
          }}
          className="space-y-4"
        >
          <div>
            <Label htmlFor="rule-name" className="mb-2 block">
              Name
            </Label>
            <Input
              id="rule-name"
              className="w-full"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="checkout events"
              autoFocus
            />
          </div>
          <div>
            <Label className="mb-2 block">Source</Label>
            <Select value={sourceId} onValueChange={(v) => setSourceId(v ?? '')} disabled={!!rule}>
              <SelectTrigger className="w-full">
                <SelectValue>
                  {(v: string | null) => sources.find((s) => s.id === v)?.name ?? 'Select a source'}
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                {sources.map((s) => (
                  <SelectItem key={s.id} value={s.id}>
                    {s.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div>
            <Label htmlFor="rule-filter" className="mb-2 block">
              Filter <span className="text-muted-foreground">(optional CEL expression)</span>
            </Label>
            <textarea
              id="rule-filter"
              className={textareaClass}
              value={filterExpr}
              onChange={(e) => setFilterExpr(e.target.value)}
              placeholder='body.type == "checkout.completed"'
            />
          </div>
          <div>
            <Label htmlFor="rule-cap" className="mb-2 block">
              Cap <span className="text-muted-foreground">(1–1000)</span>
            </Label>
            <Input
              id="rule-cap"
              type="number"
              min={1}
              max={1000}
              className="w-full"
              value={cap}
              onChange={(e) => setCap(e.target.value)}
            />
          </div>
          <div className="flex items-center gap-2">
            <input
              id="rule-enabled"
              type="checkbox"
              checked={enabled}
              onChange={(e) => setEnabled(e.target.checked)}
              disabled={!rule}
              className="h-4 w-4"
            />
            <Label htmlFor="rule-enabled">
              Enabled{!rule && <span className="text-muted-foreground"> (new rules start enabled)</span>}
            </Label>
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={save.isPending || invalid}>
              {save.isPending ? 'Saving…' : rule ? 'Save' : 'Create'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function ImportFixtureDialog({
  sources,
  open,
  onOpenChange,
}: {
  sources: Source[]
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const qc = useQueryClient()
  const [raw, setRaw] = useState('')
  const [sourceId, setSourceId] = useState('')
  const [name, setName] = useState('')
  const [tags, setTags] = useState('')
  const [jsonError, setJsonError] = useState<string | null>(null)

  function reset() {
    setRaw('')
    setSourceId('')
    setName('')
    setTags('')
    setJsonError(null)
  }

  const importFx = useMutation({
    mutationFn: (fixture: ExportedFixture) =>
      api.importBookmark({
        // Spread the pasted fixture FIRST so the form's chosen source/name/tags
        // always win over any stray keys the JSON might carry.
        ...fixture,
        source_id: sourceId,
        name: name.trim(),
        ...(tags.trim()
          ? { tags: tags.split(',').map((t) => t.trim()).filter(Boolean) }
          : {}),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['bookmarks'] })
      toast.success('Fixture imported')
      onOpenChange(false)
      reset()
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        onOpenChange(o)
        if (!o) reset()
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Import fixture</DialogTitle>
          <DialogDescription>
            Paste JSON exported from another fixture (or environment) and save it here.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            let fixture: ExportedFixture
            try {
              fixture = JSON.parse(raw) as ExportedFixture
            } catch {
              setJsonError('invalid JSON')
              return
            }
            setJsonError(null)
            importFx.mutate(fixture)
          }}
          className="space-y-4"
        >
          <div>
            <Label htmlFor="import-json" className="mb-2 block">
              Exported fixture JSON
            </Label>
            <textarea
              id="import-json"
              className={textareaClass}
              value={raw}
              onChange={(e) => {
                setRaw(e.target.value)
                setJsonError(null)
              }}
              placeholder='{ "method": "POST", "path": "/webhook", "headers": {}, "content_type": "application/json", "body_base64": "..." }'
            />
            {jsonError && <p className="mt-1 text-sm text-destructive">{jsonError}</p>}
          </div>
          <div>
            <Label className="mb-2 block">Source</Label>
            <Select value={sourceId} onValueChange={(v) => setSourceId(v ?? '')}>
              <SelectTrigger className="w-full">
                <SelectValue>
                  {(v: string | null) => sources.find((src) => src.id === v)?.name ?? 'Select a source'}
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                {sources.map((s) => (
                  <SelectItem key={s.id} value={s.id}>
                    {s.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div>
            <Label htmlFor="import-name" className="mb-2 block">
              Name
            </Label>
            <Input
              id="import-name"
              className="w-full"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="checkout.completed sample"
              autoFocus
            />
          </div>
          <div>
            <Label htmlFor="import-tags" className="mb-2 block">
              Tags <span className="text-muted-foreground">(optional, comma-separated)</span>
            </Label>
            <Input
              id="import-tags"
              className="w-full"
              value={tags}
              onChange={(e) => setTags(e.target.value)}
              placeholder="regression, checkout"
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={importFx.isPending || !sourceId || !name.trim() || !raw.trim()}
            >
              {importFx.isPending ? 'Importing…' : 'Import'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
