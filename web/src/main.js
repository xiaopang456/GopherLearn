import { createApp } from 'vue'

import App from './App.vue'
import { authStore } from './auth/store.js'
import { createAppRouter } from './router/index.js'
import {
  createHttpMemoryAdapter,
  MemoryAdapterMode,
  resolveMemoryAdapterMode,
  setMemoryAdapter,
} from './api/memory.js'
import { createMockMemoryAdapter } from './api/memoryMock.js'

import './styles/base.css'
import './styles/controls.css'
import './styles/markdown.css'

// A stored token is adopted before the first navigation so the guard can tell
// "signed in" without waiting on the network. Profile freshness is confirmed
// lazily by the shell.
authStore.restore()

/**
 * Every build talks to the Go backend through the HTTP adapter. The bundled demo
 * adapter is reachable only by asking for it explicitly under the dev server
 * (`VITE_MEMORY_MOCK=on`), and the page labels those rows as demo data wherever
 * they appear. The rule itself lives in `resolveMemoryAdapterMode()` so it can be
 * pinned by a table of cases instead of by reading this file.
 */
const memoryMode = resolveMemoryAdapterMode(import.meta.env || {})
setMemoryAdapter(
  memoryMode === MemoryAdapterMode.Mock ? createMockMemoryAdapter() : createHttpMemoryAdapter(),
)

const app = createApp(App)
app.use(createAppRouter())
app.mount('#app')
