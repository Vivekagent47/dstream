import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { ArrowLeft, Copy, MinusCircle, Trash2 } from 'lucide-react'

import { api, qk } from '#/lib/api'
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

const SUPPORTED = ['POST', 'PUT', 'PATCH', 'DELETE'] as const

const sourceQuery = (id: string) =>
  queryOptions({ queryKey: qk.source(id), queryFn: () => api.getSource(id) })

export const Route = createFileRoute('/sources/$id')({
  loader: ({ context, params }) =>
    typeof window === 'undefined'
      ? undefined
      : context.queryClient.ensureQueryData(sourceQuery(params.id)),
  component: SourceDetail,
  errorComponent: AuthErrorBoundary,
})

function ingestUrl(token: string): string {
  const origin = typeof window === 'undefined' ? '' : window.location.origin
  return `${origin}/e/${token}`
}

function SourceDetail() {
  const { id } = Route.useParams()
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { data: src } = useQuery(sourceQuery(id))

  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [methods, setMethods] = useState<string[]>([])
  const [deleteOpen, setDeleteOpen] = useState(false)

  // Seed local form state from the server row once it loads / changes.
  useEffect(() => {
    if (src) {
      setName(src.name)
      setDescription(src.description)
      setMethods(src.allowed_methods)
    }
  }, [src])

  const save = useMutation({
    mutationFn: () => api.updateSource(id, { name, description, allowed_methods: methods }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.source(id) })
      qc.invalidateQueries({ queryKey: qk.sources() })
      toast.success('Source saved')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const toggleEnabled = useMutation({
    mutationFn: () => api.updateSource(id, { enabled: !src?.enabled }),
    onSuccess: (_r) => {
      qc.invalidateQueries({ queryKey: qk.source(id) })
      qc.invalidateQueries({ queryKey: qk.sources() })
      toast.success(src?.enabled ? 'Source disabled' : 'Source enabled')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: () => api.deleteSource(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.sources() })
      toast.success('Source deleted')
      navigate({ to: '/sources' })
    },
    onError: (e) => toast.error((e as Error).message),
  })

  if (!src) {
    return (
      <div className="flex flex-1 flex-col">
        <PageHeader title="Source" />
        <p className="px-6 py-8 text-sm text-muted-foreground">Loading…</p>
      </div>
    )
  }

  const dirty =
    name !== src.name ||
    description !== src.description ||
    methods.slice().sort().join(',') !== src.allowed_methods.slice().sort().join(',')

  function toggleMethod(m: string) {
    setMethods((cur) => (cur.includes(m) ? cur.filter((x) => x !== m) : [...cur, m]))
  }

  function copyUrl() {
    navigator.clipboard
      .writeText(ingestUrl(src!.ingest_token))
      .then(() => toast.success('Ingest URL copied'))
      .catch(() => toast.error('Couldn’t copy — select the URL manually'))
  }

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title={src.name}
        actions={
          <Button size="sm" onClick={() => save.mutate()} disabled={!dirty || save.isPending}>
            {save.isPending ? 'Saving…' : 'Save'}
          </Button>
        }
      />

      <div className="flex-1 overflow-y-auto px-6 py-8">
        <div className="mx-auto max-w-3xl space-y-8">
          <Link
            to="/sources"
            className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground"
          >
            <ArrowLeft className="h-4 w-4" /> Sources
          </Link>

          {/* General */}
          <section className="space-y-4">
            <div>
              <Label htmlFor="name" className="mb-2 block">Name</Label>
              <Input id="name" value={name} onChange={(e) => setName(e.target.value)} className="w-full" />
            </div>
            <div>
              <Label htmlFor="description" className="mb-2 block">
                Description <span className="text-muted-foreground">(optional)</span>
              </Label>
              <Input
                id="description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                className="w-full"
              />
            </div>
            <div>
              <Label className="mb-2 block">Ingest URL</Label>
              <button
                type="button"
                onClick={copyUrl}
                className="group flex w-full items-center justify-between gap-2 rounded-md border border-border px-3 py-2 font-mono text-xs text-muted-foreground hover:text-foreground"
                title="Copy ingest URL"
              >
                <span className="truncate">{ingestUrl(src.ingest_token)}</span>
                <Copy className="h-3.5 w-3.5 shrink-0 opacity-60 group-hover:opacity-100" />
              </button>
            </div>
          </section>

          {/* Advanced configuration */}
          <section className="space-y-3 border-t border-border pt-6">
            <h2 className="text-sm font-semibold">Advanced configuration</h2>
            <div>
              <Label className="mb-2 block">HTTP methods</Label>
              <div className="flex flex-wrap items-center gap-4">
                <label className="flex cursor-not-allowed items-center gap-2 text-sm text-muted-foreground">
                  <input type="checkbox" disabled className="size-4 accent-primary" />
                  GET <span className="text-xs">(not supported)</span>
                </label>
                {SUPPORTED.map((m) => (
                  <label key={m} className="flex cursor-pointer items-center gap-2 text-sm">
                    <input
                      type="checkbox"
                      className="size-4 accent-primary"
                      checked={methods.includes(m)}
                      onChange={() => toggleMethod(m)}
                    />
                    {m}
                  </label>
                ))}
              </div>
            </div>
          </section>

          {/* Disable */}
          <section className="space-y-2 border-t border-border pt-6">
            <h2 className="text-sm font-semibold">
              {src.enabled ? 'Disable source' : 'Enable source'}
            </h2>
            <p className="text-sm text-muted-foreground">
              {src.enabled
                ? 'Rejects all incoming requests to this source’s ingest URL.'
                : 'Re-enable this source to start accepting incoming requests again.'}
            </p>
            <Button variant="outline" onClick={() => toggleEnabled.mutate()} disabled={toggleEnabled.isPending}>
              <MinusCircle className="h-4 w-4" />
              {src.enabled ? 'Disable source' : 'Enable source'}
            </Button>
          </section>

          {/* Delete */}
          <section className="space-y-2 border-t border-border pt-6">
            <h2 className="text-sm font-semibold text-destructive">Delete source</h2>
            <p className="text-sm text-muted-foreground">
              Deletes this source and all associated connections. Incoming webhooks to its URL will fail.
            </p>
            <Button variant="destructive" onClick={() => setDeleteOpen(true)}>
              <Trash2 className="h-4 w-4" /> Delete source
            </Button>
          </section>
        </div>
      </div>

      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {src.name}?</DialogTitle>
            <DialogDescription>
              This removes the source and its ingest URL. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setDeleteOpen(false)}>Cancel</Button>
            <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
              {remove.isPending ? 'Deleting…' : 'Delete source'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
