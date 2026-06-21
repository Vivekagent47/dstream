import { createFileRoute, Navigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { api, qk, type APIKeyCreateResult } from '#/lib/api'
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

  const [name, setName] = useState('')
  const [created, setCreated] = useState<APIKeyCreateResult | null>(null)
  const [copied, setCopied] = useState(false)

  const create = useMutation({
    mutationFn: () => api.createAPIKey(orgId as string, name),
    onSuccess: (result) => {
      setCreated(result)
      setName('')
      qc.invalidateQueries({ queryKey: qk.apiKeys(orgId as string) })
    },
  })

  const revoke = useMutation({
    mutationFn: (id: string) => api.revokeAPIKey(orgId as string, id),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.apiKeys(orgId as string) }),
  })

  if (meError) return <Navigate to="/login" />
  if (me && !orgId) return <Navigate to="/orgs/new" />

  const [copyError, setCopyError] = useState(false)
  async function copyKey() {
    if (!created) return
    setCopyError(false)
    try {
      await navigator.clipboard.writeText(created.key)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 2000)
    } catch {
      // navigator.clipboard fails on non-https origins, iframes, or when
      // permission is denied. Surface the failure so the user can hand-
      // select the key text instead of silently believing it's copied.
      setCopyError(true)
    }
  }

  return (
    <main className="page-wrap mx-auto space-y-6 px-4 pt-10 pb-16">
      <h1 className="text-2xl font-semibold">API keys</h1>

      {canManage && (
        <Card>
          <CardHeader>
            <CardTitle>Create an API key</CardTitle>
            <CardDescription>
              API keys are scoped to this organization. Store the secret somewhere safe — it
              won&rsquo;t be shown again.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <form
              onSubmit={(e) => {
                e.preventDefault()
                create.mutate()
              }}
              className="grid gap-4 sm:grid-cols-[1fr_auto] sm:items-end"
            >
              <div className="space-y-2">
                <Label htmlFor="api-name">Name</Label>
                <Input
                  id="api-name"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="ci-prod"
                  required
                />
              </div>
              <Button type="submit" disabled={create.isPending || !name.trim()}>
                {create.isPending ? 'Creating…' : 'Create key'}
              </Button>
            </form>
            {create.error && (
              <p className="mt-3 text-sm text-destructive">{(create.error as Error).message}</p>
            )}
          </CardContent>
        </Card>
      )}

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Prefix</TableHead>
              <TableHead>Last used</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="w-[100px]" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {keys.data?.map((k) => (
              <TableRow key={k.id}>
                <TableCell className="font-medium">{k.name}</TableCell>
                <TableCell>
                  <code className="rounded bg-muted px-1.5 py-0.5 text-xs">{k.prefix}…</code>
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {k.last_used_at ? new Date(k.last_used_at).toLocaleString() : 'Never'}
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {new Date(k.created_at).toLocaleDateString()}
                </TableCell>
                <TableCell>
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
            {keys.data && keys.data.length === 0 && (
              <TableRow>
                <TableCell colSpan={5} className="text-center text-sm text-muted-foreground">
                  No API keys yet.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </Card>

      {/*
        The secret-reveal dialog must NOT close on Esc or outside-click —
        the plaintext key is shown exactly once and a stray Esc would lose
        it forever. We only close via the "I've saved it" button. To enforce
        this we (a) drop the onOpenChange handler so Base UI's internal
        close requests can't drive state, and (b) ignore the Esc keydown at
        the wrapper level.
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
            <Label className="text-xs uppercase tracking-wide text-muted-foreground">
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
            {copyError && (
              <p className="text-xs text-destructive">
                Couldn&rsquo;t access the clipboard. Select the key above and copy it manually.
              </p>
            )}
          </div>
          <DialogFooter>
            <Button onClick={() => setCreated(null)}>I&rsquo;ve saved it</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </main>
  )
}
