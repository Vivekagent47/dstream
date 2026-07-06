import { createFileRoute, Navigate } from '@tanstack/react-router'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { Fragment, useMemo, useState } from 'react'

import { api, qk, type AuditFilters, type AuditPage } from '#/lib/api'
import { capitalize } from '#/lib/utils'
import { PageHeader } from '#/components/TopBar'
import { Button } from '#/components/ui/button'
import { Input } from '#/components/ui/input'
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

const TARGET_TYPES = ['source', 'destination', 'connection', 'org', 'invite', 'member', 'api_key']

// Friendly labels for target types (capitalize() would give "Api_key").
const TARGET_LABELS: Record<string, string> = {
  api_key: 'API key',
}
const targetLabel = (t: string) => TARGET_LABELS[t] ?? capitalize(t)

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
    getNextPageParam: (lastPage: AuditPage) => lastPage.next_before_id,
    enabled: !!orgId,
  })

  if (meError) return <Navigate to="/" />
  if (me && !orgId) return <Navigate to="/orgs/new" />

  const rows = audit.data?.pages.flatMap((p) => p.entries) ?? []

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader title="Audit log" />

      <div className="flex flex-wrap items-center gap-3 border-b border-border px-6 py-3">
        <Select value={actorId || 'all'} onValueChange={(v) => setActorId(v === 'all' || !v ? '' : v)}>
          <SelectTrigger className="w-[200px]">
            <SelectValue>
              {(v: string | null) => {
                if (!v || v === 'all') return 'All actors'
                return members.data?.find((m) => m.user_id === v)?.email ?? v
              }}
            </SelectValue>
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
        <Select
          value={targetType || 'all'}
          onValueChange={(v) => setTargetType(v === 'all' || !v ? '' : v)}
        >
          <SelectTrigger className="w-[160px]">
            <SelectValue>
              {(v: string | null) => (!v || v === 'all' ? 'All targets' : targetLabel(v))}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All targets</SelectItem>
            {TARGET_TYPES.map((t) => (
              <SelectItem key={t} value={t}>
                {targetLabel(t)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input
          className="max-w-xs"
          value={action}
          onChange={(e) => setAction(e.target.value)}
          placeholder="Filter action (e.g. org.update)…"
        />
      </div>

      <div className="flex-1 overflow-x-auto">
        {audit.error && (
          <p className="px-6 py-3 text-sm text-destructive">{(audit.error as Error).message}</p>
        )}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-[180px] pl-6">When</TableHead>
              <TableHead>Actor</TableHead>
              <TableHead>Action</TableHead>
              <TableHead>Target</TableHead>
              <TableHead className="w-[90px] pr-6" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((row) => {
              const open = !!expanded[row.id]
              const actor = row.actor as { email?: string; name?: string; type?: string } | null
              const target = row.target as { type?: string; id?: string; name?: string } | null
              return (
                <Fragment key={row.id}>
                  <TableRow>
                    <TableCell className="pl-6 text-xs whitespace-nowrap text-muted-foreground">
                      {new Date(row.created_at).toLocaleString()}
                    </TableCell>
                    <TableCell>
                      <div className="text-sm">
                        {actor?.email ?? actor?.name ?? actor?.type ?? '—'}
                      </div>
                    </TableCell>
                    <TableCell>
                      <code className="rounded bg-muted px-1.5 py-0.5 text-xs">{row.action}</code>
                    </TableCell>
                    <TableCell>
                      <div className="text-sm">
                        {target?.type ? targetLabel(target.type) : '—'}
                        {target?.name && (
                          <span className="ml-1 text-muted-foreground">({target.name})</span>
                        )}
                      </div>
                    </TableCell>
                    <TableCell className="pr-6 text-right">
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => setExpanded((prev) => ({ ...prev, [row.id]: !prev[row.id] }))}
                      >
                        {open ? 'Hide' : 'Details'}
                      </Button>
                    </TableCell>
                  </TableRow>
                  {open && (
                    <TableRow>
                      <TableCell colSpan={5} className="px-6">
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
                <TableCell colSpan={5} className="py-12 text-center text-sm text-muted-foreground">
                  No audit entries match these filters.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <footer className="flex items-center gap-3 border-t border-border px-6 py-3 text-sm text-muted-foreground">
        <span>
          Viewing {rows.length} {rows.length === 1 ? 'entry' : 'entries'}
        </span>
        {audit.hasNextPage && (
          <Button
            variant="outline"
            size="sm"
            className="ml-auto"
            onClick={() => audit.fetchNextPage()}
            disabled={audit.isFetchingNextPage}
          >
            {audit.isFetchingNextPage ? 'Loading…' : 'Load more'}
          </Button>
        )}
      </footer>
    </div>
  )
}
