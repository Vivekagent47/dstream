import { createFileRoute, Link } from '@tanstack/react-router'
import { useEffect, useState } from 'react'

import { api, type Event } from '#/lib/api'

export const Route = createFileRoute('/events')({ component: EventsPage })

const statusColor: Record<string, string> = {
  delivered: 'text-emerald-700 bg-emerald-50',
  queued: 'text-slate-700 bg-slate-100',
  in_flight: 'text-blue-700 bg-blue-50',
  failed: 'text-red-700 bg-red-50',
  paused: 'text-amber-700 bg-amber-50',
  dead: 'text-red-900 bg-red-100',
}

function EventsPage() {
  const [events, setEvents] = useState<Event[] | null>(null)
  const [err, setErr] = useState<string | null>(null)

  useEffect(() => {
    let dead = false
    const tick = () => {
      api.listEvents({ limit: 100 })
        .then((e) => !dead && setEvents(e))
        .catch((e) => !dead && setErr(e.message))
    }
    tick()
    const t = setInterval(tick, 5000)
    return () => {
      dead = true
      clearInterval(t)
    }
  }, [])

  return (
    <main className="page-wrap mx-auto px-4 pb-16 pt-10">
      <h1 className="mb-6 text-2xl font-semibold">Events</h1>

      {err && <p className="mb-4 text-sm text-red-600">{err}</p>}

      <div className="overflow-hidden rounded-xl border border-[rgba(23,58,64,0.08)]">
        <table className="w-full border-collapse text-sm">
          <thead className="bg-white/70 text-left text-xs uppercase tracking-wide text-[var(--sea-ink-soft)]">
            <tr>
              <th className="px-4 py-3">ID</th>
              <th className="px-4 py-3">Status</th>
              <th className="px-4 py-3">Attempts</th>
              <th className="px-4 py-3">Last attempt</th>
              <th className="px-4 py-3">Next retry</th>
              <th className="px-4 py-3">Created</th>
            </tr>
          </thead>
          <tbody className="bg-white/40">
            {events?.map((e) => (
              <tr key={e.id} className="border-t border-[rgba(23,58,64,0.06)]">
                <td className="px-4 py-3 font-mono text-xs">
                  <Link to="/events/$id" params={{ id: e.id }} className="text-[var(--lagoon-deep)]">
                    {e.id.slice(0, 8)}
                  </Link>
                </td>
                <td className="px-4 py-3">
                  <span
                    className={`inline-block rounded-full px-2 py-0.5 text-xs font-medium ${statusColor[e.status] || 'bg-slate-100'}`}
                  >
                    {e.status}
                  </span>
                </td>
                <td className="px-4 py-3">{e.attempt_count}</td>
                <td className="px-4 py-3 text-[var(--sea-ink-soft)]">
                  {e.last_attempt_at ? new Date(e.last_attempt_at).toLocaleString() : '—'}
                </td>
                <td className="px-4 py-3 text-[var(--sea-ink-soft)]">
                  {e.next_retry_at ? new Date(e.next_retry_at).toLocaleString() : '—'}
                </td>
                <td className="px-4 py-3 text-[var(--sea-ink-soft)]">
                  {new Date(e.created_at).toLocaleString()}
                </td>
              </tr>
            ))}
            {events && events.length === 0 && (
              <tr>
                <td colSpan={6} className="px-4 py-8 text-center text-sm text-[var(--sea-ink-soft)]">
                  No events yet.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </main>
  )
}
