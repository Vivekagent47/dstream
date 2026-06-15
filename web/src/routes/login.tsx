import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'

import { api } from '#/lib/api'

export const Route = createFileRoute('/login')({ component: Login })

function Login() {
  const [email, setEmail] = useState('')
  const [sent, setSent] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setErr(null)
    try {
      await api.requestMagicLink(email)
      setSent(true)
    } catch (e: any) {
      setErr(e.message)
    }
  }

  if (sent) {
    return (
      <main className="page-wrap mx-auto px-4 pt-20">
        <div className="mx-auto max-w-md rounded-2xl border border-[rgba(23,58,64,0.08)] bg-white/70 p-8 text-center">
          <h1 className="mb-2 text-xl font-semibold">Check your email</h1>
          <p className="text-sm text-[var(--sea-ink-soft)]">
            If <strong>{email}</strong> is a known account, a sign-in link is on its way.
          </p>
          <p className="mt-4 text-xs text-[var(--sea-ink-soft)]">
            Dev tip: dstream logs the link to stdout when SMTP is not configured.
          </p>
        </div>
      </main>
    )
  }

  return (
    <main className="page-wrap mx-auto px-4 pt-20">
      <form
        onSubmit={submit}
        className="mx-auto max-w-md rounded-2xl border border-[rgba(23,58,64,0.08)] bg-white/70 p-8"
      >
        <h1 className="mb-1 text-xl font-semibold">Sign in to dstream</h1>
        <p className="mb-6 text-sm text-[var(--sea-ink-soft)]">
          We'll email you a single-use link.
        </p>
        <label className="block">
          <span className="mb-1 block text-sm font-medium">Email</span>
          <input
            type="email"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            className="w-full rounded-lg border border-[rgba(23,58,64,0.15)] bg-white px-3 py-2 text-sm"
          />
        </label>
        {err && <p className="mt-3 text-sm text-red-600">{err}</p>}
        <button
          type="submit"
          className="mt-5 w-full rounded-full bg-[var(--sea-ink)] px-5 py-2.5 text-sm font-semibold text-white"
        >
          Send magic link
        </button>
      </form>
    </main>
  )
}
