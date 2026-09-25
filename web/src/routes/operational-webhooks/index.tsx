import { createFileRoute } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'

import { api, qk } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { PageHeader } from '#/components/TopBar'
import { EndpointsTab, OverviewTab } from '#/routes/applications/$id'

export const Route = createFileRoute('/operational-webhooks/')({
  component: OperationalWebhooksPage,
  errorComponent: AuthErrorBoundary,
})

function OperationalWebhooksPage() {
  const { data: app, error, isLoading } = useQuery({
    queryKey: qk.operationalApp(),
    queryFn: api.getOperationalApp,
  })

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader title="Operational webhooks"
        help="Platform notifications about your own account — delivery failures and endpoint health."
      />
      <div className="flex-1 overflow-y-auto px-6 py-4">
        <p className="mb-6 max-w-2xl text-sm text-muted-foreground">
          dstream delivers <code className="font-mono text-xs">endpoint.disabled</code> and{' '}
          <code className="font-mono text-xs">message.attempt.exhausted</code> events here.
        </p>
        {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
        {error && <p className="text-sm text-destructive">{(error as Error).message}</p>}
        {app && (
          <div className="space-y-8">
            <OverviewTab app={app} />
            <EndpointsTab appId={app.id} />
          </div>
        )}
      </div>
    </div>
  )
}
