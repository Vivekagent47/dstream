import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vitest/config'

// A standalone config, deliberately NOT reusing vite.config.ts.
//
// The app's Vite config loads nitro and tanstackStart, which start a dev
// server and pull React into the module graph. Under `vitest run` that made
// the suite pass and then hang — "something prevents Vite server from
// exiting" — and surfaced a spurious `ReferenceError: module is not defined`
// from React's CJS entry being evaluated in a node context. None of it is
// needed to test pure modules.
//
// If component tests ever land here, they need jsdom and the React plugin;
// add a second project rather than re-pointing this one, so plain logic tests
// keep starting instantly.
export default defineConfig({
  resolve: {
    alias: {
      '#': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
  },
})
