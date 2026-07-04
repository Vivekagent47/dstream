import { createFileRoute, Link } from '@tanstack/react-router'
import { ArrowRight } from 'lucide-react'

export const Route = createFileRoute('/')({ component: Home })

const PROVIDERS = ['Stripe', 'GitHub', 'Shopify', 'Twilio', 'Slack', 'Clerk']

function Home() {
  return (
    <main className="bg-background">
      <div className="mx-auto max-w-[1140px] border-x border-border">
        <Hero />
        <LogoCloud />
        <FlowSection />
        <FilterSection />
        <RateSection />
        <RetrySection />
        <TunnelSection />
        <CtaSection />
      </div>
    </main>
  )
}

/* ------------------------------------------------------------------ */
/* Shared bits                                                         */
/* ------------------------------------------------------------------ */

function SectionLabel({ children }: { children: React.ReactNode }) {
  return (
    <p className="mb-8 flex items-center gap-2 font-mono text-xs font-medium tracking-wide text-muted-foreground uppercase">
      <span className="text-primary">◇</span>
      {children}
    </p>
  )
}

function Section({ children }: { children: React.ReactNode }) {
  return <section className="border-b border-border px-6 py-16 sm:px-10 sm:py-20">{children}</section>
}

/* ------------------------------------------------------------------ */
/* Hero                                                               */
/* ------------------------------------------------------------------ */

function Hero() {
  return (
    <section className="border-b border-border px-6 py-20 text-center sm:py-28">
      <h1 className="mx-auto max-w-3xl text-5xl font-extrabold tracking-tight text-balance text-foreground sm:text-7xl">
        Never drop a{' '}
        <span className="relative whitespace-nowrap text-primary">
          webhook
          <svg
            viewBox="0 0 220 16"
            fill="none"
            preserveAspectRatio="none"
            className="absolute -bottom-2 left-0 h-3 w-full text-primary"
            aria-hidden="true"
          >
            <path
              d="M3 11c40-7 120-9 214-4"
              stroke="currentColor"
              strokeWidth="4"
              strokeLinecap="round"
            />
          </svg>
        </span>
      </h1>
      <p className="mx-auto mt-8 max-w-xl text-lg text-muted-foreground text-pretty">
        Reliable infrastructure to receive, route, retry, and replay every webhook — without
        building the delivery plumbing yourself.
      </p>
      <div className="mt-9 flex flex-wrap items-center justify-center gap-3">
        <Link
          to="/sources"
          className="inline-flex items-center gap-2 rounded-md bg-foreground px-5 py-2.5 text-sm font-semibold text-background no-underline shadow-sm transition hover:opacity-90"
        >
          Get started
          <ArrowRight className="h-4 w-4" />
        </Link>
        <a
          href="https://github.com/Vivekagent47/dstream"
          target="_blank"
          rel="noreferrer"
          className="inline-flex items-center rounded-md border border-border bg-background px-5 py-2.5 text-sm font-semibold text-foreground no-underline transition hover:bg-muted"
        >
          Read docs
        </a>
      </div>
    </section>
  )
}

/* ------------------------------------------------------------------ */
/* Logo cloud                                                         */
/* ------------------------------------------------------------------ */

function LogoCloud() {
  return (
    <section className="border-b border-border px-6 py-10">
      <p className="text-center font-mono text-[11px] tracking-[0.2em] text-muted-foreground uppercase">
        Handles every event your team relies on
      </p>
      <div className="mt-6 flex flex-wrap items-center justify-center gap-x-10 gap-y-4">
        {PROVIDERS.map((p) => (
          <span key={p} className="text-lg font-bold tracking-tight text-foreground/40">
            {p}
          </span>
        ))}
      </div>
    </section>
  )
}

/* ------------------------------------------------------------------ */
/* Flow diagram — the signature visual                                */
/* ------------------------------------------------------------------ */

function FlowSection() {
  return (
    <Section>
      <SectionLabel>Receive, route, deliver</SectionLabel>
      <div className="grid gap-10 lg:grid-cols-[1fr_1.3fr] lg:items-center">
        <div>
          <h2 className="text-3xl font-bold tracking-tight text-foreground sm:text-4xl">
            One gateway between the sender and your service
          </h2>
          <p className="mt-4 text-muted-foreground">
            Point any provider at a dstream source, wire it to a destination with a connection, and
            every event is captured, routed, and delivered — with a full audit trail.
          </p>
        </div>
        <FlowDiagram />
      </div>
    </Section>
  )
}

