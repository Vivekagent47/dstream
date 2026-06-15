import { createFileRoute } from '@tanstack/react-router'
import { useEffect, useState } from 'react'

import { api, type Attempt, type Event } from '#/lib/api'

export const Route = createFileRoute('/events/$id')({ component: EventDetail })

function EventDetail() {
  const { id } = Route.useParams()
  const [ev, setEv] = useState<(Event & { attempts: Attempt[] }) | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function load() {
    try {
      setEv(await api.getEvent(id))
    } catch (e: any) {
      setErr(e.message)
    }
  }
  useEffect(() => {
    void load()
  }, [id])

  async function retry() {
    setBusy(true)
    try {
      await api.retryEvent(id)
      await load()
    } catch (e: any) {
      setErr(e.message)
    } finally {
      setBusy(false)
    }
  }

  if (err) return <main className="page-wrap mx-auto px-4 pt-10 text-sm text-red-600">{err}</main>
  if (!ev) return <main className="page-wrap mx-auto px-4 pt-10 text-sm">Loading…</main>

  return (
    <main className="page-wrap mx-auto px-4 pb-16 pt-10">
      <div className="mb-6 flex items-center justify-between">
        <h1 className="text-2xl font-semibold">Event {ev.id.slice(0, 8)}</h1>
        <button
          onClick={retry}
          disabled={busy}
          className="rounded-full bg-[var(--sea-ink)] px-5 py-2 text-sm font-semibold text-white disabled:opacity-50"
        >
          {busy ? 'Retrying…' : 'Retry now'}
        </button>
      </div>

      <dl className="mb-8 grid grid-cols-2 gap-x-6 gap-y-2 rounded-xl border border-[rgba(23,58,64,0.08)] bg-white/60 p-5 text-sm sm:grid-cols-4">
        <Pair k="Status" v={ev.status} />
        <Pair k="Attempts" v={String(ev.attempt_count)} />
        <Pair k="Last attempt" v={ev.last_attempt_at ?? '—'} />
        <Pair k="Next retry" v={ev.next_retry_at ?? '—'} />
      </dl>

      <h2 className="mb-3 text-lg font-semibold">Attempts</h2>
      <div className="overflow-hidden rounded-xl border border-[rgba(23,58,64,0.08)]">
        <table className="w-full border-collapse text-sm">
          <thead className="bg-white/70 text-left text-xs uppercase tracking-wide text-[var(--sea-ink-soft)]">
            <tr>
              <th className="px-4 py-3">#</th>
              <th className="px-4 py-3">Status</th>
              <th className="px-4 py-3">Duration</th>
              <th className="px-4 py-3">Error</th>
              <th className="px-4 py-3">When</th>
            </tr>
          </thead>
          <tbody className="bg-white/40">
            {ev.attempts.map((a) => (
              <tr key={a.id} className="border-t border-[rgba(23,58,64,0.06)]">
                <td className="px-4 py-3">{a.attempt_num}</td>
                <td className="px-4 py-3">{a.response_status ?? '—'}</td>
                <td className="px-4 py-3">{a.duration_ms != null ? `${a.duration_ms}ms` : '—'}</td>
                <td className="px-4 py-3 text-red-700">{a.error_message ?? ''}</td>
                <td className="px-4 py-3 text-[var(--sea-ink-soft)]">
                  {new Date(a.attempted_at).toLocaleString()}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </main>
  )
}

function Pair({ k, v }: { k: string; v: string }) {
  return (
    <>
      <dt className="text-xs uppercase tracking-wide text-[var(--sea-ink-soft)]">{k}</dt>
      <dd className="text-sm">{v}</dd>
    </>
  )
}
