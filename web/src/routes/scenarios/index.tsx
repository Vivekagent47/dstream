import { createFileRoute } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { toast } from 'sonner'
import { ArrowDown, ArrowUp, Plus, X } from 'lucide-react'

import { api, qk, type Bookmark, type Scenario, type ScenarioReplayResult } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { ConfirmDialog } from '#/components/ConfirmDialog'
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

const scenariosQuery = queryOptions({
  queryKey: qk.scenarios(),
  queryFn: () => api.listScenarios(),
})
const bookmarksQuery = queryOptions({
  queryKey: qk.bookmarks({}),
  queryFn: () => api.listBookmarks(),
})

export const Route = createFileRoute('/scenarios/')({
  // Client-only prefetch — same SSR-cookie caveat as /fixtures.
  loader: ({ context }) =>
    typeof window === 'undefined'
      ? undefined
      : Promise.all([
          context.queryClient.ensureQueryData(scenariosQuery),
          context.queryClient.ensureQueryData(bookmarksQuery),
        ]),
  component: ScenariosPage,
  errorComponent: AuthErrorBoundary,
})

function bookmarkLabel(b: Bookmark) {
  return b.name || `${b.http_method} ${b.http_path}`
}

function ScenariosPage() {
  const qc = useQueryClient()
  const { data: scenarios, error } = useQuery(scenariosQuery)
  const { data: bookmarks } = useQuery(bookmarksQuery)

  const [dialogOpen, setDialogOpen] = useState(false)
  const [editingId, setEditingId] = useState<string | null>(null)
  const [replayId, setReplayId] = useState<string | null>(null)

  const remove = useMutation({
    mutationFn: (id: string) => api.deleteScenario(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.scenarios() })
      toast.success('Scenario deleted')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function openCreate() {
    setEditingId(null)
    setDialogOpen(true)
  }
  function openEdit(s: Scenario) {
    setEditingId(s.id)
    setDialogOpen(true)
  }

  const rows = scenarios ?? []

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title="Scenarios"
        help="Named, ordered sequences of fixtures replayed in order to drive multi-step test flows."
        actions={
          <Button size="sm" onClick={openCreate}>
            <Plus className="h-4 w-4" /> New scenario
          </Button>
        }
      />

      <div className="flex-1 overflow-x-auto">
        {error && <p className="px-6 py-3 text-sm text-destructive">{(error as Error).message}</p>}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Name</TableHead>
              <TableHead>Description</TableHead>
              <TableHead>Steps</TableHead>
              <TableHead>Updated</TableHead>
              <TableHead className="pr-6 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((s) => (
              <TableRow key={s.id}>
                <TableCell className="pl-6 font-medium">{s.name}</TableCell>
                <TableCell className="max-w-xs truncate text-muted-foreground">
                  {s.description || '—'}
                </TableCell>
                <TableCell>{s.steps.length}</TableCell>
                <TableCell className="whitespace-nowrap text-muted-foreground">
                  {new Date(s.updated_at).toLocaleString()}
                </TableCell>
                <TableCell className="pr-6 text-right">
                  <div className="flex justify-end gap-1.5">
                    <Button size="sm" variant="outline" onClick={() => openEdit(s)}>
                      Edit
                    </Button>
                    <Button size="sm" variant="outline" onClick={() => setReplayId(s.id)}>
                      Replay
                    </Button>
                    <ConfirmDialog
                      title={`Delete ${s.name}?`}
                      description="This removes the scenario. It doesn't affect the fixtures it replays."
                      confirmLabel="Delete"
                      destructive
                      pending={remove.isPending}
                      onConfirm={() => remove.mutate(s.id)}
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
                <TableCell colSpan={5} className="py-12 text-center text-sm text-muted-foreground">
                  No scenarios yet — build one from your saved fixtures.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <ScenarioDialog
        scenarioId={editingId}
        bookmarks={bookmarks ?? []}
        open={dialogOpen}
        onOpenChange={setDialogOpen}
      />
      <ReplayDialog
        scenarioId={replayId}
        open={replayId !== null}
        onOpenChange={(o) => !o && setReplayId(null)}
      />
    </div>
  )
}

