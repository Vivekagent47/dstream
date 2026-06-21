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

const THEME_INIT_SCRIPT = `(function(){try{var stored=window.localStorage.getItem('theme');var mode=(stored==='light'||stored==='dark'||stored==='auto')?stored:'auto';var prefersDark=window.matchMedia('(prefers-color-scheme: dark)').matches;var resolved=mode==='auto'?(prefersDark?'dark':'light'):mode;var root=document.documentElement;root.classList.remove('light','dark');root.classList.add(resolved);if(mode==='auto'){root.removeAttribute('data-theme')}else{root.setAttribute('data-theme',mode)}root.style.colorScheme=resolved;}catch(e){}})();`

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
        <script dangerouslySetInnerHTML={{ __html: THEME_INIT_SCRIPT }} />
        <HeadContent />
      </head>
      <body className="font-sans [overflow-wrap:anywhere] antialiased selection:bg-[rgba(79,184,178,0.24)]">
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
