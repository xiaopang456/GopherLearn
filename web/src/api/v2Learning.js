import { ApiError, buildUrl, currentToken, isAbort, reportIfRejected } from './http.js'

export const V2_LEARNING_PATH = '/api/v1/AI/agent/learning'

// Quote native integer identities before JSON.parse can round them. Consume
// complete strings so text containing JSON-looking fragments is never changed.
export function parseLearningResponse(text) {
  const tokens = /"(?:[^"\\]|\\.)*"|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|[{}[\]:,]|true|false|null/g
  let key = null
  let colon = false
  const safe = text.replace(tokens, (token) => {
    if (colon) {
      colon = false
      const identity = key === 'id' || key === 'user_id'
      key = null
      if (identity && /^-?\d+$/.test(token)) return JSON.stringify(token)
    }
    if (token.startsWith('"')) key = JSON.parse(token)
    else if (token === ':') colon = true
    else key = null
    return token
  })
  return JSON.parse(safe)
}

export async function nativeLearningRequest(action, { method = 'GET', query, body, signal } = {}) {
  const params = new URLSearchParams(query)
  const path = `${V2_LEARNING_PATH}/${action}${params.size ? `?${params}` : ''}`
  const token = currentToken()
  let response
  try {
    response = await fetch(buildUrl(path), { method, signal,
      headers: { accept: 'application/json', ...(body === undefined ? {} : { 'content-type': 'application/json' }),
        ...(token ? { authorization: `Bearer ${token}` } : {}) },
      body: body === undefined ? undefined : JSON.stringify(body) })
  } catch (error) {
    if (isAbort(error)) throw error
    throw new ApiError('无法连接原学习接口。', { code: 'network_error', path })
  }
  let result
  try { result = parseLearningResponse(await response.text()) } catch { result = null }
  if (!response.ok || result?.status_code !== 1000) {
    const nativeCode = result?.status_code
    const error = new ApiError(result?.status_msg || '原学习操作未完成。', {
      status: response.status, code: nativeCode === 2006 ? 'unauthorized' : `v2_${nativeCode ?? response.status}`, path })
    error.nativeCode = nativeCode
    throw reportIfRejected(error)
  }
  return result
}

// Explicit native fields only. No Collection conversion, client identity,
// implicit topic consent, retries, client grading or learning SSE.
export const startV2Learning = (body, signal) => nativeLearningRequest('start', { method: 'POST', body, signal })
export const currentV2Learning = (sessionId, signal) => nativeLearningRequest('current', { query: { sessionId }, signal })
export const answerV2Learning = (body, signal) => nativeLearningRequest('answer', { method: 'POST', body, signal })
export const actionV2Learning = (body, signal) => nativeLearningRequest('action', { method: 'POST', body, signal })
export const cancelV2Learning = (body, signal) => nativeLearningRequest('cancel', { method: 'POST', body, signal })
export const progressV2Learning = (topic, signal) => nativeLearningRequest('progress', { query: { topic }, signal })
export const correctV2Learning = (body, signal) => nativeLearningRequest('correct', { method: 'POST', body, signal })
