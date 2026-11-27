import { request } from './http.js'
import { normalizeSourceList } from '../lib/sources.js'
import {
  createSessionBody,
  normalizeMode,
  UNTITLED_SESSION,
} from '../lib/modes.js'

export { UNTITLED_SESSION }

export const SESSIONS_PATH = '/api/v1/sessions'

export function sessionPath(id) {
  return `${SESSIONS_PATH}/${encodeURIComponent(String(id))}`
}

export function messagesPath(id) {
  return `${sessionPath(id)}/messages`
}

export function streamPath(id) {
  return `${sessionPath(id)}/stream`
}

export function createSession(spec = {}) {
  const body = createSessionBody(spec)
  // The caller owns the operation key and reuses it for retries.
  if (body.mode === 'learning_agent' && spec.requestId !== undefined) {
    body.requestId = spec.requestId
  }
  return request(SESSIONS_PATH, {
    method: 'POST',
    body,
  })
}

export function listSessions() {
  return request(SESSIONS_PATH)
}

export function listMessages(sessionId, options = {}) {
  return request(messagesPath(sessionId), { signal: options.signal })
}

export function deleteSession(sessionId) {
  return request(sessionPath(sessionId), { method: 'DELETE', expectData: false })
}

/** Lists come back as `{items:[…]}`, a bare array, or `{sessions:[…]}`. */
export function readList(payload, key) {
  if (Array.isArray(payload)) return payload
  if (!payload || typeof payload !== 'object') return []
  const candidates = [key, 'items', 'sessions', 'messages', 'data'].filter(
    (name) => typeof name === 'string',
  )
  for (const name of candidates) {
    if (Array.isArray(payload[name])) return payload[name]
  }
  return []
}

/**
 * `mode` is kept, not defaulted: an unrecognised value becomes '' so the UI can
 * show "模式未识别" and withhold optional capabilities, instead of silently
 * treating the session as one it was never authorised to be.
 */
export function normalizeSession(raw) {
  const source = raw || {}
  return {
    id: String(source.id ?? source.session_id ?? source.sessionId ?? ''),
    title: String(source.title ?? source.name ?? '') || UNTITLED_SESSION,
    mode: normalizeMode(source.mode ?? source.session_mode ?? source.sessionMode),
    // The server's ceiling for this session, never inferred from the mode here:
    // only an explicit `true` grants the switch.
    canUseKnowledge:
      (source.can_use_knowledge ?? source.canUseKnowledge) === true,
    updatedAt: String(
      source.updated_at ?? source.updatedAt ?? source.created_at ?? '',
    ),
  }
}

export const MESSAGE_ROLES = new Set(['user', 'assistant', 'system'])

/**
 * The basis of a stored answer, as the history read returns it.
 *
 * The server keeps the basis on the message row, so a conversation that is
 * opened again shows the same "依据" the turn recorded. Without this the card
 * is a live-turn-only thing and the answer looks like it never had a basis.
 *
 * It is assembled into the same `originalV2` shape the live turn carries, so
 * the card reads one set of fields either way.
 */
function storedBasis(raw) {
  const answerBasis = String(raw.answer_basis ?? raw.answerBasis ?? '').trim()
  const knowledgeStatus = String(raw.knowledge_status ?? raw.knowledgeStatus ?? '').trim()
  const knowledgeReason = String(raw.knowledge_reason ?? raw.knowledgeReason ?? '').trim()
  const topics = Array.isArray(raw.answer_basis_topics ?? raw.answerBasisTopics)
    ? (raw.answer_basis_topics ?? raw.answerBasisTopics).map((name) => String(name ?? '').trim()).filter(Boolean)
    : []
  if (!answerBasis && !knowledgeStatus && !knowledgeReason) return null
  return {
    answerBasis,
    knowledgeStatus,
    knowledgeReason,
    ...(topics.length ? { answerBasisTopics: topics } : {}),
  }
}

export function normalizeMessage(raw, fallbackId) {
  const source = raw || {}
  const role = String(source.role ?? '').toLowerCase()
  return {
    id:
      String(source.id ?? source.message_id ?? source.messageId ?? '') ||
      fallbackId,
    role: MESSAGE_ROLES.has(role) ? role : 'assistant',
    content: String(source.content ?? source.text ?? ''),
    createdAt: String(
      source.created_at ?? source.createdAt ?? source.timestamp ?? '',
    ),
    status: 'done',
    error: null,
    // Same normalizer the SSE path uses, so a citation recovered from history is
    // byte-for-byte the structure the live turn produced.
    sources: normalizeSourceList(source.sources),
    // The turn this message belongs to. An ordinary chat has none, and then this is
    // the empty string rather than a made-up id: the key is only read when the server
    // sent it, and a message of a plain conversation must not look like a turn of an
    // Agent run.
    // The clarification travels with the message it was asked in. The learner
    // may answer it minutes later, in a conversation they have reloaded in
    // between, so the card has to survive the read-back: without it the
    // question that was waiting for an answer is gone with it.
    clarification:
      source.clarification && typeof source.clarification === 'object'
        ? source.clarification
        : null,
    // The basis the turn recorded, read back from the stored row. It is the
    // same object the live turn carries, so the card below the answer reads
    // the same either way.
    originalV2: storedBasis(source),
    runId: String(source.run_id ?? ''),
  }
}
