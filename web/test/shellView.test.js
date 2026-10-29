import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory } from 'vue-router'

import ShellView from '../src/views/ShellView.vue'
import { createAppRouter } from '../src/router/index.js'
import { authStore } from '../src/auth/store.js'
import { jsonResponse } from './helpers/transport.js'

/**
 * A token the server no longer accepts cannot be detected by the router guard,
 * which only runs on navigation. `ShellView` is the one place that validates a
 * restored token, so it has to both clear the session and move the user — a
 * silent sign-out would leave a page that 401s on every action.
 */

let routes
let calls

function installFetch(handlerFor) {
  globalThis.fetch = async (url, init = {}) => {
    const path = new URL(String(url), 'https://api.test').pathname
    calls.push({ path, method: init.method ?? 'GET' })
    return handlerFor(path)
  }
}

async function mountShell(startAt = '/memory', handlerFor = () => jsonResponse({ data: {}, error: null })) {
  authStore.adopt('tok.shell', { id: '1', email: 'who@example.test', displayName: '' })
  const router = createAppRouter({ authStore, history: createMemoryHistory() })
  await router.push(startAt)
  await router.isReady()

  installFetch(handlerFor)
  routes = router
  const app = mount(ShellView, {
    attachTo: document.body,
    global: {
      plugins: [router],
      stubs: { RouterView: { template: '<div data-test="outlet" />' } },
    },
  })
  await flushPromises()
  return app
}

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  calls = []
  authStore.signOut()
})

const UNAUTHORIZED = () =>
  jsonResponse(
    { data: null, error: { code: 'unauthorized', message: 'token has expired' } },
    { status: 401 },
  )

describe('a token the server rejects', () => {
  it('is cleared from memory and storage', async () => {
    const app = await mountShell('/memory', UNAUTHORIZED)

    expect(authStore.isAuthenticated()).toBe(false)
    expect(authStore.getToken()).toBe('')
    expect(localStorage.getItem('gopherlearn.auth.v1')).toBeNull()
    app.unmount()
  })

  it('navigates to the sign-in screen instead of staying on a dead page', async () => {
    const app = await mountShell('/memory', UNAUTHORIZED)

    expect(routes.currentRoute.value.name).toBe('login')
    app.unmount()
  })

  it('preserves the address the user actually asked for', async () => {
    const app = await mountShell('/chat/s-42', UNAUTHORIZED)

    expect(routes.currentRoute.value.name).toBe('login')
    expect(routes.currentRoute.value.query.redirect).toBe('/chat/s-42')
    app.unmount()
  })

  it('reacts to the machine code even when the status is not 401', async () => {
    const app = await mountShell(
      '/knowledge',
      () =>
        jsonResponse(
          { data: null, error: { code: 'unauthorized', message: 'revoked' } },
          { status: 403 },
        ),
    )

    expect(authStore.isAuthenticated()).toBe(false)
    expect(routes.currentRoute.value.name).toBe('login')
    app.unmount()
  })

  it('drops the profile name from the header', async () => {
    const app = await mountShell('/memory', UNAUTHORIZED)

    expect(app.text()).not.toContain('who@example.test')
    app.unmount()
  })
})

describe('a healthy session', () => {
  it('stays put and adopts the profile the server reports', async () => {
    const app = await mountShell(
      '/memory',
      () =>
        jsonResponse({
          data: { id: '1', email: 'fresh@example.test', display_name: '新昵称' },
          error: null,
        }),
    )

    expect(routes.currentRoute.value.name).toBe('memory')
    expect(authStore.isAuthenticated()).toBe(true)
    expect(app.text()).toContain('fresh@example.test')
    app.unmount()
  })

  it('calls /api/v1/me exactly once at boot', async () => {
    const app = await mountShell('/memory', () =>
      jsonResponse({ data: { email: 'a@b.test' }, error: null }),
    )

    expect(calls.filter((call) => call.path === '/api/v1/me')).toHaveLength(1)
    app.unmount()
  })
})

describe('a transport failure is not a sign-out', () => {
  it('keeps the token and the current route when the network is down', async () => {
    const app = await mountShell('/memory', () => {
      throw new TypeError('Failed to fetch')
    })

    expect(authStore.isAuthenticated()).toBe(true)
    expect(authStore.getToken()).toBe('tok.shell')
    expect(routes.currentRoute.value.name).toBe('memory')
    app.unmount()
  })

  it('keeps the token when the server fails for an unrelated reason', async () => {
    const app = await mountShell(
      '/learning',
      () =>
        jsonResponse(
          { data: null, error: { code: 'internal_error', message: 'db down' } },
          { status: 500 },
        ),
    )

    expect(authStore.isAuthenticated()).toBe(true)
    expect(routes.currentRoute.value.name).toBe('learning')
    app.unmount()
  })
})
