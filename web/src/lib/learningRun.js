import { isOpenStatus, isTerminalStatus, LearningStatus } from '../api/learning.js'

/**
 * The quiz page's visible state, derived rather than stored.
 *
 * Three rules this file exists to keep straight, because each one was a real bug
 * class elsewhere:
 *
 * 1. **The server's view is the only fact about progress.** `advanced` is a bigger
 *    `current_question` in the response, never a `+1` here. A client that moves its
 *    own question number shows a card the server never served.
 * 2. **`expired` is observed, not decided.** The store applies expiry on read, so the
 *    client can only ever see it in a response. Past the deadline this module stops
 *    offering actions — it does not rename the state.
 * 3. **A submission id is the caller's deduplication token, so its lifetime is the
 *    rule that matters.** `SubmissionSlot` below encodes exactly one policy: keep the
 *    id while the outcome is unknown, replace it only when the server has said this
 *    attempt was not scored.
 */

export const LearningPhase = {
  Idle: 'idle',
  Resuming: 'resuming',
  Starting: 'starting',
  Answering: 'answering',
  Submitting: 'submitting',
  Grading: 'grading',
  Completed: 'completed',
  Cancelled: 'cancelled',
  Expired: 'expired',
  Failed: 'failed',
  Unreadable: 'unreadable',
}

/** What the page may offer next, once an action is in flight or has failed. */
export const LearningWork = {
  None: null,
  Start: 'start',
  Answer: 'answer',
  Skip: 'skip',
  Cancel: 'cancel',
}

const PHASE_TITLES = {
  [LearningPhase.Idle]: '',
  [LearningPhase.Resuming]: '正在看这一轮还在不在。',
  [LearningPhase.Starting]: '正在准备题目。',
  [LearningPhase.Answering]: '轮到你了。',
  [LearningPhase.Submitting]: '正在发送。',
  [LearningPhase.Grading]: '答案已经收到，正在评分。',
  [LearningPhase.Completed]: '这一轮做完了。',
  [LearningPhase.Cancelled]: '你已经结束了这一轮。',
  [LearningPhase.Expired]: '这一轮超时结束了。',
  [LearningPhase.Failed]: '这一步没有成功。',
  [LearningPhase.Unreadable]: '这一页读不懂服务端给的状态。',
}

export function learningPhaseTitle(phase) {
  return PHASE_TITLES[phase] ?? ''
}

/**
 * @param {object} state
 * @param {object|null} state.view      last server-shaped quiz, or null
 * @param {string|null} state.work      the in-flight action, `LearningWork`
 * @param {'idle'|'loading'|'ready'} state.resume
 * @param {string} state.error          visible failure sentence, '' when none
 */
export function deriveLearningPhase({ view = null, work = null, resume = 'idle', error = '' } = {}) {
  // An in-flight action outranks everything: the buttons must lock the moment a tap
  // lands, whatever the last response said.
  if (work === LearningWork.Start) return LearningPhase.Starting
  if (work === LearningWork.Answer || work === LearningWork.Skip || work === LearningWork.Cancel) {
    return LearningPhase.Submitting
  }
  // A failure is an overlay, not a replacement: the card underneath still renders
  // from `view`, because losing the question you were answering would turn a
  // server-side fault into a lost turn.
  if (error !== '') return LearningPhase.Failed
  if (resume === 'loading' && view === null) return LearningPhase.Resuming
  if (view === null) return LearningPhase.Idle

  switch (view.status) {
    case LearningStatus.Expired:
      return LearningPhase.Expired
    case LearningStatus.Cancelled:
      return LearningPhase.Cancelled
    case LearningStatus.Completed:
      return LearningPhase.Completed
    case LearningStatus.Grading:
      return LearningPhase.Grading
    case LearningStatus.Created:
    case LearningStatus.Asking:
      return LearningPhase.Starting
    case LearningStatus.WaitingAnswer:
      return view.question ? LearningPhase.Answering : LearningPhase.Starting
    default:
      // `''` is a status this build does not know. Guessing an open state here would
      // let the learner submit into a quiz that may already be closed.
      return LearningPhase.Unreadable
  }
}

const OPEN_PHASES = [LearningPhase.Answering, LearningPhase.Failed, LearningPhase.Unreadable]

/** Whether the action buttons are usable at all in this phase. */
export function phaseAcceptsActions(phase) {
  return OPEN_PHASES.includes(phase)
}

/**
 * The deadline the server set, as epoch milliseconds. `null` means there is nothing
 * to count down to — a missing or unparsable stamp is *not* read as "already over",
 * because that would disable a quiz the server still considers open.
 */
export function quizDeadline(view) {
  const raw = view?.expiresAt
  if (typeof raw !== 'string' || raw === '') return null
  const parsed = Date.parse(raw)
  return Number.isNaN(parsed) ? null : parsed
}

