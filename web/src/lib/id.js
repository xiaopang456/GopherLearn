/** Local ids for optimistic messages, before the server assigns real ones. */
export function localId(prefix = 'tmp') {
  const cryptoObj = typeof globalThis !== 'undefined' ? globalThis.crypto : null
  if (cryptoObj && typeof cryptoObj.randomUUID === 'function') {
    return `${prefix}-${cryptoObj.randomUUID()}`
  }
  return `${prefix}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`
}
