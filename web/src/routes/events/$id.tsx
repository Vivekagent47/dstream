import { createFileRoute, Link } from '@tanstack/react-router'
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { toast } from 'sonner'
import { ChevronDown, ChevronRight, Copy, MoveRight, RotateCcw, Terminal } from 'lucide-react'

import { api, qk, type Attempt, type EventDetail } from '#/lib/api'
import { capitalize } from '#/lib/utils'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { CopyValue, DetailRow, copyText } from '#/components/detail-page'
import { Badge } from '#/components/ui/badge'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '#/components/ui/table'

const statusVariant: Record<string, React.ComponentProps<typeof Badge>['variant']> = {
  delivered: 'success',
  queued: 'secondary',
  in_flight: 'info',
  failed: 'destructive',
  paused: 'warning',
  dead: 'destructive',
  discarded: 'warning',
}

const eventQuery = (id: string) =>
  queryOptions({ queryKey: qk.event(id), queryFn: () => api.getEvent(id) })
const sourcesQuery = queryOptions({ queryKey: qk.sources(), queryFn: () => api.listSources() })
const destinationsQuery = queryOptions({
  queryKey: qk.destinations(),
  queryFn: () => api.listDestinations(),
})

export const Route = createFileRoute('/events/$id')({
  loader: ({ context, params }) =>
    typeof window === 'undefined'
      ? undefined
      : context.queryClient.ensureQueryData(eventQuery(params.id)),
  component: EventDetail,
  errorComponent: AuthErrorBoundary,
})

