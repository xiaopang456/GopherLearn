/**
 * One in-flight assistant answer, modelled as an explicit finite state machine.
 *
 *   idle -> streaming -> completed
 *                    \-> failed
 *                    \-> aborted
 *
 * `completed`, `failed` and `aborted` are terminal: once a run reaches one,
 * every later event for that run is counted and discarded. That single rule
 * gives the three guarantees the phase brief asks for:
 *
 *  - a late `message.failed` cannot overwrite an answer that already completed;
 *  - a delta can only ever touch the placeholder this run owns;
 *  - a failure keeps whatever text already arrived next to the error card, and
 *    the user's question is never part of this state machine at all, so no
 *    transition here can remove it.
 *
 * Identity is deliberately split in two, because the client and the server each
 * own one side of it and neither sees the other's value:
 *
 *  - `clientRunId` is minted locally per request. It never crosses the wire (the
 *    contract's request body carries only `content`), so it cannot be forged or
 *    echoed; it exists so a cancelled request's still-draining callback can be
 *    recognised as stale and refuse to touch the current run.
 *  - `serverRunId` starts empty and binds to the first non-empty `run_id` the
 *    stream carries. Only a *different* later server id is foreign. Comparing
 *    the server id against the local one instead would drop every event.
 *
 * `placeholderId` keys the local transcript entry, and the assistant message id
 * is adopted the same lazy way, so a delta can only ever touch the entry this
 * run created.
 */

import { normalizeSourceList } from './sources.js'

export const RunPhase = {
  Idle: 'idle',
  Streaming: 'streaming',
  Completed: 'completed',
  Failed: 'failed',
  Aborted: 'aborted',
}

const TERMINAL = new Set([RunPhase.Completed, RunPhase.Failed, RunPhase.Aborted])

export function isTerminalPhase(phase) {
  return TERMINAL.has(phase)
}

/**
 * @param {object} spec
 * @param {string} spec.clientRunId   local id isolating this request
 * @param {string} spec.placeholderId id of the local transcript entry
 */
export function createRunState({ clientRunId, placeholderId }) {
  return {
    clientRunId: String(clientRunId ?? ''),
    serverRunId: '',
    placeholderId: String(placeholderId ?? ''),
    messageId: '',
    phase: RunPhase.Idle,
    text: '',
    error: null,
    // Citations belong to this run alone: they arrive on its `completed` event
    // and are mirrored only into its own placeholder message.
    sources: [],
    ignoredEvents: 0,
  }
}

function drop(state) {
  return { ...state, ignoredEvents: state.ignoredEvents + 1 }
}

/** @returns {string|false} why the event does not belong to this run */
function classify(state, event) {
  if (!event || typeof event !== 'object') return 'malformed'
  const runId = typeof event.runId === 'string' ? event.runId : ''
  if (runId && state.serverRunId && runId !== state.serverRunId) return 'foreign-run'
  const messageId = typeof event.messageId === 'string' ? event.messageId : ''
  if (messageId && state.messageId && messageId !== state.messageId) {
    return 'foreign-message'
  }
  return false
}

/** Bind the server-side identifiers the first time each is seen. */
function adopt(state, event) {
  let next = state
  if (!next.serverRunId && typeof event.runId === 'string' && event.runId) {
    next = { ...next, serverRunId: event.runId }
  }
  if (!next.messageId && typeof event.messageId === 'string' && event.messageId) {
    next = { ...next, messageId: event.messageId }
  }
  return next
}

/**
 * @param {ReturnType<typeof createRunState>} state
 * @param {object} event output of `toStreamEvent`
 */
export function reduceRun(state, event) {
  if (classify(state, event)) return drop(state)
  const owned = adopt(state, event)

  switch (event.kind) {
    case 'started':
      if (isTerminalPhase(owned.phase)) return drop(owned)
      return { ...owned, phase: RunPhase.Streaming }

    case 'delta':
      if (isTerminalPhase(owned.phase)) return drop(owned)
      return {
        ...owned,
        phase: RunPhase.Streaming,
        text: owned.text + (typeof event.delta === 'string' ? event.delta : ''),
      }

    case 'completed':
      if (isTerminalPhase(owned.phase)) return drop(owned)
      return {
        ...owned,
        phase: RunPhase.Completed,
        // Trust the streamed text unless the server restates the final body.
        text: event.content ? event.content : owned.text,
        error: null,
        // Normalized here as well as upstream: whatever a caller hands the
        // reducer, an un-addressable or duplicate citation cannot reach the
        // transcript through it.
        sources: normalizeSourceList(event.sources),
      }

    case 'failed':
      if (isTerminalPhase(owned.phase)) return drop(owned)
      return {
        ...owned,
        phase: RunPhase.Failed,
        error: {
          code: event.code || 'stream_failed',
          message: event.message || '回答生成失败，请重试。',
        },
      }

    default:
      return drop(owned)
  }
}

/** The user pressed stop, or the request was aborted. */
export function abortRun(state) {
  if (isTerminalPhase(state.phase)) return state
  return {
    ...state,
    phase: RunPhase.Aborted,
    error: { code: 'aborted', message: '已停止本次回答的生成。' },
  }
}

/** The transport or server produced no usable terminal event. */
export function failRun(state, code, message) {
  if (isTerminalPhase(state.phase)) return state
  return {
    ...state,
    phase: RunPhase.Failed,
    error: {
      code: code || 'stream_transport_error',
      message: message || '与服务的连接中断，回答未完成。',
    },
  }
}

/**
 * Point the run's text at the body the learner is actually reading.
 *
 * An agent turn publishes its text as blocks rather than as `message.delta` frames, and
 * everything downstream - the transcript entry, the caret, the autoscroll watcher - reads one
 * string off this state. Rewriting that string here, instead of keeping a second copy of the
 * answer somewhere else, is what stops the two from disagreeing mid-turn: the same field a chat
 * turn grows from its deltas is the field an agent turn grows from its blocks, and the
 * `completed` case below still replaces it with the server's confirmed body for both.
 *
 * A terminal run has nothing left to grow, so this refuses to edit one: a fragment still
 * draining from a socket the learner stopped cannot lengthen a paragraph the page has already
 * explained.
 */
export function setPublishedText(state, text) {
  if (isTerminalPhase(state.phase)) return state
  if (state.text === text) return state
  return { ...state, phase: RunPhase.Streaming, text }
}

/** Collapse the run into the shape the transcript renders. */
export function toMessageStatus(state) {
  switch (state.phase) {
    case RunPhase.Completed:
      return { status: 'done', streaming: false, error: null }
    case RunPhase.Failed:
      return { status: 'error', streaming: false, error: state.error }
    case RunPhase.Aborted:
      return { status: 'stopped', streaming: false, error: state.error }
    default:
      return { status: 'pending', streaming: true, error: null }
  }
}
