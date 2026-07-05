import { createFileRoute } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { toast } from 'sonner'

import { api, qk } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { Badge } from '#/components/ui/badge'
import { Button } from '#/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '#/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'

const eventQuery = (id: string) =>
  queryOptions({
    queryKey: qk.event(id),
    queryFn: () => api.getEvent(id),
  })

export const Route = createFileRoute('/events/$id')({
  // Client-only prefetch — SSR can't forward the session cookie (see sources).
  loader: ({ context, params }) =>
    typeof window === 'undefined'
      ? undefined
      : context.queryClient.ensureQueryData(eventQuery(params.id)),
  component: EventDetail,
  errorComponent: AuthErrorBoundary,
})

function EventDetail() {
  const { id } = Route.useParams()
  const qc = useQueryClient()
  const { data: ev, error } = useQuery(eventQuery(id))

  const retry = useMutation({
    mutationFn: () => api.retryEvent(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.event(id) })
      toast.success('Retry queued')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  if (error) {
    return (
      <main className="page-wrap mx-auto px-4 pt-10">
        <p className="text-sm text-destructive">{(error as Error).message}</p>
      </main>
    )
  }
  if (!ev) {
    return (
      <main className="page-wrap mx-auto px-4 pt-10">
        <p className="text-sm text-muted-foreground">Loading…</p>
      </main>
    )
  }

  return (
    <main className="page-wrap mx-auto space-y-6 px-4 pt-10 pb-16">
      <div className="flex items-center justify-between gap-4">
        <h1 className="text-2xl font-semibold">
          Event <span className="font-mono text-base">{ev.id.slice(0, 8)}</span>
        </h1>
        <Button onClick={() => retry.mutate()} disabled={retry.isPending}>
          {retry.isPending ? 'Retrying…' : 'Retry now'}
        </Button>
      </div>


      <Card>
        <CardContent className="grid grid-cols-2 gap-x-6 gap-y-3 p-6 sm:grid-cols-4">
          <Pair k="Status" v={<Badge variant="secondary">{ev.status}</Badge>} />
          <Pair k="Attempts" v={String(ev.attempt_count)} />
          <Pair k="Last attempt" v={ev.last_attempt_at ?? '—'} />
          <Pair k="Next retry" v={ev.next_retry_at ?? '—'} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Attempts</CardTitle>
        </CardHeader>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>#</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Duration</TableHead>
              <TableHead>Error</TableHead>
              <TableHead>When</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {ev.attempts.map((a) => (
              <TableRow key={a.id}>
                <TableCell>{a.attempt_num}</TableCell>
                <TableCell>{a.response_status ?? '—'}</TableCell>
                <TableCell>{a.duration_ms != null ? `${a.duration_ms}ms` : '—'}</TableCell>
                <TableCell className="text-destructive">{a.error_message ?? ''}</TableCell>
                <TableCell className="text-muted-foreground">
                  {new Date(a.attempted_at).toLocaleString()}
                </TableCell>
              </TableRow>
            ))}
            {ev.attempts.length === 0 && (
              <TableRow>
                <TableCell colSpan={5} className="text-center text-sm text-muted-foreground">
                  No attempts yet.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </Card>
    </main>
  )
}

function Pair({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div>
      <dt className="text-xs tracking-wide text-muted-foreground uppercase">{k}</dt>
      <dd className="mt-1 text-sm">{v}</dd>
    </div>
  )
}
