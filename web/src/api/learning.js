import { ApiError, request } from './http.js'
import { recordDevIssue } from '../lib/devlog.js'

/**
 * The learning (quiz) surface: paths, request bodies, response whitelist and the
 * user-visible sentence for each failure — all in this file, so a component never
 * learns a path, a field name or a status code.
 *
 * Nothing below is invented. Every path, key and code comes from
 * `spec/API.md` §"Learning workflow (quiz surface)" and matches the recorded
 * response bytes in `internal/httpapi/testdata/http_contract.golden`.
 *
 * Two rules are worth stating because they are the whole point of this module:
 *
 * 1. **A whitelist, not a mapping.** `normalizeQuizView` copies named keys out and
 *    ignores everything else, so a server that ever adds a field cannot widen what
 *    reaches the screen. The reference answer, the rubric, the evidence snapshot and
 *    any score or feedback text must not be renderable, and the way to guarantee that
 *    is to never let them become a property of anything.
 * 2. **The numbers live in exactly one place.** `topic_key`, `mastery`, `confidence`,
 *    `valid_attempts`, `level` and `weak_points` are read only from
 *    `GET /api/v1/learning/progress`. Quiz responses carry no projection, so the
 *    client has no quiz-shaped field to accidentally display as a verdict.
 *
 * Unlike the memory screen, there is no adapter seam and no environment switch here:
 * every build of this page talks to the real backend. The memory page's
 * mock-by-default flag was a defect that had to be reverted, and a quiz that fakes
 * its own questions would be worse.
 */

export const LEARNING_SESSIONS_PATH = '/api/v1/learning/sessions'
export const LEARNING_PROGRESS_PATH = '/api/v1/learning/progress'

/** The three actions a running quiz accepts, from the contract's action table. */
export const LearningAction = {
  Answer: 'answer',
  Skip: 'skip',
  Cancel: 'cancel',
}

const LEARNING_ACTIONS = Object.values(LearningAction)

/**
 * Seven states, one closed table (spec §"Status machine"). `expired` is applied by the
 * store on read, which is why the client can only ever *observe* it — see
 * `lib/learningRun.js`.
 */
export const LearningStatus = {
  Created: 'created',
  Asking: 'asking',
  WaitingAnswer: 'waiting_answer',
  Grading: 'grading',
  Completed: 'completed',
  Cancelled: 'cancelled',
  Expired: 'expired',
}

const LEARNING_STATUSES = Object.values(LearningStatus)

const TERMINAL_STATUSES = [LearningStatus.Completed, LearningStatus.Cancelled, LearningStatus.Expired]

/** `question` is absent once the quiz is terminal, so status decides openness. */
export function isOpenStatus(status) {
  return LEARNING_STATUSES.includes(status) && !TERMINAL_STATUSES.includes(status)
}

export function isTerminalStatus(status) {
  return TERMINAL_STATUSES.includes(status)
}

/** The three lawful question types. Any other value renders as no type at all. */
export const LearningQuestionType = {
  ShortAnswer: 'short_answer',
  ConceptComparison: 'concept_comparison',
  CodeAnalysis: 'code_analysis',
}

const QUESTION_TYPES = Object.values(LearningQuestionType)

/** A label the learner can read, or '' — never a passthrough of an unknown token. */
export function questionTypeLabel(type) {
  switch (type) {
    case LearningQuestionType.ShortAnswer:
      return '用自己的话解释'
    case LearningQuestionType.ConceptComparison:
      return '说明两者的区别'
    case LearningQuestionType.CodeAnalysis:
      return '读一段代码'
    default:
      return ''
  }
}

/**
 * Bounds this client enforces before asking, so an over-long topic is a disabled
 * button rather than a round trip. Values from the contract; a request that ignores
 * them is still refused by the server.
 */
export const MAX_TOPIC_RUNES = 160
export const MAX_ANSWER_RUNES = 12000
export const MAX_SUBMISSION_ID_RUNES = 64

/** rune count, matching the server's `utf8.RuneCountInString` rather than `.length`. */
export function runeCount(value) {
  const text = String(value ?? '')
  return Array.from(text).length
}

function firstString(...values) {
  for (const value of values) {
    if (typeof value === 'string' && value.trim() !== '') return value.trim()
  }
  return ''
}

function boundedInt(value, { min = 0, max = Number.MAX_SAFE_INTEGER } = {}) {
  const parsed = Number(value)
  if (!Number.isInteger(parsed) || parsed < min || parsed > max) return null
  return parsed
}

/** `confidence` is `min(1, attempts/6)`: a number in [0,1] or nothing. */
function unitRatio(value) {
  if (value === null || value === undefined || value === '') return null
  const parsed = Number(value)
  if (!Number.isFinite(parsed) || parsed < 0 || parsed > 1) return null
  return parsed
}

