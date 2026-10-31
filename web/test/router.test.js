import { describe, expect, it } from 'vitest'
import { createMemoryHistory } from 'vue-router'

import { createAppRouter, appRoutes, legacyRoutes } from '../src/router/index.js'
import { safeRedirectTarget } from '../src/lib/redirect.js'

function makeStore(signedIn = false) {
  const state = { signedIn }
  return {
    state: { token: '', user: { email: '' } },
    get signedIn() {
      return state.signedIn
    },
    isAuthenticated: () => state.signedIn,
    signIn: () => {
      state.signedIn = true
    },
    signOut: () => {
      state.signedIn = false
    },
  }
}

function makeRouter(signedIn = false) {
  const store = makeStore(signedIn)
  const router = createAppRouter({ authStore: store, history: createMemoryHistory() })
  return { router, store }
}

describe('application routes require a session', () => {
  const protectedPaths = ['/chat', '/memory', '/knowledge/documents', '/learning', '/chat/s-1']

  it.each(protectedPaths)('%s redirects to the sign-in screen', async (path) => {
    const { router } = makeRouter(false)
    await router.push(path)
    await router.isReady()

    expect(router.currentRoute.value.name).toBe('login')
    expect(router.currentRoute.value.query.redirect).toBe(path)
  })

  it.each(protectedPaths)('%s renders when signed in', async (path) => {
    const { router } = makeRouter(true)
    await router.push(path)
    await router.isReady()

    expect(router.currentRoute.value.name).not.toBe('login')
    expect(router.currentRoute.value.matched.length).toBeGreaterThan(0)
  })

  it('re-checks the guard on every navigation, not only at boot', async () => {
    const { router, store } = makeRouter(true)
    await router.push('/memory')
    expect(router.currentRoute.value.name).toBe('memory')

    store.signOut()
    await router.push('/learning')
    expect(router.currentRoute.value.name).toBe('login')
    expect(router.currentRoute.value.query.redirect).toBe('/learning')
  })

  it('sends a signed-in visitor away from the guest screens', async () => {
    const { router } = makeRouter(true)
    await router.push('/login')
    expect(router.currentRoute.value.name).toBe('chat')

    await router.push('/register')
    expect(router.currentRoute.value.name).toBe('chat')
  })

  it('leaves unknown paths on the 404 screen rather than exposing routes', async () => {
    const { router } = makeRouter(false)
    await router.push('/definitely-not-a-page')
    expect(router.currentRoute.value.name).toBe('not-found')
  })
})

describe('the addresses this app used to publish', () => {
  const LEGACY = [
    // The registration page is the one screen a signed-in visitor is turned away from,
    // so the old address is checked with a signed-out visitor.
    ['/register/v2', '/register', 'register', false],
    ['/knowledge/v2-documents', '/knowledge/documents', 'documents', true],
    ['/memory/v2', '/memory', 'memory', true],
    ['/learning/v2', '/learning', 'learning', true],
  ]

  it.each(LEGACY)('%s lands on %s', async (oldPath, newPath, name, signedIn) => {
    const { router } = makeRouter(signedIn)
    await router.push(oldPath)
    await router.isReady()

    expect(router.currentRoute.value.path).toBe(newPath)
    expect(router.currentRoute.value.name).toBe(name)
  })

  it('keeps the document and the line range on the way to the source page', async () => {
    const { router } = makeRouter(true)
    await router.push('/knowledge/v2-documents/doc-7/source?version=2&start_line=5&end_line=9')
    await router.isReady()

    expect(router.currentRoute.value.path).toBe('/knowledge/documents/doc-7/source')
    expect(router.currentRoute.value.name).toBe('document-source')
    expect(router.currentRoute.value.params.documentId).toBe('doc-7')
    expect(router.currentRoute.value.query).toEqual({ version: '2', start_line: '5', end_line: '9' })
  })

  it('still asks for a session before the page behind the old address', async () => {
    const { router } = makeRouter(false)
    await router.push('/memory/v2')
    await router.isReady()

    expect(router.currentRoute.value.name).toBe('login')
    expect(router.currentRoute.value.query.redirect).toBe('/memory')
  })

  it('keeps the old addresses as redirects rather than as pages', () => {
    for (const route of legacyRoutes) {
      expect(route.redirect, route.path).toBeTruthy()
      expect(route.component, route.path).toBeUndefined()
    }
  })

  it('publishes no other address with a version suffix in it', () => {
    const published = appRoutes
      .filter((route) => !legacyRoutes.includes(route))
      .flatMap((route) => [route.path, ...(route.children ?? []).map((child) => child.path)])

    for (const path of published) {
      expect(path, path).not.toMatch(/v2/)
    }
  })
})

describe('post-login destination', () => {
  it('keeps an in-site path', () => {
    expect(safeRedirectTarget('/chat/s-1')).toBe('/chat/s-1')
    expect(safeRedirectTarget('/memory')).toBe('/memory')
  })

  it('falls back for anything that could leave the site', () => {
    for (const attempt of [
      'https://evil.test',
      '//evil.test',
      '/\\evil.test',
      'javascript:alert(1)',
      '/chat\nhttps://evil.test',
      'chat/session',
      '',
      undefined,
      null,
      42,
    ]) {
      expect(safeRedirectTarget(attempt)).toBe('/chat')
    }
  })
})
