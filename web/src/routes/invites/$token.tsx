import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { toast } from 'sonner'

import { api, qk } from '#/lib/api'
import { useAuthMethods } from '#/lib/useAuthMethods'
import { Button, buttonVariants } from '#/components/ui/button'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '#/components/ui/card'

export const Route = createFileRoute('/invites/$token')({ component: InvitePage })

function InvitePage() {
  const { token } = Route.useParams()
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [sentEmail, setSentEmail] = useState<string | null>(null)

  const peek = useQuery({
    queryKey: ['invite', token],
    queryFn: () => api.peekInvite(token),
    retry: false,
  })

  // Accepting while signed out mints a magic link, which the server refuses
  // (403) under SSO enforcement. Offer the way through: signing in with the
  // IdP applies the pending invite during session bootstrap.
  //
  // Shared with the login page on purpose. Gating this block on the probe
  // SUCCEEDING would reproduce the dead end the hook exists to prevent: an
  // enforced deployment with a dead probe would show only "Accept invite",
  // which the server 403s, and hide the one control that works.
  const { methods, fromServer } = useAuthMethods()

  const accept = useMutation({
    mutationFn: () => api.acceptInvite(token),
    onSuccess: async (result) => {
      if (result.requires_login) {
        setSentEmail(result.email ?? peek.data?.email ?? null)
        return
      }
      // Path A: server rotated the session cookie to point at the newly-
      // joined org. Force `me` to refetch first so any consumer of qk.me()
      // sees the new active_org_id BEFORE org-scoped queries start firing;
      // then evict everything else so loaders on /events refetch with the
      // new cookie. Plain invalidateQueries() can refetch with the stale
      // cookie if the browser hasn't committed Set-Cookie yet.
      await qc.refetchQueries({ queryKey: qk.me() })
      qc.removeQueries({
        predicate: (q) => q.queryKey[0] !== 'me',
      })
      toast.success(`Joined ${peek.data?.org_name ?? 'the organization'}`)
      navigate({ to: '/events' })
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <main className="page-wrap mx-auto space-y-6 px-4 pt-10 pb-16">
      <Card className="sm:max-w-lg">
        <CardHeader>
          <CardTitle>Invite</CardTitle>
          <CardDescription>
            Accept this invite to join the organization with the assigned role.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {peek.isLoading && <p className="text-sm text-muted-foreground">Loading invite…</p>}

          {peek.error && (
            <p className="text-sm text-destructive">
              {(peek.error as Error).message ||
                'This invite is invalid, expired, or has already been used.'}
            </p>
          )}

          {peek.data && !sentEmail && (
            <>
              <p className="text-sm">
                You&rsquo;ve been invited to{' '}
                <strong className="font-semibold">{peek.data.org_name}</strong> as{' '}
                <strong className="font-semibold">{peek.data.role}</strong>.
              </p>
              <p className="text-xs text-muted-foreground">
                Invite addressed to <code>{peek.data.email}</code>. Expires{' '}
                {new Date(peek.data.expires_at).toLocaleString()}.
              </p>
              <Button onClick={() => accept.mutate()} disabled={accept.isPending}>
                {accept.isPending ? 'Accepting…' : 'Accept invite'}
              </Button>

              {methods?.sso && (
                <div className="space-y-2 border-t border-border pt-4">
                  {/* Names the address on purpose: BootstrapSession applies
                      the invite by matching the email the IdP verified, so
                      signing in as anyone else yields a session with no
                      invited-org membership and no explanation. */}
                  <p className="text-xs text-muted-foreground">
                    {/* Neither phrasing is safe when `methods` is the
                        fallback: one calls SSO optional, the other calls it
                        mandatory, and we do not know which. */}
                    {!fromServer
                      ? 'If this deployment uses single sign-on: '
                      : methods.magic_link
                        ? 'Already use single sign-on here? '
                        : 'This deployment uses single sign-on. '}
                    Sign in with <code>{peek.data.email}</code> and your invitation is applied
                    automatically.
                  </p>
                  {/* Anchor, not a fetch: /api/auth/sso/start 302s to the IdP. */}
                  <a href={api.ssoStartUrl()} className={buttonVariants({ variant: 'outline' })}>
                    Sign in with SSO
                  </a>
                </div>
              )}
            </>
          )}

          {sentEmail && (
            <div className="space-y-2">
              <p className="text-sm">
                We sent a sign-in link to <strong>{sentEmail}</strong>. Open it on this device to
                finish accepting the invite.
              </p>
              <p className="text-xs text-muted-foreground">
                You can close this tab — the link will route you back into the dashboard.
              </p>
            </div>
          )}
        </CardContent>
      </Card>
    </main>
  )
}
