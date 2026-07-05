import { createFileRoute, Navigate, useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { toast } from 'sonner'

import { api, qk } from '#/lib/api'
import { PageHeader } from '#/components/TopBar'
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
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '#/components/ui/dialog'

export const Route = createFileRoute('/settings/org')({ component: OrgSettingsPage })

function OrgSettingsPage() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { data: me, error: meError } = useQuery({
    queryKey: qk.me(),
    queryFn: api.me,
    retry: false,
  })
  const orgId = me?.active_org_id
  const activeOrg = me?.orgs?.find((o) => o.id === orgId)
  const myRole = activeOrg?.role
  const isAdmin = myRole === 'owner' || myRole === 'admin'
  const isOwner = myRole === 'owner'

  const members = useQuery({
    queryKey: orgId ? qk.members(orgId) : ['members', 'none'],
    queryFn: () => api.listMembers(orgId as string),
    enabled: !!orgId && isOwner,
  })

  const [transferTo, setTransferTo] = useState<string>('')
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [deleteText, setDeleteText] = useState('')

  const transfer = useMutation({
    mutationFn: () => api.transferOrg(orgId as string, transferTo),
    onSuccess: async () => {
      await qc.invalidateQueries()
      setTransferTo('')
      toast.success('Ownership transferred')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const removeOrg = useMutation({
    mutationFn: () => api.deleteOrg(orgId as string),
    onSuccess: async () => {
      setConfirmDelete(false)
      await qc.invalidateQueries()
      toast.success('Organization deleted')
      navigate({ to: '/' })
    },
    onError: (e) => toast.error((e as Error).message),
  })

  if (meError) return <Navigate to="/" />
  if (me && !orgId) return <Navigate to="/orgs/new" />

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader title="Organization settings" />
      <div className="flex-1 overflow-y-auto px-6 py-8">
        <div className="mx-auto max-w-3xl space-y-6">

      {orgId && (
        <RenameOrgCard
          key={`${orgId}:${activeOrg?.name ?? ''}`}
          orgId={orgId}
          initialName={activeOrg?.name ?? ''}
          isAdmin={isAdmin}
        />
      )}

      {isOwner && (
        <Card>
          <CardHeader>
            <CardTitle>Transfer ownership</CardTitle>
            <CardDescription>
              Promote a current member to owner. You&rsquo;ll be demoted to admin.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <div className="space-y-2">
              <Label>New owner</Label>
              <div className="flex gap-3">
                <Select
                  value={transferTo}
                  onValueChange={(v) => setTransferTo(v ?? '')}
                >
                  <SelectTrigger className="flex-1">
                    <SelectValue>
                      {(value: string | null) => {
                        if (!value) return 'Pick a member'
                        const m = members.data?.find((mem) => mem.user_id === value)
                        return m?.email ?? value
                      }}
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    {members.data
                      ?.filter((m) => m.user_id !== me?.user?.id)
                      .map((m) => (
                        <SelectItem key={m.user_id} value={m.user_id}>
                          {m.email}
                        </SelectItem>
                      ))}
                  </SelectContent>
                </Select>
                <ConfirmDialog
                  title="Transfer ownership?"
                  description="You will be demoted to admin. The new owner gains full control of this org, including the right to delete it."
                  confirmLabel="Transfer"
                  destructive
                  pending={transfer.isPending}
                  onConfirm={() => transfer.mutate()}
                >
                  {(open) => (
                    <Button onClick={open} disabled={!transferTo || transfer.isPending}>
                      {transfer.isPending ? 'Transferring…' : 'Transfer'}
                    </Button>
                  )}
                </ConfirmDialog>
              </div>
            </div>
          </CardContent>
        </Card>
      )}

      {isOwner && (
        <Card className="border-destructive/40">
          <CardHeader>
            <CardTitle className="text-destructive">Delete organization</CardTitle>
            <CardDescription>
              Permanently removes this org, all members, sources, destinations, events, and audit
              logs. This cannot be undone.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Button variant="destructive" onClick={() => setConfirmDelete(true)}>
              Delete organization
            </Button>
          </CardContent>
        </Card>
      )}

      <Dialog
        open={confirmDelete}
        onOpenChange={(o) => {
          setConfirmDelete(o)
          if (!o) setDeleteText('')
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {activeOrg?.name}?</DialogTitle>
            <DialogDescription>
              Type <strong>{activeOrg?.name}</strong> to confirm. This action is permanent.
            </DialogDescription>
          </DialogHeader>
          <Input
            value={deleteText}
            onChange={(e) => setDeleteText(e.target.value)}
            placeholder={activeOrg?.name}
            autoFocus
          />
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmDelete(false)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={deleteText !== activeOrg?.name || removeOrg.isPending}
              onClick={() => removeOrg.mutate()}
            >
              {removeOrg.isPending ? 'Deleting…' : 'Delete forever'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
        </div>
      </div>
    </div>
  )
}

function RenameOrgCard({
  orgId,
  initialName,
  isAdmin,
}: {
  orgId: string
  initialName: string
  isAdmin: boolean
}) {
  const qc = useQueryClient()
  const [name, setName] = useState(initialName)

  const rename = useMutation({
    mutationFn: () => api.updateOrg(orgId, { name }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.me() })
      qc.invalidateQueries({ queryKey: qk.orgs() })
      toast.success('Organization renamed')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Card>
      <CardHeader>
        <CardTitle>Rename</CardTitle>
        <CardDescription>Change the display name for this organization.</CardDescription>
      </CardHeader>
      <CardContent>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            rename.mutate()
          }}
          className="space-y-2"
        >
          <Label htmlFor="org-name">Name</Label>
          <div className="flex gap-3">
            <Input
              id="org-name"
              className="flex-1"
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
              disabled={!isAdmin}
            />
            <Button
              type="submit"
              disabled={!isAdmin || rename.isPending || name === initialName || !name.trim()}
            >
              {rename.isPending ? 'Saving…' : 'Save'}
            </Button>
          </div>
        </form>
        {!isAdmin && (
          <p className="mt-3 text-xs text-muted-foreground">
            Only admins and owners can rename the org.
          </p>
        )}
      </CardContent>
    </Card>
  )
}
