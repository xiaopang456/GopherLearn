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
    updatedAt: String(
      source.updated_at ?? source.updatedAt ?? source.created_at ?? '',
    ),
  }
}

export const MESSAGE_ROLES = new Set(['user', 'assistant', 'system'])

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
  }
}
