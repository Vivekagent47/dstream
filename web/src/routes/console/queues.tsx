import { createFileRoute } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'

import { api, qk, type QueueItem } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { PageHeader } from '#/components/TopBar'
import { ConfirmDialog } from '#/components/ConfirmDialog'
import { Button } from '#/components/ui/button'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'

export const Route = createFileRoute('/console/queues')({
  component: QueuesView,
  errorComponent: AuthErrorBoundary,
})

const LANES = ['dead', 'scheduled', 'processing', 'pending'] as const
type Lane = (typeof LANES)[number]

function ageMs(ms: number): string {
  if (!ms) return '—'
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000))
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.round(s / 60)}m`
  return `${Math.round(s / 3600)}h`
}

function QueuesView() {
  const qc = useQueryClient()
  const [lane, setLane] = useState<Lane>('dead')
  const [org, setOrg] = useState<string>('')

  const orgs = useQuery({ queryKey: qk.adminQueueOrgs(), queryFn: () => api.adminQueueOrgs() })
  const items = useQuery({
    queryKey: qk.adminQueueItems(lane, org),
    queryFn: () => api.adminQueueItems(lane, lane === 'pending' ? { org } : undefined),
    enabled: lane !== 'pending' || !!org,
  })

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['admin', 'queue-items'] })
    qc.invalidateQueries({ queryKey: qk.adminQueueOrgs() })
  }
  const requeue = useMutation({
    mutationFn: (raw: string) => api.adminRequeueDead(raw),
    onSuccess: (r) => { toast.success(r.requeued ? 'Requeued' : 'Item already moved'); invalidate() },
    onError: (e) => toast.error((e as Error).message),
  })
  const promote = useMutation({
    mutationFn: (raw: string) => api.adminPromoteScheduled(raw),
    onSuccess: (r) => { toast.success(r.promoted ? 'Promoted' : 'Item already moved'); invalidate() },
    onError: (e) => toast.error((e as Error).message),
  })
  const drain = useMutation({
    mutationFn: () => api.adminDrainDead(),
    onSuccess: (r) => { toast.success(`Drained ${r.drained}`); invalidate() },
    onError: (e) => toast.error((e as Error).message),
  })

  const rows: QueueItem[] = items.data?.items ?? []

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader title="Delivery queue" help="Inspect and act on individual queued/scheduled/processing/dead events." />
      <div className="flex-1 space-y-6 overflow-y-auto px-6 py-6">
        <div className="flex items-center gap-2">
          {LANES.map((l) => (
            <Button key={l} size="sm" variant={l === lane ? 'default' : 'outline'} onClick={() => setLane(l)}>
              {l}
            </Button>
          ))}
          {lane === 'dead' && rows.length > 0 && (
            <ConfirmDialog
              title="Drain the dead list?"
              description="Permanently discards all dead queue records. Event rows stay marked failed."
              confirmLabel="Drain"
              destructive
              pending={drain.isPending}
              onConfirm={() => drain.mutate()}
            >
              {(open) => (
                <Button size="sm" variant="destructive" className="ml-auto" onClick={open}>
                  Drain dead
                </Button>
              )}
            </ConfirmDialog>
          )}
        </div>

        {lane === 'pending' && (
          <div className="flex flex-wrap gap-2">
            {(orgs.data ?? []).map((o) => (
              <Button key={o.org_id} size="sm" variant={o.org_id === org ? 'default' : 'outline'} onClick={() => setOrg(o.org_id)}>
                {o.org_name || o.org_id.slice(0, 8)} ({o.pending})
              </Button>
            ))}
          </div>
        )}

        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-4">Event</TableHead>
              <TableHead>Org</TableHead>
              <TableHead>Attempt</TableHead>
              <TableHead>Age</TableHead>
              {lane === 'scheduled' && <TableHead>Next run</TableHead>}
              {lane === 'processing' && <TableHead>Lease</TableHead>}
              <TableHead className="pr-4 text-right">Action</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((it) => (
              <TableRow key={it.raw}>
                <TableCell className="pl-4 font-mono text-xs">
                  {it.decode_error ? <span className="text-destructive">decode error</span> : it.event_id.slice(0, 8)}
                </TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">{it.org_id.slice(0, 8)}</TableCell>
                <TableCell className="tabular-nums">{it.attempt}</TableCell>
                <TableCell className="text-muted-foreground">{ageMs(it.enqueued_at_unix_ms)}</TableCell>
                {lane === 'scheduled' && <TableCell className="text-muted-foreground">in {ageMs(2 * Date.now() - (it.next_run_ms ?? 0))}</TableCell>}
                {lane === 'processing' && <TableCell className="text-muted-foreground">{ageMs(2 * Date.now() - (it.lease_deadline_ms ?? 0))}</TableCell>}
                <TableCell className="pr-4 text-right">
                  {lane === 'dead' && !it.decode_error && (
                    <Button size="sm" variant="outline" onClick={() => requeue.mutate(it.raw)} disabled={requeue.isPending}>
                      Requeue
                    </Button>
                  )}
                  {lane === 'scheduled' && !it.decode_error && (
                    <Button size="sm" variant="outline" onClick={() => promote.mutate(it.raw)} disabled={promote.isPending}>
                      Promote
                    </Button>
                  )}
                </TableCell>
              </TableRow>
            ))}
            {rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={6} className="py-10 text-center text-sm text-muted-foreground">
                  {lane === 'pending' && !org ? 'Pick an org.' : 'No items.'}
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
        {items.data?.truncated && (
          <p className="text-xs text-muted-foreground">Showing the first {rows.length} — more exist.</p>
        )}
      </div>
    </div>
  )
}
