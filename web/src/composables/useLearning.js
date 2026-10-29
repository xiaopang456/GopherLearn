import { computed, ref } from 'vue'

import {
  answerLearning,
  cancelLearning,
  learningError,
  LearningNextStep,
  normalizeProgress,
  normalizeQuizView,
  readLearningProgress,
  resumeLearning,
  skipLearning,
  startLearning,
  validateAnswer,
  validateTopic,
} from '../api/learning.js'
import {
  commitTopicMemory,
  mapTopicMemoryError,
  normalizeTopicPreference,
  readTopicMemory,
  TOPIC_MEMORY_COPY,
} from '../api/topicMemory.js'
import { isAbort, reportIfRejected } from '../api/http.js'

import { authStore } from '../auth/store.js'
import { localId } from '../lib/id.js'
import {
  canActOnView,
  canCancelView,
  clearSubmission,
  createSubmissionSlot,
  deriveLearningPhase,
  expiryNote,
  quizDeadline,
  releaseSubmission,
  submissionIdFor,
} from '../lib/learningRun.js'

/**
 * The quiz page's state: one conversation, at most one open quiz, and the three
 * actions a quiz in flight accepts.
 *
 * Module level for the same reason the memory store is: a quiz is suspended state on
 * the server, so leaving the page and coming back must not start a second one. That
 * is also the hazard — this state outlives the component *and* the account — so
 * every read and write is keyed on an epoch, and `resetLearningState` is wired to the
 * one sign-out funnel.
 *
 * What this store never does: invent a progress counter, mark a quiz expired, or
 * report a score. The server's response is the only source for where the learner is,
 * `expired` is applied on read by the store, and the numbers exist only in the
 * projection endpoint.
 */

let epoch = 0

const sessionId = ref('')
const view = ref(null)
/** 'idle' | 'loading' | 'ready' — 'ready' with a null view is the ordinary empty answer. */
const resumeState = ref('idle')
const work = ref(null)
const error = ref('')
const flash = ref('')
const draft = ref('')
const topic = ref('')

const progress = ref(null)
const progressLoading = ref(false)
const progressError = ref('')


/**
 * The conversation's topic-memory consent, exactly as the server last answered.
 *
 * `null` means "not read yet" or "read something we could not interpret" — it is not
 * `false`. Off is a fact this store may only state after a GET said so, because a
 * defaulted switch would let the page tell a learner their memory is being read when
 * nobody ever confirmed that. Nothing here is mirrored to storage: reopening the page
 * re-reads, which is the only way a decision taken in another tab shows up.
 */
const topicMemory = ref(null)
const topicMemoryLoading = ref(false)
const topicMemorySaving = ref(false)
const topicMemoryError = ref('')

const nowMs = ref(Date.now())

let resumeGeneration = 0
let resumeController = null
let progressGeneration = 0
let progressController = null
let topicMemoryGeneration = 0
let topicMemoryController = null

/**
 * One slot for the whole store: at most one question is open, so at most one attempt
 * token is live. `localId` keeps it under the contract's 64-character bound.
 */
const slot = createSubmissionSlot({ makeId: () => localId('sub') })

const phase = computed(() =>
  deriveLearningPhase({
    view: view.value,
    work: work.value,
    resume: resumeState.value,
    error: error.value,
  }),
)
const question = computed(() => view.value?.question ?? null)
const deadline = computed(() => quizDeadline(view.value))
const expiry = computed(() =>
  expiryNote({
    deadline: deadline.value,
    nowMs: nowMs.value,
    status: view.value?.status ?? '',
  }),
)
/**
 * Actions are gated by the server's view plus the clock, never by a local guess about
 * what question comes next. A missing deadline does not block: no known deadline is
 * not evidence that time ran out.
 */
const canAct = computed(
  () => work.value === null && canActOnView(view.value, { deadline: deadline.value, nowMs: nowMs.value }),
)
/**
 * Ending a quiz does not need a question on screen, so it has its own gate: the
 * contract lets `cancelled` be reached from any unfinished state, and a quiz stuck in
 * `created`/`asking` must still be exitable rather than wait out the lifetime.
 *
 * Past the deadline it stops too, because the page has already told the learner that
 * actions are paused until a refresh shows what the server decided.
 */
