/**
 * Thin HTTP layer for the JSON envelope defined in spec/API.md:
 * `{ "data": ..., "error": { "code", "message" } }`.
 *
 * Two rules are enforced here rather than at each call site:
 *  1. the bearer token travels in a header, never in a query string, so it
 *     cannot leak into access logs, the Address bar, or `Referer`;
 *  2. only the server's own `error.message` is surfaced. A proxy or a stack
 *     trace in a non-JSON body is reduced to a generic sentence, because a
 *     response we cannot parse may contain anything.
 */

const env = import.meta.env || {}

/** Configured once at boot so `http.js` stays free of an import cycle. */
let tokenProvider = () => null

/**
 * One place decides what a rejected credential means. The alternative — every
 * call site comparing `status === 401` itself — is how a session ends up half
 * signed out: the panel shows an error while the shell still looks logged in.
 */
let unauthorizedHandler = null

export function setUnauthorizedHandler(handler) {
  unauthorizedHandler = typeof handler === 'function' ? handler : null
}

/** A rejected bearer, as opposed to a wrong password on the sign-in form. */
export function isSessionRejected(error) {
  if (!error) return false
  if (error.code === 'invalid_credentials') return false
  return error.status === 401 || error.code === 'unauthorized'
}

export function reportIfRejected(error) {
  if (isSessionRejected(error) && unauthorizedHandler) unauthorizedHandler(error)
  return error
}

export function setTokenProvider(provider) {
  tokenProvider = typeof provider === 'function' ? provider : () => null
}

export function currentToken() {
  const token = tokenProvider()
  return typeof token === 'string' && token !== '' ? token : null
}

export function apiBase() {
  const configured = env.VITE_API_BASE_URL
  if (typeof configured !== 'string') return ''
  return configured.trim().replace(/\/+$/, '')
}

/** Join the configured base and an absolute API path without double slashes. */
export function buildUrl(path, params) {
  const base = apiBase()
  const normalized = String(path).startsWith('/') ? path : `/${path}`
  let url = base ? `${base}${normalized}` : normalized

  if (params && Object.keys(params).length > 0) {
    const query = new URLSearchParams()
    for (const [key, value] of Object.entries(params)) {
      if (value !== undefined && value !== null) query.set(key, String(value))
    }
    const qs = query.toString()
    if (qs) url += (url.includes('?') ? '&' : '?') + qs
  }
  return url
}

export class ApiError extends Error {
  constructor(message, { code = 'unknown', status = 0, path = '' } = {}) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
    this.path = path
  }
}

/** Text the client may show as-is; anything else becomes GENERIC. */
const TRUSTED_CODES = new Set([
  'invalid_request',
  'invalid_credentials',
  'email_taken',
  'display_name_taken',
  'unauthorized',
  'forbidden',
  'not_found',
  'rate_limited',
])

export const GENERIC_ERROR = '服务暂时无法完成这个请求，请稍后重试。'

export function safeErrorMessage(error, fallback = GENERIC_ERROR) {
  if (!error) return fallback
  if (error.code && TRUSTED_CODES.has(error.code) && error.message) {
    return String(error.message).slice(0, 300)
  }
  return fallback
}

function readEnvelope(payload) {
  if (!payload || typeof payload !== 'object') return null
  const error = payload.error
  if (error && typeof error === 'object') {
    return {
      code: typeof error.code === 'string' ? error.code : 'unknown',
      message: typeof error.message === 'string' ? error.message : '',
    }
  }
  if (error === null || error === undefined) {
    return { code: '', message: '', data: payload.data ?? payload }
  }
  return null
}

/**
 * @param {string} path
 * @param {{method?: string, body?: unknown, token?: string|null,
 *   signal?: AbortSignal, expectData?: boolean}} [options]
 */
export async function request(path, options = {}) {
  const { method = 'GET', body, expectData = true } = options
  // Candidate native registration tokens are not the current signed-in token.
  // Their rejection must not sign out a newer session via the global handler.
  const report = options.reportRejection === false ? (error) => error : reportIfRejected
  const token = options.token === undefined ? currentToken() : options.token

  const headers = { accept: 'application/json' }
  if (body !== undefined) headers['content-type'] = 'application/json'
  if (token) headers.authorization = `Bearer ${token}`

  let response
  try {
    response = await fetch(buildUrl(path), {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: options.signal,
    })
  } catch (cause) {
    if (isAbort(cause)) throw cause
    throw new ApiError('无法连接到服务，请检查网络或稍后重试。', {
      code: 'network_error',
      path,
    })
  }

  const payload = await parseBody(response)
  const envelope = readEnvelope(payload)

  if (!response.ok) {
    throw report(
      new ApiError(envelope?.message || GENERIC_ERROR, {
        code: envelope?.code || `http_${response.status}`,
        status: response.status,
        path,
      }),
    )
  }

  if (envelope && envelope.code) {
    // 200 with an error code: still an error.
    throw report(
      new ApiError(envelope.message || GENERIC_ERROR, {
        code: envelope.code,
        status: response.status,
        path,
      }),
    )
  }

  if (!expectData) return null
  if (envelope && 'data' in envelope) return envelope.data
  return payload
}

async function parseBody(response) {
  const text = await response.text()
  if (text === '') return null
  try {
    return JSON.parse(text)
  } catch {
    return null
  }
}

/**
 * Read an error envelope off a non-OK response without ever echoing an
 * unparseable body back to the UI.
 */
export async function parseErrorBody(response) {
  const payload = await parseBody(response)
  const envelope = readEnvelope(payload)
  return {
    code: envelope?.code || `http_${response?.status ?? 0}`,
    message:
      envelope?.code && TRUSTED_CODES.has(envelope.code) && envelope.message
        ? String(envelope.message).slice(0, 300)
        : GENERIC_ERROR,
  }
}

export function isAbort(error) {
  return (
    !!error &&
    (error.name === 'AbortError' ||
      error.name === 'APIAbortedError' ||
      error.code === 'aborted')
  )
}
