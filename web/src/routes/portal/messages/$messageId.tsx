import { createFileRoute, Link } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'

import type { MessageDeliveryAttempt } from '#/lib/api'
import { portalApi, portalQk } from '#/lib/portal-api'
import { capitalize } from '#/lib/utils'
import { CopyValue, DetailRow } from '#/components/detail-page'
import { Badge } from '#/components/ui/badge'
import { Button } from '#/components/ui/button'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'

export const Route = createFileRoute('/portal/messages/$messageId')({ component: PortalMessageDetail })

const statusVariant: Record<string, React.ComponentProps<typeof Badge>['variant']> = {
  delivered: 'success',
  queued: 'secondary',
  in_flight: 'info',
  disabled: 'secondary',
  dead: 'destructive',
}

function statusBadge(status: number | null | undefined) {
  if (status == null) return <span className="text-muted-foreground">—</span>
  return (
    <Badge variant={status >= 200 && status < 300 ? 'success' : 'destructive'}>{status}</Badge>
  )
}

function PortalMessageDetail() {
  const { messageId } = Route.useParams()
  const { data: msg, error } = useQuery({
    queryKey: portalQk.message(messageId),
    queryFn: () => portalApi.getMessage(messageId),
  })

  if (error) {
    return (
      <div className="space-y-3">
        <Link to="/portal/messages" className="text-sm text-muted-foreground hover:text-foreground">
          ← Back to messages
        </Link>
        <p className="text-sm text-muted-foreground">Message not found.</p>
      </div>
    )
  }

  if (!msg) {
    return <p className="text-sm text-muted-foreground">Loading…</p>
  }

  return (
    <div className="space-y-8">
      <Link to="/portal/messages" className="text-sm text-muted-foreground hover:text-foreground">
        ← Back to messages
      </Link>

      <section className="space-y-3">
        <h2 className="text-base font-semibold">Message details</h2>
        <div className="max-w-2xl space-y-3">
          <DetailRow label="Event type">
            <span className="font-mono text-xs">{msg.event_type}</span>
          </DetailRow>
          <DetailRow label="Event ID">
            {msg.event_id ? <CopyValue value={msg.event_id} what="Event ID" mono /> : '—'}
          </DetailRow>
          <DetailRow label="Message ID">
            <CopyValue value={msg.id} what="Message ID" mono />
          </DetailRow>
          <DetailRow label="Created at">{new Date(msg.created_at).toLocaleString()}</DetailRow>
        </div>
        <div>
          <div className="mb-1 text-xs font-medium text-muted-foreground">Payload</div>
          <pre className="overflow-x-auto rounded border border-border bg-muted px-3 py-2 font-mono text-xs">
            {JSON.stringify(msg.payload, null, 2)}
          </pre>
        </div>
      </section>

      <Activity messageId={messageId} />
    </div>
  )
}

