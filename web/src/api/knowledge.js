import { request } from './http.js'

/**
 * Client for the knowledge endpoints. Everything here goes through the shared
 * `request` helper so the bearer header, the `{data,error}` envelope, the 401
 * signal and the message whitelist behave exactly as they do for sessions —
 * there is deliberately no second fetch implementation in this file.
 */

export const COLLECTIONS_PATH = '/api/v1/collections'
export const DOCUMENTS_PATH = '/api/v1/documents'

/** Matches `document.MaximumContentBytes = 2 << 20` on the server. */
export const MAX_CONTENT_BYTES = 2 << 20

export function collectionDocumentsPath(collectionId) {
  return `${COLLECTIONS_PATH}/${encodeURIComponent(String(collectionId))}/documents`
}

export function documentPath(documentId) {
  return `${DOCUMENTS_PATH}/${encodeURIComponent(String(documentId))}`
}

export function listCollections(options = {}) {
  return request(COLLECTIONS_PATH, { signal: options.signal })
}

export function createCollection(title) {
  return request(COLLECTIONS_PATH, {
    method: 'POST',
    body: { title: String(title ?? '').trim() },
  })
}

export function listDocuments(collectionId, options = {}) {
  return request(collectionDocumentsPath(collectionId), { signal: options.signal })
}

/**
 * A document is submitted as JSON text, never as a file part: the server chunks
 * `content`, so the browser reads the file and sends what it read.
 */
export function addDocument(collectionId, { title, format, content }) {
  return request(collectionDocumentsPath(collectionId), {
    method: 'POST',
    body: documentBody({ title, format, content }),
  })
}

/** Re-chunks the text and advances the document version server-side. */
export function replaceDocument(documentId, { title, format, content }) {
  return request(documentPath(documentId), {
    method: 'PUT',
    body: documentBody({ title, format, content }),
  })
}

export function deleteDocument(documentId) {
  return request(documentPath(documentId), {
    method: 'DELETE',
    expectData: false,
  })
}

function documentBody({ title, format, content }) {
  return {
    title: String(title ?? '').trim(),
    format: String(format ?? '').trim(),
    content: String(content ?? ''),
  }
}

/**
 * The server's extension/format table, kept in one place so the upload picker
 * and the request body cannot disagree about what "a .md file" means.
 */
const FORMATS_BY_EXTENSION = {
  md: 'markdown',
  markdown: 'markdown',
  mkd: 'markdown',
  txt: 'text',
  text: 'text',
  plain: 'text',
}

export const ACCEPTED_EXTENSIONS = ['.md', '.markdown', '.txt']
export const ACCEPT_ATTRIBUTE = ACCEPTED_EXTENSIONS.join(',')

/** @returns {{format: string}|{error: string}} never a guessed format. */
export function formatForFile(name) {
  const filename = String(name ?? '').trim()
  const dot = filename.lastIndexOf('.')
  if (dot <= 0 || dot === filename.length - 1) {
    return { error: '文件没有可扩展名，无法判断格式。' }
  }
  const extension = filename.slice(dot + 1).toLowerCase()
  const format = FORMATS_BY_EXTENSION[extension]
  if (!format) {
    return {
      error: `只支持 ${ACCEPTED_EXTENSIONS.join(' / ')} 文件，收到 .${extension}。`,
    }
  }
  return { format }
}

export function utf8Length(text) {
  const value = String(text ?? '')
  if (typeof TextEncoder !== 'undefined') return new TextEncoder().encode(value).length
  return unescape(encodeURIComponent(value)).length
}

/**
 * The same 2 MiB ceiling the server enforces, applied before it is asked. A
 * whitespace-only file counts as empty here too, because `prepare` rejects
 * `strings.TrimSpace(content) == ""` and a chunker run would only fail later.
 */
export function validateDocumentContent(content) {
  const value = String(content ?? '')
  if (value.trim() === '') return '文件内容是空的。'
  const bytes = utf8Length(value)
  if (bytes > MAX_CONTENT_BYTES) {
    return `文件 ${bytes} 字节，超过 ${MAX_CONTENT_BYTES} 字节（2 MiB）上限。`
  }
  return null
}

export function normalizeCollection(raw) {
  const source = raw || {}
  return {
    id: String(source.id ?? source.collection_id ?? source.collectionId ?? ''),
    title: String(source.title ?? source.name ?? '') || '未命名知识库',
    createdAt: String(source.created_at ?? source.createdAt ?? ''),
    updatedAt: String(source.updated_at ?? source.updatedAt ?? ''),
  }
}

export function normalizeDocument(raw) {
  const source = raw || {}
  return {
    id: String(source.id ?? source.document_id ?? source.documentId ?? ''),
    collectionId: String(
      source.collection_id ?? source.collectionId ?? source.collection ?? '',
    ),
    title: String(source.title ?? source.name ?? '') || '未命名文档',
    format: String(source.format ?? ''),
    version: Number.isFinite(Number(source.version)) ? Number(source.version) : 0,
    chunkCount: Number.isFinite(Number(source.chunk_count ?? source.chunkCount))
      ? Number(source.chunk_count ?? source.chunkCount)
      : 0,
    createdAt: String(source.created_at ?? source.createdAt ?? ''),
    updatedAt: String(source.updated_at ?? source.updatedAt ?? ''),
  }
}

/** Lists arrive as `{items:[…]}` under `data`; anything else reads as empty. */
export function readItems(payload) {
  if (Array.isArray(payload)) return payload
  if (payload && Array.isArray(payload.items)) return payload.items
  return []
}

export function normalizeCollectionList(payload) {
  return readItems(payload)
    .map(normalizeCollection)
    .filter((item) => item.id !== '')
}

export function normalizeDocumentList(payload) {
  return readItems(payload)
    .map(normalizeDocument)
    .filter((item) => item.id !== '')
}

/**
 * The write endpoints answer with the object they just stored, while the read
 * endpoints answer with `{items:[…]}`. Accepting either shape here means the
 * store does not have to know which kind of call it is finishing, and a body
 * that carries no addressable id still reads as "nothing was created" rather
 * than as a record with an empty key.
 */
function asRecord(payload) {
  if (Array.isArray(payload)) return payload[0] ?? null
  if (payload && Array.isArray(payload.items)) return payload.items[0] ?? null
  return payload && typeof payload === 'object' ? payload : null
}

export function normalizeCollectionResponse(payload) {
  const record = asRecord(payload)
  const normalized = record ? normalizeCollection(record) : null
  return normalized && normalized.id ? normalized : null
}

export function normalizeDocumentResponse(payload) {
  const record = asRecord(payload)
  const normalized = record ? normalizeDocument(record) : null
  return normalized && normalized.id ? normalized : null
}
