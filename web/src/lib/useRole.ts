import { useQuery } from '@tanstack/react-query'

import { api, qk, type Role } from '#/lib/api'

/**
 * useRole reports the caller's role in the active org.
 *
 * This mirrors the server matrix in internal/api/router.go: members read
 * and write, admins additionally delete and handle secret material. The
 * server is the gate — this only avoids offering an action that would 403.
 */
export function useRole(): { role?: Role; isAdmin: boolean; isOwner: boolean } {
  const { data: me } = useQuery({ queryKey: qk.me(), queryFn: api.me, retry: false })
  const role = me?.orgs?.find((o) => o.id === me?.active_org_id)?.role
  return {
    role,
    isAdmin: role === 'owner' || role === 'admin',
    isOwner: role === 'owner',
  }
}
