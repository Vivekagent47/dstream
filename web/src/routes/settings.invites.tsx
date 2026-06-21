import { createFileRoute, Navigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { api, qk } from '#/lib/api'
import { Button } from '#/components/ui/button'
import { Card } from '#/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'
import { Badge } from '#/components/ui/badge'

export const Route = createFileRoute('/settings/invites')({ component: InvitesPage })

function InvitesPage() {
  const qc = useQueryClient()
  const { data: me, error: meError } = useQuery({
    queryKey: qk.me(),
    queryFn: api.me,
    retry: false,
  })
  const orgId = me?.active_org_id
  const myRole = me?.orgs?.find((o) => o.id === orgId)?.role
  const canManage = myRole === 'owner' || myRole === 'admin'

  const invites = useQuery({
    queryKey: orgId ? qk.invites(orgId) : ['invites', 'none'],
    queryFn: () => api.listInvites(orgId as string),
    enabled: !!orgId,
  })

  const revoke = useMutation({
    mutationFn: (id: string) => api.revokeInvite(orgId as string, id),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.invites(orgId as string) }),
  })

  if (meError) return <Navigate to="/login" />
  if (me && !orgId) return <Navigate to="/orgs/new" />

  return (
    <main className="page-wrap mx-auto space-y-6 px-4 pt-10 pb-16">
      <h1 className="text-2xl font-semibold">Pending invites</h1>
      <p className="text-sm text-muted-foreground">
        Issue new invites from the{' '}
        <a href="/settings/members" className="underline">
          Members
        </a>{' '}
        page.
      </p>

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Email</TableHead>
              <TableHead>Role</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Invited by</TableHead>
              <TableHead>Expires</TableHead>
              <TableHead className="w-[100px]" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {invites.data?.map((inv) => {
              const accepted = !!inv.accepted_at
              return (
                <TableRow key={inv.id}>
                  <TableCell className="font-medium">{inv.email}</TableCell>
                  <TableCell>{inv.role}</TableCell>
                  <TableCell>
                    <Badge variant={accepted ? 'success' : 'secondary'}>
                      {accepted ? 'accepted' : 'pending'}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {inv.invited_by_email ?? '—'}
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {new Date(inv.expires_at).toLocaleString()}
                  </TableCell>
                  <TableCell>
                    {canManage && !accepted && (
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => revoke.mutate(inv.id)}
                        disabled={revoke.isPending}
                      >
                        Revoke
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              )
            })}
            {invites.data && invites.data.length === 0 && (
              <TableRow>
                <TableCell colSpan={6} className="text-center text-sm text-muted-foreground">
                  No invites.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </Card>
    </main>
  )
}