interface StepRow {
  key: string
  bookmark_id: string
  delay_ms: number
}

function ScenarioDialog({
  scenarioId,
  bookmarks,
  open,
  onOpenChange,
}: {
  scenarioId: string | null
  bookmarks: Bookmark[]
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [steps, setSteps] = useState<StepRow[]>([])
  const keySeq = useRef(0)
  const nextKey = () => `s${keySeq.current++}`

  // The scenarios list omits steps, so editing needs the detail GET.
  const { data: full } = useQuery({
    queryKey: qk.scenario(scenarioId ?? ''),
    queryFn: () => api.getScenario(scenarioId as string),
    enabled: open && !!scenarioId,
  })

  // (Re)populate the form whenever the dialog opens, from the fetched
  // scenario when editing or blank defaults when creating — mirrors
  // fixtures' CaptureRuleDialog reset pattern.
  useEffect(() => {
    if (!open) return
    if (scenarioId) {
      if (!full) return
      setName(full.name)
      setDescription(full.description)
      setSteps(
        full.steps.map((st) => ({
          key: nextKey(),
          bookmark_id: st.bookmark_id,
          delay_ms: st.delay_ms,
        })),
      )
    } else {
      setName('')
      setDescription('')
      setSteps([])
    }
  }, [open, scenarioId, full])

  const save = useMutation({
    mutationFn: () => {
      const body = {
        name: name.trim(),
        // Always send description (empty string clears it); undefined would be
        // dropped by axios and the server treats a missing field as "unchanged".
        description: description.trim(),
        steps: steps.map((s) => ({ bookmark_id: s.bookmark_id, delay_ms: s.delay_ms })),
      }
      return scenarioId ? api.updateScenario(scenarioId, body) : api.createScenario(body)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.scenarios() })
      toast.success(scenarioId ? 'Scenario updated' : 'Scenario created')
      onOpenChange(false)
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function addStep() {
    if (bookmarks.length === 0) return
    setSteps((rows) => [...rows, { key: nextKey(), bookmark_id: bookmarks[0].id, delay_ms: 0 }])
  }
  function removeStep(key: string) {
    setSteps((rows) => rows.filter((r) => r.key !== key))
  }
  function moveStep(index: number, dir: -1 | 1) {
    setSteps((rows) => {
      const j = index + dir
      if (j < 0 || j >= rows.length) return rows
      const next = [...rows]
      ;[next[index], next[j]] = [next[j], next[index]]
      return next
    })
  }
  function updateStep(key: string, patch: Partial<Pick<StepRow, 'bookmark_id' | 'delay_ms'>>) {
    setSteps((rows) => rows.map((r) => (r.key === key ? { ...r, ...patch } : r)))
  }

  const invalid = !name.trim() || steps.length === 0 || steps.some((s) => !s.bookmark_id)

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>{scenarioId ? 'Edit scenario' : 'New scenario'}</DialogTitle>
          <DialogDescription>
            An ordered sequence of fixture replays, each fired after its own delay.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            if (!name.trim()) {
              toast.error('Name is required')
              return
            }
            if (steps.length === 0) {
              toast.error('Add at least one step')
              return
            }
            save.mutate()
          }}
          className="space-y-4"
        >
          <div>
            <Label htmlFor="scenario-name" className="mb-2 block">
              Name
            </Label>
            <Input
              id="scenario-name"
              className="w-full"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="checkout flow"
              autoFocus
            />
          </div>
          <div>
            <Label htmlFor="scenario-description" className="mb-2 block">
              Description <span className="text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="scenario-description"
              className="w-full"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Simulates a full checkout"
            />
          </div>
          <div>
            <Label className="mb-2 block">Steps</Label>
            <div className="space-y-2">
              {steps.map((step, i) => (
                <div key={step.key} className="flex items-center gap-1.5">
                  <Select
                    value={step.bookmark_id}
                    onValueChange={(v) => updateStep(step.key, { bookmark_id: v ?? '' })}
                  >
                    <SelectTrigger className="h-8 flex-1 text-xs">
                      <SelectValue>
                        {(v: string | null) => {
                          const b = bookmarks.find((bm) => bm.id === v)
                          return b ? bookmarkLabel(b) : 'Select a fixture'
                        }}
                      </SelectValue>
                    </SelectTrigger>
                    <SelectContent>
                      {bookmarks.map((b) => (
                        <SelectItem key={b.id} value={b.id}>
                          {bookmarkLabel(b)}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <Input
                    type="number"
                    min={0}
                    max={60000}
                    className="h-8 w-24 text-xs"
                    value={step.delay_ms}
                    onChange={(e) =>
                      updateStep(step.key, { delay_ms: Number(e.target.value) || 0 })
                    }
                  />
                  <Button
                    type="button"
                    size="sm"
                    variant="ghost"
                    onClick={() => moveStep(i, -1)}
                    disabled={i === 0}
                  >
                    <ArrowUp className="h-4 w-4" />
                  </Button>
                  <Button
                    type="button"
                    size="sm"
                    variant="ghost"
                    onClick={() => moveStep(i, 1)}
                    disabled={i === steps.length - 1}
                  >
                    <ArrowDown className="h-4 w-4" />
                  </Button>
                  <Button type="button" size="sm" variant="ghost" onClick={() => removeStep(step.key)}>
                    <X className="h-4 w-4" />
                  </Button>
                </div>
              ))}
              <Button
                type="button"
                size="sm"
                variant="outline"
                onClick={addStep}
                disabled={bookmarks.length === 0}
              >
                <Plus className="h-4 w-4" /> Add step
              </Button>
              {bookmarks.length === 0 && (
                <p className="text-xs text-muted-foreground">
                  No fixtures saved yet — save one from an event's detail page first.
                </p>
              )}
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={save.isPending || invalid}>
              {save.isPending ? 'Saving…' : scenarioId ? 'Save' : 'Create'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function ReplayDialog({
  scenarioId,
  open,
  onOpenChange,
}: {
  scenarioId: string | null
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const [url, setUrl] = useState('')
  const [results, setResults] = useState<ScenarioReplayResult[] | null>(null)

  useEffect(() => {
    if (!open) return
    setUrl('')
    setResults(null)
  }, [open])

  const replay = useMutation({
    mutationFn: () => api.replayScenarioTo(scenarioId as string, url),
    onSuccess: (r) => {
      setResults(r.results)
      toast.success('Replay complete')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Replay scenario</DialogTitle>
          <DialogDescription>
            Replays every step in order to this URL, stopping on the first error. Private and
            localhost URLs are rejected.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            replay.mutate()
          }}
          className="space-y-4"
        >
          <div>
            <Label htmlFor="scenario-replay-url" className="mb-2 block">
              URL
            </Label>
            <Input
              id="scenario-replay-url"
              className="w-full"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="https://example.com/webhook"
              autoFocus
            />
          </div>
          {results && (
            <div className="space-y-1 rounded-md border border-border p-3 text-xs">
              {results.map((r) => (
                <div key={r.position} className="flex items-center justify-between gap-2">
                  <span>Step {r.position + 1}</span>
                  {r.error ? (
                    <span className="text-destructive">{r.error}</span>
                  ) : (
                    <span className="text-muted-foreground">
                      {r.status} in {r.duration_ms}ms
                    </span>
                  )}
                </div>
              ))}
            </div>
          )}
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Close
            </Button>
            <Button type="submit" disabled={replay.isPending || !url.trim()}>
              {replay.isPending ? 'Replaying…' : 'Replay'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
