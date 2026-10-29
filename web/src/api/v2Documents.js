import { ApiError, buildUrl, currentToken, reportIfRejected, request, isAbort } from './http.js'

export const V2_DOCUMENTS_PATH = '/api/v1/knowledge/documents'
export const V2_MAX_FILE_BYTES = 5 << 20
export const V2_STATES = ['pending', 'indexing', 'ready', 'failed', 'cleanup_pending', 'deleting', 'deleted']
const path = (id) => `${V2_DOCUMENTS_PATH}/${encodeURIComponent(String(id))}`

// Native controller envelope and multipart are deliberately distinct from
// Collection's JSON envelope. Original status codes/messages remain observable.
export async function nativeDocumentRequest(url, { method = 'GET', file, signal } = {}) {
  const token = currentToken()
  let body
  if (file !== undefined) { body = new FormData(); body.append('file', file, file.name) }
  let response
  try {
    response = await fetch(buildUrl(url), { method, signal, body,
      headers: { accept: 'application/json', ...(token ? { authorization: `Bearer ${token}` } : {}) } })
  } catch (error) {
    if (isAbort(error)) throw error
    throw new ApiError('无法连接原文档接口。', { code: 'network_error', path: url })
  }
  let data
  try { data = JSON.parse(await response.text()) } catch { data = null }
  if (!response.ok || data?.status_code !== 1000) {
    const nativeCode = data?.status_code
    const message = typeof data?.status_msg === 'string' && nativeCode >= 7001 && nativeCode <= 7010
      ? data.status_msg : '原文档接口未完成请求。'
    const error = new ApiError(message, { status: response.status,
      code: nativeCode === 2006 ? 'unauthorized' : `v2_${nativeCode ?? response.status}`, path: url })
    error.nativeCode = nativeCode
    throw reportIfRejected(error)
  }
  return data
}
export const listV2Documents = (options) => nativeDocumentRequest(V2_DOCUMENTS_PATH, options)
export const getV2Document = (id, options) => nativeDocumentRequest(path(id), options)
export const uploadV2Document = (file) => nativeDocumentRequest(V2_DOCUMENTS_PATH, { method: 'POST', file })
export const updateV2Document = (id, file) => nativeDocumentRequest(path(id), { method: 'PUT', file })
export const retryV2Document = (id) => nativeDocumentRequest(`${path(id)}/retry`, { method: 'POST' })
export const reindexV2Document = (id) => nativeDocumentRequest(`${path(id)}/reindex`, { method: 'POST' })
export const deleteV2Document = (id) => nativeDocumentRequest(path(id), { method: 'DELETE' })
export function readV2Source(id, version, start, end, signal) {
  const query = { version: String(version) }
  if (start !== undefined || end !== undefined) { query.start_line = String(start); query.end_line = String(end) }
  return request(`${path(id)}/source?${new URLSearchParams(query)}`, { signal })
}
