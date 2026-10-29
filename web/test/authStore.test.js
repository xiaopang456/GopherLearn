import { afterEach, describe, expect, it, vi } from 'vitest'

import { AUTH_STORAGE_KEY, createAuthStore } from '../src/auth/store.js'
import { installFetchStub, jsonResponse } from './helpers/transport.js'

const PROFILE = { id: '42', email: 'learner@example.test', display_name: '学习者' }
const LOGIN_OK = { data: { token: 'tok.abc.def', user: PROFILE }, error: null }

function fakeStorage(initial = {}) {
  const map = new Map(Object.entries(initial))
  return {
    map,
    getItem: (key) => (map.has(key) ? map.get(key) : null),
    setItem: (key, value) => map.set(key, String(value)),
    removeItem: (key) => map.delete(key),
  }
}

/** Wire an isolated store (never the app singleton) against a routed fetch. */
function boot({ saved = {}, routes = {} } = {}) {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  const storage = fakeStorage(saved)
  const store = createAuthStore({ storage })
  const handlers = {
    '/api/v1/auth/login': jsonResponse(LOGIN_OK),
    '/api/v1/auth/register': jsonResponse(LOGIN_OK),
    '/api/v1/me': jsonResponse({ data: PROFILE, error: null }),
    ...routes,
  }
  const { calls } = installFetchStub(
    (url) => handlers[new URL(String(url), 'https://api.test').pathname],
  )
  return { store, storage, calls }
}

afterEach(() => {
  vi.unstubAllEnvs()
})

describe('sign-in', () => {
  it('stores the bearer token and the public profile', async () => {
    const { store, storage } = boot()

    const user = await store.login({
      email: 'learner@example.test',
      password: 'abcdef12',
    })

    expect(store.isAuthenticated()).toBe(true)
    expect(store.getToken()).toBe('tok.abc.def')
    expect(user.email).toBe('learner@example.test')
    expect(user.displayName).toBe('学习者')
    expect(JSON.parse(storage.map.get(AUTH_STORAGE_KEY)).token).toBe('tok.abc.def')
  })

  it('registers with the snake_case body the contract names', async () => {
    const { store, calls } = boot()

    await store.register({
      email: 'new@example.test',
      password: 'abcdef12',
      displayName: '新人',
    })

    expect(calls[0].url).toBe('https://api.test/api/v1/auth/register')
    expect(JSON.parse(calls[0].init.body)).toEqual({
      email: 'new@example.test',
      password: 'abcdef12',
      display_name: '新人',
    })
    expect(store.isAuthenticated()).toBe(true)
  })

  it('sends no authorization header on the anonymous auth endpoints', async () => {
    const { store, calls } = boot()

    await store.login({ email: 'a@b.test', password: 'abcdef12' })

    expect(calls[0].init.headers.authorization).toBeUndefined()
    expect(calls[0].url).not.toContain('tok.abc.def')
  })

  it('refuses an authenticated state when the response carries no token', async () => {
    const { store } = boot({
      routes: {
        '/api/v1/auth/login': jsonResponse({ data: { user: PROFILE }, error: null }),
      },
    })

    await expect(
      store.login({ email: 'a@b.test', password: 'abcdef12' }),
    ).rejects.toThrow('服务未返回有效的登录凭证。')
    expect(store.isAuthenticated()).toBe(false)
  })
})

describe('registration is two calls, per the contract', () => {
  const NO_TOKEN = { data: PROFILE, error: null }

  it('signs the new account in with a follow-up login', async () => {
    const { store, calls } = boot({
      routes: { '/api/v1/auth/register': jsonResponse(NO_TOKEN) },
    })

    const user = await store.register({
      email: 'new@example.test',
      password: 'abcdef12',
      displayName: '新人',
    })

    expect(calls.map((call) => new URL(call.url).pathname)).toEqual([
      '/api/v1/auth/register',
      '/api/v1/auth/login',
    ])
    expect(store.isAuthenticated()).toBe(true)
    expect(store.getToken()).toBe('tok.abc.def')
    expect(user.email).toBe('learner@example.test')
  })

  it('still accepts a register response that does carry a token', async () => {
    const { store } = boot()

    await store.register({ email: 'a@b.test', password: 'abcdef12', displayName: '甲' })

    expect(store.isAuthenticated()).toBe(true)
  })

  it('does not attempt a login when the account was never created', async () => {
    const { store, calls } = boot({
      routes: {
        '/api/v1/auth/register': jsonResponse(
          { data: null, error: { code: 'email_taken', message: '该邮箱已被注册' } },
          { status: 409 },
        ),
      },
    })

    await expect(
      store.register({ email: 'a@b.test', password: 'abcdef12', displayName: '甲' }),
    ).rejects.toMatchObject({ code: 'email_taken' })

    expect(calls).toHaveLength(1)
    expect(store.isAuthenticated()).toBe(false)
  })

  it('reports a created-but-not-signed-in account distinctly', async () => {
    const { store } = boot({
      routes: {
        '/api/v1/auth/register': jsonResponse(NO_TOKEN),
        '/api/v1/auth/login': jsonResponse(
          { data: null, error: { code: 'invalid_credentials', message: 'no' } },
          { status: 401 },
        ),
      },
    })

    await expect(
      store.register({ email: 'a@b.test', password: 'abcdef12', displayName: '甲' }),
    ).rejects.toMatchObject({
      code: 'login_after_register',
      message: '账号已创建，请改用登录页登入。',
    })
    expect(store.isAuthenticated()).toBe(false)
  })

  it('passes a genuine transport failure through unchanged', async () => {
    const { store } = boot({
      routes: {
        '/api/v1/auth/register': jsonResponse(NO_TOKEN),
        '/api/v1/auth/login': new TypeError('Failed to fetch'),
      },
    })

    await expect(
      store.register({ email: 'a@b.test', password: 'abcdef12', displayName: '甲' }),
    ).rejects.toMatchObject({ code: 'network_error' })
  })
})

