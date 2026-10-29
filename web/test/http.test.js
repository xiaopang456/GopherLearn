import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  ApiError,
  apiBase,
  buildUrl,
  currentToken,
  isAbort,
  request,
  safeErrorMessage,
  setTokenProvider,
} from '../src/api/http.js'
import { installFetchStub, jsonResponse } from './helpers/transport.js'

const SECRET = 'eyJhbGciOi.bearer.secret-token-value'

beforeEach(() => {
  setTokenProvider(() => SECRET)
})

afterEach(() => {
  setTokenProvider(null)
  vi.unstubAllEnvs()
})

describe('apiBase / buildUrl', () => {
  it('uses the configured environment variable', () => {
    vi.stubEnv('VITE_API_BASE_URL', 'https://api.gopherlearn.test/')
    expect(apiBase()).toBe('https://api.gopherlearn.test')
    expect(buildUrl('/api/v1/me')).toBe('https://api.gopherlearn.test/api/v1/me')
  })

  it('falls back to same-origin when unset', () => {
    vi.stubEnv('VITE_API_BASE_URL', '')
    expect(apiBase()).toBe('')
    expect(buildUrl('/api/v1/sessions')).toBe('/api/v1/sessions')
  })

  it('normalizes a path missing its leading slash', () => {
    vi.stubEnv('VITE_API_BASE_URL', 'http://host:1')
    expect(buildUrl('api/v1/me')).toBe('http://host:1/api/v1/me')
  })

  it('appends query parameters without duplicating the separator', () => {
    vi.stubEnv('VITE_API_BASE_URL', '')
    expect(buildUrl('/api/v1/sessions', { page: 2, q: 'a b' })).toBe(
      '/api/v1/sessions?page=2&q=a+b',
    )
    expect(buildUrl('/api/v1/sessions?x=1', { page: 2 })).toBe(
      '/api/v1/sessions?x=1&page=2',
    )
  })
})

describe('credential placement', () => {
  it('sends the bearer token in a header and keeps it out of the URL', async () => {
    vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
    const { calls } = installFetchStub(jsonResponse({ data: { id: '7' }, error: null }))

    await request('/api/v1/me')

    expect(calls).toHaveLength(1)
    expect(calls[0].url).toBe('https://api.test/api/v1/me')
    expect(calls[0].url).not.toContain(SECRET)
    expect(calls[0].url).not.toContain('token')
    expect(calls[0].init.headers.authorization).toBe(`Bearer ${SECRET}`)
  })

  it('omits the authorization header when there is no token', async () => {
    setTokenProvider(() => null)
    const { calls } = installFetchStub(jsonResponse({ data: {}, error: null }))

    await request('/api/v1/auth/login', { method: 'POST', token: null, body: {} })

    expect(calls[0].init.headers.authorization).toBeUndefined()
    expect(calls[0].url).not.toContain('?')
  })

  it('currentToken ignores an empty provider result', () => {
    setTokenProvider(() => '')
    expect(currentToken()).toBe(null)
  })
})

describe('response envelope', () => {
  it('unwraps data on success', async () => {
    installFetchStub(jsonResponse({ data: { email: 'a@b.test' }, error: null }))
    await expect(request('/api/v1/me')).resolves.toEqual({ email: 'a@b.test' })
  })

  it('throws an ApiError carrying the machine code', async () => {
    installFetchStub(
      jsonResponse(
        { data: null, error: { code: 'invalid_credentials', message: 'bad' } },
        { status: 401 },
      ),
    )
    const error = await request('/api/v1/auth/login', { body: {} }).catch((e) => e)
    expect(error).toBeInstanceOf(ApiError)
    expect(error.code).toBe('invalid_credentials')
    expect(error.status).toBe(401)
  })

  it('treats a 200 that carries an error as a failure', async () => {
    installFetchStub(
      jsonResponse({ data: null, error: { code: 'not_found', message: 'gone' } }),
    )
    const error = await request('/api/v1/sessions/x').catch((e) => e)
    expect(error.code).toBe('not_found')
  })

  it('never renders an unparseable body', async () => {
    installFetchStub(
      jsonResponse('<html>500 Internal Server Error /var/lib/secrets</html>', {
        status: 500,
      }),
    )
    const error = await request('/api/v1/me').catch((e) => e)
    expect(error.message).not.toContain('/var/lib/secrets')
    expect(error.message).not.toContain('<html>')
    expect(error.code).toBe('http_500')
  })

  it('maps a rejected fetch to a network error', async () => {
    installFetchStub(new TypeError('Failed to fetch'))
    const error = await request('/api/v1/me').catch((e) => e)
    expect(error).toBeInstanceOf(ApiError)
    expect(error.code).toBe('network_error')
  })

  it('returns null when a caller expects no body', async () => {
    installFetchStub(jsonResponse(null, { status: 204 }))
    await expect(
      request('/api/v1/sessions/1', { method: 'DELETE', expectData: false }),
    ).resolves.toBe(null)
  })
})

describe('safeErrorMessage', () => {
  const cases = [
    ['invalid_request', '邮箱格式不正确'],
    ['invalid_credentials', '邮箱或密码不正确'],
    ['email_taken', '该邮箱已被注册'],
    ['not_found', '内容不存在'],
    ['rate_limited', '请求过于频繁'],
  ]

  it.each(cases)('passes through the whitelisted code %s', (code, message) => {
    expect(safeErrorMessage({ code, message })).toBe(message)
  })

  it.each([
    'internal_error',
    'db_error',
    'panic',
    'sql_error',
    'validation_failed_nested',
    '',
  ])('hides the message for non-whitelisted code "%s"', (code) => {
    const leaked = 'connect tcp 10.0.3.7:3306: connection refused'
    expect(safeErrorMessage({ code, message: leaked })).not.toContain('10.0.3.7')
  })

  it('caps an accepted message so a verbose server cannot flood the panel', () => {
    const long = 'x'.repeat(900)
    expect(safeErrorMessage({ code: 'invalid_request', message: long }).length).toBe(300)
  })

  it('isAbort recognises only cancellation', () => {
    expect(isAbort(null)).toBe(false)
    expect(isAbort(new Error('x'))).toBe(false)
    expect(isAbort({ name: 'AbortError' })).toBe(true)
    expect(isAbort({ code: 'aborted' })).toBe(true)
  })
})
