import { ApiError, buildUrl, currentToken, isAbort, reportIfRejected } from './http.js'

export const V2_MEMORY_PATH = '/api/v1/AI/agent/memories'
const memoryPath = (id) => `${V2_MEMORY_PATH}/${encodeURIComponent(String(id))}`

// Preserve native fields, statuses and replay semantics. This is not a clean
// memory adapter: no state merge, implicit decision, scope conversion or defaults.
export async function nativeMemoryRequest(path, { method = 'GET', body, signal } = {}) {
  const token = currentToken()
  let response
  try {
    response = await fetch(buildUrl(path), { method, signal,
      headers: { accept: 'application/json', ...(body === undefined ? {} : { 'content-type': 'application/json' }),
        ...(token ? { authorization: `Bearer ${token}` } : {}) },
      body: body === undefined ? undefined : JSON.stringify(body) })
  } catch (error) {
    if (isAbort(error)) throw error
    throw new ApiError('无法连接原记忆治理接口。', { code: 'network_error', path })
  }
  let result
  try { result = JSON.parse(await response.text()) } catch { result = null }
  if (!response.ok || result?.status_code !== 1000) {
    const nativeCode = result?.status_code
    const error = new ApiError(result?.status_msg || '原记忆治理操作未完成。', {
      status: response.status, code: nativeCode === 2006 ? 'unauthorized' : `v2_${nativeCode ?? response.status}`, path })
    error.nativeCode = nativeCode
    throw reportIfRejected(error)
  }
  return result
}
export function listV2Memory({ scope, status, signal } = {}) {
  const query = new URLSearchParams()
  if (scope !== undefined) query.set('scope', scope)
  if (status !== undefined) query.set('status', status)
  return nativeMemoryRequest(`${V2_MEMORY_PATH}${query.size ? `?${query}` : ''}`, { signal })
}
export const createV2Memory = (body, signal) => nativeMemoryRequest(V2_MEMORY_PATH, { method: 'POST', body, signal })
export const correctV2Memory = (id, body, signal) => nativeMemoryRequest(memoryPath(id), { method: 'PATCH', body, signal })
export const archiveV2Memory = (id, reason, signal) => nativeMemoryRequest(`${memoryPath(id)}/archive`, { method: 'POST', body: { reason }, signal })
export const deleteV2Memory = (id, reason, signal) => nativeMemoryRequest(memoryPath(id), { method: 'DELETE', body: { reason }, signal })
export const revisionsV2Memory = (id, signal) => nativeMemoryRequest(`${memoryPath(id)}/revisions`, { signal })
export const resolveV2Draft = (id, body, signal) => nativeMemoryRequest(`/api/v1/AI/agent/memory-drafts/${encodeURIComponent(String(id))}/resolve`, { method: 'POST', body, signal })