function FlowDiagram() {
  const sources = ['Stripe', 'GitHub', 'Twilio']
  const dests = ['HTTP', 'Local', 'Mock API']
  const ys = [58, 150, 242]
  return (
    <svg
      viewBox="0 0 720 300"
      className="w-full font-mono"
      role="img"
      aria-label="Sources route through dstream to destinations"
    >
      {/* connectors: sources -> node */}
      {ys.map((y, i) => (
        <path
          key={`l${i}`}
          d={`M180 ${y} C 245 ${y} 245 150 300 150`}
          className="fill-none stroke-border"
          strokeWidth="2"
        />
      ))}
      {/* connectors: node -> dests */}
      {ys.map((y, i) => (
        <path
          key={`r${i}`}
          d={`M420 150 C 475 150 475 ${y} 540 ${y}`}
          className="fill-none stroke-border"
          strokeWidth="2"
        />
      ))}

      {/* source chips */}
      {sources.map((s, i) => (
        <Chip key={s} x={20} y={ys[i] - 24} label={s} />
      ))}

      {/* central node */}
      <rect
        x={300}
        y={116}
        width={120}
        height={68}
        rx={10}
        className="fill-primary/5 stroke-primary"
        strokeWidth="2"
      />
      <text
        x={360}
        y={146}
        textAnchor="middle"
        className="fill-primary text-[15px] font-bold"
      >
        dstream
      </text>
      <text x={360} y={166} textAnchor="middle" className="fill-muted-foreground text-[10px]">
        route · retry · replay
      </text>

      {/* dest chips */}
      {dests.map((d, i) => (
        <Chip key={d} x={550} y={ys[i] - 24} label={d} />
      ))}
    </svg>
  )
}

function Chip({ x, y, label }: { x: number; y: number; label: string }) {
  return (
    <g>
      <rect
        x={x}
        y={y}
        width={150}
        height={48}
        rx={10}
        className="fill-card stroke-border"
        strokeWidth="2"
      />
      <circle cx={x + 24} cy={y + 24} r={5} className="fill-primary" />
      <text x={x + 42} y={y + 29} className="fill-foreground text-[13px] font-medium">
        {label}
      </text>
    </g>
  )
}

/* ------------------------------------------------------------------ */
/* Filter / transform / route                                         */
/* ------------------------------------------------------------------ */

function FilterSection() {
  return (
    <Section>
      <SectionLabel>Filter, transform, route</SectionLabel>
      <div className="grid gap-10 lg:grid-cols-2 lg:items-center">
        <div>
          <h2 className="text-3xl font-bold tracking-tight text-foreground sm:text-4xl">
            Only deliver the events that matter
          </h2>
          <p className="mt-4 text-muted-foreground">
            Match on payload shape and route per source-destination pair. Skip the noise before it
            ever reaches your service.
          </p>
        </div>
        <CodeCard title="filtering shopify → local">
          <span className="text-muted-foreground">{'{'}</span>
          {'\n  '}
          <span className="text-primary">"product"</span>: {'{'}
          {'\n    '}
          <span className="text-primary">"inventory"</span>:{' '}
          <span className="text-foreground">0</span>
          {'\n  }'}
          {'\n'}
          <span className="text-muted-foreground">{'}'}</span>
        </CodeCard>
      </div>
    </Section>
  )
}