/** Whole seconds left, clamped at zero, or null when no deadline is known. */
export function remainingSeconds(deadline, nowMs) {
  if (deadline === null || !Number.isFinite(nowMs)) return null
  return Math.max(0, Math.floor((deadline - nowMs) / 1000))
}

/** Warn while there is still time to finish; the warning never blocks anything. */
export const EXPIRY_WARNING_SECONDS = 120

/**
 * What to say about the clock. Past the deadline the page stops offering actions and
 * asks for a refresh, because only the server can say whether the quiz expired — and
 * that is a read, which is exactly what `resume` is for.
 */
export function expiryNote({ deadline, nowMs, status }) {
  if (deadline === null) return { level: 'none', text: '' }
  if (isTerminalStatus(status)) return { level: 'none', text: '' }
  const left = remainingSeconds(deadline, nowMs)
  if (left === null) return { level: 'none', text: '' }
  if (left === 0) {
    return {
      level: 'passed',
      text: '设定时间已经到了，操作先停在这里。刷新看服务端怎么判这一轮——过期由服务端认定，这一页不会替它下结论。',
    }
  }
  if (left <= EXPIRY_WARNING_SECONDS) {
    return { level: 'soon', text: `还剩 ${formatDuration(left)}，作答还来得及。` }
  }
  return { level: 'ok', text: `本轮剩余 ${formatDuration(left)}。` }
}

/** mm:ss under an hour, then h:mm:ss. No fake precision when the clock is unknown. */
export function formatDuration(totalSeconds) {
  const seconds = Math.max(0, Math.floor(Number(totalSeconds) || 0))
  const h = Math.floor(seconds / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const s = seconds % 60
  const pad = (value) => String(value).padStart(2, '0')
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${pad(m)}:${pad(s)}`
}

/**
 * The submission id slot for the current question.
 *
 * `makeId` is injected so the store can use the shared id helper and a test can watch
 * exactly how many ids were minted — the number of ids *is* the deduplication
 * guarantee, so it has to be observable.
 */
export function createSubmissionSlot({ makeId } = {}) {
  const mint = typeof makeId === 'function' ? makeId : () => ''
  return { mint, current: null }
}

/**
 * The id to send for this (quiz, question). Reuse is the whole point: the same id
 * makes a retry land on the stored verdict instead of grading the learner twice.
 */
export function submissionIdFor(slot, { quizId, ordinal }) {
  const id = String(quizId ?? '')
  const position = Number(ordinal)
  if (id === '' || !Number.isInteger(position)) return ''
  if (
    slot.current !== null &&
    slot.current.quizId === id &&
    slot.current.ordinal === position &&
    slot.current.released === false
  ) {
    return slot.current.submissionId
  }
  const submissionId = slot.mint()
  slot.current = { quizId: id, ordinal: position, submissionId, released: false }
  return submissionId
}

/**
 * Release the id: the next ask mints a fresh one.
 *
 * Called only when the server has said this attempt was not scored
 * (`learning_grading_failed` / `learning_grading_invalid`) — the contract's own
 * instruction that the same answer may be sent again under a new id. A transport
 * failure, a timeout or a 5xx must *not* call this: the verdict may already be
 * stored, and a second id would buy a second grading.
 */
export function releaseSubmission(slot) {
  if (slot.current !== null) slot.current = { ...slot.current, released: true }
}

/** Drop the slot entirely: new question, cancel, terminal state, sign-out, account switch. */
export function clearSubmission(slot) {
  slot.current = null
}

export function currentSubmissionId(slot) {
  return slot.current?.released === false ? slot.current.submissionId : ''
}

/**
 * A card is submittable only when the server says the quiz is waiting for an answer
 * and the clock has not run out.
 */
export function quizAcceptsAnswer(view, { deadline = null, nowMs = Number.NaN } = {}) {
  // `waiting_answer` and nothing else: a card in `created`/`asking` does not exist
  // yet, and one in `grading` already has an answer in flight — the server answers a
  // second submit there with `learning_answer_busy`, so offering the button would be
  // offering a refusal.
  if (!view || view.status !== LearningStatus.WaitingAnswer || view.question === null) return false
  if (view.question.ordinal < 1) return false
  if (deadline !== null && Number.isFinite(nowMs) && nowMs >= deadline) return false
  return true
}

/**
 * Whether the learner is allowed to send anything for this view right now.
 * `deadline === null` deliberately does not block: no known deadline is not evidence
 * that time ran out.
 */
export function canActOnView(view, { deadline = null, nowMs = Number.NaN } = {}) {
  return quizAcceptsAnswer(view, { deadline, nowMs })
}

/**
 * Ending a quiz does not need a question on screen: the contract lets `cancelled` be
 * reached from every unfinished state, and a quiz stuck in `created`/`asking` must
 * still be exitable — that is the only way out of a failed generation without
 * waiting for the 24h expiry.
 */
export function canCancelView(view) {
  return !!view && isOpenStatus(view.status)
}
