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
  const rows = orgs ?? []

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader title="Admin overview" />

      <div className="grid gap-4 p-6 sm:grid-cols-2">
        <StatCard label="Organizations" value={data?.organizations} />
        <StatCard label="Users" value={data?.users} />
      </div>

      <h2 className="px-6 pb-3 text-sm font-semibold">Delivery queue</h2>
      <div className="grid gap-4 px-6 pb-6 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard label="Pending" value={queues?.pending} />
        <StatCard label="Scheduled" value={queues?.scheduled} />
        <StatCard label="Processing" value={queues?.processing} />
        <StatCard label="Dead" value={queues?.dead} />
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
function StatCard({ label, value }: { label: string; value: number | undefined }) {
  return (
    <div className="rounded-lg border border-border p-4">
      <div className="text-sm text-muted-foreground">{label}</div>
      <div className="mt-1 text-2xl font-semibold tabular-nums">{value ?? '—'}</div>
    </div>
  )
}
