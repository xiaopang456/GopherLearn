/**
 * Document-processing client.
 *
 * - the owner's processing state:  GET /api/v1/knowledge/documents/:id/processing
 *
 * This build processes documents in-process, so the processing endpoint answers with the
 * processing mode plus the document's index status. It is the only document-processing
 * route this client knows about.
 */
import { ApiError, buildUrl, currentToken, isAbort, reportIfRejected } from './http.js'

const DOCUMENTS = '/api/v1/knowledge/documents'

function authHeaders(extra = {}) {
  const token = currentToken()
  return { accept: 'application/json', ...extra, ...(token ? { authorization: `Bearer ${token}` } : {}) }
}

async function readJson(response) {
  try { return JSON.parse(await response.text()) } catch { return null }
}

/** The owner's processing state. Errors keep the native document-interface shape. */
export async function getDocumentProcessing(id, { signal } = {}) {
  const url = `${DOCUMENTS}/${encodeURIComponent(String(id))}/processing`
  let response
  try {
    response = await fetch(buildUrl(url), { method: 'GET', signal, headers: authHeaders() })
  } catch (error) {
    if (isAbort(error)) throw error
    throw new ApiError('无法连接原文档接口。', { code: 'network_error', path: url })
  }
  const data = await readJson(response)
  if (!response.ok) {
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
