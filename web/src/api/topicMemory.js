import { ApiError, request } from './http.js'

/**
 * The conversation-scoped topic-memory switch.
 *
 * Frozen contract, and it is deliberately this small:
 *
 *   GET  /api/v1/sessions/{id}/topic-memory  -> data: { "enabled": boolean }
 *   PUT  /api/v1/sessions/{id}/topic-memory  body: { "enabled": boolean }
 *
 * `PUT` — not a pair of POST actions — because the stored state *is* the whole
 * decision, it has no version to pass, and writing the same answer twice changes
 * nothing. So a client that retries a click the network lost is not a second
 * decision, and this module needs no idempotency token of its own.
 *
 * What the body must never carry: `topic_key`, `user_id`, `scope`, `session_id`,
 * `all_users`. Which subject a "yes" unlocks is worked out on the server, from the
 * quiz this conversation has open; the conversation the decision belongs to is named
 * by the path, not by the caller; and nobody gets to answer for another account. A
 * body that could name any of those would be a client claiming an authority it was
 * never given, so the object literal below is the only shape this file can produce.
 *
 * The read side is what makes the default honest: absence of a row means `false`,
 * and nothing here stores the switch locally. A page that remembered last visit's
 * position would be showing a decision the server never confirmed.
 */

const SESSIONS_PATH = '/api/v1/sessions'

/** Session ids are short opaque tokens; a longer value is a bug, not a request. */
export const MAX_SESSION_ID_RUNES = 128

/** Fixed sentences only — the server's own `message` is never rendered. */
export const TOPIC_MEMORY_COPY = {
  missingSession: '还没有选定练习会话，这个设置这次没有发出。',
  invalidSession: '会话标识不合法，这个设置这次没有发出。',
  readFailed: '读不到这段对话当前的主题记忆设置。',
  writeFailed: '这个设置没有保存成功，当前状态没有改动，可以再试一次。',
  refused: '这个开关在这段对话上不能用：主题记忆只挂在学习 Agent 会话上。',
  unreadable: '服务端返回的内容里读不出开关状态。',
}

function firstString(value) {
  return typeof value === 'string' ? value.trim() : ''
}

function runeCount(value) {
  return [...value].length
}

/**
 * One conversation, one path. The id is escaped, so a value containing a slash
 * cannot add a segment to the route — which is the property the tests assert, not
 * the absence of a `..` in the string.
 */
export function topicMemoryPath(sessionId) {
  const id = firstString(sessionId)
  if (id === '') {
    throw new ApiError(TOPIC_MEMORY_COPY.missingSession, { code: 'local_invalid', status: 0 })
  }
  if (runeCount(id) > MAX_SESSION_ID_RUNES) {
    throw new ApiError(TOPIC_MEMORY_COPY.invalidSession, { code: 'local_invalid', status: 0 })
  }
  return `${SESSIONS_PATH}/${encodeURIComponent(id)}/topic-memory`
}

/**
 * Whitelist, not mapping: `enabled` is the only thing that leaves this function, and
 * only when it really is a boolean. A response without it — or with `"true"`, or with
 * the field in a nested object — becomes `null`, which the store shows as "unknown"
 * rather than as off. Guessing `false` from a body we cannot read would be the same
 * mistake as a localStorage default.
 */
export function normalizeTopicPreference(payload) {
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) return null
  if (typeof payload.enabled !== 'boolean') return null
  return { enabled: payload.enabled }
}

/**
 * One sentence per failure *class*. Two rules here are load-bearing: a conversation
 * that is somebody else's and one that never existed answer the same way, so the copy
 * must not differ; and no branch returns `error.message`, because that text belongs to
 * the server and this build has not decided what it is allowed to say to a learner.
 */
export function mapTopicMemoryError(error) {
  const code = error && typeof error.code === 'string' && error.code !== '' ? error.code : 'unknown'
  const status = error && Number.isFinite(error.status) ? error.status : 0
  if (status === 401 || code === 'unauthorized') {
    return { copy: TOPIC_MEMORY_COPY.readFailed, code: 'unauthorized' }
  }
  if (code === 'not_found' || status === 404) {
    return { copy: TOPIC_MEMORY_COPY.readFailed, code: 'not_found' }
  }
  if (code === 'invalid_request' || code === 'forbidden' || status === 403 || status === 400) {
    return { copy: TOPIC_MEMORY_COPY.refused, code }
  }
  return { copy: TOPIC_MEMORY_COPY.writeFailed, code }
}

/** Read the stored decision. `expectData` stays on: an answer is `{enabled}` or nothing. */
export function readTopicMemory({ sessionId, signal } = {}) {
  let path
  try {
    path = topicMemoryPath(sessionId)
  } catch (caught) {
    return Promise.reject(caught)
  }
  return request(path, { signal })
}

/**
 * Write the decision. The body is built here — one key, one boolean — so a caller
 * cannot widen it by accident and a test can pin the serialized bytes.
 */
export function commitTopicMemory({ sessionId, enabled } = {}) {
  if (typeof enabled !== 'boolean') {
    return Promise.reject(
      new ApiError(TOPIC_MEMORY_COPY.writeFailed, { code: 'local_invalid', status: 0 }),
    )
  }
  let path
  try {
    path = topicMemoryPath(sessionId)
  } catch (caught) {
    return Promise.reject(caught)
  }
  return request(path, { method: 'PUT', body: { enabled } })
}
