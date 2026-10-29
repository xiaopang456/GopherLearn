/**
 * Map a transport frame onto the conversation state machine's vocabulary.
 *
 * `spec/API.md` fixes the four event names but deliberately leaves the JSON
 * bodies open, so field lookup accepts the snake_case names a Go server is
 * most likely to emit as well as camelCase. Anything unrecognised degrades to
 * an explicit `unknown` kind, which the reducer counts and discards instead of
 * guessing at.
 */

import { normalizeSourceList } from './sources.js'

const KIND_BY_EVENT = {
  'message.started': 'started',
  'message.delta': 'delta',
  'message.completed': 'completed',
  'message.failed': 'failed',
}

export const STREAM_EVENT_NAMES = Object.keys(KIND_BY_EVENT)

function firstString(...candidates) {
  for (const value of candidates) {
    if (typeof value === 'string' && value !== '') return value
  }
  return ''
}

function parsePayload(raw) {
  if (typeof raw !== 'string' || raw.trim() === '') return {}
  try {
    const decoded = JSON.parse(raw)
    return decoded && typeof decoded === 'object' ? decoded : { value: decoded }
  } catch {
    // A malformed frame must not break the transcript: the reducer will simply
    // record it as an ignored event.
    return null
  }
}

/**
 * @param {import('./sseParser.js').SSEFrame} frame
 * @returns {{kind: string, runId: string, messageId: string, delta: string,
 *   content: string, code: string, message: string, sources: object[],
 *   finalBlockId: string}}
 */
export function toStreamEvent(frame) {
  const kind = KIND_BY_EVENT[frame.event] ?? 'unknown'
  const payload = parsePayload(frame.data) ?? {}

  return {
    kind,
    originalV2: payload.original_v2 && typeof payload.original_v2 === 'object' ? payload.original_v2 : null,
    runId: firstString(payload.run_id, payload.runId, payload.request_id),
    messageId: firstString(
      payload.message_id,
      payload.messageId,
      payload.assistant_message_id,
    ),
    delta: typeof payload.delta === 'string' ? payload.delta : '',
    content: typeof payload.content === 'string' ? payload.content : '',
    code: firstString(payload.code, errorField(payload, 'code')),
    message: firstString(payload.message, errorField(payload, 'message')),
    // Only `message.completed` carries citations, and it omits the key entirely
    // when there are none; `normalizeSourceList` turns that absence into [].
    sources: normalizeSourceList(payload.sources),
    // And only `message.completed` names the text block its content came from, on a turn
    // that published its answer as blocks. Absent means "this answer was not a block" — a
    // chat turn, a knowledge turn, an agent turn on the legacy channel — so it stays ''
    // rather than becoming a block id that happens to be empty.
    finalBlockId: typeof payload.final_block_id === 'string' ? payload.final_block_id : '',
  }
}

function errorField(payload, key) {
  const nested = payload.error
  return nested && typeof nested === 'object' ? nested[key] : undefined
}