const canCancel = computed(() => {
  if (work.value !== null || !canCancelView(view.value)) return false
  return !(deadline.value !== null && nowMs.value >= deadline.value)
})
const hasOpenQuiz = computed(() => view.value !== null)
const progressText = computed(() => {
  if (progress.value === 'absent') return '这个主题还没有可展示的掌握度记录。'
  if (progress.value === null) return ''
  const level = LEVEL_LABELS[progress.value.level] ?? '还没有可信的评定'
  const attempts = `${progress.value.validAttempts} 次有效作答`
  const gaps = progress.value.weakPoints.length > 0
    ? `常见的缺口：${progress.value.weakPoints.join('；')}`
    : '还没有记下常见缺口。'
  return `${level}（${attempts}）。${gaps}`
})

const LEVEL_LABELS = {
  insufficient: '样本还不够，暂时给不出等级',
  developing: '正在形成',
  mastered: '已经掌握',
}

function isStale(startedIn) {
  return startedIn !== epoch
}

/** Move the clock forward; the component owns the interval, this only answers. */
function tick(now) {
  const value = typeof now === 'number' ? now : Date.now()
  nowMs.value = value
}

/**
 * Ask the server where this conversation actually is. The empty answer is
 * `200 data:null`, which is a state and not a failure — the most common path through
 * this feature, taken by every client that reloads after finishing a quiz.
 */
async function resume({ silent = false } = {}) {
  const id = sessionId.value
  if (id === '') {
    view.value = null
    resumeState.value = 'idle'
    return null
  }
  const generation = ++resumeGeneration
  const startedIn = epoch
  if (resumeController) resumeController.abort()
  const controller = new AbortController()
  resumeController = controller
  if (!silent) {
    resumeState.value = 'loading'
    error.value = ''
  }

  try {
    const payload = await resumeLearning({ sessionId: id, signal: controller.signal })
    if (isStale(startedIn) || generation !== resumeGeneration) return null
    // `data` is the parsed `data` field: null means "nothing open", per the contract.
    view.value = payload === null || payload === undefined ? null : normalizeQuizView(payload)
    resumeState.value = 'ready'
    if (!silent) flash.value = ''
    if (view.value === null) clearSubmission(slot)
    return view.value
  } catch (caught) {
    if (isAbort(caught)) return null
    // A rejected bearer is a session fact: hand it to the one funnel that signs the
    // user out and moves them, wherever the rejection was detected.
    if (isStale(startedIn) || generation !== resumeGeneration) return null
    const mapped = learningError(caught)
    error.value = mapped.copy
    // A 404 here means the conversation is not ours or was never stored. Saying
    // "idle" would hide that; anything else leaves the last view on screen.
    if (mapped.code === 'not_found') view.value = null

    return null
  } finally {
    if (resumeController === controller) resumeController = null
    if (generation === resumeGeneration && !isStale(startedIn)) {
      resumeState.value = 'ready'
    }
  }
}

/**
 * Drop every claim this store makes about consent. Called on a session switch, on
 * close, and on sign-out: a setting that survived the conversation it belongs to would
 * be shown against the next one.
 */
function clearTopicMemory() {
  topicMemoryGeneration += 1
  if (topicMemoryController) {
    const pending = topicMemoryController
    topicMemoryController = null
    pending.abort()
  }
  topicMemory.value = null
  topicMemoryLoading.value = false
  topicMemorySaving.value = false
  topicMemoryError.value = ''
}

