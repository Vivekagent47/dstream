import { createFileRoute, Navigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { toast } from 'sonner'

import { api, qk, type Plan, type QuotaPeriod, type Usage } from '#/lib/api'
import { useRole } from '#/lib/useRole'
import { PageHeader } from '#/components/TopBar'
import { Badge } from '#/components/ui/badge'
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

export const Route = createFileRoute('/settings/usage')({ component: UsagePage })

// The two gating metrics (design §3/§6) — shown as a meter against their
// soft/hard limits. requests and attempts are metered but never enforced, so
// they get a plain count instead.
const GATED_METRICS = [
  {
    key: 'events',
    label: 'Events',
    note: 'Inbound fan-out. The primary billable unit, and the one gate enforces.',
  },
  { key: 'messages', label: 'Messages', note: 'Outbound messages published to applications.' },
] as const

const COUNTED_ONLY_METRICS = [
  {
    key: 'requests',
    label: 'Requests',
    note: 'Inbound HTTP requests. Not separately limited — gating events already gates the request that produced them.',
  },
  {
    key: 'attempts',
    label: 'Delivery attempts',
    note: 'Every attempt including retries. Never enforced — rejecting a retry would punish you for your own endpoint being down.',
  },
] as const

function UsagePage() {
  const { isOwner } = useRole()
  // Shares the 'me' cache every other settings page already populates — no
  // extra request.
  const { data: me } = useQuery({ queryKey: qk.me(), queryFn: api.me, retry: false })
  const orgId = me?.active_org_id
  const { data, error, isLoading } = useQuery({
    queryKey: qk.usage(),
    queryFn: api.getUsage,
    retry: false,
  })

  if (error) return <Navigate to="/" />

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title="Usage"
        help="What this org has used this period, against its plan limits. Only the hard ceiling ever rejects a request — the soft limit accepts and warns, because a sender that doesn't retry loses the webhook for good."
      />
      <div className="flex-1 overflow-y-auto px-6 py-8">
        <div className="mx-auto max-w-3xl space-y-6">
          <Card>
            <CardHeader>
              <div className="flex items-start justify-between gap-3">
                <div>
                  <CardTitle>This period</CardTitle>
                  <CardDescription>
                    Plan <span className="font-medium text-foreground">{data?.plan ?? '…'}</span>
                    {' · resets every '}
                    {data?.period ?? '…'}
                  </CardDescription>
                </div>
                {data?.partial && (
                  <Badge
                    variant="outline"
                    title="This period is still accumulating; the count will keep rising until it ends."
                  >
                    Partial period
                  </Badge>
                )}
              </div>
            </CardHeader>
            <CardContent className="space-y-5">
              {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
              {data && (
                <>
                  {GATED_METRICS.map((m) => (
                    <UsageMeter
                      key={m.key}
                      label={m.label}
                      note={m.note}
                      count={data.usage[m.key]}
                      soft={data.limits[`${m.key}_soft`]}
                      hard={data.limits[`${m.key}_hard`]}
                    />
                  ))}
                  <div className="grid gap-4 border-t border-border pt-4 sm:grid-cols-2">
                    {COUNTED_ONLY_METRICS.map((m) => (
                      <div key={m.key}>
                        <div className="text-sm font-medium">{m.label}</div>
                        <div className="text-lg font-semibold tabular-nums">
                          {data.usage[m.key].toLocaleString()}
                        </div>
                        <p className="mt-1 text-xs text-muted-foreground">{m.note}</p>
                      </div>
                    ))}
                  </div>
                </>
              )}
            </CardContent>
          </Card>

          {isOwner && data && <QuotaEditCard orgId={orgId} quota={data} />}
        </div>
      </div>
    </div>
  )
}

function UsageMeter({
  label,
  note,
  count,
  soft,
  hard,
}: {
  label: string
  note: string
  count: number
  soft: number
  hard: number
}) {
  // 0 means unlimited (design §4.1) independently per tier: hard == 0 means
  // no ceiling to scale a bar against, but soft > 0 with hard == 0 is a
  // legitimate "warn me, never reject" config, and overSoft must still fire
  // in that state — it must never be gated behind hard having a value.
  const noCeiling = hard === 0
  const fullyUnlimited = noCeiling && soft === 0
  const overHard = !noCeiling && count >= hard
  const overSoft = soft > 0 && count >= soft
  const pct = noCeiling ? 0 : Math.min(100, (count / hard) * 100)
  const barColor = overHard ? 'bg-destructive' : overSoft ? 'bg-amber-800' : 'bg-primary'

  return (
    <div>
      <div className="flex items-center justify-between text-sm">
        <span className="font-medium">{label}</span>
        <div className="flex items-center gap-2">
          <span className="text-muted-foreground tabular-nums">
            {count.toLocaleString()}
            {!noCeiling && ` / ${hard.toLocaleString()}`}
          </span>
          {fullyUnlimited ? (
            <Badge variant="outline">Unlimited</Badge>
          ) : overHard ? (
            <Badge variant="destructive">At hard ceiling</Badge>
          ) : overSoft ? (
            <Badge variant="warning">Over soft limit</Badge>
          ) : noCeiling ? (
            <Badge variant="outline">No hard ceiling</Badge>
          ) : null}
        </div>
      </div>
      {/* No bar when there's no hard ceiling — nothing to scale it against,
          but the badge above still reports over-soft regardless. */}
      {!noCeiling && (
        <div className="mt-1.5 h-2 w-full overflow-hidden rounded-full bg-muted">
          <div className={`h-full rounded-full ${barColor}`} style={{ width: `${pct}%` }} />
        </div>
      )}
      <p className="mt-1 text-xs text-muted-foreground">{note}</p>
    </div>
  )
}

function QuotaEditCard({ orgId, quota }: { orgId: string | undefined; quota: Usage }) {
  const qc = useQueryClient()
  const [plan, setPlan] = useState<Plan>(quota.plan)
  const [period, setPeriod] = useState<QuotaPeriod>(quota.period)
  const [eventsSoft, setEventsSoft] = useState(String(quota.limits.events_soft))
  const [eventsHard, setEventsHard] = useState(String(quota.limits.events_hard))
  const [messagesSoft, setMessagesSoft] = useState(String(quota.limits.messages_soft))
  const [messagesHard, setMessagesHard] = useState(String(quota.limits.messages_hard))

  const save = useMutation({
    mutationFn: () =>
      api.patchOrgPlan(orgId as string, {
        plan,
        quota_period: period,
        quota_events_soft: Number(eventsSoft),
        quota_events_hard: Number(eventsHard),
        quota_messages_soft: Number(messagesSoft),
        quota_messages_hard: Number(messagesHard),
      }),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: qk.usage() })
      toast.success('Plan updated')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const dirty =
    plan !== quota.plan ||
    period !== quota.period ||
    eventsSoft !== String(quota.limits.events_soft) ||
    eventsHard !== String(quota.limits.events_hard) ||
    messagesSoft !== String(quota.limits.messages_soft) ||
    messagesHard !== String(quota.limits.messages_hard)

  // Every limit is a non-negative integer; 0 means unlimited rather than
  // "zero capacity" (design §4.1), so there is no separate "unlimited"
  // checkbox to wire up — clearing a field to 0 already means that.
  const valid = [eventsSoft, eventsHard, messagesSoft, messagesHard].every(
    (s) => s.trim() !== '' && Number.isInteger(Number(s)) && Number(s) >= 0,
  )

  return (
    <Card>
      <CardHeader>
        <CardTitle>Plan and limits</CardTitle>
        <CardDescription>
          Owner-only: changing a quota is a spend decision. 0 means unlimited on both tiers.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <Label className="mb-2 block">Plan</Label>
            <Select value={plan} onValueChange={(v) => setPlan((v as Plan) ?? 'free')}>
              <SelectTrigger>
                <SelectValue>{(v: string | null) => v ?? 'free'}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="free">free</SelectItem>
                <SelectItem value="pro">pro</SelectItem>
                <SelectItem value="enterprise">enterprise</SelectItem>
                <SelectItem value="custom">custom</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div>
            <Label className="mb-2 block">Quota period</Label>
            <Select value={period} onValueChange={(v) => setPeriod((v as QuotaPeriod) ?? 'month')}>
              <SelectTrigger>
                <SelectValue>{(v: string | null) => v ?? 'month'}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="day">day</SelectItem>
                <SelectItem value="month">month</SelectItem>
              </SelectContent>
            </Select>
          </div>
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <Label htmlFor="events-soft" className="mb-2 block">
              Events soft limit
            </Label>
            <Input
              id="events-soft"
              type="number"
              min="0"
              value={eventsSoft}
              onChange={(e) => setEventsSoft(e.target.value)}
            />
          </div>
          <div>
            <Label htmlFor="events-hard" className="mb-2 block">
              Events hard ceiling
            </Label>
            <Input
              id="events-hard"
              type="number"
              min="0"
              value={eventsHard}
              onChange={(e) => setEventsHard(e.target.value)}
            />
          </div>
          <div>
            <Label htmlFor="messages-soft" className="mb-2 block">
              Messages soft limit
            </Label>
            <Input
              id="messages-soft"
              type="number"
              min="0"
              value={messagesSoft}
              onChange={(e) => setMessagesSoft(e.target.value)}
            />
          </div>
          <div>
            <Label htmlFor="messages-hard" className="mb-2 block">
              Messages hard ceiling
            </Label>
            <Input
              id="messages-hard"
              type="number"
              min="0"
              value={messagesHard}
              onChange={(e) => setMessagesHard(e.target.value)}
            />
          </div>
        </div>
        <Button
          size="sm"
          disabled={!orgId || !dirty || !valid || save.isPending}
          onClick={() => save.mutate()}
        >
          {save.isPending ? 'Saving…' : 'Save'}
        </Button>
      </CardContent>
    </Card>
  )
}