function CodeCard({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="overflow-hidden rounded-lg border border-border bg-muted/40 shadow-sm">
      <div className="flex items-center gap-2 border-b border-border px-4 py-2.5 font-mono text-xs tracking-wide text-muted-foreground uppercase">
        <span className="text-primary">◇</span>
        {title}
      </div>
      <pre className="overflow-x-auto p-5 font-mono text-sm leading-relaxed text-foreground">
        <code>{children}</code>
      </pre>
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* Queue with rate control                                            */
/* ------------------------------------------------------------------ */

function RateSection() {
  const bars = [40, 62, 48, 80, 55, 70, 45, 90, 60, 75, 50, 68]
  return (
    <Section>
      <SectionLabel>Queue with rate control</SectionLabel>
      <div className="grid gap-10 lg:grid-cols-2 lg:items-center">
        <div>
          <h2 className="text-3xl font-bold tracking-tight text-foreground sm:text-4xl">
            Never overwhelm a slow endpoint
          </h2>
          <p className="mt-4 text-muted-foreground">
            Per-destination rate limits and max-inflight caps keep delivery smooth. Bursts queue up
            and drain at exactly the pace your service can handle.
          </p>
        </div>
        <div className="rounded-lg border border-border bg-card p-6 shadow-sm">
          <div className="mb-4 flex items-center justify-between font-mono text-xs tracking-wide text-muted-foreground uppercase">
            <span>Delivery rate</span>
            <span className="text-primary">120 / s</span>
          </div>
          <div className="flex h-32 items-end gap-1.5">
            {bars.map((h, i) => (
              <div
                key={i}
                style={{ height: `${h}%` }}
                className={`flex-1 rounded-sm ${i % 4 === 3 ? 'bg-primary' : 'bg-primary/25'}`}
              />
            ))}
          </div>
        </div>
      </div>
    </Section>
  )
}

/* ------------------------------------------------------------------ */
/* Auto-detect & replay                                               */
/* ------------------------------------------------------------------ */

function RetrySection() {
  return (
    <Section>
      <SectionLabel>Auto-detect issues & replay</SectionLabel>
      <div className="grid gap-10 lg:grid-cols-2 lg:items-center">
        <div>
          <h2 className="text-3xl font-bold tracking-tight text-foreground sm:text-4xl">
            Catch failures, then replay in one click
          </h2>
          <p className="mt-4 text-muted-foreground">
            Failed deliveries are retried automatically with backoff. When an endpoint recovers,
            bulk-replay everything it missed — no re-triggering the source.
          </p>
        </div>
        <div className="rounded-lg border border-border bg-card p-5 shadow-sm">
          <p className="flex items-center gap-2 text-sm font-semibold text-destructive">
            <span className="flex h-5 w-5 items-center justify-center rounded-full bg-destructive/10 text-xs">
              !
            </span>
            HTTP 400 on “shopify → local”
          </p>
          <pre className="mt-4 overflow-x-auto rounded-md bg-muted/50 p-4 font-mono text-sm leading-relaxed text-foreground">
            <code>
              {'{ "type": '}
              <span className="text-destructive">"validation_error"</span>
              {', "code":\n  "invalid_payload", "detail": "Invalid\n  payload. Field '}
              <span className="text-primary">"distinct_id"</span>
              {'\n  should not be blank" }'}
            </code>
          </pre>
          <button className="mt-4 inline-flex w-full items-center justify-center gap-2 rounded-md bg-foreground px-4 py-2.5 text-sm font-semibold text-background transition hover:opacity-90">
            Bulk retry events
          </button>
        </div>
      </div>
    </Section>
  )
}

/* ------------------------------------------------------------------ */
/* CLI tunnel                                                         */
/* ------------------------------------------------------------------ */

function TunnelSection() {
  const terminals = [
    ['alex', 'POST webhooks/shopify/orders'],
    ['maurice', 'POST webhooks/stripe/charges'],
    ['thomas', 'POST webhooks/github/push'],
  ]
  return (
    <Section>
      <SectionLabel>Test locally</SectionLabel>
      <div className="mx-auto max-w-2xl text-center">
        <h2 className="text-3xl font-bold tracking-tight text-foreground sm:text-4xl">
          Forward live traffic to localhost
        </h2>
        <p className="mt-4 text-muted-foreground">
          The CLI tunnel streams real webhook payloads to your machine — the whole team can share one
          source and debug against production traffic.
        </p>
      </div>
      <div className="mx-auto mt-10 max-w-2xl space-y-3">
        <div className="flex items-center gap-3 rounded-lg border border-border bg-muted/40 px-4 py-3 font-mono text-sm">
          <span className="text-muted-foreground">↳</span>
          <span className="text-foreground">https://in.dstream.dev/src_1a2b3c</span>
        </div>
        {terminals.map(([who, req]) => (
          <div
            key={who}
            className="flex items-center gap-3 rounded-lg border border-border bg-card px-4 py-3 font-mono text-sm shadow-xs"
          >
            <span className="text-muted-foreground">{who}'s terminal</span>
            <span className="ml-auto flex items-center gap-2">
              <span className="rounded bg-primary/10 px-1.5 py-0.5 text-xs font-semibold text-primary">
                200
              </span>
              <span className="hidden text-foreground sm:inline">{req}</span>
            </span>
          </div>
        ))}
      </div>
      <div className="mt-8 text-center">
        <a
          href="https://github.com/Vivekagent47/dstream"
          target="_blank"
          rel="noreferrer"
          className="inline-flex items-center gap-2 rounded-md border border-border bg-background px-5 py-2.5 text-sm font-semibold text-foreground no-underline transition hover:bg-muted"
        >
          Install the CLI
          <ArrowRight className="h-4 w-4" />
        </a>
      </div>
    </Section>
  )
}

/* ------------------------------------------------------------------ */
/* CTA                                                               */
/* ------------------------------------------------------------------ */

function CtaSection() {
  return (
    <section className="px-6 py-20 text-center sm:py-24">
      <h2 className="mx-auto max-w-2xl text-4xl font-extrabold tracking-tight text-foreground sm:text-5xl">
        Ship webhooks you can trust.
      </h2>
      <p className="mx-auto mt-4 max-w-lg text-muted-foreground">
        Spin up a source, connect a destination, and let dstream handle the retries.
      </p>
      <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
        <Link
          to="/sources"
          className="inline-flex items-center gap-2 rounded-md bg-foreground px-5 py-2.5 text-sm font-semibold text-background no-underline shadow-sm transition hover:opacity-90"
        >
          Get started
          <ArrowRight className="h-4 w-4" />
        </Link>
        <Link
          to="/events"
          className="inline-flex items-center rounded-md border border-border bg-background px-5 py-2.5 text-sm font-semibold text-foreground no-underline transition hover:bg-muted"
        >
          View events
        </Link>
      </div>
    </section>
  )
}