function Activity({ messageId }: { messageId: string }) {
  const qc = useQueryClient()
  const [expanded, setExpanded] = useState<Set<string>>(new Set())

  const { data: deliveries, error: delErr } = useQuery({
    queryKey: portalQk.messageDeliveries(messageId),
    queryFn: () => portalApi.listMessageDeliveries(messageId),
    refetchInterval: 5000,
  })
  const { data: attempts, error: attErr } = useQuery({
    queryKey: portalQk.messageAttempts(messageId),
    queryFn: () => portalApi.listMessageAttempts(messageId),
    refetchInterval: 5000,
  })

  // endpoint_id is unique per delivery row within a message, so gating on the
  // in-flight mutation's variable disables only the clicked row's button.
  const replay = useMutation({
    mutationFn: (endpointId: string) => portalApi.replayDelivery(messageId, endpointId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: portalQk.messageDeliveries(messageId) })
      qc.invalidateQueries({ queryKey: portalQk.messageAttempts(messageId) })
      toast.success('Replay queued')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  function toggle(id: string) {
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const deliveryRows = deliveries ?? []
  const attemptRows = attempts ?? []
  const epByDelivery = new Map(deliveryRows.map((d) => [d.delivery_id, d.endpoint_url]))

  return (
    <>
      <section className="space-y-3">
        <h2 className="text-base font-semibold">Deliveries</h2>
        {delErr && <p className="py-3 text-sm text-destructive">{(delErr as Error).message}</p>}
        <div className="rounded-lg border border-border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-6">Endpoint</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Attempts</TableHead>
                <TableHead>Next retry</TableHead>
                <TableHead className="pr-6" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {deliveryRows.map((d) => (
                <TableRow key={d.delivery_id}>
                  <TableCell className="pl-6 font-mono text-xs">{d.endpoint_url}</TableCell>
                  <TableCell>
                    <Badge variant={statusVariant[d.status] || 'secondary'}>
                      {capitalize(d.status.replace('_', ' '))}
                    </Badge>
                  </TableCell>
                  <TableCell>{d.attempt_count}</TableCell>
                  <TableCell className="whitespace-nowrap text-muted-foreground">
                    {d.next_retry_at ? new Date(d.next_retry_at).toLocaleString() : '—'}
                  </TableCell>
                  <TableCell className="pr-6 text-right">
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => replay.mutate(d.endpoint_id)}
                      disabled={replay.isPending && replay.variables === d.endpoint_id}
                    >
                      Replay
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
              {deliveryRows.length === 0 && (
                <TableRow>
                  <TableCell colSpan={5} className="py-12 text-center text-sm text-muted-foreground">
                    No deliveries yet.
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </div>
      </section>

      <section className="space-y-3">
        <h2 className="text-base font-semibold">Attempts</h2>
        {attErr && <p className="py-3 text-sm text-destructive">{(attErr as Error).message}</p>}
        <div className="rounded-lg border border-border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-6">Endpoint</TableHead>
                <TableHead>Attempt</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Duration</TableHead>
                <TableHead>Attempted</TableHead>
                <TableHead className="pr-6">Error</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {attemptRows.map((a) => (
                <AttemptRow
                  key={a.id}
                  attempt={a}
                  endpoint={epByDelivery.get(a.delivery_id) ?? a.delivery_id}
                  open={expanded.has(a.id)}
                  onToggle={() => toggle(a.id)}
                />
              ))}
              {attemptRows.length === 0 && (
                <TableRow>
                  <TableCell colSpan={6} className="py-12 text-center text-sm text-muted-foreground">
                    No delivery attempts yet.
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </div>
      </section>
    </>
  )
}

function AttemptRow({
  attempt: a,
  endpoint,
  open,
  onToggle,
}: {
  attempt: MessageDeliveryAttempt
  endpoint: string
  open: boolean
  onToggle: () => void
}) {
  return (
    <>
      <TableRow className="cursor-pointer" onClick={onToggle}>
        <TableCell className="pl-6 font-mono text-xs">{endpoint}</TableCell>
        <TableCell>{a.attempt_num}</TableCell>
        <TableCell>{statusBadge(a.response_status)}</TableCell>
        <TableCell className="text-muted-foreground">
          {a.duration_ms == null ? '—' : `${a.duration_ms}ms`}
        </TableCell>
        <TableCell className="whitespace-nowrap text-muted-foreground">
          {new Date(a.attempted_at).toLocaleString()}
        </TableCell>
        <TableCell className="pr-6 text-muted-foreground">{a.error_message || '—'}</TableCell>
      </TableRow>
      {open && (
        <TableRow>
          <TableCell colSpan={6} className="bg-muted/40 pl-6">
            <div className="space-y-3 py-2">
              <div>
                <div className="mb-1 text-xs font-medium text-muted-foreground">
                  Response headers
                </div>
                <pre className="overflow-x-auto rounded border border-border bg-muted px-3 py-2 font-mono text-xs">
                  {a.response_headers != null
                    ? JSON.stringify(a.response_headers, null, 2)
                    : '—'}
                </pre>
              </div>
              <div>
                <div className="mb-1 text-xs font-medium text-muted-foreground">Response body</div>
                <pre className="overflow-x-auto rounded border border-border bg-muted px-3 py-2 font-mono text-xs">
                  {a.response_body || '—'}
                </pre>
              </div>
            </div>
          </TableCell>
        </TableRow>
      )}
    </>
  )
}
