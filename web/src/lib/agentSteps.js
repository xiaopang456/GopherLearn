/**
 * One agent turn's progress, as a projection of the step frames the server sent.
 *
 * The protocol has four frames and **no frame that closes a step** — a client learns a
 * step ended from the next step starting, or from that step's own result frame. That is
 * the shape of the source protocol, so this module carries the closing rule explicitly
 * rather than pretending a `step_completed` exists:
 *
 *   step_started        opens one step, and closes whichever step is still running
 *   tool_started        names the capability on the step with the same sequence
 *   tool_result         closes that step, with the tool's own sentence and its duration
 *   waiting_confirmation a draft this turn stored, asking to be decided where it appeared
 *
 * Identity: `message.started` and every step frame of one turn carry the same `run_id`,
 * so a tracker binds the first one it sees and drops another turn's frames. That binding
 * is also why the frames must never be fed to the answer state machine: `chatStream.js`
 * binds identity the same way, and a step frame arriving first would make the real
 * deltas look foreign.
 *
 * Nothing here invents content. `content` and `message` are the server's own sentences,
 * copied as-is after a type check; the tool's arguments, its raw observation, and every
 * identifier it names (`source_id`, `session_id`, `user_id`, a message id) are not on the
 * wire at all, so they cannot be dropped later — they are never read. One identifier is
 * on the wire, and only because the page cannot ask for a decision without an address to
 * send it to: the draft a confirmation card is about. `toDraftProjection` reads that
 * object field by field, so it stays the only thing a card can ever show.
 */

import { MemoryScope } from '../api/memory.js'

export const StepPhase = {
  Started: 'step_started',
  ToolStarted: 'tool_started',
  ToolResult: 'tool_result',
  WaitingConfirmation: 'waiting_confirmation',
}

/** The two kinds the protocol names. Anything else stays unlabelled. */
export const StepKind = {
  Plan: 'plan',
  Tool: 'tool',
}

export const StepStatus = {
  Running: 'running',
  Completed: 'completed',
  Failed: 'failed',
  Cancelled: 'cancelled',
}

const PHASES = new Set(Object.values(StepPhase))
const KINDS = new Set(Object.values(StepKind))
/** The same three layers the memory page offers, from one shared vocabulary. */
const SCOPES = new Set(Object.values(MemoryScope))
const TERMINAL_STATUSES = new Set([
  StepStatus.Completed,
  StepStatus.Failed,
  StepStatus.Cancelled,
])

function firstString(...candidates) {
  for (const value of candidates) {
    if (typeof value === 'string' && value !== '') return value
  }
  return ''
}

function positiveInt(value) {
  const num = typeof value === 'number' ? value : Number(value)
  return Number.isInteger(num) && num > 0 ? num : 0
}

function nonNegativeInt(value) {
  const num = typeof value === 'number' ? value : Number(value)
  return Number.isFinite(num) && num >= 0 ? Math.floor(num) : null
}

/**
 * Read the draft a `waiting_confirmation` frame offers for a decision, or `null` when it
 * does not carry one that can be acted on.
 *
 * Four fields are read by name and nothing else is copied, so the object a card can render
 * is exactly the object this function returns: an id, a layer, an optional subject, and the
 * sentence. A frame that omits any of the three required ones is not a request this page
 * understands, and the honest answer to that is no card — not a card with a blank in it,
 * and above all not a card assembled by going to look up the newest pending draft, which
 * in a second conversation would be somebody else's.
 */
export function toDraftProjection(raw) {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null
  const draftId = firstString(raw.draft_id)
  const scope = firstString(raw.scope).toLowerCase()
  const content = typeof raw.content === 'string' ? raw.content.trim() : ''
  if (draftId === '' || content === '' || !SCOPES.has(scope)) return null
  // A subject belongs to a topic draft and to nothing else; the server omits it, and a
  // card that drew one onto another layer would be labelling a claim the server did not.
  const topicKey = scope === MemoryScope.Topic ? firstString(raw.topic_key) : ''
  if (scope === MemoryScope.Topic && topicKey === '') return null
  return { draftId, scope, topicKey, content }
}

/**
 * Read one step frame's payload into a step event, or `null` if it is not a step frame.
 *
 * The keys listed here are the whole surface this module can ever see: a payload field
 * that is not named below never becomes a property of the returned object, so a server
 * that grew a `session_id` onto a step could not leak it through here.
 */
export function toStepEvent(frame) {
  const phase = frame && typeof frame.event === 'string' ? frame.event : ''
  if (!PHASES.has(phase)) return null
  let payload = {}
  try {
    payload = JSON.parse(frame.data || '{}')
  } catch {
    return null
  }
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) return null

  return {
    phase,
    runId: firstString(payload.run_id),
    sequence: positiveInt(payload.sequence),
    stepType: (() => {
      const raw = firstString(payload.step_type)
      return KINDS.has(raw) ? raw : ''
    })(),
    toolName: firstString(payload.tool_name),
    content: typeof payload.content === 'string' ? payload.content : '',
    message: typeof payload.message === 'string' ? payload.message : '',
    durationMillis: nonNegativeInt(payload.duration_millis),
    draft: toDraftProjection(payload.memory_draft),
  }
}

export function isStepEventName(name) {
  return PHASES.has(name)
}

export function createStepState({ clientRunId } = {}) {
  return {
    clientRunId: String(clientRunId ?? ''),
    serverRunId: '',
    /** Ordered by the server's dense `sequence`, oldest first. */
    steps: [],
    /**
     * The one draft this turn offered for a decision, or null. One per turn because the
     * server writes at most one per turn: a second would be two cards asking the same
     * question, and the second would have nothing left to decide.
     */
    draft: null,
    ignoredEvents: 0,
  }
}

