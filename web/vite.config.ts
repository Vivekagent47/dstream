import { defineConfig } from 'vite'
import { devtools } from '@tanstack/devtools-vite'

import { tanstackStart } from '@tanstack/react-start/plugin/vite'

import viteReact from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { nitro } from 'nitro/vite'

const apiTarget = process.env.DSTREAM_API_URL || 'http://localhost:8080'

const config = defineConfig({
  resolve: { tsconfigPaths: true },
  server: {
    proxy: {
      '/api': { target: apiTarget, changeOrigin: true, ws: true },
      '/admin': { target: apiTarget, changeOrigin: true },
      '/e': { target: apiTarget, changeOrigin: true },
      '/healthz': apiTarget,
      '/readyz': apiTarget,
    },
  },
  plugins: [
    devtools(),
    nitro({ rollupConfig: { external: [/^@sentry\//] } }),
    tailwindcss(),
    tanstackStart(),
    viteReact(),
  ],
})

export default config
