import { lazy, Suspense } from 'react'
import {
  HeadContent,
  Scripts,
  createRootRouteWithContext,
  useRouterState,
} from '@tanstack/react-router'
import type { QueryClient } from '@tanstack/react-query'
import Footer from '../components/Footer'
import Header from '../components/Header'
import { AppSidebar } from '../components/app-sidebar'
import { TopBar } from '../components/TopBar'
import { SidebarInset, SidebarProvider } from '../components/ui/sidebar'

import appCss from '../styles.css?url'
import { Toaster } from '#/components/ui/sonner'

// Devtools are dev-only — kept out of the prod bundle. import.meta.env.DEV
// is statically true under `vite dev` and statically false under `vite
// build`, so this Lazy + null branch dead-code-eliminates the devtools
// modules + their ~200KB of code in production.
const Devtools = import.meta.env.DEV
  ? lazy(() =>
      import('@tanstack/react-devtools').then(async (m) => {
        const routerMod = await import('@tanstack/react-router-devtools')
        return {
          default: function DevtoolsWrapper() {
            return (
              <m.TanStackDevtools
                config={{ position: 'bottom-right' }}
                plugins={[
                  {
                    name: 'Tanstack Router',
                    render: <routerMod.TanStackRouterDevtoolsPanel />,
                  },
                ]}
              />
            )
          },
        }
      }),
    )
  : null

export interface RouterContext {
  queryClient: QueryClient
}

export const Route = createRootRouteWithContext<RouterContext>()({
  head: () => ({
    meta: [
      {
        charSet: 'utf-8',
      },
      {
        name: 'viewport',
        content: 'width=device-width, initial-scale=1',
      },
      {
        title: 'dstream — webhook IDE',
      },
    ],
    links: [
      {
        rel: 'stylesheet',
        href: appCss,
      },
      {
        rel: 'icon',
        type: 'image/svg+xml',
        href: '/favicon.svg',
      },
    ],
  }),
  shellComponent: RootDocument,
})

function RootDocument({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        <HeadContent />
      </head>
      <body className="bg-background font-sans [overflow-wrap:anywhere] text-foreground antialiased selection:bg-primary/20">
        <Shell>{children}</Shell>
        <Toaster />
        {Devtools && (
          <Suspense fallback={null}>
            <Devtools />
          </Suspense>
        )}
        <Scripts />
      </body>
    </html>
  )
}

// Shell picks the chrome for the current route:
//   - chromeless (auth + onboarding): bare, the page owns the whole screen
//   - home ('/'): marketing Header + Footer (the "Dashboard" CTA lives here)
//   - everything else (the logged-in app): the collapsible Sidebar
// Pathname-based, same approach Header/Footer already use — SSR renders the
// request path so there's no hydration flip.
function Shell({ children }: { children: React.ReactNode }) {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const isChromeless =
    pathname === '/login' ||
    pathname === '/auth/verify' ||
    pathname === '/orgs/new' ||
    pathname.startsWith('/invites/')

  if (isChromeless) {
    return <>{children}</>
  }

  if (pathname === '/') {
    return (
      <>
        <Header />
        {children}
        <Footer />
      </>
    )
  }

  return (
    <SidebarProvider>
      <AppSidebar />
      <SidebarInset>
        <TopBar>{children}</TopBar>
      </SidebarInset>
    </SidebarProvider>
  )
}
