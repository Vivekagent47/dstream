import { createFileRoute, Link, Outlet } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'

import { portalApi, portalQk, setPortalToken, getPortalToken } from '#/lib/portal-api'

export const Route = createFileRoute('/portal')({ component: PortalLayout })

function PortalLayout() {
  const [ready, setReady] = useState(false)
  const [token, setToken] = useState<string | null>(null)
  useEffect(() => {
    const m = window.location.hash.match(/token=([^&]+)/)
    if (m) {
      const t = decodeURIComponent(m[1])
      setPortalToken(t)
      history.replaceState(null, '', window.location.pathname + window.location.search)
    }
    setToken(getPortalToken())
    setReady(true)
  }, [])

  const app = useQuery({
    queryKey: portalQk.app,
    queryFn: portalApi.getApp,
    enabled: ready && !!token,
    retry: false,
  })

  // Server has no hash → hold a stable shell until the client effect runs so
  // there's no hydration flip.
  if (!ready) {
    return <div className="p-10 text-center text-sm text-muted-foreground">Loading…</div>
  }
  if (!token || app.isError) {
    return <PortalExpired />
  }
  return (
    <div className="mx-auto max-w-4xl px-6 py-8">
      <header className="mb-6 border-b pb-4">
        <h1 className="text-lg font-semibold">{app.data?.name ?? 'Webhooks'}</h1>
        <p className="text-sm text-muted-foreground">Manage your webhook endpoints</p>
        <nav className="mt-3 flex gap-4 text-sm">
          <Link
            to="/portal"
            className="text-muted-foreground hover:text-foreground [&.active]:font-medium [&.active]:text-foreground"
            activeOptions={{ exact: true }}
          >
            Endpoints
          </Link>
          <Link
            to="/portal/messages"
            className="text-muted-foreground hover:text-foreground [&.active]:font-medium [&.active]:text-foreground"
          >
            Messages
          </Link>
        </nav>
      </header>
      <Outlet />
    </div>
  )
}

function PortalExpired() {
  return (
    <div className="mx-auto max-w-md px-6 py-16 text-center">
      <h1 className="text-lg font-semibold">This portal link has expired</h1>
      <p className="mt-2 text-sm text-muted-foreground">
        Ask whoever shared it with you for a fresh link.
      </p>
    </div>
  )
}
