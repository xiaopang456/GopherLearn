import { fileURLToPath, URL } from 'node:url'

import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

/** Loopback origin of the local Go API. Override with VITE_DEV_API_ORIGIN. */
const DEV_API_ORIGIN = process.env.VITE_DEV_API_ORIGIN || 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 5273,
    strictPort: false,
    // Dev-server only: `vite build` never sees this, so a production bundle
    // still calls same-origin `/api`. Proxying keeps the browser on one origin,
    // which means the Go server needs no CORS allowance at all.
    proxy: {
      '/api': {
        target: DEV_API_ORIGIN,
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: 'jsdom',
    globals: false,
    setupFiles: ['./test/setup.js'],
    css: false,
    include: ['test/**/*.test.js'],
  },
})
