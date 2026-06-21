import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { api, qk } from '#/lib/api'
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

export const Route = createFileRoute('/orgs/new')({ component: NewOrgPage })

function NewOrgPage() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [name, setName] = useState('')

  const create = useMutation({
    mutationFn: (input: { name: string }) => api.createOrg(input),
    onSuccess: async (org) => {
      // createOrg returns 201 but does NOT rotate the session cookie.
      // selectOrg then re-issues the cookie pointing at the new org. We
      // await both before refetching me — otherwise the me query reads the
      // stale cookie and the UI thinks the user is still on the old org.
      await api.selectOrg(org.id)
      await qc.refetchQueries({ queryKey: qk.me() })
      qc.removeQueries({
        predicate: (q) => q.queryKey[0] !== 'me',
      })
      navigate({ to: '/sources' })
    },
  })

  function submit(e: React.FormEvent) {
    e.preventDefault()
    create.mutate({ name })
  }

  return (
    <main className="page-wrap mx-auto space-y-6 px-4 pt-10 pb-16">
      <h1 className="text-2xl font-semibold">Create organization</h1>

      <Card>
        <CardHeader>
          <CardTitle>New org</CardTitle>
          <CardDescription>
            Organizations isolate sources, destinations, members, and audit logs. You can switch
            between orgs from the header.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={submit} className="grid gap-4 sm:max-w-md">
            <div className="space-y-2">
              <Label htmlFor="name">Name</Label>
              <Input
                id="name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Acme, Inc."
                required
                autoFocus
              />
            </div>
            <Button type="submit" disabled={create.isPending || !name.trim()}>
              {create.isPending ? 'Creating…' : 'Create organization'}
            </Button>
            {create.error && (
              <p className="text-sm text-destructive">{(create.error as Error).message}</p>
            )}
          </form>
        </CardContent>
      </Card>

    </main>
  )
}
