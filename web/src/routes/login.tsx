import { createFileRoute } from '@tanstack/react-router'
import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'
import { ArrowRight, CheckCircle2, MailCheck } from 'lucide-react'

import { toast } from 'sonner'

import { api } from '#/lib/api'
import { useAuthMethods } from '#/lib/useAuthMethods'
import ThemeToggle from '#/components/ThemeToggle'
import { Button, buttonVariants } from '#/components/ui/button'
import { Input } from '#/components/ui/input'
import { Label } from '#/components/ui/label'

export const Route = createFileRoute('/login')({ component: Login })

const PERKS = [
  'Receive, route, retry, and replay every webhook',
  'Test against live traffic with the CLI tunnel',
  'Full audit trail on every delivery',
]

function BrandPanel() {
  return (
    <div className="relative hidden flex-col justify-between bg-foreground p-12 text-background lg:flex">
      <div className="flex items-center gap-2 text-lg font-bold tracking-tight">
        <span className="flex h-7 w-7 items-center justify-center rounded-md bg-background text-foreground">
          <svg viewBox="0 0 16 16" className="h-4 w-4" aria-hidden="true">
            <path
              fill="currentColor"
              d="M2 4.5 8 1l6 3.5v7L8 15l-6-3.5v-7Zm6 1.2L4.7 7.6 8 9.5l3.3-1.9L8 5.7Z"
            />
          </svg>
        </span>
        dstream
      </div>

      <div>
        <h2 className="max-w-sm text-3xl font-extrabold tracking-tight text-balance">
          Reliable infrastructure for every webhook your team relies on.
        </h2>
        <ul className="mt-8 space-y-3">
          {PERKS.map((p) => (
            <li key={p} className="flex items-start gap-3 text-sm text-background/80">
              <CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0 text-background/60" />
              {p}
            </li>
          ))}
        </ul>
      </div>

      <p className="font-mono text-xs tracking-wide text-background/50 uppercase">
        OSS webhook gateway
      </p>
    </div>
  )
}

function Login() {
  const [email, setEmail] = useState('')
  // Ask the server which methods exist before rendering any control; see
  // useAuthMethods for the probe-failure fallback and why it offers both.
  const { methods: available, fromServer } = useAuthMethods()

  const request = useMutation({
    // Normalize email to match the server (trim + lowercase) so the
    // per-email rate-limit key is stable across casing variations the
    // user might type.
    mutationFn: (e: string) => api.requestMagicLink(e.trim().toLowerCase()),
    onError: (e) => toast.error((e as Error).message),
  })

  function submit(e: React.FormEvent) {
    e.preventDefault()
    request.mutate(email)
  }

  return (
    <main className="grid min-h-screen lg:grid-cols-2">
      <BrandPanel />

      <div className="absolute top-4 right-4">
        <ThemeToggle />
      </div>

      <div className="flex items-center justify-center px-6 py-16">
        <div className="w-full max-w-sm">
          {request.isSuccess ? (
            <div className="text-center">
              <div className="mx-auto flex h-12 w-12 items-center justify-center rounded-full bg-primary/10 text-primary">
                <MailCheck className="h-6 w-6" />
              </div>
              <h1 className="mt-6 text-2xl font-bold tracking-tight text-foreground">
                Check your email
              </h1>
              <p className="mt-2 text-sm text-muted-foreground">
                If <strong className="text-foreground">{email}</strong> is a known account, a
                single-use sign-in link is on its way.
              </p>
              <p className="mt-6 rounded-md border border-border bg-muted/50 px-4 py-3 text-xs text-muted-foreground">
                Dev tip: dstream logs the link to stdout when SMTP is not configured.
              </p>
              <Button
                variant="ghost"
                size="sm"
                className="mt-4"
                onClick={() => {
                  request.reset()
                  setEmail('')
                }}
              >
                Use a different email
              </Button>
            </div>
          ) : (
            <>
              <h1 className="text-2xl font-bold tracking-tight text-foreground">
                Sign in to dstream
              </h1>
              {available && (
                <p className="mt-2 text-sm text-muted-foreground">
                  {/* Neutral copy when `available` is the fallback rather than
                      the server's answer: both controls are rendered then, and
                      describing either as THE way in would be a guess. An
                      enforced deployment with a dead probe would otherwise
                      read "Passwordless — enter your email" directly above the
                      SSO button that is its only working route. */}
                  {!fromServer ? (
                    "We couldn't reach the server to check which sign-in methods are enabled — both are offered below. Use whichever this deployment supports."
                  ) : available.magic_link ? (
                    <>
                      Passwordless — enter your email and we&apos;ll send a single-use link. New
                      here? This creates your account.
                    </>
                  ) : available.sso ? (
                    'This deployment uses single sign-on. Continue with your identity provider.'
                  ) : (
                    'No sign-in method is configured for this deployment. Ask your administrator.'
                  )}
                </p>
              )}

              {!available ? (
                <p className="mt-8 text-sm text-muted-foreground">Loading sign-in options…</p>
              ) : (
                <>
                  {available.sso && (
                    // A plain anchor, not a Button+fetch: /api/auth/sso/start
                    // 302s to the IdP, so it needs a document navigation.
                    <a
                      href={api.ssoStartUrl()}
                      className={buttonVariants({ className: 'mt-8 w-full' })}
                    >
                      Sign in with SSO
                      <ArrowRight className="h-4 w-4" />
                    </a>
                  )}

                  {available.sso && available.magic_link && (
                    <div className="mt-6 flex items-center gap-3">
                      <span className="h-px flex-1 bg-border" />
                      <span className="font-mono text-xs tracking-wide text-muted-foreground uppercase">
                        or
                      </span>
                      <span className="h-px flex-1 bg-border" />
                    </div>
                  )}

                  {available.magic_link && (
                    <form onSubmit={submit} className="mt-8 space-y-4">
                      <div className="space-y-2">
                        <Label htmlFor="email">Email</Label>
                        <Input
                          id="email"
                          type="email"
                          required
                          autoFocus
                          value={email}
                          onChange={(e) => setEmail(e.target.value)}
                          placeholder="you@example.com"
                        />
                      </div>
                      <Button type="submit" className="w-full" disabled={request.isPending}>
                        {request.isPending ? (
                          'Sending…'
                        ) : (
                          <>
                            Send magic link
                            <ArrowRight className="h-4 w-4" />
                          </>
                        )}
                      </Button>
                    </form>
                  )}
                </>
              )}

              <p className="mt-6 text-center text-xs text-muted-foreground">
                By continuing you agree to the terms of the dstream instance you&apos;re signing into.
              </p>
            </>
          )}
        </div>
      </div>
    </main>
  )
}
