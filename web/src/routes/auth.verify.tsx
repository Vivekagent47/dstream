import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { Loader2, XCircle } from 'lucide-react'

import { api, qk } from '#/lib/api'

export const Route = createFileRoute('/auth/verify')({
  validateSearch: (search: Record<string, unknown>): { token?: string } => ({
    token: typeof search.token === 'string' ? search.token : undefined,
  }),
  component: Verify,
})

function Verify() {
  const { token } = Route.useSearch()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const [error, setError] = useState<string | null>(null)
  // Guard against React StrictMode's double effect invocation — the token is
  // single-use, so a second call would consume-fail and flash a false error.
  const ran = useRef(false)

  useEffect(() => {
    if (ran.current) {
      return
    }
    ran.current = true

    // Drop the token from the address bar immediately so it doesn't linger in
    // browser history or leak via the Referer header. We already captured it.
    if (typeof window !== 'undefined' && window.location.search) {
      window.history.replaceState({}, '', '/auth/verify')
    }

    if (!token) {
      setError('This link is missing its token. Request a new one.')
      return
    }

    api
      .verifyMagicLink(token)
      .then(async () => {
        await qc.invalidateQueries({ queryKey: qk.me() })
        navigate({ to: '/sources' })
      })
      .catch((e) => setError((e as Error).message || 'This link is invalid or has expired.'))
  }, [token, navigate, qc])

  return (
    <main className="flex min-h-screen items-center justify-center px-6">
      {error ? (
        <div className="w-full max-w-sm text-center">
          <div className="mx-auto flex h-12 w-12 items-center justify-center rounded-full bg-destructive/10 text-destructive">
            <XCircle className="h-6 w-6" />
          </div>
          <h1 className="mt-6 text-2xl font-bold tracking-tight text-foreground">
            Sign-in link failed
          </h1>
          <p className="mt-2 text-sm text-muted-foreground">{error}</p>
          <Link
            to="/login"
            className="mt-6 inline-flex items-center rounded-md bg-foreground px-5 py-2.5 text-sm font-semibold text-background no-underline shadow-sm transition hover:opacity-90"
          >
            Back to sign in
          </Link>
        </div>
      ) : (
        <div className="flex items-center gap-3 text-muted-foreground">
          <Loader2 className="h-5 w-5 animate-spin" />
          Signing you in…
        </div>
      )}
    </main>
  )
}
