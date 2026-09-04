import { createFileRoute, Link } from '@tanstack/react-router'
import { useInfiniteQuery } from '@tanstack/react-query'

import type { Message, Page } from '#/lib/api'
import { portalApi, portalQk } from '#/lib/portal-api'
import { Button } from '#/components/ui/button'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'

export const Route = createFileRoute('/portal/messages/')({ component: PortalMessages })

function PortalMessages() {
  const { data, error, fetchNextPage, hasNextPage, isFetchingNextPage } = useInfiniteQuery({
    queryKey: portalQk.messages,
    queryFn: ({ pageParam }) => portalApi.listMessages(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last: Page<Message>) => last.next_cursor ?? undefined,
    refetchInterval: 5000,
  })

  const messages = data?.pages.flatMap((p) => p.data) ?? []

  return (
    <div className="space-y-3">
      <h2 className="text-base font-semibold">Messages</h2>
      {error && <p className="py-3 text-sm text-destructive">{(error as Error).message}</p>}
      <div className="rounded-lg border border-border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Event type</TableHead>
              <TableHead>Event ID</TableHead>
              <TableHead className="pr-6">Created</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {messages.map((m) => (
              <TableRow key={m.id}>
                <TableCell className="pl-6 font-mono text-xs">
                  <Link
                    to="/portal/messages/$messageId"
                    params={{ messageId: m.id }}
                    className="text-primary hover:underline"
                  >
                    {m.event_type}
                  </Link>
                </TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">
                  {m.event_id || '—'}
                </TableCell>
                <TableCell className="pr-6 whitespace-nowrap text-muted-foreground">
                  {new Date(m.created_at).toLocaleString()}
                </TableCell>
              </TableRow>
            ))}
            {messages.length === 0 && (
              <TableRow>
                <TableCell colSpan={3} className="py-12 text-center text-sm text-muted-foreground">
                  No messages yet — send one to see it here.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>
      {hasNextPage && (
        <Button
          variant="outline"
          size="sm"
          onClick={() => fetchNextPage()}
          disabled={isFetchingNextPage}
        >
          {isFetchingNextPage ? 'Loading…' : 'Load more'}
        </Button>
      )}
    </div>
  )
}