function stringList(value, limit) {
  if (!Array.isArray(value)) return []
  return value
    .map((item) => firstString(item))
    .filter((item) => item !== '')
    .slice(0, limit)
}

/**
 * `session_id` addresses which conversation's quiz is meant, which is why it is a
 * query parameter and never a body field: a body field would let a request quiz a
 * conversation the URL never named. Built here so no component can hand-encode it.
 */
export function learningSessionsUrl(sessionId) {
  const id = firstString(sessionId)
  if (id === '') {
    recordDevIssue('learning.session_id.missing', {})
    throw new Error('练习接口必须指名一段对话。')
  }
  return `${LEARNING_SESSIONS_PATH}?session_id=${encodeURIComponent(id)}`
}

export function learningActionPath(quizId, action) {
  const id = firstString(quizId)
  if (id === '') throw new Error('练习操作需要测验标识。')
  if (!LEARNING_ACTIONS.includes(action)) {
    recordDevIssue('learning.action.unknown', { received: action })
    throw new Error('练习操作只允许 answer、skip 或 cancel。')
  }
  return `${LEARNING_SESSIONS_PATH}/${encodeURIComponent(id)}/${encodeURIComponent(action)}`
}

export function learningProgressUrl(topic) {
  const text = firstString(topic)
  if (text === '') {
    recordDevIssue('learning.topic.missing', {})
    throw new Error('查询掌握度记录必须给出主题。')
  }
  return `${LEARNING_PROGRESS_PATH}?topic=${encodeURIComponent(text)}`
}

/**
 * Question card as the learner may see it: three keys. The server does not send more,
 * and this would drop more if it ever did.
 */
function normalizeQuestion(raw) {
  const source = raw && typeof raw === 'object' && !Array.isArray(raw) ? raw : {}
  const type = QUESTION_TYPES.includes(source.type) ? source.type : ''
  if (!QUESTION_TYPES.includes(source.type) && source.type !== undefined) {
    recordDevIssue('learning.question_type.unknown', { received: source.type })
  }
  return {
    ordinal: boundedInt(source.ordinal, { min: 1, max: 1000 }) ?? 0,
    type,
    text: firstString(source.question),
  }
}

/**
 * One quiz view, exactly the eight documented keys. `topic_key` and `progress` are
 * deliberately absent: the first is the server's join label, and the second does not
 * exist on this response at all.
 */
export function normalizeQuizView(raw) {
  const source = raw && typeof raw === 'object' && !Array.isArray(raw) ? raw : {}
  const status = LEARNING_STATUSES.includes(source.status) ? source.status : ''
  if (!LEARNING_STATUSES.includes(source.status) && source.status !== undefined) {
    recordDevIssue('learning.status.unknown', { received: source.status })
  }
  return {
    id: firstString(source.id),
    sessionId: firstString(source.session_id),
    topic: firstString(source.topic),
    status,
    currentQuestion: boundedInt(source.current_question, { min: 0, max: 1000 }) ?? 0,
    totalQuestions: boundedInt(source.total_questions, { min: 0, max: 1000 }) ?? 0,
    question: source.question ? normalizeQuestion(source.question) : null,
    expiresAt: firstString(source.expires_at),
  }
}

const PROGRESS_LEVELS = ['insufficient', 'developing', 'mastered']

/**
 * The projection — the only response allowed to carry numbers, and the only place
 * this client reads them from. `level` is what gets displayed; `mastery` and
 * `confidence` are kept as normalized data because a later screen may need them,
 * and are never rendered (see `lib/learningRun.js`'s absence test).
 */
export function normalizeProgress(raw) {
  const source = raw && typeof raw === 'object' && !Array.isArray(raw) ? raw : {}
  const level = PROGRESS_LEVELS.includes(source.level) ? source.level : ''
  if (!PROGRESS_LEVELS.includes(source.level) && source.level !== undefined) {
    recordDevIssue('learning.level.unknown', { received: source.level })
  }
  return {
    topicKey: firstString(source.topic_key),
    topic: firstString(source.topic),
    mastery: boundedInt(source.mastery, { min: 0, max: 100 }),
    confidence: unitRatio(source.confidence),
    validAttempts: boundedInt(source.valid_attempts, { min: 0, max: 100000 }) ?? 0,
    level,
    weakPoints: stringList(source.weak_points, 8),
    lastAttempt: firstString(source.last_attempt),
  }
}

/**
 * Failure copy, owned here. Two rules:
 *
 * - `message` from the server is never echoed. Every sentence below is ours.
 * - Cross-owner and never-stored are the *same* 404 on this surface, so they get the
 *   same sentence; a copy that said "you are not allowed to see this quiz" would turn
 *   an indistinguishable answer back into a probe.
 */
