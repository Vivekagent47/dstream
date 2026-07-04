import { lazy, Suspense } from 'react'
import { HeadContent, Scripts, createRootRouteWithContext } from '@tanstack/react-router'
import type { QueryClient } from '@tanstack/react-query'
import Footer from '../components/Footer'
import Header from '../components/Header'

import appCss from '../styles.css?url'

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
      <body className="bg-background font-sans text-foreground [overflow-wrap:anywhere] antialiased selection:bg-primary/20">
        <Header />
        {children}
        <Footer />
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
