import * as React from 'react'
import { Link, useNavigate, useRouterState } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Activity,
  Building2,
  ChevronsUpDown,
  Inbox,
  KeyRound,
  Link2,
  LogOut,
  Mail,
  ScrollText,
  Send,
  Users,
} from 'lucide-react'

import { api, qk } from '#/lib/api'
import { OrgSwitcher } from '#/components/OrgSwitcher'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '#/components/ui/dropdown-menu'
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
} from '#/components/ui/sidebar.tsx'

// dstream's real nav. `end` marks links whose active state must be exact
// (so /events doesn't stay highlighted while on /events/$id — actually we
// WANT it highlighted there, so /events is a prefix match; settings links
// are exact). Icons are lucide.
const PLATFORM = [
  { label: 'Sources', to: '/sources', icon: Inbox, prefix: true },
  { label: 'Destinations', to: '/destinations', icon: Send, prefix: true },
  { label: 'Connections', to: '/connections', icon: Link2, prefix: true },
  { label: 'Events', to: '/events', icon: Activity, prefix: true },
] as const

const SETTINGS = [
  { label: 'Organization', to: '/settings/org', icon: Building2 },
  { label: 'Members', to: '/settings/members', icon: Users },
  { label: 'Invites', to: '/settings/invites', icon: Mail },
  { label: 'API keys', to: '/settings/api-keys', icon: KeyRound },
  { label: 'Audit log', to: '/settings/audit', icon: ScrollText },
] as const

// The dstream diamond mark — same path as the marketing Header/Footer
// wordmark and the favicon. Inherits currentColor.
function LogoMark() {
  return (
    <svg viewBox="0 0 16 16" className="h-4 w-4" aria-hidden="true">
      <path
        fill="currentColor"
        d="M2 4.5 8 1l6 3.5v7L8 15l-6-3.5v-7Zm6 1.2L4.7 7.6 8 9.5l3.3-1.9L8 5.7Z"
      />
    </svg>
  )
}

export function AppSidebar({ ...props }: React.ComponentProps<typeof Sidebar>) {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const isActive = (to: string, prefix?: boolean) =>
    prefix ? pathname === to || pathname.startsWith(to + '/') : pathname === to

  return (
    <Sidebar collapsible="icon" {...props}>
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton asChild size="lg" tooltip="Back to home">
              <Link to="/">
                <span className="flex aspect-square size-8 items-center justify-center rounded-md bg-sidebar-primary text-sidebar-primary-foreground">
                  <LogoMark />
                </span>
                <span className="grid flex-1 text-left text-sm leading-tight">
                  <span className="truncate font-semibold">dstream</span>
                  <span className="truncate text-xs text-muted-foreground">webhook gateway</span>
                </span>
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
        <OrgSwitcher />
      </SidebarHeader>

      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupLabel>Platform</SidebarGroupLabel>
          <SidebarMenu>
            {PLATFORM.map((item) => (
              <SidebarMenuItem key={item.to}>
                <SidebarMenuButton asChild isActive={isActive(item.to, item.prefix)} tooltip={item.label}>
                  <Link to={item.to}>
                    <item.icon />
                    <span>{item.label}</span>
                  </Link>
                </SidebarMenuButton>
              </SidebarMenuItem>
            ))}
          </SidebarMenu>
        </SidebarGroup>

        <SidebarGroup>
          <SidebarGroupLabel>Settings</SidebarGroupLabel>
          <SidebarMenu>
            {SETTINGS.map((item) => (
              <SidebarMenuItem key={item.to}>
                <SidebarMenuButton asChild isActive={isActive(item.to)} tooltip={item.label}>
                  <Link to={item.to}>
                    <item.icon />
                    <span>{item.label}</span>
                  </Link>
                </SidebarMenuButton>
              </SidebarMenuItem>
            ))}
          </SidebarMenu>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter>
        <NavUser />
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  )
}

// NavUser: email + sign-out at the foot of the sidebar. Logout bumps the
// server session epoch (logout-all) and clears the cookie; onSuccess we drop
// all cached queries and send the user to the homepage.
function NavUser() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { data: me } = useQuery({ queryKey: qk.me(), queryFn: api.me })
  const email = me?.user?.email ?? '—'

  const logout = useMutation({
    mutationFn: api.logout,
    onSuccess: () => {
      qc.clear()
      navigate({ to: '/' })
    },
  })

  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <SidebarMenuButton
              size="lg"
              className="data-[state=open]:bg-sidebar-accent data-[state=open]:text-sidebar-accent-foreground"
            >
              <span className="flex aspect-square size-8 items-center justify-center rounded-lg bg-sidebar-primary text-sidebar-primary-foreground text-xs font-semibold">
                {email.charAt(0).toUpperCase()}
              </span>
              <span className="grid flex-1 text-left text-sm leading-tight">
                <span className="truncate font-medium">{email}</span>
              </span>
              <ChevronsUpDown className="ml-auto size-4" />
            </SidebarMenuButton>
          </DropdownMenuTrigger>
          <DropdownMenuContent className="w-(--radix-dropdown-menu-trigger-width) min-w-56" side="top" align="start">
            <DropdownMenuItem onClick={() => logout.mutate()} disabled={logout.isPending}>
              <LogOut className="mr-2 h-4 w-4" /> Sign out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem>
    </SidebarMenu>
  )
}
