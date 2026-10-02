import { useQuery } from '@tanstack/react-query'

import { api, qk, type AuthMethods } from '#/lib/api'

/**
 * useAuthMethods asks the server which sign-in methods exist, with the
 * fallback both unauthenticated pages need.
 *
 * Shared because the login page and the invite page have the same problem and
 * must not drift on it: the invite page's accept button mints a magic link,
 * which the server 403s under DSTREAM_OIDC_ENFORCE, so a page that hides the
 * SSO control when the probe fails offers only the control that cannot work.
 *
 * `methods` is null only while the probe is still in flight — pages render
 * neither control then, since flashing a form that disappears is worse than a
 * blank beat. Anything other than "still trying" falls back to offering BOTH
 * methods: we cannot tell which one this deployment has, and offering both is
 * strictly safer than guessing. Under enforcement the email form 403s and the
 * SSO button is the only way in; in the other direction api.ssoStartUrl() is a
 * static URL that either 302s to the IdP or returns a legible "sso not
 * configured" 404. isPaused covers the offline case, where react-query's
 * `online` networkMode parks the query instead of failing it; the 5s timeout
 * in api.authMethods covers a proxy that accepts the connection and never
 * answers.
 *
 * `fromServer` distinguishes the server's answer from that guess, so copy can
 * avoid asserting which methods a deployment has when we are guessing.
 */
export function useAuthMethods(): { methods: AuthMethods | null; fromServer: boolean } {
  const q = useQuery({ queryKey: qk.authMethods(), queryFn: api.authMethods, retry: false })
  if (q.data) return { methods: q.data, fromServer: true }
  if (q.isError || q.isPaused) return { methods: { sso: true, magic_link: true }, fromServer: false }
  return { methods: null, fromServer: false }
}
