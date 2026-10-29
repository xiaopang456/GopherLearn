/**
 * Per-turn knowledge grounding for Chat sessions.
 *
 * This is deliberately a *draft* shape owned by the composer, separate from the
 * wire shape the API expects, so that wiring the real collection endpoints in a
 * later phase changes only where `collections` comes from — not the component
 * contract, and not the reducer or transcript code.
 *
 * There is no fallback collection id anywhere: an id that was never issued by
 * the server would either retrieve nothing or retrieve somebody else's data.
 */

export function emptyKnowledgeDraft() {
  return { enabled: false, selectedCollectionIds: [] }
}

function uniqueIds(ids) {
  const seen = []
  for (const value of Array.isArray(ids) ? ids : []) {
    const id = typeof value === 'string' ? value.trim() : ''
    if (id && !seen.includes(id)) seen.push(id)
  }
  return seen
}

/** A draft that could actually ground an answer: switched on, and ≥1 real id. */
export function isKnowledgeUsable(draft) {
  return (
    !!draft && draft.enabled === true && uniqueIds(draft.selectedCollectionIds).length > 0
  )
}

/**
 * @returns {{enabled: true, collection_ids: string[]}|null} null means the
 * `knowledge` key is omitted entirely, which is the one representation this
 * client standardises on for the disabled case.
 */
export function toWireKnowledge(draft) {
  if (!isKnowledgeUsable(draft)) return null
  return { enabled: true, collection_ids: uniqueIds(draft.selectedCollectionIds) }
}
