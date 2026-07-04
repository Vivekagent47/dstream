import { Link, useRouterState } from '@tanstack/react-router'

const COLUMNS: { title: string; links: { label: string; to: string; external?: boolean }[] }[] = [
  {
    title: 'Product',
    links: [
      { label: 'Sources', to: '/sources' },
      { label: 'Events', to: '/events' },
      { label: 'Settings', to: '/settings/org' },
    ],
  },
  {
    title: 'Resources',
    links: [
      { label: 'Docs', to: 'https://github.com/Vivekagent47/dstream', external: true },
      { label: 'GitHub', to: 'https://github.com/Vivekagent47/dstream', external: true },
    ],
  },
  {
    title: 'Account',
    links: [
      { label: 'Sign in', to: '/login' },
      { label: 'New org', to: '/orgs/new' },
    ],
  },
]

function Wordmark() {
  return (
    <span className="inline-flex items-center gap-2 text-[15px] font-bold tracking-tight text-foreground">
      <span className="flex h-6 w-6 items-center justify-center rounded-md bg-foreground text-background">
        <svg viewBox="0 0 16 16" className="h-3.5 w-3.5" aria-hidden="true">
          <path
            fill="currentColor"
            d="M2 4.5 8 1l6 3.5v7L8 15l-6-3.5v-7Zm6 1.2L4.7 7.6 8 9.5l3.3-1.9L8 5.7Z"
          />
        </svg>
      </span>
      dstream
    </span>
  )
}

export default function Footer() {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const isPublicRoute =
    pathname === '/login' || pathname === '/auth/verify' || pathname.startsWith('/invites/')
  if (isPublicRoute) {
    return null
  }

  const year = new Date().getFullYear()
  const linkClass = 'text-sm text-muted-foreground no-underline transition hover:text-foreground'

  return (
    <footer className="border-t border-border bg-muted/30">
      <div className="mx-auto max-w-285 px-6">
        <div className="grid gap-10 py-14 sm:grid-cols-2 lg:grid-cols-4">
          <div>
            <Wordmark />
            <p className="mt-4 max-w-xs text-sm text-muted-foreground">
              Open-source infrastructure to receive, route, retry, and replay webhooks.
            </p>
          </div>
          {COLUMNS.map((col) => (
            <div key={col.title}>
              <h3 className="font-mono text-xs font-medium tracking-wide text-foreground uppercase">
                {col.title}
              </h3>
              <ul className="mt-4 space-y-3">
                {col.links.map((l) => (
                  <li key={l.label}>
                    {l.external ? (
                      <a href={l.to} target="_blank" rel="noreferrer" className={linkClass}>
                        {l.label}
                      </a>
                    ) : (
                      <Link to={l.to} className={linkClass}>
                        {l.label}
                      </Link>
                    )}
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>
        <div className="flex flex-col items-center justify-between gap-3 border-t border-border py-6 text-sm text-muted-foreground sm:flex-row">
          <p>&copy; {year} dstream</p>
          <p className="font-mono text-xs tracking-wide uppercase">OSS webhook gateway</p>
        </div>
      </div>
    </footer>
  )
}
