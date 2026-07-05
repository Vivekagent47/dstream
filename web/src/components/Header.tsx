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

function Wordmark() {
  return (
    <Link
      to="/"
      className="inline-flex items-center gap-2 text-[15px] font-bold tracking-tight text-foreground no-underline"
    >
      <span className="flex h-6 w-6 items-center justify-center rounded-md bg-foreground text-background">
        <svg viewBox="0 0 16 16" className="h-3.5 w-3.5" aria-hidden="true">
          <path
            fill="currentColor"
            d="M2 4.5 8 1l6 3.5v7L8 15l-6-3.5v-7Zm6 1.2L4.7 7.6 8 9.5l3.3-1.9L8 5.7Z"
          />
        </svg>
      </span>
      dstream
    </Link>
  )
}

export default function Header() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const me = useQuery({ queryKey: qk.me(), queryFn: api.me, retry: false })
  const authed = !!me.data?.user

  // Hide nav links on public routes (signed-out): /login and /invites/$token
  // render the Header for the brand/theme toggle only — the nav links
  // would otherwise 401 when clicked from a signed-out context.
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const isPublicRoute =
    pathname === '/login' || pathname === '/auth/verify' || pathname.startsWith('/invites/')

  // Auth routes render full-bleed with their own branding — no site chrome.
  if (isPublicRoute) {
    return null
  }

  const logout = useMutation({
    mutationFn: api.logout,
    onSuccess: async () => {
      qc.clear()
      navigate({ to: '/' })
    },
  })

  return (
    <header className="sticky top-0 z-50 border-b border-border bg-background/80 backdrop-blur-md">
      <nav className="mx-auto flex h-16 max-w-285 items-center gap-6 px-6">
        <Wordmark />

        {authed && !isPublicRoute && <OrgSwitcher />}

        <div className="ml-auto flex items-center gap-2">
          <ThemeToggle />
          {authed && !isPublicRoute ? (
            <>
              <Link
                to="/sources"
                className="inline-flex items-center rounded-md bg-foreground px-4 py-2 text-sm font-semibold text-background no-underline shadow-sm transition hover:opacity-90"
              >
                Dashboard
              </Link>
              <DropdownMenu>
                <DropdownMenuTrigger
                  className="rounded-md p-2 text-muted-foreground transition hover:bg-muted hover:text-foreground"
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
                  <DropdownMenuItem onClick={() => logout.mutate()} disabled={logout.isPending}>
                    <LogOut className="mr-2 h-4 w-4" /> Sign out
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </>
          ) : (
            !isPublicRoute && (
              <Link
                to="/login"
                className="inline-flex items-center rounded-md bg-foreground px-4 py-2 text-sm font-semibold text-background no-underline shadow-sm transition hover:opacity-90"
              >
                Get started
              </Link>
            )
          )}
        </div>
      </nav>
    </header>
  )
}
