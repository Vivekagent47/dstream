import { createFileRoute } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'

import { api, qk, type AdminOrgUsage, type Plan, type PlanPreset } from '#/lib/api'
import { formatAgainst, quotaState } from '#/lib/quota'
import { AuthErrorBoundary } from '#/components/AuthErrorBoundary'
import { PageHeader } from '#/components/TopBar'
import { Badge } from '#/components/ui/badge'
import { Button } from '#/components/ui/button'
import { Input } from '#/components/ui/input'
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

export const Route = createFileRoute('/console/usage')({
  component: AdminUsageView,
  errorComponent: AuthErrorBoundary,
})

// The 0-means-unlimited rules live in #/lib/quota, shared with the tenant's
// own usage page so the operator and the customer never read the same numbers
// differently. Covered by src/lib/quota.test.ts.
const against = formatAgainst

function state(count: number, soft: number, hard: number) {
  const q = quotaState(count, soft, hard)
  if (q.overHard) return { label: 'At ceiling', variant: 'destructive' as const }
  if (q.overSoft) return { label: 'Over soft', variant: 'warning' as const }
  return null
}

function AdminUsageView() {
  const orgs = useQuery({ queryKey: qk.adminUsage(), queryFn: api.adminUsage })
  const plans = useQuery({ queryKey: qk.adminPlans(), queryFn: api.adminPlans })

  const rows = orgs.data ?? []

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader
        title="Plans and quotas"
        help="Every org against its own limits, for the current period. Choosing a tier applies that tier's limits; `custom` unlocks the numbers for a negotiated exception."
      />
      <div className="flex-1 space-y-6 overflow-y-auto px-6 py-6">
        {plans.data && (
          <div className="flex flex-wrap gap-2">
            {plans.data.map((p) => (
              <span
                key={p.plan}
                className="rounded-md border border-border px-2 py-1 text-xs text-muted-foreground"
              >
                <span className="font-medium text-foreground">{p.plan}</span>
                {p.custom
                  ? ' — operator-set numbers'
                  : ` — ${p.events_hard === 0 ? '∞' : p.events_hard.toLocaleString()} events, ${
                      p.messages_hard === 0 ? '∞' : p.messages_hard.toLocaleString()
                    } messages / ${p.period}`}
              </span>
            ))}
          </div>
        )}

        <div className="overflow-x-auto rounded-lg border border-border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-4">Organization</TableHead>
                <TableHead>Plan</TableHead>
                <TableHead>Events</TableHead>
                <TableHead>Messages</TableHead>
                <TableHead className="pr-4">Limits</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((o) => (
                // Keyed on org_id plus every persisted quota field, not just
                // org_id: org_id alone would keep this row's instance (and
                // its draft inputs) mounted across a plan switch, so a typed
                // but unsaved number would survive invisibly through a
                // free->custom round trip and later overwrite the real
                // server value (see fix-round-1 in task-5-report.md). Widening
                // the key remounts the row — resetting its draft — exactly
                // when the server's stored numbers change, and only then; an
                // ordinary refetch that returns the same values is not a key
                // change, so it won't blow away a draft mid-edit. org_id stays
                // the leading component so two tenants can never collide.
                <OrgQuotaRow
                  key={`${o.org_id}:${o.plan}:${o.period}:${o.quota_events_soft}:${o.quota_events_hard}:${o.quota_messages_soft}:${o.quota_messages_hard}`}
                  org={o}
                  plans={plans.data ?? []}
                />
              ))}
              {rows.length === 0 && (
                <TableRow>
                  <TableCell
                    colSpan={5}
                    className="py-10 text-center text-sm text-muted-foreground"
                  >
                    {orgs.isLoading ? 'Loading…' : 'No organizations.'}
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </div>
      </div>
    </div>
  )
}

/**
 * OrgQuotaRow is one org's row, holding its own form state.
 *
 * Deliberately a component per row rather than a table with a selected-org
 * state: the mutation closes over this row's `org.org_id` and nothing else,
 * so a re-sort, a refetch, or an edit in a second row cannot write a quota to
 * the wrong tenant. Don't hoist this state into the table.
 */
