import { createFileRoute, Navigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'
import { Plus } from 'lucide-react'

import { api, qk, type Role } from '#/lib/api'
import { capitalize } from '#/lib/utils'
import { ConfirmDialog } from '#/components/ConfirmDialog'
import { PageHeader } from '#/components/TopBar'
import { Button } from '#/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '#/components/ui/dialog'
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

  const [inviteOpen, setInviteOpen] = useState(false)

  const patchRole = useMutation({
    mutationFn: ({ user_id, role }: { user_id: string; role: Role }) =>
      api.patchMember(orgId as string, user_id, role),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.members(orgId as string) })
      toast.success('Role updated')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const remove = useMutation({
    mutationFn: (user_id: string) => api.removeMember(orgId as string, user_id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.members(orgId as string) })
      toast.success('Member removed')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  if (meError) return <Navigate to="/" />
  if (me && !orgId) return <Navigate to="/orgs/new" />

  const rows = members.data ?? []

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title="Members"
        actions={
          canManage ? (
            <Button size="sm" onClick={() => setInviteOpen(true)}>
              <Plus className="h-4 w-4" /> Invite
            </Button>
          ) : undefined
        }
      />

      <div className="flex-1 overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Email</TableHead>
              <TableHead>Name</TableHead>
              <TableHead>Role</TableHead>
              <TableHead>Joined</TableHead>
              <TableHead className="w-[100px] pr-6" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((m) => {
              const isSelf = m.user_id === me?.user?.id
              const canEditRole = canManage && !isSelf
              const canRemove = canManage || isSelf
              return (
                <TableRow key={m.user_id}>
                  <TableCell className="pl-6 font-medium">{m.email}</TableCell>
                  <TableCell className="text-muted-foreground">{m.name ?? '—'}</TableCell>
                  <TableCell>
                    {canEditRole ? (
                      <Select
                        value={m.role}
                        onValueChange={(v) =>
                          v && patchRole.mutate({ user_id: m.user_id, role: v as Role })
                        }
                      >
                        <SelectTrigger className="w-[130px]">
                          <SelectValue>
                            {(v: string | null) => (v ? capitalize(v) : '')}
                          </SelectValue>
                        </SelectTrigger>
                        <SelectContent>
                          {ROLE_OPTIONS.map((r) => (
                            <SelectItem key={r} value={r}>
                              {capitalize(r)}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                    ) : (
                      <Badge variant="secondary">{capitalize(m.role)}</Badge>
                    )}
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-muted-foreground">
                    {new Date(m.created_at).toLocaleDateString()}
                  </TableCell>
                  <TableCell className="pr-6 text-right">
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
            {rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={5} className="py-12 text-center text-sm text-muted-foreground">
                  No members yet.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <footer className="border-t border-border px-6 py-3 text-sm text-muted-foreground">
        Viewing {rows.length} {rows.length === 1 ? 'member' : 'members'}
      </footer>

      {orgId && (
        <InviteDialog open={inviteOpen} onOpenChange={setInviteOpen} orgId={orgId} />
      )}
    </div>
  )
}

function InviteDialog({
  open,
  onOpenChange,
  orgId,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  orgId: string
}) {
  const qc = useQueryClient()
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<'admin' | 'member'>('member')

  const invite = useMutation({
    mutationFn: () => api.createInvite(orgId, { email, role }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.invites(orgId) })
      toast.success('Invite sent')
      onOpenChange(false)
      setEmail('')
      setRole('member')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Invite a teammate</DialogTitle>
          <DialogDescription>
            They&rsquo;ll receive a sign-in link that auto-joins the org on first use.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            invite.mutate()
          }}
          className="space-y-4"
        >
          <div>
            <Label htmlFor="invite-email" className="mb-2 block">
              Email
            </Label>
            <Input
              id="invite-email"
              type="email"
              className="w-full"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="teammate@example.com"
              required
              autoFocus
            />
          </div>
          <div>
            <Label className="mb-2 block">Role</Label>
            <Select value={role} onValueChange={(v) => setRole((v as 'admin' | 'member') ?? 'member')}>
              <SelectTrigger className="w-full">
                <SelectValue>{(v: string | null) => (v ? capitalize(v) : 'Select a role')}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {(['admin', 'member'] as const).map((r) => (
                  <SelectItem key={r} value={r}>
                    {capitalize(r)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={invite.isPending || !email.trim()}>
              {invite.isPending ? 'Sending…' : 'Send invite'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
