import { createFileRoute, Link } from '@tanstack/react-router'
import { useInfiniteQuery } from '@tanstack/react-query'

import { api, qk, type EventsPage as EventsPageData } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { Badge } from '#/components/ui/badge'
import { Button } from '#/components/ui/button'
import { Card } from '#/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'

const PAGE_SIZE = 100

export const Route = createFileRoute('/events')({
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
  const { data, error, fetchNextPage, hasNextPage, isFetchingNextPage } = useInfiniteQuery({
    queryKey: qk.events({ limit: PAGE_SIZE }),
    queryFn: ({ pageParam }) => api.listEvents({ limit: PAGE_SIZE, cursor: pageParam }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage: EventsPageData) => lastPage.next_cursor,
    // Poll for live status. react-query refetches all loaded pages; the first
    // page (newest events) is where status churn happens.
    refetchInterval: 5000,
  })

  const events = data?.pages.flatMap((p) => p.events) ?? []

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
            {events.map((e) => (
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
            {events.length === 0 && (
              <TableRow>
                <TableCell colSpan={6} className="text-center text-sm text-muted-foreground">
                  No events yet.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </Card>

      {hasNextPage && (
        <div className="flex justify-center">
          <Button variant="outline" onClick={() => fetchNextPage()} disabled={isFetchingNextPage}>
            {isFetchingNextPage ? 'Loading…' : 'Load more'}
          </Button>
        </div>
      )}
    </main>
  )
}