function EventDetail() {
  const { id } = Route.useParams()
  const qc = useQueryClient()
  const [tab, setTab] = useState<'overview' | 'attempts'>('overview')

  const { data: ev, error } = useQuery(eventQuery(id))
  const { data: sources } = useQuery(sourcesQuery)
  const { data: destinations } = useQuery(destinationsQuery)

  const retry = useMutation({
    mutationFn: () => api.retryEvent(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.event(id) })
      toast.success('Retry queued')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const conn = useMemo(() => {
    if (!ev) return null
    const src = (sources ?? []).find((s) => s.id === ev.source_id)?.name
    const dst = (destinations ?? []).find((d) => d.id === ev.destination_id)?.name
    return { source: src ?? ev.source_id.slice(0, 8), dest: dst ?? ev.destination_id.slice(0, 8) }
  }, [ev, sources, destinations])

  if (error) {
    return <p className="px-6 py-10 text-sm text-destructive">{(error as Error).message}</p>
  }
  if (!ev) {
    return <p className="px-6 py-10 text-sm text-muted-foreground">Loading…</p>
  }

  const latest = ev.attempts.length
    ? ev.attempts.reduce((a, b) => (b.attempt_num > a.attempt_num ? b : a))
    : undefined

  return (
    <div className="flex flex-1 flex-col">
      {/* header: breadcrumb + actions */}
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border px-6 py-3">
        <div className="flex items-center gap-2 text-sm">
          <Link to="/events" className="text-muted-foreground hover:text-foreground">
            Events
          </Link>
          <span className="text-muted-foreground">/</span>
          <span className="font-mono text-xs">{ev.id}</span>
        </div>
        <div className="flex items-center gap-2">
          <ActionButton icon={Terminal} label="cURL" onClick={() => copyText(buildCurl(ev), 'cURL command')} />
          <ActionButton
            icon={RotateCcw}
            label={retry.isPending ? 'Retrying…' : 'Retry'}
            onClick={() => retry.mutate()}
            disabled={retry.isPending}
          />
        </div>
      </div>

      {/* tabs */}
      <div className="flex items-center gap-6 border-b border-border px-6">
        <Tab active={tab === 'overview'} onClick={() => setTab('overview')}>
          Overview
        </Tab>
        <Tab active={tab === 'attempts'} onClick={() => setTab('attempts')}>
          Delivery attempts <span className="ml-1 text-muted-foreground">{ev.attempts.length}</span>
        </Tab>
      </div>

      {tab === 'overview' ? (
        <div className="grid flex-1 grid-cols-1 gap-8 overflow-y-auto p-6 lg:grid-cols-2">
          {/* left: event details */}
          <section className="space-y-4">
            <h2 className="text-lg font-semibold">Event details</h2>

            {latest?.error_message && (
              <div className="rounded-md border border-destructive/40 bg-destructive/10 px-4 py-3 text-sm text-destructive">
                {latest.error_message}
              </div>
            )}

            <DetailRow label="Status">
              <Badge variant={statusVariant[ev.status] || 'secondary'}>
                {capitalize(ev.status.replace('_', ' '))}
              </Badge>
            </DetailRow>
            <DetailRow label="Event ID">
              <CopyValue value={ev.id} what="Event ID" mono />
            </DetailRow>
            <DetailRow label="Connection">
              <Link
                to="/connections/$id"
                params={{ id: ev.connection_id }}
                className="inline-flex items-center gap-1.5 font-mono text-xs hover:underline"
              >
                {conn?.source}
                <MoveRight className="h-3 w-3 text-muted-foreground" />
                {conn?.dest}
              </Link>
            </DetailRow>
            <DetailRow label="Request">
              <CopyValue value={ev.request_id} what="Request ID" mono />
            </DetailRow>
            {latest && (
              <>
                <DetailRow label="Latest attempt">
                  <CopyValue value={latest.id} what="Attempt ID" mono />
                </DetailRow>
                <DetailRow label="Latest attempt at">
                  <span className="text-sm">{new Date(latest.attempted_at).toLocaleString()}</span>
                </DetailRow>
                <DetailRow label="Latest response">
                  {latest.response_status != null ? (
                    <Badge variant={latest.response_status < 300 ? 'success' : 'destructive'}>
                      {latest.response_status}
                    </Badge>
                  ) : latest.error_message ? (
                    <Badge variant="destructive">{latest.error_message}</Badge>
                  ) : (
                    '—'
                  )}
                </DetailRow>
              </>
            )}
            <DetailRow label="Created at">
              <span className="text-sm">{new Date(ev.created_at).toLocaleString()}</span>
            </DetailRow>
          </section>

          {/* right: request data */}
          <section className="space-y-4">
            <h2 className="text-lg font-semibold">Request data</h2>

            <div className="flex items-center gap-2 rounded-md border border-border px-3 py-2 text-xs">
              <span className="rounded bg-muted px-1.5 py-0.5 font-mono font-semibold">
                {ev.request.method}
              </span>
              <span className="truncate font-mono text-muted-foreground">
                {ev.destination.url ?? `(${ev.destination.type})`}
              </span>
              {ev.destination.url && (
                <button
                  type="button"
                  onClick={() => copyText(ev.destination.url!, 'Destination URL')}
                  className="ml-auto shrink-0 text-muted-foreground hover:text-foreground"
                  title="Copy URL"
                >
                  <Copy className="h-3.5 w-3.5" />
                </button>
              )}
            </div>

            <CollapseSection
              title="Headers"
              count={Object.keys(ev.request.headers).length}
              onCopy={() => copyText(formatHeaders(ev.request.headers), 'Headers')}
            >
              <div className="space-y-1 font-mono text-xs">
                {Object.entries(ev.request.headers).map(([k, v]) => (
                  <div key={k} className="break-all">
                    <span className="text-muted-foreground">{k}:</span>{' '}
                    <span>{Array.isArray(v) ? v.join(', ') : v}</span>
                  </div>
                ))}
                {Object.keys(ev.request.headers).length === 0 && (
                  <div className="text-muted-foreground">No headers.</div>
                )}
              </div>
            </CollapseSection>

            <CollapseSection
              title="Body"
              onCopy={() => copyText(ev.request.body, 'Body')}
            >
              <pre className="overflow-x-auto rounded-md bg-muted/50 p-3 font-mono text-xs">
                {prettyBody(ev.request.body) || <span className="text-muted-foreground">Empty body.</span>}
              </pre>
            </CollapseSection>
          </section>
        </div>
      ) : (
        <div className="flex-1 overflow-x-auto">
          <AttemptsTable attempts={ev.attempts} />
        </div>
      )}
    </div>
  )
}

function Tab({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={
        'border-b-2 py-3 text-sm transition-colors ' +
        (active
          ? 'border-foreground font-medium text-foreground'
          : 'border-transparent text-muted-foreground hover:text-foreground')
      }
    >
      {children}
    </button>
  )
}

function ActionButton({
  icon: Icon,
  label,
  onClick,
  disabled,
}: {
  icon: React.ComponentType<{ className?: string }>
  label: string
  onClick: () => void
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className="inline-flex items-center gap-1.5 rounded-md border border-border px-3 py-1.5 text-xs transition-colors hover:bg-accent disabled:opacity-50"
    >
      <Icon className="h-3.5 w-3.5" />
      {label}
    </button>
  )
}

function CollapseSection({
  title,
  count,
  onCopy,
  children,
}: {
  title: string
  count?: number
  onCopy?: () => void
  children: React.ReactNode
}) {
  const [open, setOpen] = useState(true)
  return (
    <div className="rounded-md border border-border">
      <div className="flex items-center gap-2 px-3 py-2">
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="flex flex-1 items-center gap-1.5 text-sm font-medium"
        >
          {open ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
          {title}
          {count != null && <span className="text-xs text-muted-foreground">{count}</span>}
        </button>
        {onCopy && (
          <button
            type="button"
            onClick={onCopy}
            className="text-muted-foreground hover:text-foreground"
            title={`Copy ${title.toLowerCase()}`}
          >
            <Copy className="h-3.5 w-3.5" />
          </button>
        )}
      </div>
      {open && <div className="border-t border-border p-3">{children}</div>}
    </div>
  )
}

function AttemptsTable({ attempts }: { attempts: Attempt[] }) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead className="pl-6">#</TableHead>
          <TableHead>Response</TableHead>
          <TableHead>Duration</TableHead>
          <TableHead>Error</TableHead>
          <TableHead className="pr-6">When</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {attempts.map((a) => (
          <TableRow key={a.id}>
            <TableCell className="pl-6">{a.attempt_num}</TableCell>
            <TableCell>{a.response_status ?? '—'}</TableCell>
            <TableCell>{a.duration_ms != null ? `${a.duration_ms}ms` : '—'}</TableCell>
            <TableCell className="text-destructive">{a.error_message ?? ''}</TableCell>
            <TableCell className="pr-6 text-muted-foreground">
              {new Date(a.attempted_at).toLocaleString()}
            </TableCell>
          </TableRow>
        ))}
        {attempts.length === 0 && (
          <TableRow>
            <TableCell colSpan={5} className="py-12 text-center text-sm text-muted-foreground">
              No attempts yet.
            </TableCell>
          </TableRow>
        )}
      </TableBody>
    </Table>
  )
}

function prettyBody(body: string): string {
  if (!body) return ''
  try {
    return JSON.stringify(JSON.parse(body), null, 2)
  } catch {
    return body
  }
}

function formatHeaders(headers: Record<string, string | string[]>): string {
  return Object.entries(headers)
    .map(([k, v]) => `${k}: ${Array.isArray(v) ? v.join(', ') : v}`)
    .join('\n')
}

// Reproduce the delivered request as a copy-pasteable curl. Body is passed raw
// via --data; headers replayed as sent.
function buildCurl(ev: EventDetail): string {
  const url = ev.destination.url ?? ''
  const parts = [`curl -X ${ev.request.method} ${JSON.stringify(url)}`]
  for (const [k, v] of Object.entries(ev.request.headers)) {
    parts.push(`  -H ${JSON.stringify(`${k}: ${Array.isArray(v) ? v.join(', ') : v}`)}`)
  }
  if (ev.request.body) parts.push(`  --data ${JSON.stringify(ev.request.body)}`)
  return parts.join(' \\\n')
}
