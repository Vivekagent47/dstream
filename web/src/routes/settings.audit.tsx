import { createFileRoute, Navigate } from '@tanstack/react-router'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { Fragment, useMemo, useState } from 'react'

import { api, qk, type AuditFilters, type AuditPage } from '#/lib/api'
import { Button } from '#/components/ui/button'
import { Card } from '#/components/ui/card'
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

export const Route = createFileRoute('/settings/audit')({ component: AuditPage })

const PAGE_SIZE = 50

const TARGET_TYPES = ['', 'source', 'destination', 'connection', 'org', 'invite', 'member', 'api_key']

function AuditPage() {
  const { data: me, error: meError } = useQuery({
    queryKey: qk.me(),
    queryFn: api.me,
    retry: false,
  })
  const orgId = me?.active_org_id

  const members = useQuery({
    queryKey: orgId ? qk.members(orgId) : ['members', 'none'],
    queryFn: () => api.listMembers(orgId as string),
    enabled: !!orgId,
  })

  const [action, setAction] = useState('')
  const [targetType, setTargetType] = useState('')
  const [actorId, setActorId] = useState('')
  const [expanded, setExpanded] = useState<Record<number, boolean>>({})

  const filters: AuditFilters = useMemo(
    () => ({
      action: action || undefined,
      target_type: targetType || undefined,
      actor_user_id: actorId || undefined,
      limit: PAGE_SIZE,
    }),
    [action, targetType, actorId],
  )

  const audit = useInfiniteQuery({
    queryKey: qk.audit(filters),
    queryFn: ({ pageParam }) =>
      api.listAudit({
        ...filters,
        before_id: pageParam ?? undefined,
      }),
    initialPageParam: undefined as number | undefined,
    // Server returns {entries, next_before_id} — the cursor is server-driven
    // (server omits next_before_id on the final page). We surface it as the
    // pageParam for fetchNextPage; if undefined, react-query knows we're done.
    getNextPageParam: (lastPage: AuditPage) => lastPage.next_before_id,
    enabled: !!orgId,
  })

  if (meError) return <Navigate to="/login" />
  if (me && !orgId) return <Navigate to="/orgs/new" />

  const rows = audit.data?.pages.flatMap((p) => p.entries) ?? []

  return (
    <main className="page-wrap mx-auto space-y-6 px-4 pt-10 pb-16">
      <h1 className="text-2xl font-semibold">Audit log</h1>

      <Card className="p-4">
        <div className="grid gap-4 sm:grid-cols-3">
          <div className="space-y-2">
            <Label>Actor</Label>
            <Select
              value={actorId || 'all'}
              onValueChange={(v) => setActorId(v === 'all' || !v ? '' : v)}
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">All actors</SelectItem>
                {members.data?.map((m) => (
                  <SelectItem key={m.user_id} value={m.user_id}>
                    {m.email}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label>Target type</Label>
            <Select
              value={targetType || 'all'}
              onValueChange={(v) => setTargetType(v === 'all' || !v ? '' : v)}
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">All targets</SelectItem>
                {TARGET_TYPES.filter((t) => t).map((t) => (
                  <SelectItem key={t} value={t}>
                    {t}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label htmlFor="action-filter">Action</Label>
            <Input
              id="action-filter"
              value={action}
              onChange={(e) => setAction(e.target.value)}
              placeholder="org.update, member.remove…"
            />
          </div>
        </div>
      </Card>

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>When</TableHead>
              <TableHead>Actor</TableHead>
              <TableHead>Action</TableHead>
              <TableHead>Target</TableHead>
              <TableHead className="w-[80px]" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((row) => {
              const open = !!expanded[row.id]
              const actor = row.actor as
                | { email?: string; name?: string; type?: string }
                | null
              const target = row.target as
                | { type?: string; id?: string; name?: string }
                | null
              return (
                // Keyed Fragment so React reconciles row pairs by audit id.
                // A bare <> without a key was reusing the wrong DOM nodes
                // when filters changed, leaving stale expand state.
                <Fragment key={row.id}>
                  <TableRow>
                    <TableCell className="text-xs text-muted-foreground">
                      {new Date(row.created_at).toLocaleString()}
                    </TableCell>
                    <TableCell>
                      <div className="text-sm">
                        {actor?.email ?? actor?.name ?? actor?.type ?? '—'}
                      </div>
                    </TableCell>
                    <TableCell>
                      <code className="rounded bg-muted px-1.5 py-0.5 text-xs">
                        {row.action}
                      </code>
                    </TableCell>
                    <TableCell>
                      <div className="text-sm">
                        {target?.type ? `${target.type}` : '—'}
                        {target?.name && (
                          <span className="ml-1 text-muted-foreground">
                            ({target.name})
                          </span>
                        )}
                      </div>
                    </TableCell>
                    <TableCell>
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() =>
                          setExpanded((prev) => ({ ...prev, [row.id]: !prev[row.id] }))
                        }
                      >
                        {open ? 'Hide' : 'Details'}
                      </Button>
                    </TableCell>
                  </TableRow>
                  {open && (
                    <TableRow>
                      <TableCell colSpan={5}>
                        <pre className="overflow-x-auto rounded bg-muted p-3 text-xs">
                          {JSON.stringify(
                            { actor: row.actor, target: row.target, metadata: row.metadata },
                            null,
                            2,
                          )}
                        </pre>
                      </TableCell>
                    </TableRow>
                  )}
                </Fragment>
              )
            })}
            {rows.length === 0 && !audit.isLoading && (
              <TableRow>
                <TableCell colSpan={5} className="text-center text-sm text-muted-foreground">
                  No audit entries match these filters.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>

        {audit.hasNextPage && (
          <div className="border-t p-3">
            <Button
              variant="outline"
              size="sm"
              onClick={() => audit.fetchNextPage()}
              disabled={audit.isFetchingNextPage}
            >
              {audit.isFetchingNextPage ? 'Loading…' : 'Load more'}
            </Button>
          </div>
        )}
        {audit.error && (
          <div className="border-t p-3 text-sm text-destructive">
            {(audit.error as Error).message}
          </div>
        )}
      </Card>
    </main>
  )
}
