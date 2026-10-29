import { request } from './http.js'

export const AUTH_REGISTER_PATH = '/api/v1/auth/register'
export const AUTH_LOGIN_PATH = '/api/v1/auth/login'
export const ME_PATH = '/api/v1/me'

export function register({ email, password, displayName }) {
  return request(AUTH_REGISTER_PATH, {
    method: 'POST',
    token: null,
    body: {
      email: String(email).trim(),
      password: String(password),
      display_name: String(displayName).trim(),
    },
  })
}

export function login({ email, password }) {
  return request(AUTH_LOGIN_PATH, {
    method: 'POST',
    token: null,
    body: { email: String(email).trim(), password: String(password) },
  })
}

export function fetchMe(token, signal, reportRejection = true) {
  return request(ME_PATH, { token, signal, reportRejection })
}

/**
 * Normalize the auth payload. `spec/API.md` says login "returns bearer token
 * and public user profile" without fixing the key casing, so accept the common
 * spellings and hand one shape to the store.
 */
export function readAuthPayload(payload) {
  const source = payload || {}
  const token =
    firstString(source.token, source.access_token, source.accessToken) || ''
  const rawUser = source.user || source.profile || source
  return {
    token,
    user: normalizeUser(rawUser),
  }
}

export function normalizeUser(payload) {
  const source = payload || {}
  return {
    id: firstString(source.id, String(source.id ?? '')),
    email: firstString(source.email, source.username),
    displayName: firstString(
      source.display_name,
      source.displayName,
      source.nickname,
      source.name,
    ),
  }
}

function firstString(...candidates) {
  for (const value of candidates) {
    if (typeof value === 'string' && value !== '') return value
  }
  return ''
}
