import { createFileRoute, Link } from '@tanstack/react-router'

export const Route = createFileRoute('/')({ component: Home })

function Home() {
  return (
    <main className="page-wrap mx-auto px-4 pt-14 pb-8">
      <section className="rounded-2xl border border-[rgba(23,58,64,0.08)] bg-white/60 p-8">
        <p className="island-kicker mb-3">dstream — webhook IDE</p>
        <h1 className="display-title mb-4 text-4xl font-bold tracking-tight text-[var(--sea-ink)]">
          Manage every webhook your team relies on.
        </h1>
        <p className="mb-6 max-w-2xl text-[var(--sea-ink-soft)]">
          Receive, route, retry, and replay webhook traffic. Test locally with the CLI tunnel.
          Visual workflow builder and record/replay coming in later phases.
        </p>
        <div className="flex gap-3">
          <Link
            to="/sources"
            className="rounded-full bg-[var(--sea-ink)] px-5 py-2.5 text-sm font-semibold text-white no-underline"
          >
            View sources
          </Link>
          <Link
            to="/events"
            className="rounded-full border border-[rgba(23,58,64,0.2)] bg-white px-5 py-2.5 text-sm font-semibold text-[var(--sea-ink)] no-underline"
          >
            View events
          </Link>
        </div>
      </section>

      <section className="mt-8 grid gap-4 sm:grid-cols-3">
        {[
          ['Sources', 'Receive webhook traffic; pick provider type (Stripe, GitHub, generic).'],
          ['Destinations', 'HTTP endpoints + per-destination RPS limits and max-inflight.'],
          ['Connections', 'Route source → destination with per-pair retry policy.'],
        ].map(([title, desc]) => (
          <article
            key={title}
            className="rounded-2xl border border-[rgba(23,58,64,0.08)] bg-white/60 p-5"
          >
            <h2 className="mb-2 text-base font-semibold">{title}</h2>
            <p className="m-0 text-sm text-[var(--sea-ink-soft)]">{desc}</p>
          </article>
        ))}
      </section>
    </main>
  )
}