/** Point the store at a conversation. Same address + already loaded ⇒ no refetch. */
async function openSession(id) {
  const next = String(id ?? '').trim()
  if (next === sessionId.value && resumeState.value !== 'idle') return view.value
  sessionId.value = next
  view.value = null
  error.value = ''
  flash.value = ''
  draft.value = ''
  progress.value = null
  clearSubmission(slot)
  clearTopicMemory()
  if (next === '') return null
  const loaded = await resume()
  // The consent belongs to the conversation, so pointing at one reads its answer.
  // Awaiting keeps the page from rendering an enabled switch before the server has
  // said anything at all.
  await loadTopicMemory()
  return loaded
}

function closeSession() {
  sessionId.value = ''
  view.value = null
  resumeState.value = 'idle'
  error.value = ''
  flash.value = ''
  draft.value = ''
  progress.value = null
  clearSubmission(slot)
  clearTopicMemory()
}

/**
 * The one runner behind every write.
 *
 * `work` is the lock: two concurrent writes on one confirmation-style gate have no
 * defined order, and here a double submit would also buy a second grading. On
 * success the response replaces the view wholesale — the store never advances a
 * question number itself. On failure the view is left exactly as it was, because a
 * server-side fault must not cost the learner the card they were looking at.
 */
async function runWrite({ kind, operation, succeed }) {
  if (work.value !== null) return false
  const startedIn = epoch
  work.value = kind
  error.value = ''
  flash.value = ''
  let applied = false

  try {
    const payload = await operation()
    if (isStale(startedIn)) return false
    const next = payload === null || payload === undefined ? null : normalizeQuizView(payload)
    if (next !== null && next.id === '') {
      // An answer without an addressable id is not a quiz view; keeping the old one
      // is better than pretending the write succeeded.
      error.value = '服务返回的内容里找不到这一轮的标识，页面没有替你推进。'
      return false
    }
    view.value = next
    succeed?.(next)
    applied = true
    return true
  } catch (caught) {
    reportIfRejected(caught)
    if (isStale(startedIn)) return false
    const mapped = learningError(caught)
    error.value = mapped.copy
    // The contract's only "send it again under a new id" answers are the two grading
    // refusals. Everything else keeps the id, so a lost response replays the verdict
    // instead of grading the same answer twice.
    if (mapped.action === LearningNextStep.RetryFresh) releaseSubmission(slot)
    // Several refusals mean the server has already moved: the question advanced, the
    // quiz closed under the learner, or another answer holds the grading lease. Their
    // sentences say the page is now showing the server's state, and that promise is
    // kept here — a copy that claimed a refresh without asking would outlive the error
    // and leave the learner staring at a card the server no longer serves.
    if (mapped.action === LearningNextStep.Refresh) await resume({ silent: true })
    return false
  } finally {
    if (!isStale(startedIn)) work.value = null
    // A successful write moved the server's state, so the projection on disk is
    // stale as far as this page is concerned.
    if (applied) progress.value = null
  }
}

async function start() {
  const id = sessionId.value
  if (id === '') {
    error.value = '先选一段对话，练习要挂在具体的一段对话上。'
    return false
  }
  const problem = validateTopic(topic.value)
  if (problem) {
    error.value = problem
    return false
  }
  return runWrite({
    kind: 'start',
    operation: () => startLearning({ sessionId: id, topic: topic.value }),
    succeed: (next) => {
      error.value = ''
      flash.value = next?.question ? '第一轮题目已经准备好了。' : ''
      draft.value = ''
      clearSubmission(slot)
    },
  })
}

async function submitAnswer() {
  const current = view.value
  if (!current || current.question === null) return false
  const problem = validateAnswer(draft.value)
  if (problem) {
    error.value = problem
    return false
  }
  const submissionId = submissionIdFor(slot, {
    quizId: current.id,
    ordinal: current.question.ordinal,
  })
  return runWrite({
    kind: 'answer',
    operation: () =>
      answerLearning({
        quizId: current.id,
        submissionId,
        ordinal: current.question.ordinal,
        answer: draft.value,
      }),
    succeed: (next) => {
      draft.value = ''
      // Accepted and counted is all a client may say: the graded attempt and the
      // projection it caused are stored and echoed nowhere.
      flash.value =
        next && next.status === 'completed'
          ? '最后一题已经作答并计入你的掌握度记录，这一轮到此结束。'
          : '上一题已经计入你的掌握度记录。'
      clearSubmission(slot)
    },
  })
}

