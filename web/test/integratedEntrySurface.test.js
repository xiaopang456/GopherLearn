import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory } from 'vue-router'

import ShellView from '../src/views/ShellView.vue'
import LoginView from '../src/views/LoginView.vue'
import { createAppRouter } from '../src/router/index.js'
import { authStore } from '../src/auth/store.js'
import { jsonResponse } from './helpers/transport.js'

/**
 * The integrated entry answers the mainline contract paths from its compatibility
 * surface and everything else from the original V2 router
 * (`hybrid/http.go:65-94`: `/auth/login`, `/me`, `/sessions…`, and the
 * `/knowledge/documents/{id}/source` read, with the rest passed through). A page in
 * the default navigation that asks for anything outside those two tables gets a
 * 404, and the legacy registration path is an explicit 501. So the rail's targets
 * and the shell's boot requests are part of the contract, not styling.
 *
 * Collection management and the legacy `/api/v1/memories` + `/api/v1/learning`
 * pages belong to the baseline entry (`cmd/gopherlearn`), which is a different
 * process against a different schema; they stay reachable by direct address for
 * that entry but are not offered here.
 */

const NAV_TARGETS = ['/chat', '/knowledge/documents', '/memory', '/learning']

let calls

function installFetch() {
  globalThis.fetch = async (url, init = {}) => {
    const path = new URL(String(url), 'https://api.test').pathname
    calls.push({ path, method: init.method ?? 'GET' })
    // A backend without knowledgeConfig.opsUsers does not route the monitor: 404, entry stays hidden.
    if (path.startsWith('/api/v1/ops/')) return new Response('404 page not found', { status: 404 })
    return jsonResponse({
      data: { id: '1', email: 'who@example.test', username: 'who', display_name: 'who' },
      error: null,
    })
  }
}

async function mountWith(component, startAt) {
  authStore.adopt('tok.surface', { id: '1', email: 'who@example.test', displayName: 'who' })
  const router = createAppRouter({ authStore, history: createMemoryHistory() })
  await router.push(startAt)
  await router.isReady()
  installFetch()
  const app = mount(component, {
    attachTo: document.body,
    global: {
      plugins: [router],
      stubs: { RouterView: { template: '<div data-test="outlet" />' } },
    },
  })
  await flushPromises()
  return { app, router }
}

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  calls = []
  authStore.signOut()
})

describe('the integrated boot sequence', () => {
  it('validates the token and asks for nothing else', async () => {
    const { app } = await mountWith(ShellView, '/chat')
    // This build has no processing-monitor entry, so the rail asks nothing beyond the token check.
    expect(calls.map((call) => call.path).sort()).toEqual(['/api/v1/me'])
    app.unmount()
  })

  it('never fetches a Collection list, which this backend does not serve', async () => {
    const { app } = await mountWith(ShellView, '/chat')
    const collections = calls.filter((call) => call.path.includes('collections'))
    expect(collections).toEqual([])
    app.unmount()
  })
})

describe('the default navigation', () => {
  it('offers exactly the pages the integrated backend serves', async () => {
    const { app, router } = await mountWith(ShellView, '/chat')
    const hrefs = app.findAll('.shell__rail a').map((link) => link.attributes('href'))
    expect(hrefs).toEqual(NAV_TARGETS)
    for (const href of hrefs) {
      const resolved = router.resolve(href)
      expect(resolved.matched.length, `${href} resolves to no route`).toBeGreaterThan(0)
    }
    app.unmount()
  })

  it('does not route the grounding note back to Collection management', async () => {
    const { app, router } = await mountWith(ShellView, '/chat')
    const documents = router.resolve({ name: 'documents' })
    expect(documents.href).toBe('/knowledge/documents')
    app.unmount()
  })
})

describe('the sign-in screen', () => {
  it('sends registration to the original V2 captcha contract', async () => {
    const { app } = await mountWith(LoginView, '/login')
    const hrefs = app.findAll('a').map((link) => link.attributes('href'))
    // The compatibility surface refuses the legacy registration with 501
    // (registration_not_adapted), so the screen offers the one registration there is,
    // at the address without a version suffix.
    expect(hrefs).toContain('/register')
    app.unmount()
  })
})
