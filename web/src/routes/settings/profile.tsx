import { createFileRoute, Navigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { toast } from 'sonner'

import { api, qk } from '#/lib/api'
import { PageHeader } from '#/components/TopBar'
import { Button } from '#/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '#/components/ui/card'
import { Input } from '#/components/ui/input'
import { Label } from '#/components/ui/label'

export const Route = createFileRoute('/settings/profile')({ component: ProfilePage })

function ProfilePage() {
  const { data: me, error: meError } = useQuery({
    queryKey: qk.me(),
    queryFn: api.me,
    retry: false,
  })

  if (meError) return <Navigate to="/" />

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader title="Profile" help="Your account — display name and sign-in email." />
      <div className="flex-1 overflow-y-auto px-6 py-8">
        <div className="mx-auto max-w-3xl space-y-6">
          {me?.user && (
            <ProfileCard key={me.user.id} email={me.user.email} initialName={me.user.name ?? ''} />
          )}
        </div>
      </div>
    </div>
  )
}

function ProfileCard({ email, initialName }: { email: string; initialName: string }) {
  const qc = useQueryClient()
  const [name, setName] = useState(initialName)

  // Reseed if the query refetches with a different stored name.
  useEffect(() => setName(initialName), [initialName])

  const save = useMutation({
    mutationFn: () => api.updateMe({ name: name.trim() }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.me() })
      toast.success('Profile saved')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Card>
      <CardHeader>
        <CardTitle>Your profile</CardTitle>
        <CardDescription>The display name shown for your account.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="space-y-2">
          <Label htmlFor="profile-email">Email</Label>
          <Input id="profile-email" value={email} disabled className="w-full" />
        </div>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            save.mutate()
          }}
          className="space-y-2"
        >
          <Label htmlFor="profile-name">Name</Label>
          <div className="flex gap-3">
            <Input
              id="profile-name"
              className="flex-1"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Your name"
            />
            <Button type="submit" disabled={save.isPending || name.trim() === initialName.trim()}>
              {save.isPending ? 'Saving…' : 'Save'}
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  )
}