async function skip() {
  const current = view.value
  if (!current) return false
  const submissionId = submissionIdFor(slot, {
    quizId: current.id,
    ordinal: current.question?.ordinal ?? 0,
  })
  return runWrite({
    kind: 'skip',
    operation: () => skipLearning({ quizId: current.id, submissionId }),
    succeed: () => {
      // A skipped question was never graded, so nothing about it is counted: the
      // sentence says only what happened.
      flash.value = '这一题已经跳过。'
      draft.value = ''
      clearSubmission(slot)
    },
  })
}

async function cancel() {
  const current = view.value
  if (!current) return false
  const keepGoing =
    typeof window === 'undefined' ||
    typeof window.confirm !== 'function' ||
    window.confirm('结束这一轮？剩下的题目不会再来一次。')
  if (!keepGoing) return false
  return runWrite({
    kind: 'cancel',
    operation: () => cancelLearning({ quizId: current.id }),
    succeed: () => {
      flash.value = '这一轮已经结束。'
      draft.value = ''
      clearSubmission(slot)
    },
  })
}

/**
 * The projection, on request only. Reading it unprompted would fire a 404 for every
 * topic nobody has practised, which looks like a broken page for an ordinary fact.
 */
async function loadProgress() {
  const current = view.value
  const name = current?.topic ?? topic.value
  if (!name) {
    progressError.value = '还没有主题可查。'
    return
  }
  const generation = ++progressGeneration
  const startedIn = epoch
  if (progressController) progressController.abort()
  const controller = new AbortController()
  progressController = controller
  progressLoading.value = true
  progressError.value = ''

  try {
    const payload = await readLearningProgress({ topic: name, signal: controller.signal })
    if (isStale(startedIn) || generation !== progressGeneration) return
    progress.value = payload ? normalizeProgress(payload) : 'absent'
  } catch (caught) {
    if (isAbort(caught)) return
    reportIfRejected(caught)
    if (isStale(startedIn) || generation !== progressGeneration) return
    const mapped = learningError(caught)
    // Not yours and never stored are the same answer on purpose, so they get the
    // same sentence here: "no record yet", not a permission complaint.
    if (mapped.code === 'not_found') {
      progress.value = 'absent'
    } else {
      progressError.value = mapped.copy
    }
  } finally {
    if (progressController === controller) progressController = null
    if (generation === progressGeneration) progressLoading.value = false
  }
}

/**
 * Read this conversation's stored consent. The answer is a boolean or nothing: an
 * unreadable body leaves `topicMemory` null and shows a fixed sentence, because
 * "could not read" must never be rendered as "it is off".
 */
async function loadTopicMemory() {
  const id = sessionId.value
  if (id === '') {
    clearTopicMemory()
    return null
  }
  const generation = ++topicMemoryGeneration
  const startedIn = epoch
  if (topicMemoryController) topicMemoryController.abort()
  const controller = new AbortController()
  topicMemoryController = controller
  topicMemoryLoading.value = true
  topicMemoryError.value = ''

  try {
    const payload = await readTopicMemory({ sessionId: id, signal: controller.signal })
    if (isStale(startedIn) || generation !== topicMemoryGeneration) return null
    const normalized = normalizeTopicPreference(payload)
    topicMemory.value = normalized ? normalized.enabled : null
    if (!normalized) topicMemoryError.value = TOPIC_MEMORY_COPY.unreadable
    return topicMemory.value
  } catch (caught) {
    if (isAbort(caught)) return null
    if (isStale(startedIn) || generation !== topicMemoryGeneration) return null
    // Not yours and never stored get the same sentence, which is what the server's
    // identical 404 is for: no branch here may reveal whose conversation it is.
    topicMemory.value = null
    topicMemoryError.value = mapTopicMemoryError(caught).copy

    return null
  } finally {
    if (topicMemoryController === controller) topicMemoryController = null
    if (generation === topicMemoryGeneration) topicMemoryLoading.value = false
  }
}

