import { Link, useNavigate, useRouterState } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { LogOut, Settings, User as UserIcon } from 'lucide-react'

import { api, qk } from '#/lib/api'
import { OrgSwitcher } from '#/components/OrgSwitcher'
import ThemeToggle from '#/components/ThemeToggle'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '#/components/ui/dropdown-menu'

export default function Header() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const me = useQuery({ queryKey: qk.me(), queryFn: api.me, retry: false })
  const authed = !!me.data?.user

  // Hide nav links on public routes (signed-out): /login and /invites/$token
  // render the Header for the brand/theme toggle only — the nav links
  // would otherwise 401 when clicked from a signed-out context. We read
  // the pathname directly off the router state to avoid a useMatch
  // round-trip during hydration.
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const isPublicRoute = pathname === '/login' || pathname.startsWith('/invites/')

  const logout = useMutation({
    mutationFn: api.logout,
    onSuccess: async () => {
      // Drop every cached query — orgs, events, members, audit. Then send
      // the user to /login. Without removeQueries, react-query would
      // happily refetch protected endpoints with the now-empty cookie and
      // populate the cache with 401 errors.
      qc.clear()
      navigate({ to: '/login' })
    },
  })

  return (
    <header className="sticky top-0 z-50 border-b border-[var(--line)] bg-[var(--header-bg)] px-4 backdrop-blur-lg">
      <nav className="page-wrap flex flex-wrap items-center gap-x-3 gap-y-2 py-3 sm:py-4">
        <Link
          to="/"
          className="inline-flex items-center gap-2 text-base font-semibold tracking-tight text-[var(--sea-ink)] no-underline"
        >
          dstream
        </Link>

        {authed && !isPublicRoute && <OrgSwitcher />}

        {!isPublicRoute && (
          <div className="order-3 flex w-full flex-wrap items-center gap-x-4 gap-y-1 pb-1 text-sm font-semibold sm:order-none sm:w-auto sm:flex-nowrap sm:pb-0">
            <Link to="/" className="nav-link" activeProps={{ className: 'nav-link is-active' }}>
              Home
            </Link>
            {authed && (
              <>
                <Link
                  to="/sources"
                  className="nav-link"
                  activeProps={{ className: 'nav-link is-active' }}
                >
                  Sources
                </Link>
                <Link
                  to="/events"
                  className="nav-link"
                  activeProps={{ className: 'nav-link is-active' }}
                >
                  Events
                </Link>
              </>
            )}
            {!authed && (
              <Link
                to="/login"
                className="nav-link"
                activeProps={{ className: 'nav-link is-active' }}
              >
                Sign in
              </Link>
            )}
          </div>
        )}

        <div className="ml-auto flex items-center gap-1.5 sm:gap-2">
          <ThemeToggle />
          {authed && !isPublicRoute && (
            <DropdownMenu>
              <DropdownMenuTrigger
                className="rounded-xl p-2 text-[var(--sea-ink-soft)] transition hover:bg-[var(--link-bg-hover)] hover:text-[var(--sea-ink)]"
                aria-label="Account menu"
              >
                <UserIcon className="h-5 w-5" />
              </DropdownMenuTrigger>
              <DropdownMenuContent className="w-56">
                <div className="px-2 py-1.5 text-xs text-muted-foreground">
                  {me.data?.user?.email}
                </div>
                <DropdownMenuSeparator />
                <DropdownMenuItem onClick={() => navigate({ to: '/settings/org' })}>
                  <Settings className="mr-2 h-4 w-4" /> Settings
                </DropdownMenuItem>
                <DropdownMenuItem
                  onClick={() => logout.mutate()}
                  disabled={logout.isPending}
                >
                  <LogOut className="mr-2 h-4 w-4" /> Sign out
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          )}
          <a
            href="https://github.com/Vivekagent47/dstream"
            target="_blank"
            rel="noreferrer"
            className="rounded-xl p-2 text-[var(--sea-ink-soft)] transition hover:bg-[var(--link-bg-hover)] hover:text-[var(--sea-ink)]"
          >
            <span className="sr-only">GitHub</span>
            <svg viewBox="0 0 16 16" aria-hidden="true" width="22" height="22">
              <path
                fill="currentColor"
                d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.012 8.012 0 0 0 16 8c0-4.42-3.58-8-8-8z"
              />
            </svg>
          </a>
        </div>
      </nav>
    </header>
  )
}