function OrgQuotaRow({ org, plans }: { org: AdminOrgUsage; plans: PlanPreset[] }) {
  const qc = useQueryClient()
  const [eventsSoft, setEventsSoft] = useState(String(org.quota_events_soft))
  const [eventsHard, setEventsHard] = useState(String(org.quota_events_hard))
  const [messagesSoft, setMessagesSoft] = useState(String(org.quota_messages_soft))
  const [messagesHard, setMessagesHard] = useState(String(org.quota_messages_hard))

  const save = useMutation({
    mutationFn: (input: Parameters<typeof api.adminPatchOrgPlan>[1]) =>
      api.adminPatchOrgPlan(org.org_id, input),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: qk.adminUsage() })
      toast.success(`Updated ${org.org_name}`)
    },
    onError: (e) => toast.error((e as Error).message),
  })

  const isCustom = org.plan === 'custom'
  const eventsState = state(org.usage.events, org.quota_events_soft, org.quota_events_hard)
  const messagesState = state(org.usage.messages, org.quota_messages_soft, org.quota_messages_hard)

  // Every limit is a non-negative integer, and 0 means unlimited rather than
  // "no capacity", so there is no separate unlimited control to wire up.
  const valid = [eventsSoft, eventsHard, messagesSoft, messagesHard].every(
    (s) => s.trim() !== '' && Number.isInteger(Number(s)) && Number(s) >= 0,
  )
  // Numeric, not string, comparison: a non-canonical numeral like "007"
  // round-trips through the server as "7", and comparing strings would leave
  // dirty stuck true forever after a successful save. Number() is safe here
  // only because `valid` has already confirmed each string is an integer.
  const dirty =
    Number(eventsSoft) !== org.quota_events_soft ||
    Number(eventsHard) !== org.quota_events_hard ||
    Number(messagesSoft) !== org.quota_messages_soft ||
    Number(messagesHard) !== org.quota_messages_hard

  return (
    <TableRow>
      <TableCell className="pl-4">
        <div className="font-medium">{org.org_name}</div>
        <div className="font-mono text-xs text-muted-foreground">{org.org_slug}</div>
      </TableCell>
      <TableCell>
        {/* Switching to a preset applies its limits server-side; switching to
            custom sends plan alone, which just unlocks the numbers. Either
            way the request carries no quota_* field, so it never trips the
            preset-rejects-limits rule. */}
        <Select
          value={org.plan}
          onValueChange={(v) => save.mutate({ plan: (v as Plan) ?? 'free' })}
        >
          <SelectTrigger className="w-[140px]">
            <SelectValue>{(v: string | null) => v ?? org.plan}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {plans.map((p) => (
              <SelectItem key={p.plan} value={p.plan}>
                {p.plan}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <div className="mt-1 text-xs text-muted-foreground">per {org.period}</div>
      </TableCell>
      <TableCell className="tabular-nums">
        {against(org.usage.events, org.quota_events_hard)}
        {eventsState && (
          <Badge variant={eventsState.variant} className="ml-2">
            {eventsState.label}
          </Badge>
        )}
      </TableCell>
      <TableCell className="tabular-nums">
        {against(org.usage.messages, org.quota_messages_hard)}
        {messagesState && (
          <Badge variant={messagesState.variant} className="ml-2">
            {messagesState.label}
          </Badge>
        )}
      </TableCell>
      <TableCell className="pr-4">
        {!isCustom ? (
          <span className="text-xs text-muted-foreground">
            Set by the {org.plan} tier — switch to custom to edit.
          </span>
        ) : (
          <div className="flex flex-wrap items-center gap-2">
            <Input
              aria-label={`${org.org_name} events soft limit`}
              className="w-24"
              type="number"
              min="0"
              value={eventsSoft}
              onChange={(e) => setEventsSoft(e.target.value)}
            />
            <Input
              aria-label={`${org.org_name} events hard ceiling`}
              className="w-24"
              type="number"
              min="0"
              value={eventsHard}
              onChange={(e) => setEventsHard(e.target.value)}
            />
            <Input
              aria-label={`${org.org_name} messages soft limit`}
              className="w-24"
              type="number"
              min="0"
              value={messagesSoft}
              onChange={(e) => setMessagesSoft(e.target.value)}
            />
            <Input
              aria-label={`${org.org_name} messages hard ceiling`}
              className="w-24"
              type="number"
              min="0"
              value={messagesHard}
              onChange={(e) => setMessagesHard(e.target.value)}
            />
            <Button
              size="sm"
              disabled={!dirty || !valid || save.isPending}
              onClick={() =>
                save.mutate({
                  quota_events_soft: Number(eventsSoft),
                  quota_events_hard: Number(eventsHard),
                  quota_messages_soft: Number(messagesSoft),
                  quota_messages_hard: Number(messagesHard),
                })
              }
            >
              {save.isPending ? 'Saving…' : 'Save'}
            </Button>
          </div>
        )}
      </TableCell>
    </TableRow>
  )
}
