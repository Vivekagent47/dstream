import { createFileRoute, useRouter } from '@tanstack/react-router'
import { useEffect, useState } from 'react'

import { api, type Source } from '#/lib/api'

export const Route = createFileRoute('/sources')({ component: SourcesPage })

function SourcesPage() {
  const router = useRouter()
  const [sources, setSources] = useState<Source[] | null>(null)
  const [name, setName] = useState('')
  const [type, setType] = useState('generic')
  const [err, setErr] = useState<string | null>(null)

  async function load() {
    try {
      const s = await api.listSources()
      setSources(s)
    } catch (e: any) {
      setErr(e.message)
    }
  }
  useEffect(() => {
    void load()
  }, [])

  async function create(e: React.FormEvent) {
    e.preventDefault()
    setErr(null)
    try {
      await api.createSource({ name, type })
      setName('')
      await load()
      router.invalidate()
    } catch (e: any) {
      setErr(e.message)
    }
  }

  return (
    <main className="page-wrap mx-auto px-4 pb-16 pt-10">
      <h1 className="mb-6 text-2xl font-semibold">Sources</h1>

      <form
        onSubmit={create}
        className="mb-8 flex flex-wrap items-end gap-3 rounded-xl border border-[rgba(23,58,64,0.08)] bg-white/60 p-4"
      >
        <label className="flex-1 min-w-[200px]">
          <span className="mb-1 block text-xs font-medium uppercase tracking-wide text-[var(--sea-ink-soft)]">
            Name
          </span>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="stripe-prod"
            className="w-full rounded-lg border border-[rgba(23,58,64,0.15)] bg-white px-3 py-2 text-sm"
            required
          />
        </label>
        <label>
          <span className="mb-1 block text-xs font-medium uppercase tracking-wide text-[var(--sea-ink-soft)]">
            Type
          </span>
          <select
            value={type}
            onChange={(e) => setType(e.target.value)}
            className="rounded-lg border border-[rgba(23,58,64,0.15)] bg-white px-3 py-2 text-sm"
          >
            <option value="generic">generic</option>
            <option value="stripe">stripe</option>
            <option value="github">github</option>
            <option value="shopify">shopify</option>
          </select>
        </label>
        <button
          type="submit"
          className="rounded-full bg-[var(--sea-ink)] px-5 py-2.5 text-sm font-semibold text-white"
        >
          Create
        </button>
      </form>

      {err && <p className="mb-4 text-sm text-red-600">{err}</p>}

      <div className="overflow-hidden rounded-xl border border-[rgba(23,58,64,0.08)]">
        <table className="w-full border-collapse text-sm">
          <thead className="bg-white/70 text-left text-xs uppercase tracking-wide text-[var(--sea-ink-soft)]">
            <tr>
              <th className="px-4 py-3">Name</th>
              <th className="px-4 py-3">Type</th>
              <th className="px-4 py-3">Ingest URL</th>
              <th className="px-4 py-3">Created</th>
            </tr>
          </thead>
          <tbody className="bg-white/40">
            {sources?.map((s) => (
              <tr key={s.id} className="border-t border-[rgba(23,58,64,0.06)]">
                <td className="px-4 py-3 font-medium">{s.name}</td>
                <td className="px-4 py-3">{s.type}</td>
                <td className="px-4 py-3 font-mono text-xs">
                  <code>/e/{s.ingest_token}</code>
                </td>
                <td className="px-4 py-3 text-[var(--sea-ink-soft)]">
                  {new Date(s.created_at).toLocaleString()}
                </td>
              </tr>
            ))}
            {sources && sources.length === 0 && (
              <tr>
                <td colSpan={4} className="px-4 py-8 text-center text-sm text-[var(--sea-ink-soft)]">
                  No sources yet — create one above.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </main>
  )
}
