import { createFileRoute, Navigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'

import { api, qk } from '#/lib/api'
import { capitalize } from '#/lib/utils'
import { PageHeader } from '#/components/TopBar'
import { Button } from '#/components/ui/button'
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
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.invites(orgId as string) })
      toast.success('Invite revoked')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  if (meError) return <Navigate to="/" />
  if (me && !orgId) return <Navigate to="/orgs/new" />

  const rows = invites.data ?? []

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader title="Pending invites" />

      <div className="flex-1 overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Email</TableHead>
              <TableHead>Role</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Invited by</TableHead>
              <TableHead>Expires</TableHead>
              <TableHead className="w-[90px] pr-6" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((inv) => {
              const accepted = !!inv.accepted_at
              return (
                <TableRow key={inv.id}>
                  <TableCell className="pl-6 font-medium">{inv.email}</TableCell>
                  <TableCell>{capitalize(inv.role)}</TableCell>
                  <TableCell>
                    <Badge variant={accepted ? 'success' : 'secondary'}>
                      {accepted ? 'Accepted' : 'Pending'}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {inv.invited_by_email ?? '—'}
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-muted-foreground">
                    {new Date(inv.expires_at).toLocaleString()}
                  </TableCell>
                  <TableCell className="pr-6 text-right">
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
            {rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={6} className="py-12 text-center text-sm text-muted-foreground">
                  No pending invites — issue one from the Members page.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <footer className="border-t border-border px-6 py-3 text-sm text-muted-foreground">
        Viewing {rows.length} {rows.length === 1 ? 'invite' : 'invites'}
      </footer>
    </div>
  )
}
