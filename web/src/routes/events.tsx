import { createFileRoute, Link } from '@tanstack/react-router'
import { queryOptions, useQuery } from '@tanstack/react-query'

import { api, qk } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { Badge } from '#/components/ui/badge'
import { Card } from '#/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'

const eventsQuery = (params: { limit: number }) =>
  queryOptions({
    queryKey: qk.events(params),
    queryFn: () => api.listEvents(params),
    refetchInterval: 5000,
  })

export const Route = createFileRoute('/events')({
  loader: ({ context }) => context.queryClient.ensureQueryData(eventsQuery({ limit: 100 })),
  component: EventsPage,
  errorComponent: AuthErrorBoundary,
})

const statusVariant: Record<string, React.ComponentProps<typeof Badge>['variant']> = {
  delivered: 'success',
  queued: 'secondary',
  in_flight: 'info',
  failed: 'destructive',
  paused: 'warning',
  dead: 'destructive',
}

function EventsPage() {
  const { data: events, error } = useQuery(eventsQuery({ limit: 100 }))

  return (
    <main className="page-wrap mx-auto space-y-6 px-4 pt-10 pb-16">
      <h1 className="text-2xl font-semibold">Events</h1>

      {error && <p className="text-sm text-destructive">{(error as Error).message}</p>}

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>ID</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Attempts</TableHead>
              <TableHead>Last attempt</TableHead>
              <TableHead>Next retry</TableHead>
              <TableHead>Created</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {events?.map((e) => (
              <TableRow key={e.id}>
                <TableCell className="font-mono text-xs">
                  <Link
                    to="/events/$id"
                    params={{ id: e.id }}
                    className="text-primary hover:underline"
                  >
                    {e.id.slice(0, 8)}
                  </Link>
                </TableCell>
                <TableCell>
                  <Badge variant={statusVariant[e.status] || 'secondary'}>{e.status}</Badge>
                </TableCell>
                <TableCell>{e.attempt_count}</TableCell>
                <TableCell className="text-muted-foreground">
                  {e.last_attempt_at ? new Date(e.last_attempt_at).toLocaleString() : '—'}
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {e.next_retry_at ? new Date(e.next_retry_at).toLocaleString() : '—'}
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {new Date(e.created_at).toLocaleString()}
                </TableCell>
              </TableRow>
            ))}
            {events && events.length === 0 && (
              <TableRow>
                <TableCell colSpan={6} className="text-center text-sm text-muted-foreground">
                  No events yet.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </Card>
    </main>
  )
}