describe('sign-out', () => {
  it('clears the token from memory and from storage', async () => {
    const { store, storage } = boot()
    await store.login({ email: 'a@b.test', password: 'abcdef12' })
    expect(storage.map.has(AUTH_STORAGE_KEY)).toBe(true)

    store.signOut()

    expect(store.getToken()).toBe('')
    expect(store.isAuthenticated()).toBe(false)
    expect(store.state.user.email).toBe('')
    expect(storage.map.has(AUTH_STORAGE_KEY)).toBe(false)
  })

  it('survives a storage layer that throws', async () => {
    const locked = {
      getItem: () => null,
      setItem: () => {
        throw new Error('QuotaExceededError')
      },
      removeItem: () => {
        throw new Error('SecurityError')
      },
    }
    const store = createAuthStore({ storage: locked })
    store.adopt('tok.in-memory', PROFILE)

    expect(store.isAuthenticated()).toBe(true)
    expect(() => store.signOut()).not.toThrow()
    expect(store.isAuthenticated()).toBe(false)
  })
})

describe('restore from storage', () => {
  it('adopts a stored token so a reload stays signed in', () => {
    const { store } = boot({
      saved: {
        [AUTH_STORAGE_KEY]: JSON.stringify({
          token: 'tok.saved',
          user: { email: 'a@b.test' },
        }),
      },
    })

    expect(store.restore()).toBe(true)
    expect(store.getToken()).toBe('tok.saved')
    expect(store.state.user.email).toBe('a@b.test')
  })

  it('treats an absent entry as signed out', () => {
    const { store } = boot()
    expect(store.restore()).toBe(false)
    expect(store.getToken()).toBe('')
  })

  it('discards a corrupt entry', () => {
    const { store, storage } = boot({ saved: { [AUTH_STORAGE_KEY]: 'not-json{' } })

    expect(store.restore()).toBe(false)
    expect(store.isAuthenticated()).toBe(false)
    expect(storage.map.has(AUTH_STORAGE_KEY)).toBe(false)
  })

  it('discards an entry with an empty token', async () => {
    const { store, calls } = boot({
      saved: { [AUTH_STORAGE_KEY]: JSON.stringify({ token: '' }) },
    })

    expect(store.restore()).toBe(false)
    await store.refreshProfile()
    expect(calls).toHaveLength(0)
  })

  it('drops the session when the server rejects the stored token', async () => {
    const { store, storage } = boot({
      saved: { [AUTH_STORAGE_KEY]: JSON.stringify({ token: 'tok.old' }) },
      routes: {
        '/api/v1/me': jsonResponse(
          { data: null, error: { code: 'unauthorized', message: 'token expired' } },
          { status: 401 },
        ),
      },
    })
    store.restore()

    await expect(store.refreshProfile()).rejects.toMatchObject({ code: 'unauthorized' })
    expect(store.isAuthenticated()).toBe(false)
    expect(storage.map.has(AUTH_STORAGE_KEY)).toBe(false)
  })

  it('keeps the session when the failure is only a network problem', async () => {
    const { store } = boot({
      saved: { [AUTH_STORAGE_KEY]: JSON.stringify({ token: 'tok.good' }) },
      routes: { '/api/v1/me': new TypeError('Failed to fetch') },
    })
    store.restore()

    await expect(store.refreshProfile()).rejects.toThrow()
    expect(store.isAuthenticated()).toBe(true)
    expect(store.getToken()).toBe('tok.good')
  })
})
