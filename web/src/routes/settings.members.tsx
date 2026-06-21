import { createFileRoute, Navigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { api, qk, type Role } from '#/lib/api'
import { ConfirmDialog } from '#/components/ConfirmDialog'
import { Button } from '#/components/ui/button'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '#/components/ui/card'
import { Input } from '#/components/ui/input'
import { Label } from '#/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '#/components/ui/select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'
import { Badge } from '#/components/ui/badge'

export const Route = createFileRoute('/settings/members')({ component: MembersPage })

const ROLE_OPTIONS: Role[] = ['owner', 'admin', 'member']
const INVITE_ROLE_OPTIONS: Array<'admin' | 'member'> = ['admin', 'member']

function MembersPage() {
  const qc = useQueryClient()
  const { data: me, error: meError } = useQuery({
    queryKey: qk.me(),
    queryFn: api.me,
    retry: false,
  })
  const orgId = me?.active_org_id
  const myRole = me?.orgs?.find((o) => o.id === orgId)?.role
  const canManage = myRole === 'owner' || myRole === 'admin'

  const members = useQuery({
    queryKey: orgId ? qk.members(orgId) : ['members', 'none'],
    queryFn: () => api.listMembers(orgId as string),
    enabled: !!orgId,
  })

  const [email, setEmail] = useState('')
  const [inviteRole, setInviteRole] = useState<'admin' | 'member'>('member')

  const invite = useMutation({
    mutationFn: () =>
      api.createInvite(orgId as string, { email, role: inviteRole }),
    onSuccess: () => {
      setEmail('')
      qc.invalidateQueries({ queryKey: qk.invites(orgId as string) })
    },
  })

  const patchRole = useMutation({
    mutationFn: ({ user_id, role }: { user_id: string; role: Role }) =>
      api.patchMember(orgId as string, user_id, role),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.members(orgId as string) }),
  })

  const remove = useMutation({
    mutationFn: (user_id: string) => api.removeMember(orgId as string, user_id),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.members(orgId as string) }),
  })

  if (meError) return <Navigate to="/login" />
  if (me && !orgId) return <Navigate to="/orgs/new" />

  return (
    <main className="page-wrap mx-auto space-y-6 px-4 pt-10 pb-16">
      <h1 className="text-2xl font-semibold">Members</h1>

      {canManage && (
        <Card>
          <CardHeader>
            <CardTitle>Invite a teammate</CardTitle>
            <CardDescription>
              They&rsquo;ll receive a sign-in link that auto-joins the org on first use.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <form
              onSubmit={(e) => {
                e.preventDefault()
                invite.mutate()
              }}
              className="grid gap-4 sm:grid-cols-[1fr_180px_auto] sm:items-end"
            >
              <div className="space-y-2">
                <Label htmlFor="invite-email">Email</Label>
                <Input
                  id="invite-email"
                  type="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  placeholder="teammate@example.com"
                  required
                />
              </div>
              <div className="space-y-2">
                <Label>Role</Label>
                <Select
                  value={inviteRole}
                  onValueChange={(v) => setInviteRole(v as 'admin' | 'member')}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {INVITE_ROLE_OPTIONS.map((r) => (
                      <SelectItem key={r} value={r}>
                        {r}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <Button type="submit" disabled={invite.isPending}>
                {invite.isPending ? 'Sending…' : 'Send invite'}
              </Button>
            </form>
            {invite.error && (
              <p className="mt-3 text-sm text-destructive">{(invite.error as Error).message}</p>
            )}
            {invite.isSuccess && (
              <p className="mt-3 text-sm text-muted-foreground">Invite sent.</p>
            )}
          </CardContent>
        </Card>
      )}

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Email</TableHead>
              <TableHead>Name</TableHead>
              <TableHead>Role</TableHead>
              <TableHead>Joined</TableHead>
              <TableHead className="w-[100px]" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {members.data?.map((m) => {
              const isSelf = m.user_id === me?.user?.id
              const canEditRole = canManage && !isSelf
              const canRemove = canManage || isSelf
              return (
                <TableRow key={m.user_id}>
                  <TableCell className="font-medium">{m.email}</TableCell>
                  <TableCell>{m.name ?? '—'}</TableCell>
                  <TableCell>
                    {canEditRole ? (
                      <Select
                        value={m.role}
                        onValueChange={(v) =>
                          patchRole.mutate({ user_id: m.user_id, role: v as Role })
                        }
                      >
                        <SelectTrigger className="w-[120px]">
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          {ROLE_OPTIONS.map((r) => (
                            <SelectItem key={r} value={r}>
                              {r}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                    ) : (
                      <Badge variant="secondary">{m.role}</Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {new Date(m.created_at).toLocaleDateString()}
                  </TableCell>
                  <TableCell>
                    {canRemove && (
                      <ConfirmDialog
                        title={isSelf ? 'Leave organization?' : `Remove ${m.email}?`}
                        description={
                          isSelf
                            ? 'You will lose access to this org. An admin will have to re-invite you.'
                            : `Remove ${m.email} from this org. They will lose access immediately.`
                        }
                        confirmLabel={isSelf ? 'Leave' : 'Remove'}
                        destructive
                        pending={remove.isPending}
                        onConfirm={() => remove.mutate(m.user_id)}
                      >
                        {(open) => (
                          <Button
                            size="sm"
                            variant="ghost"
                            onClick={open}
                            disabled={remove.isPending}
                          >
                            {isSelf ? 'Leave' : 'Remove'}
                          </Button>
                        )}
                      </ConfirmDialog>
                    )}
                  </TableCell>
                </TableRow>
              )
            })}
            {members.data && members.data.length === 0 && (
              <TableRow>
                <TableCell colSpan={5} className="text-center text-sm text-muted-foreground">
                  No members yet.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
        {(patchRole.error || remove.error) && (
          <div className="border-t p-3 text-sm text-destructive">
            {(patchRole.error as Error | null)?.message ??
              (remove.error as Error | null)?.message}
          </div>
        )}
      </Card>
    </main>
  )
}
