import { createFileRoute } from '@tanstack/react-router'
import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'

import { api } from '#/lib/api'
import { Button } from '#/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '#/components/ui/card'
import { Input } from '#/components/ui/input'
import { Label } from '#/components/ui/label'

export const Route = createFileRoute('/login')({ component: Login })

function Login() {
  const [email, setEmail] = useState('')
  const request = useMutation({
    // Normalize email to match the server (trim + lowercase) so the
    // per-email rate-limit key is stable across casing variations the
    // user might type.
    mutationFn: (e: string) => api.requestMagicLink(e.trim().toLowerCase()),
  })

  function submit(e: React.FormEvent) {
    e.preventDefault()
    request.mutate(email)
  }

  if (request.isSuccess) {
    return (
      <main className="page-wrap mx-auto px-4 pt-20">
        <Card className="mx-auto max-w-md text-center">
          <CardHeader>
            <CardTitle>Check your email</CardTitle>
            <CardDescription>
              If <strong>{email}</strong> is a known account, a sign-in link is on its way.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <p className="text-xs text-muted-foreground">
              Dev tip: dstream logs the link to stdout when SMTP is not configured.
            </p>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => {
                request.reset()
                setEmail('')
              }}
            >
              Use a different email
            </Button>
          </CardContent>
        </Card>
      </main>
    )
  }

  return (
    <main className="page-wrap mx-auto px-4 pt-20">
      <Card className="mx-auto max-w-md">
        <CardHeader>
          <CardTitle>Sign in to dstream</CardTitle>
          <CardDescription>We&apos;ll email you a single-use link.</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={submit} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="email">Email</Label>
              <Input
                id="email"
                type="email"
                required
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder="you@example.com"
              />
            </div>
            {request.error && (
              <p className="text-sm text-destructive">{(request.error as Error).message}</p>
            )}
            <Button type="submit" className="w-full" disabled={request.isPending}>
              {request.isPending ? 'Sending…' : 'Send magic link'}
            </Button>
          </form>
        </CardContent>
      </Card>
    </main>
  )
}