export const LEARNING_ERROR_COPY = {
  sessionActive: '这个对话已经有一轮没结束。先把它做完或点"结束这一轮"，再开新的。',
  answerBusy: '上一题还在评分中。稍等一下，不用重复提交。',
  submissionConflict: '这次提交和之前那次对不上，页面已经刷新为服务端的状态。',
  sessionClosed: '这一轮已经结束了，不能再接收作答。',
  noEvidence: '你选的资料里没有这个主题的内容，出题至少需要一段相关笔记。',
  questionOrder: '题目已经往前走了，这一条没有发出去。页面已刷新。',
  generationFailed: '出题没有成功。可以再试一次，或者换一个主题。',
  gradingFailed: '评分没有成功，你的答案没有被算进去。可以重新提交这一题。',
  gradingInvalid: '评分结果没能用上，你的答案没有被算进去。可以重新提交这一题。',
  unavailable: '这个服务还没有配置可用的模型，练习暂时开不了。',
  notFound: '这段对话或这一轮不存在，或者不属于当前账号。',
  invalidRequest: '这个请求没有被接受，内容没有被改动。',
  unauthorized: '登录状态已经失效，请重新登入。',
  network: '无法连接到服务，这次提交没有发出。',
  server: '服务没有完成这个操作，你的输入还在这页上。',
}

/**
 * Machine codes the contract publishes for this surface. Listed so a test can prove
 * each one has its own sentence: "an answer is being scored" and "that quiz is over"
 * are both 409 and only one of them is worth retrying.
 */
export const LEARNING_ERROR_CODES = [
  'learning_session_active',
  'learning_answer_busy',
  'learning_submission_conflict',
  'learning_session_closed',
  'learning_no_evidence',
  'learning_question_order',
  'learning_generation_failed',
  'learning_grading_failed',
  'learning_grading_invalid',
  'learning_unavailable',
]

const CODE_COPY = {
  learning_session_active: LEARNING_ERROR_COPY.sessionActive,
  learning_answer_busy: LEARNING_ERROR_COPY.answerBusy,
  learning_submission_conflict: LEARNING_ERROR_COPY.submissionConflict,
  learning_session_closed: LEARNING_ERROR_COPY.sessionClosed,
  learning_no_evidence: LEARNING_ERROR_COPY.noEvidence,
  learning_question_order: LEARNING_ERROR_COPY.questionOrder,
  learning_generation_failed: LEARNING_ERROR_COPY.generationFailed,
  learning_grading_failed: LEARNING_ERROR_COPY.gradingFailed,
  learning_grading_invalid: LEARNING_ERROR_COPY.gradingInvalid,
  learning_unavailable: LEARNING_ERROR_COPY.unavailable,
  not_found: LEARNING_ERROR_COPY.notFound,
  invalid_request: LEARNING_ERROR_COPY.invalidRequest,
  unauthorized: LEARNING_ERROR_COPY.unauthorized,
  network_error: LEARNING_ERROR_COPY.network,
}

/**
 * What the page may offer next. This is a real distinction, not decoration:
 * `retry_same` must reuse the submission id, `retry_fresh` must not, and `refresh`
 * means the server already moved and the client's view is stale.
 */
const CODE_ACTION = {
  learning_session_active: 'refresh',
  learning_answer_busy: 'refresh',
  learning_submission_conflict: 'refresh',
  learning_session_closed: 'refresh',
  learning_no_evidence: 'change_topic',
  learning_question_order: 'refresh',
  learning_generation_failed: 'retry_same',
  learning_grading_failed: 'retry_fresh',
  learning_grading_invalid: 'retry_fresh',
  learning_unavailable: 'none',
  not_found: 'restart',
  invalid_request: 'none',
  network_error: 'retry_same',
}

export const LearningNextStep = {
  Refresh: 'refresh',
  RetrySame: 'retry_same',
  RetryFresh: 'retry_fresh',
  ChangeTopic: 'change_topic',
  Restart: 'restart',
  None: 'none',
}

/** @returns {{copy: string, code: string, action: string}} */
export function learningError(error) {
  const code = typeof error?.code === 'string' ? error.code : ''
  const status = Number(error?.status) || 0

  if (status === 401 || code === 'unauthorized') {
    return { copy: LEARNING_ERROR_COPY.unauthorized, code: 'unauthorized', action: LearningNextStep.None }
  }
  if (code && CODE_COPY[code]) {
    return {
      copy: CODE_COPY[code],
      code,
      action: CODE_ACTION[code] ?? LearningNextStep.None,
    }
  }
  if (code === 'network_error') {
    return { copy: LEARNING_ERROR_COPY.network, code, action: LearningNextStep.RetrySame }
  }
  return {
    copy: LEARNING_ERROR_COPY.server,
    code: code || `http_${status || 0}`,
    action: status >= 500 ? LearningNextStep.RetrySame : LearningNextStep.None,
  }
}

