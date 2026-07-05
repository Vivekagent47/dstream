import { useEffect } from 'react'
import { useNavigate } from '@tanstack/react-router'

import { isUnauthorized } from '#/lib/http'

// AuthErrorBoundary is the per-route `errorComponent` for authenticated
// pages. If the underlying loader/query failed with 401/403 we send the
// user to the homepage (their cookie likely expired or they were removed
// from the org). Other errors render a plain message — they're rare and
// usually transient.
//
// Tanstack Router's errorComponent receives the thrown error from
// loaders + suspended queries. We can't throw `redirect()` from inside
// a rendered component, so the navigate lives in a useEffect.
export function AuthErrorBoundary({ error }: { error: unknown }) {
  const navigate = useNavigate()
  const unauth = isUnauthorized(error)

  useEffect(() => {
    if (unauth) {
      navigate({ to: '/' })
    }
  }, [unauth, navigate])

  if (unauth) {
    return (
      <main className="page-wrap mx-auto px-4 pt-10 pb-16">
        <p className="text-sm text-muted-foreground">Redirecting…</p>
      </main>
    )
  }
  return (
    <main className="page-wrap mx-auto space-y-3 px-4 pt-10 pb-16">
      <h1 className="text-xl font-semibold">Something went wrong</h1>
      <p className="text-sm text-destructive">{(error as Error)?.message ?? 'Unknown error.'}</p>
    </main>
  )
}