/**
 * Write the decision.
 *
 * The lock is its own flag rather than the quiz's `work`, because saving a permission
 * is not an act on the quiz and a pending grading must not make this button look dead.
 * Nothing is applied optimistically: the switch keeps showing the last answer the
 * server gave while the PUT is in flight, then takes the value from the response — the
 * response, not the request, so a server that answers differently than asked is
 * followed rather than argued with. On any failure the previous value comes back, which
 * is the only way "revert on failure" can be tested rather than intended.
 */
async function saveTopicMemory(enabled) {
  if (typeof enabled !== 'boolean') return false
  if (topicMemorySaving.value) return false
  const id = sessionId.value
  if (id === '') {
    topicMemoryError.value = TOPIC_MEMORY_COPY.missingSession
    return false
  }
  const previous = topicMemory.value
  const startedIn = epoch
  // A save is the newest word on this conversation's consent, so it supersedes any
  // read still in flight instead of letting an older GET overwrite the result.
  const generation = ++topicMemoryGeneration
  if (topicMemoryController) {
    const pending = topicMemoryController
    topicMemoryController = null
    pending.abort()
  }
  topicMemorySaving.value = true
  topicMemoryError.value = ''

  try {
    const payload = await commitTopicMemory({ sessionId: id, enabled })
    if (isStale(startedIn) || generation !== topicMemoryGeneration) return false
    const normalized = normalizeTopicPreference(payload)
    if (!normalized) {
      topicMemory.value = previous
      topicMemoryError.value = TOPIC_MEMORY_COPY.unreadable
      return false
    }
    topicMemory.value = normalized.enabled
    return true
  } catch (caught) {
    if (isAbort(caught)) return false
    if (isStale(startedIn) || generation !== topicMemoryGeneration) return false
    topicMemory.value = previous
    topicMemoryError.value = mapTopicMemoryError(caught).copy
    return false
  } finally {
    if (generation === topicMemoryGeneration) topicMemorySaving.value = false
  }
}

function clearFlash() {
  flash.value = ''
}

function clearError() {
  error.value = ''
}

/** Set the topic text without starting anything. */
function setTopic(value) {
  topic.value = String(value ?? '')
}

/** The store keeps no per-account caches beyond this view, so a wipe is complete. */
export function resetLearningState() {
  epoch += 1
  resumeGeneration += 1
  progressGeneration += 1
  if (resumeController) {
    const pending = resumeController
    resumeController = null
    pending.abort()
  }
  if (progressController) {
    const pending = progressController
    progressController = null
    pending.abort()
  }
  sessionId.value = ''
  view.value = null
  resumeState.value = 'idle'
  work.value = null
  error.value = ''

  flash.value = ''
  draft.value = ''
  topic.value = ''
  progress.value = null
  progressLoading.value = false
  progressError.value = ''
  clearSubmission(slot)
  // Consent is per conversation and per account, so it is the first thing that has to
  // go when either changes: leaving it would show the next learner's switch.
  clearTopicMemory()
}

// One registration at module load covers every exit path, because they all funnel
// through `authStore.signOut()`. The auth layer never learns what this store holds.
authStore.subscribeSignOut(resetLearningState)

export function useLearning() {
  return {
    sessionId,
    view,
    question,
    resumeState,
    phase,
    work,
    error,

    flash,
    draft,
    topic,
    expiry,
    canAct,
    canCancel,
    hasOpenQuiz,
    nowMs,
    progress,
    progressLoading,
    progressError,
    progressText,
    topicMemory,
    topicMemoryLoading,
    topicMemorySaving,
    topicMemoryError,
    tick,
    openSession,
    closeSession,
    resume,
    start,
    submitAnswer,
    skip,
    cancel,
    loadProgress,
    loadTopicMemory,
    saveTopicMemory,
    clearTopicMemory,
    setTopic,
    clearFlash,
    clearError,
    resetLearningState,
  }
}
