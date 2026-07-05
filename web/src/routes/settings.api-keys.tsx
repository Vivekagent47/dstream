import { createFileRoute, Navigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'
import { Plus } from 'lucide-react'

import { api, qk, type APIKeyCreateResult } from '#/lib/api'
import { ConfirmDialog } from '#/components/ConfirmDialog'
import { PageHeader } from '#/components/TopBar'
import { Button } from '#/components/ui/button'
import { Input } from '#/components/ui/input'
import { Label } from '#/components/ui/label'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '#/components/ui/dialog'

export const Route = createFileRoute('/settings/api-keys')({ component: APIKeysPage })

function APIKeysPage() {
  const qc = useQueryClient()
  const { data: me, error: meError } = useQuery({
    queryKey: qk.me(),
    queryFn: api.me,
    retry: false,
  })
  const orgId = me?.active_org_id
  const myRole = me?.orgs?.find((o) => o.id === orgId)?.role
  const canManage = myRole === 'owner' || myRole === 'admin'

  const keys = useQuery({
    queryKey: orgId ? qk.apiKeys(orgId) : ['api-keys', 'none'],
    queryFn: () => api.listAPIKeys(orgId as string),
    enabled: !!orgId,
  })

  const [createOpen, setCreateOpen] = useState(false)
  const [created, setCreated] = useState<APIKeyCreateResult | null>(null)
  const [copied, setCopied] = useState(false)

  const revoke = useMutation({
    mutationFn: (id: string) => api.revokeAPIKey(orgId as string, id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.apiKeys(orgId as string) })
      toast.success('API key revoked')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  if (meError) return <Navigate to="/" />
  if (me && !orgId) return <Navigate to="/orgs/new" />

  function copyKey() {
    if (!created) return
    navigator.clipboard
      .writeText(created.key)
      .then(() => {
        setCopied(true)
        window.setTimeout(() => setCopied(false), 2000)
      })
      .catch(() =>
        toast.error('Couldn’t access the clipboard. Select the key and copy it manually.'),
      )
  }

  const rows = keys.data ?? []

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title="API keys"
        actions={
          canManage ? (
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <Plus className="h-4 w-4" /> New key
            </Button>
          ) : undefined
        }
      />

      <div className="flex-1 overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-6">Name</TableHead>
              <TableHead>Prefix</TableHead>
              <TableHead>Last used</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="w-[100px] pr-6" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((k) => (
              <TableRow key={k.id}>
                <TableCell className="pl-6 font-medium">{k.name}</TableCell>
                <TableCell>
                  <code className="rounded bg-muted px-1.5 py-0.5 text-xs">{k.prefix}…</code>
                </TableCell>
                <TableCell className="whitespace-nowrap text-muted-foreground">
                  {k.last_used_at ? new Date(k.last_used_at).toLocaleString() : 'Never'}
                </TableCell>
                <TableCell className="whitespace-nowrap text-muted-foreground">
                  {new Date(k.created_at).toLocaleDateString()}
                </TableCell>
                <TableCell className="pr-6 text-right">
                  {canManage && (
                    <ConfirmDialog
                      title="Revoke API key?"
                      description={`This will immediately invalidate "${k.name}". Any consumer using it will start failing.`}
                      confirmLabel="Revoke"
                      destructive
                      pending={revoke.isPending}
                      onConfirm={() => revoke.mutate(k.id)}
                    >
                      {(open) => (
                        <Button size="sm" variant="ghost" onClick={open} disabled={revoke.isPending}>
                          Revoke
                        </Button>
                      )}
                    </ConfirmDialog>
                  )}
                </TableCell>
              </TableRow>
            ))}
            {rows.length === 0 && (
              <TableRow>
                <TableCell colSpan={5} className="py-12 text-center text-sm text-muted-foreground">
                  No API keys yet.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>

      <footer className="border-t border-border px-6 py-3 text-sm text-muted-foreground">
        Viewing {rows.length} {rows.length === 1 ? 'key' : 'keys'}
      </footer>

      {orgId && (
        <CreateKeyDialog
          open={createOpen}
          onOpenChange={setCreateOpen}
          orgId={orgId}
          onCreated={setCreated}
        />
      )}

      {/*
        The secret-reveal dialog must NOT close on Esc or outside-click — the
        plaintext key is shown exactly once and a stray Esc would lose it
        forever. We only close via the "I've saved it" button.
      */}
      <Dialog open={!!created}>
        <DialogContent
          onKeyDown={(e) => {
            if (e.key === 'Escape') e.preventDefault()
          }}
        >
          <DialogHeader>
            <DialogTitle>API key created</DialogTitle>
            <DialogDescription>
              Save this key now — it won&rsquo;t be shown again. Anyone with this key can act on
              behalf of your org.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Label className="text-xs tracking-wide text-muted-foreground uppercase">
              Secret key
            </Label>
            <div className="flex items-center gap-2">
              <code className="flex-1 overflow-x-auto rounded border bg-muted px-3 py-2 text-xs">
                {created?.key}
              </code>
              <Button size="sm" variant="outline" onClick={copyKey}>
                {copied ? 'Copied' : 'Copy'}
              </Button>
            </div>
          </div>
          <DialogFooter>
            <Button onClick={() => setCreated(null)}>I&rsquo;ve saved it</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function CreateKeyDialog({
  open,
  onOpenChange,
  orgId,
  onCreated,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  orgId: string
  onCreated: (result: APIKeyCreateResult) => void
}) {
  const qc = useQueryClient()
  const [name, setName] = useState('')

  const create = useMutation({
    mutationFn: () => api.createAPIKey(orgId, name),
    onSuccess: (result) => {
      qc.invalidateQueries({ queryKey: qk.apiKeys(orgId) })
      onOpenChange(false)
      setName('')
      onCreated(result)
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New API key</DialogTitle>
          <DialogDescription>
            API keys are scoped to this organization. The secret is shown once on creation.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            create.mutate()
          }}
          className="space-y-4"
        >
          <div>
            <Label htmlFor="api-name" className="mb-2 block">
              Name
            </Label>
            <Input
              id="api-name"
              className="w-full"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="ci-prod"
              required
              autoFocus
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={create.isPending || !name.trim()}>
              {create.isPending ? 'Creating…' : 'Create key'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
