import { defineConfig } from 'vite'
import { devtools } from '@tanstack/devtools-vite'

import { tanstackStart } from '@tanstack/react-start/plugin/vite'

import viteReact from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { nitro } from 'nitro/vite'

const apiTarget = process.env.DSTREAM_API_URL || 'http://localhost:8080'

// The dev server is Nitro (via TanStack Start); it handles requests before
// Vite's `server.proxy` middleware, so /api etc. must go through Nitro's own
// devProxy — otherwise they fall through to the SPA catch-all and 404.
const config = defineConfig({
  resolve: { tsconfigPaths: true },
  plugins: [
    devtools(),
    nitro({
      rollupConfig: { external: [/^@sentry\//] },
      // routeRules proxies run inside nitro's router and reliably forward
      // subpaths via the `**` splat. devProxy entries do NOT match subpaths in
      // this nitro build (verified: /admin/overview fell through to the SPA
      // catch-all and 404'd), so anything with subpaths must live here.
      // `/api/**` is also nitro-reserved and can only be claimed this way.
      routeRules: {
        '/api/**': { proxy: { to: `${apiTarget}/api/**` } },
        // Root-level admin surface: JSON endpoints (/admin/overview, /admin/orgs,
        // /admin/queues, /admin/system). All super-admin gated on the Go side.
        '/admin/**': { proxy: { to: `${apiTarget}/admin/**` } },
      },
      devProxy: {
        // NOTE: these lack subpaths in practice (bare health checks) or are hit
        // directly against the Go server. /e, /healthz, /readyz likely need the
        // same routeRules treatment if ever called with subpaths via the dev
        // server — out of scope for this change.
        '/e': { target: apiTarget, changeOrigin: true },
        '/healthz': { target: apiTarget, changeOrigin: true },
        '/readyz': { target: apiTarget, changeOrigin: true },
      },
    }),
    tailwindcss(),
    tanstackStart(),
    viteReact(),
  ],
})

export default config
