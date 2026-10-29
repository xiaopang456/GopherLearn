/**
 * The one place a citation list becomes frontend state.
 *
 * A grounded answer can reach the transcript through three routes — the
 * synchronous message response, `message.completed` over SSE, and history
 * reloaded after a refresh — and the server spells every one of them the same
 * way. Funnelling all three through this single function is what makes the
 * rendered citation identical regardless of which path delivered it, instead of
 * three near-copies that drift.
 *
 * Scores are not in the payload and are not invented here: retrieval ranking is
 * a server implementation detail.
 */

const MAX_SOURCES = 5

function text(value) {
  return typeof value === 'string' ? value : value == null ? '' : String(value)
}

export function normalizeSource(raw) {
  const source = raw || {}
  const original = source.original_v2 ?? (source.doc_id ? source : null)
  if (original) return {
    sourceId: text(original.citation), documentId: text(original.doc_id),
    collectionId: '', chunkId: '', title: text(original.file_name),
    section: Array.isArray(original.title_path) ? original.title_path.join(' / ') : '',
    snippet: text(original.snippet), documentVersion: text(original.doc_version),
    originalV2: original, navigationUnavailable: true,
  }
  if (source.originalV2) return { ...source }
  return {
    sourceId: text(source.source_id ?? source.sourceId),
    documentId: text(source.document_id ?? source.documentId),
    collectionId: text(source.collection_id ?? source.collectionId),
    chunkId: text(source.chunk_id ?? source.chunkId),
    title: text(source.title),
    section: text(source.section),
    snippet: text(source.snippet),
  }
}

/**
 * @returns {object[]} always an array; an ordinary message gets `[]`, which is
 * what lets the transcript skip the citation block without a null check.
 */
export function normalizeSourceList(raw) {
  if (!Array.isArray(raw)) return []
  const seen = new Set()
  const result = []
  for (const entry of raw) {
    const normalized = normalizeSource(entry)
    // An entry with nothing to show is not a citation, it is a gap.
    if (!normalized.title && !normalized.snippet) continue
    const key = normalized.sourceId || normalized.chunkId || normalized.documentId
    if (key) {
      if (seen.has(key)) continue
      seen.add(key)
    }
    result.push(normalized)
    if (result.length >= MAX_SOURCES && !normalized.originalV2) break
  }
  return result
}

export function hasSources(message) {
  return Array.isArray(message?.sources) && message.sources.length > 0
}