/**
 * Topic validation in runes, matching the server's bound. Returns a sentence (in
 * this module's voice) or null when the topic may be sent.
 */
export function validateTopic(topic) {
  const text = firstString(topic)
  if (text === '') return '先写上要练的主题。'
  if (runeCount(text) > MAX_TOPIC_RUNES) {
    return `主题 ${runeCount(text)} 字，超过 ${MAX_TOPIC_RUNES} 字上限。`
  }
  return null
}

export function validateAnswer(answer) {
  const text = firstString(answer)
  if (text === '') return '作答不能是空的。跳过这题请点"跳过"。'
  if (runeCount(text) > MAX_ANSWER_RUNES) {
    return `作答 ${runeCount(text)} 字，超过 ${MAX_ANSWER_RUNES} 字上限。`
  }
  return null
}

/**
 * `http.js` hands a caller the whole envelope when `data` is null — its unwrap is
 * `payload.data ?? payload`, which cannot tell "explicitly no resource" from "field
 * missing". On this surface a null `data` *is* the answer: it is how
 * `GET /api/v1/learning/sessions` says "this conversation has nothing open", the most
 * common path through the feature. Unwrapping it here keeps every caller from having
 * to recognize the envelope shape, and leaves the shared reader alone while the
 * wider question is decided.
 */
function unwrap(payload) {
  if (
    payload && typeof payload === "object" && !Array.isArray(payload) &&
    Object.prototype.hasOwnProperty.call(payload, "data") &&
    Object.prototype.hasOwnProperty.call(payload, "error") &&
    payload.data === null &&
    (payload.error === null || payload.error === undefined)
  ) {
    return null
  }
  return payload
}

/**
 * Start a quiz on one conversation. `collection_ids` omitted or empty means
 * "everything this user has stored"; it is never sent as null.
 */
export function startLearning({ sessionId, topic, collectionIds = [] } = {}) {
  const topicText = firstString(topic)
  const tooLong = validateTopic(topicText)
  if (tooLong) return Promise.reject(new ApiError(tooLong, { code: 'local_invalid', status: 0 }))
  const ids = Array.isArray(collectionIds)
    ? collectionIds.map((id) => firstString(id)).filter((id) => id !== '')
    : []
  return request(learningSessionsUrl(sessionId), {
    method: 'POST',
    body: { topic: topicText, collection_ids: ids },
  }).then(unwrap)
}

export function resumeLearning({ sessionId, signal } = {}) {
  return request(learningSessionsUrl(sessionId), { signal }).then(unwrap)
}

export function answerLearning({ quizId, submissionId, ordinal, answer, signal } = {}) {
  const id = firstString(submissionId)
  const text = firstString(answer)
  if (id === '' || runeCount(id) > MAX_SUBMISSION_ID_RUNES) {
    return Promise.reject(
      new ApiError('提交标识缺失或过长，这次没有发出。', { code: 'local_invalid', status: 0 }),
    )
  }
  const tooLong = validateAnswer(text)
  if (tooLong) return Promise.reject(new ApiError(tooLong, { code: 'local_invalid', status: 0 }))
  const position = boundedInt(ordinal, { min: 1, max: 1000 })
  if (position === null) {
    return Promise.reject(
      new ApiError('题号缺失，这次没有发出。', { code: 'local_invalid', status: 0 }),
    )
  }
  return request(learningActionPath(quizId, LearningAction.Answer), {
    method: 'POST',
    body: { submission_id: id, ordinal: position, answer: text },
    signal,
  }).then(unwrap)
}

/** Skip carries `submission_id` and nothing else — the contract refuses extra keys. */
export function skipLearning({ quizId, submissionId } = {}) {
  const id = firstString(submissionId)
  if (id === '' || runeCount(id) > MAX_SUBMISSION_ID_RUNES) {
    return Promise.reject(
      new ApiError('提交标识缺失或过长，这次没有发出。', { code: 'local_invalid', status: 0 }),
    )
  }
  return request(learningActionPath(quizId, LearningAction.Skip), {
    method: 'POST',
    body: { submission_id: id },
  }).then(unwrap)
}

/** Cancel takes no body at all: an empty body or `{}` is accepted, anything is 400. */
export function cancelLearning({ quizId } = {}) {
  return request(learningActionPath(quizId, LearningAction.Cancel), {
    method: 'POST',
  }).then(unwrap)
}

export function readLearningProgress({ topic, signal } = {}) {
  return request(learningProgressUrl(topic), { signal }).then(unwrap)
}
