import { createFileRoute } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { api, qk } from '#/lib/api'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { Button } from '#/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '#/components/ui/card'
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

const sourcesQuery = queryOptions({
  queryKey: qk.sources(),
  queryFn: () => api.listSources(),
})

export const Route = createFileRoute('/sources')({
  loader: ({ context }) => context.queryClient.ensureQueryData(sourcesQuery),
  component: SourcesPage,
  errorComponent: AuthErrorBoundary,
})

const SOURCE_TYPES = ['generic', 'stripe', 'github', 'shopify'] as const

function SourcesPage() {
  const qc = useQueryClient()
  const { data: sources } = useQuery(sourcesQuery)
  const [name, setName] = useState('')
  const [type, setType] = useState<string>('generic')

  const create = useMutation({
    mutationFn: (input: { name: string; type: string }) => api.createSource(input),
    onSuccess: () => {
      setName('')
      qc.invalidateQueries({ queryKey: qk.sources() })
    },
  })

  function submit(e: React.FormEvent) {
    e.preventDefault()
    create.mutate({ name, type })
  }

  return (
    <main className="page-wrap mx-auto space-y-6 px-4 pt-10 pb-16">
      <h1 className="text-2xl font-semibold">Sources</h1>

      <Card>
        <CardHeader>
          <CardTitle>Create a source</CardTitle>
          <CardDescription>
            Sources receive webhook traffic. Each gets a unique ingest URL.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={submit} className="grid gap-4 sm:grid-cols-[1fr_180px_auto] sm:items-end">
            <div className="space-y-2">
              <Label htmlFor="name">Name</Label>
              <Input
                id="name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="stripe-prod"
                required
              />
            </div>
            <div className="space-y-2">
              <Label>Type</Label>
              <Select value={type} onValueChange={(v) => setType(v as string)}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {SOURCE_TYPES.map((t) => (
                    <SelectItem key={t} value={t}>
                      {t}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <Button type="submit" disabled={create.isPending}>
              {create.isPending ? 'Creating…' : 'Create'}
            </Button>
          </form>
          {create.error && (
            <p className="mt-3 text-sm text-destructive">{(create.error as Error).message}</p>
          )}
        </CardContent>
      </Card>

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Ingest URL</TableHead>
              <TableHead>Created</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {sources?.map((s) => (
              <TableRow key={s.id}>
                <TableCell className="font-medium">{s.name}</TableCell>
                <TableCell>{s.type}</TableCell>
                <TableCell>
                  <code className="rounded bg-muted px-1.5 py-0.5 text-xs">
                    /e/{s.ingest_token}
                  </code>
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {new Date(s.created_at).toLocaleString()}
                </TableCell>
              </TableRow>
            ))}
            {sources && sources.length === 0 && (
              <TableRow>
                <TableCell colSpan={4} className="text-center text-sm text-muted-foreground">
                  No sources yet — create one above.
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </Card>
    </main>
  )
}
