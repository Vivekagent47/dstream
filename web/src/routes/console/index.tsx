import { createFileRoute } from '@tanstack/react-router'
import { queryOptions, useQuery } from '@tanstack/react-query'

import { api, qk } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { PageHeader } from '#/components/TopBar'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'

const overviewQuery = queryOptions({
  queryKey: qk.adminOverview(),
  queryFn: () => api.adminOverview(),
})

const orgsQuery = queryOptions({
  queryKey: qk.adminOrgs(),
  queryFn: () => api.adminOrgs(),
})

const queuesQuery = queryOptions({
  queryKey: qk.adminQueues(),
  queryFn: () => api.adminQueues(),
})

const hotQuery = queryOptions({
  queryKey: qk.adminHotDestinations(),
  queryFn: () => api.adminHotDestinations(),
})

const systemQuery = queryOptions({
  queryKey: qk.adminSystem(),
  queryFn: () => api.adminSystem(),
})

export const Route = createFileRoute('/console/')({
  // Client-only prefetch: on SSR the Node server can't forward the session
  // cookie, so an authed fetch here would 401. The component's useQuery loads
  // the data (with the cookie) after hydration. Same pattern as /sources.
  loader: async ({ context }) => {
    if (typeof window === 'undefined') return
    await Promise.all([
      context.queryClient.ensureQueryData(overviewQuery),
      context.queryClient.ensureQueryData(orgsQuery),
      context.queryClient.ensureQueryData(queuesQuery),
      context.queryClient.ensureQueryData(hotQuery),
      context.queryClient.ensureQueryData(systemQuery),
    ])
  },
  component: ConsoleOverview,
  // AuthErrorBoundary redirects to '/' on 401/403 (isUnauthorized covers both),
  // which is how a non-super-admin who URL-hacks to /console gets bounced.
  errorComponent: AuthErrorBoundary,
})

function ConsoleOverview() {
  const { data } = useQuery(overviewQuery)
  const { data: orgs } = useQuery(orgsQuery)
  const { data: queues } = useQuery(queuesQuery)
  const { data: hot } = useQuery(hotQuery)
  const { data: system } = useQuery(systemQuery)
  const rows = orgs ?? []
  const hotRows = hot ?? []

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader title="Admin overview" />

      <div className="grid gap-4 p-6 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard label="Organizations" value={data?.organizations} />
        <StatCard label="Users" value={data?.users} />
        <StatCard label="Events (24h)" value={data?.events_24h} />
        <StatCard
          label="Events / min (24h avg)"
          value={data ? data.events_per_min.toFixed(1) : undefined}
        />
      </div>

      <h2 className="px-6 pb-3 text-sm font-semibold">Top sources (last 24h)</h2>
      <div className="px-6 pb-6">
        <div className="overflow-x-auto rounded-lg border border-border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-4">Source</TableHead>
                <TableHead className="pr-4 text-right">Events</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(data?.top_sources ?? []).map((s) => (
                <TableRow key={s.source_id}>
                  <TableCell className="pl-4 font-medium">{s.source_name}</TableCell>
                  <TableCell className="pr-4 text-right tabular-nums">{s.events}</TableCell>
                </TableRow>
              ))}
              {(data?.top_sources ?? []).length === 0 && (
                <TableRow>
                  <TableCell colSpan={2} className="py-8 text-center text-sm text-muted-foreground">
                    No events in the last 24h.
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </div>
      </div>

      <h2 className="px-6 pb-3 text-sm font-semibold">Delivery queue</h2>
      <div className="grid gap-4 px-6 pb-6 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard label="Pending" value={queues?.pending} />
        <StatCard label="Scheduled" value={queues?.scheduled} />
        <StatCard label="Processing" value={queues?.processing} />
        <StatCard label="Dead" value={queues?.dead} />
      </div>

      <h2 className="px-6 pb-3 text-sm font-semibold">Hot destinations (last 24h)</h2>
      <div className="px-6 pb-6">
        <div className="overflow-x-auto rounded-lg border border-border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-4">Destination</TableHead>
                <TableHead className="text-right">Total</TableHead>
                <TableHead className="text-right">Failed</TableHead>
                <TableHead className="pr-4 text-right">Failure rate</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {hotRows.map((h) => (
                <TableRow key={h.destination_id}>
                  <TableCell className="pl-4 font-medium">{h.destination_name}</TableCell>
                  <TableCell className="text-right tabular-nums">{h.total}</TableCell>
                  <TableCell className="text-right tabular-nums text-destructive">{h.failed}</TableCell>
                  <TableCell className="pr-4 text-right tabular-nums">
                    {(h.failure_rate * 100).toFixed(1)}%
                  </TableCell>
                </TableRow>
              ))}
              {hotRows.length === 0 && (
                <TableRow>
                  <TableCell colSpan={4} className="py-8 text-center text-sm text-muted-foreground">
                    No destinations with failures in the last 24h.
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </div>
      </div>

      <h2 className="px-6 pb-3 text-sm font-semibold">System</h2>
      <div className="grid gap-4 px-6 pb-6 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard label="Version" value={system?.version} />
        <StatCard label="DB conns (in use)" value={system?.postgres.acquired_conns} />
        <StatCard label="DB conns (idle)" value={system?.postgres.idle_conns} />
        <StatCard label="DB conns (max)" value={system?.postgres.max_conns} />
      </div>
      <div className="px-6 pb-6">
        <div className="rounded-lg border border-border">
          <div className="border-b border-border px-4 py-2 text-sm text-muted-foreground">
            Redis info
          </div>
          <pre className="max-h-64 overflow-auto p-4 text-xs whitespace-pre-wrap text-muted-foreground">
            {system?.redis_info ?? '—'}
          </pre>
        </div>
      </div>

      <h2 className="px-6 pb-3 text-sm font-semibold">Organizations</h2>
      <div className="flex-1 overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Name</TableHead>
              <TableHead>Slug</TableHead>
              <TableHead>Created</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((o) => (
              <TableRow key={o.id}>
                <TableCell className="pl-6 font-medium">{o.name}</TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">{o.slug}</TableCell>
                <TableCell className="whitespace-nowrap text-muted-foreground">
                  {new Date(o.created_at).toLocaleDateString()}
                </TableCell>
              </TableRow>
            ))}
            {rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={3} className="py-12 text-center text-sm text-muted-foreground">
                  No organizations.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>
    </div>
  )
}

// ponytail: 8-line copy of the StatCard in routes/connections/$id.tsx. Not
// worth a shared module + refactoring that page for one more consumer.
function StatCard({ label, value }: { label: string; value: number | string | undefined }) {
  return (
    <div className="rounded-lg border border-border p-4">
      <div className="text-sm text-muted-foreground">{label}</div>
      <div className="mt-1 text-2xl font-semibold tabular-nums">{value ?? '—'}</div>
    </div>
  )
}
