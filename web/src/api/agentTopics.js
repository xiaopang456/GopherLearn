import { nativeMemoryRequest } from './v2Memory.js'

const TOPIC_BASE = '/api/v1/AI/agent'

/**
 * The read-side confirmation chain: suggestions, catalogue browse and ticket
 * issuance.
 *
 * These endpoints answer in the native governance envelope
 * ({status_code, status_msg, ...}), the same one the memory endpoints use, so
 * they go through the same transport instead of the data/error envelope.
 */
export function suggestTopics(body, signal) {
  return nativeMemoryRequest(`${TOPIC_BASE}/topic-suggestions`, { method: 'POST', body, signal })
}

export function browseTopicCatalogue(body, signal) {
  return nativeMemoryRequest(`${TOPIC_BASE}/topic-catalogue`, { method: 'POST', body, signal })
}

export function issueTopicConsent(body, signal) {
  return nativeMemoryRequest(`${TOPIC_BASE}/topic-consent`, { method: 'POST', body, signal })
}