function withRun(state, event) {
  if (!event.runId) return state
  if (!state.serverRunId) return { ...state, serverRunId: event.runId }
  return state
}

/** A frame from another turn is not evidence about this one. */
function isForeign(state, event) {
  return !!event.runId && !!state.serverRunId && event.runId !== state.serverRunId
}

function drop(state) {
  return { ...state, ignoredEvents: state.ignoredEvents + 1 }
}

function cloneSteps(state) {
  return state.steps.map((step) => ({ ...step }))
}

function indexOfSequence(steps, sequence) {
  for (let index = 0; index < steps.length; index += 1) {
    if (steps[index].sequence === sequence) return index
  }
  return -1
}

function closeRunning(steps, status) {
  for (const step of steps) {
    if (!TERMINAL_STATUSES.has(step.status)) step.status = status
  }
}

/**
 * Apply one step frame. Pure: every branch returns a new object, so a cancelled run's
 * still-draining frames cannot mutate what the next turn is showing.
 */
export function applyStepEvent(state, event) {
  if (!event || typeof event !== 'object' || !PHASES.has(event.phase)) return drop(state)
  if (!event.sequence) return drop(state)

  const owned = withRun(state, event)
  if (isForeign(owned, event)) return drop(owned)
  const steps = cloneSteps(owned)
  const existing = indexOfSequence(steps, event.sequence)

  switch (event.phase) {
    case StepPhase.Started: {
      // A new step means the previous one is over: the protocol sends no closer, and a
      // step still reading "running" after its successor opened would be a claim about
      // work that has demonstrably moved on.
      closeRunning(steps, StepStatus.Completed)
      const kind = event.stepType || (existing >= 0 ? steps[existing].kind : '')
      const label = event.message || (kind === StepKind.Tool ? '工具调用' : '模型规划')
      if (existing >= 0) {
        steps[existing] = { ...steps[existing], kind: kind || steps[existing].kind, label, status: StepStatus.Running }
      } else {
        steps.push({ sequence: event.sequence, kind, label, toolName: '', status: StepStatus.Running, detail: '', durationMillis: null })
      }
      return { ...owned, steps }
    }

    case StepPhase.ToolStarted: {
      if (existing >= 0) {
        steps[existing] = {
          ...steps[existing],
          kind: StepKind.Tool,
          toolName: event.toolName || steps[existing].toolName,
          status: StepStatus.Running,
        }
      } else {
        // The opening frame did not arrive, but this one is real server progress about a
        // call that is running: showing it as a step is reporting a fact, not inventing one.
        steps.push({
          sequence: event.sequence,
          kind: StepKind.Tool,
          label: '工具调用',
          toolName: event.toolName,
          status: StepStatus.Running,
          detail: '',
          durationMillis: null,
        })
      }
      return { ...owned, steps }
    }

    case StepPhase.ToolResult: {
      const duration = event.durationMillis === null ? null : event.durationMillis
      if (existing >= 0) {
        steps[existing] = {
          ...steps[existing],
          kind: StepKind.Tool,
          toolName: event.toolName || steps[existing].toolName,
          status: StepStatus.Completed,
          detail: event.content || steps[existing].detail,
          durationMillis: duration,
        }
      } else {
        steps.push({
          sequence: event.sequence,
          kind: StepKind.Tool,
          label: '工具调用',
          toolName: event.toolName,
          status: StepStatus.Completed,
          detail: event.content,
          durationMillis: duration,
        })
      }
      steps.sort((left, right) => left.sequence - right.sequence)
      return { ...owned, steps }
    }

    case StepPhase.WaitingConfirmation: {
      // The frame is a request to decide, and the projection is the whole of it: without a
      // draft that can be addressed there is nothing to put a button on, so the frame is
      // counted and the screen stays as it was. Nothing is looked up to fill the gap, and a
      // turn already holding a card does not collect a second one.
      if (!event.draft || owned.draft) return drop(owned)
      return { ...owned, draft: event.draft }
    }

    default:
      return drop(owned)
  }
}

/** `message.completed`: whatever is still open is over, and the answer is on screen. */
export function completeSteps(state) {
  if (!state.steps.some((step) => !TERMINAL_STATUSES.has(step.status))) return state
  const steps = cloneSteps(state)
  closeRunning(steps, StepStatus.Completed)
  return { ...state, steps }
}

/** A failed or cancelled turn closes its steps too — never with a fabricated result. */
export function finishSteps(state, status) {
  if (!TERMINAL_STATUSES.has(status) && status !== StepStatus.Failed && status !== StepStatus.Cancelled) {
    return state
  }
  const steps = cloneSteps(state)
  closeRunning(steps, status)
  return { ...state, steps }
}

/** The step a learner should read as "what is happening now", if any. */
export function currentStep(state) {
  for (let index = state.steps.length - 1; index >= 0; index -= 1) {
    if (state.steps[index].status === StepStatus.Running) return state.steps[index]
  }
  return null
}

export function hasSteps(state) {
  return Array.isArray(state?.steps) && state.steps.length > 0
}

/** Display rows for the panel. Identity fields are not part of the shape by design. */
export function stepRows(state) {
  return (state?.steps ?? []).map((step) => ({
    id: `step-${step.sequence}`,
    sequence: step.sequence,
    label: step.toolName ? `${step.label} · ${step.toolName}` : step.label,
    status: step.status,
    detail: step.detail,
    durationMillis: step.durationMillis,
  }))
}

export function formatStepDuration(durationMillis) {
  if (durationMillis === null || typeof durationMillis !== 'number' || !Number.isFinite(durationMillis)) {
    return ''
  }
  if (durationMillis < 1000) return `${durationMillis} 毫秒`
  return `${(durationMillis / 1000).toFixed(1)} 秒`
}
