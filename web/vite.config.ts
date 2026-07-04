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
      // /api/** is a nitro-reserved namespace, so devProxy can't claim it —
      // a routeRules proxy runs inside nitro's router and does. Everything
      // else goes through devProxy.
      routeRules: {
        '/api/**': { proxy: { to: `${apiTarget}/api/**` } },
      },
      devProxy: {
        '/admin': { target: apiTarget, changeOrigin: true },
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
