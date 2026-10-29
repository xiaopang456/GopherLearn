import { describe, expect, it } from 'vitest'

import {
  answerLearning,
  cancelLearning,
  LEARNING_ERROR_CODES,
  LEARNING_ERROR_COPY,
  LEARNING_PROGRESS_PATH,
  LEARNING_SESSIONS_PATH,
  LearningAction,
  LearningNextStep,
  LearningStatus,
  MAX_ANSWER_RUNES,
  MAX_TOPIC_RUNES,
  learningActionPath,
  learningError,
  learningProgressUrl,
  learningSessionsUrl,
  normalizeProgress,
  normalizeQuizView,
  questionTypeLabel,
  readLearningProgress,
  resumeLearning,
  runeCount,
  skipLearning,
  startLearning,
  validateAnswer,
  validateTopic,
} from '../src/api/learning.js'
import { installFetchStub, jsonResponse } from './helpers/transport.js'
import { setTokenProvider } from '../src/api/http.js'
import { clearDevIssues, devIssues } from '../src/lib/devlog.js'

/**
 * The learning API module is the only place that knows this surface's paths, field
 * names and failure codes, so these cases are the fence: the whitelist in particular
 * is what keeps a server that ever grows a field from widening what can reach the
 * screen.
 *
 * Wire shapes here are copied from `internal/httpapi/testdata/http_contract.golden`,
 * not written from imagination.
 */

const QUIZ_GOLDEN = {
  id: 'lsn-O',
  session_id: 'session-N',
  topic: 'RDB',
  status: 'waiting_answer',
  current_question: 1,
  total_questions: 3,
  question: {
    ordinal: 1,
    type: 'short_answer',
    question: '卷 C 第 1 题：说明两种做法各解决什么问题？',
  },
  expires_at: '2026-09-27T03:40:00Z',
}

beforeEachHelper()
function beforeEachHelper() {
  setTokenProvider(() => 'tok.learning')
}

describe('paths and addressing', () => {
  it('addresses the two listings by query, because session_id is not a body field', () => {
    expect(learningSessionsUrl('session-N')).toBe(`${LEARNING_SESSIONS_PATH}?session_id=session-N`)
    expect(learningProgressUrl('Redis 持久化')).toBe(
      `${LEARNING_PROGRESS_PATH}?topic=${encodeURIComponent('Redis 持久化')}`,
    )
  })

  it('escapes so an id cannot add a path segment', () => {
    expect(learningSessionsUrl('a/b')).toBe(`${LEARNING_SESSIONS_PATH}?session_id=a%2Fb`)
    expect(learningActionPath('a/b', LearningAction.Answer)).toBe(
      `${LEARNING_SESSIONS_PATH}/a%2Fb/answer`,
    )
  })

  it('refuses an empty session id instead of asking for somebody else list', () => {
    clearDevIssues()
    expect(() => learningSessionsUrl('  ')).toThrow(/必须指名一段对话/)
    expect(() => learningSessionsUrl(undefined)).toThrow(/必须指名一段对话/)
    expect(devIssues().map((entry) => entry.kind)).toContain('learning.session_id.missing')
  })

  it('keeps the action name inside the three the route knows', () => {
    clearDevIssues()
    expect(() => learningActionPath('lsn-1', 'grade')).toThrow(/只允许 answer、skip 或 cancel/)
    expect(devIssues().map((entry) => entry.detail.received)).toContain('grade')
  })

  it('refuses an empty topic for the progress query', () => {
    expect(() => learningProgressUrl('')).toThrow(/必须给出主题/)
  })

  it('sends the documented body shape and nothing else', async () => {
    const stub = installFetchStub(() => jsonResponse({ data: QUIZ_GOLDEN, error: null }))
    await startLearning({
      sessionId: 'session-N',
      topic: '  RDB  ',
      collectionIds: ['col-1', '', null],
    })
    const [call] = stub.calls
    expect(call.url).toContain('session_id=session-N')
    expect(call.init.method).toBe('POST')
    expect(JSON.parse(call.init.body)).toEqual({
      topic: 'RDB',
      collection_ids: ['col-1'],
    })
  })

  it('omits collection_ids content rather than sending nulls', async () => {
    const stub = installFetchStub(() => jsonResponse({ data: QUIZ_GOLDEN, error: null }))
    await startLearning({ sessionId: 's', topic: 'AOF' })
    expect(JSON.parse(stub.calls[0].init.body)).toEqual({ topic: 'AOF', collection_ids: [] })
  })

  it('sends submission_id, ordinal and answer for an answer', async () => {
    const stub = installFetchStub(() => jsonResponse({ data: QUIZ_GOLDEN, error: null }))
    await answerLearning({
      quizId: 'lsn-O',
      submissionId: 'sub-1',
      ordinal: 2,
      answer: ' RDB 面向恢复。 ',
    })
    expect(stub.calls[0].url).toBe(`/api/v1/learning/sessions/lsn-O/answer`)
    expect(JSON.parse(stub.calls[0].init.body)).toEqual({
      submission_id: 'sub-1',
      ordinal: 2,
      answer: 'RDB 面向恢复。',
    })
  })

  it('sends submission_id and nothing else for a skip', async () => {
    const stub = installFetchStub(() => jsonResponse({ data: QUIZ_GOLDEN, error: null }))
    await skipLearning({ quizId: 'lsn-O', submissionId: 'sub-1', ordinal: 3 })
    expect(JSON.parse(stub.calls[0].init.body)).toEqual({ submission_id: 'sub-1' })
  })

  it('sends no body at all for cancel, which the route insists on', async () => {
    const stub = installFetchStub(() =>
      jsonResponse({ data: { ...QUIZ_GOLDEN, status: 'cancelled' }, error: null }),
    )
    await cancelLearning({ quizId: 'lsn-O' })
    const [call] = stub.calls
    expect(call.init.method).toBe('POST')
    expect(call.init.body).toBeUndefined()
    expect(call.init.headers['content-type']).toBeUndefined()
  })

  it('keeps a client-side bound from becoming a request', async () => {
    const stub = installFetchStub(() => jsonResponse({ data: QUIZ_GOLDEN, error: null }))
    await expect(
      startLearning({ sessionId: 's', topic: 'x'.repeat(MAX_TOPIC_RUNES + 1) }),
    ).rejects.toMatchObject({ code: 'local_invalid' })
    await expect(
      answerLearning({ quizId: 'q', submissionId: 'sub', ordinal: 1, answer: '   ' }),
    ).rejects.toMatchObject({ code: 'local_invalid' })
    await expect(
      answerLearning({ quizId: 'q', submissionId: '', ordinal: 1, answer: 'a' }),
    ).rejects.toMatchObject({ code: 'local_invalid' })
    expect(stub.calls).toHaveLength(0)
  })
})

describe('the quiz view whitelist', () => {
  it('reads exactly the eight published keys', () => {
    const view = normalizeQuizView(QUIZ_GOLDEN)
    expect(Object.keys(view).sort()).toEqual(
      [
        'currentQuestion',
        'expiresAt',
        'id',
        'question',
        'sessionId',
        'status',
        'topic',
        'totalQuestions',
      ].sort(),
    )
    expect(view.question).toEqual({ ordinal: 1, type: 'short_answer', text: QUIZ_GOLDEN.question.question })
  })

  it('drops topic_key and any projection, which belong to the progress endpoint only', () => {
    const view = normalizeQuizView({
      ...QUIZ_GOLDEN,
      topic_key: 'rdb',
      progress: { mastery: 100, level: 'mastered', weak_points: ['全部'] },
      score: 99,
      feedback: '答得真好',
    })
    expect(view).not.toHaveProperty('topicKey')
    expect(view).not.toHaveProperty('progress')
    expect(view).not.toHaveProperty('score')
    expect(view).not.toHaveProperty('feedback')
    expect(JSON.stringify(view)).not.toContain('mastered')
    expect(JSON.stringify(view)).not.toContain('答得真好')
  })

  it('drops every grading-internal field it is ever handed', () => {
    const polluted = {
      ...QUIZ_GOLDEN,
      user_id: 'user-1',
      reference_answer: 'RDB 面向恢复速度，AOF 面向数据完整性。',
      rubric: [{ point: '指出恢复速度', weight: 25 }],
      evidence: [{ chunk_id: 'chk-1', snapshot: '原文……' }],
      submission_id: 'sub-should-never-come-back',
      lease_token: 'lease-77',
      grading_lease_expires_at: '2026-09-26T04:00:00Z',
      version: 9,
    }
    const text = JSON.stringify(normalizeQuizView(polluted))
    for (const secret of [
      'user-1',
      'RDB 面向恢复速度',
      '指出恢复速度',
      'chk-1',
      'sub-should-never-come-back',
      'lease-77',
      'lease_expires',
    ]) {
      expect(text).not.toContain(secret)
    }
  })

  it('treats a terminal response with no question as no card, not as an empty card', () => {
    const view = normalizeQuizView({
      ...QUIZ_GOLDEN,
      status: LearningStatus.Cancelled,
      question: undefined,
    })
    expect(view.question).toBeNull()
  })

  it('keeps an unknown status as unknown instead of picking one', () => {
    clearDevIssues()
    const view = normalizeQuizView({ ...QUIZ_GOLDEN, status: 'awaiting_review' })
    expect(view.status).toBe('')
    expect(devIssues().map((entry) => entry.kind)).toContain('learning.status.unknown')
  })

  it('drops an unknown question type instead of labelling it', () => {
    const view = normalizeQuizView({
      ...QUIZ_GOLDEN,
      question: { ordinal: 1, type: 'fill_blank', question: '填空' },
    })
    expect(view.question.type).toBe('')
    expect(questionTypeLabel('fill_blank')).toBe('')
  })

  it('refuses to invent numbers out of junk', () => {
    const view = normalizeQuizView({
      id: 'lsn-1',
      status: 'waiting_answer',
      current_question: 'first',
      total_questions: null,
      question: { ordinal: -4, type: 'short_answer', question: 'q' },
      expires_at: 'tomorrow',
    })
    expect(view.currentQuestion).toBe(0)
    expect(view.totalQuestions).toBe(0)
    expect(view.question.ordinal).toBe(0)
    expect(view.expiresAt).toBe('tomorrow')
  })

  it('survives a body that is not an object', () => {
    expect(normalizeQuizView(null).id).toBe('')
    expect(normalizeQuizView('nonsense').status).toBe('')
    expect(normalizeQuizView([]).question).toBeNull()
  })

  it('reads a null data payload as no quiz', async () => {
    installFetchStub(() => jsonResponse({ data: null, error: null }))
    const payload = await resumeLearning({ sessionId: 'session-N' })
    expect(payload).toBeNull()
  })
})

describe('the progress whitelist', () => {
  it('keeps the numbers as data and the level as the readable part', () => {
    const progress = normalizeProgress({
      topic_key: 'aof',
      topic: 'AOF',
      mastery: 64,
      confidence: 0.16666666666666666,
      valid_attempts: 1,
      level: 'insufficient',
      weak_points: ['没说清恢复速度与写入开销的关系'],
      last_attempt: '2026-09-26T03:00:00Z',
    })
    expect(progress).toEqual({
      topicKey: 'aof',
      topic: 'AOF',
      mastery: 64,
      confidence: 0.16666666666666666,
      validAttempts: 1,
      level: 'insufficient',
      weakPoints: ['没说清恢复速度与写入开销的关系'],
      lastAttempt: '2026-09-26T03:00:00Z',
    })
  })

  it('caps weak points at the published eight and drops blanks', () => {
    const progress = normalizeProgress({
      topic_key: 'k',
      weak_points: ['a', '', null, 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j'],
    })
    expect(progress.weakPoints).toHaveLength(8)
    expect(progress.weakPoints[0]).toBe('a')
  })

  it('refuses an out-of-range mastery or confidence instead of clamping them', () => {
    expect(normalizeProgress({ topic_key: 'k', mastery: 140 }).mastery).toBeNull()
    expect(normalizeProgress({ topic_key: 'k', mastery: -1 }).mastery).toBeNull()
    expect(normalizeProgress({ topic_key: 'k', confidence: 1.5 }).confidence).toBeNull()
    expect(normalizeProgress({ topic_key: 'k', mastery: 64 }).mastery).toBe(64)
    expect(normalizeProgress({ topic_key: 'k', confidence: 1 }).confidence).toBe(1)
  })

  it('keeps an unknown level as unknown', () => {
    clearDevIssues()
    expect(normalizeProgress({ topic_key: 'k', level: 'expert' }).level).toBe('')
    expect(devIssues().map((entry) => entry.kind)).toContain('learning.level.unknown')
  })

  it('treats a missing last_attempt as no attempt, not as a date', () => {
    expect(normalizeProgress({ topic_key: 'k' }).lastAttempt).toBe('')
    expect(normalizeProgress({ topic_key: 'k', last_attempt: null }).lastAttempt).toBe('')
  })
})

describe('failure copy', () => {
  it('gives every published learning code its own sentence', () => {
    const seen = new Set()
    for (const code of LEARNING_ERROR_CODES) {
      const mapped = learningError({ code, status: 409, message: 'server detail' })
      expect(mapped.code).toBe(code)
      expect(mapped.copy.length).toBeGreaterThan(8)
      expect(mapped.copy).not.toContain('server detail')
      seen.add(mapped.copy)
    }
    // Ten codes, and the pair that must never collapse: "being scored right now" and
    // "that quiz is over" are different next actions.
    expect(LEARNING_ERROR_CODES).toHaveLength(10)
    expect(seen.size).toBe(LEARNING_ERROR_CODES.length)
  })

  it('separates retry-same from retry-fresh from refresh', () => {
    const actionOf = (code, status = 409) => learningError({ code, status }).action
    expect(actionOf('learning_grading_failed', 503)).toBe(LearningNextStep.RetryFresh)
    expect(actionOf('learning_grading_invalid', 503)).toBe(LearningNextStep.RetryFresh)
    expect(actionOf('learning_generation_failed', 503)).toBe(LearningNextStep.RetrySame)
    expect(actionOf('learning_answer_busy')).toBe(LearningNextStep.Refresh)
    expect(actionOf('learning_question_order', 400)).toBe(LearningNextStep.Refresh)
    expect(actionOf('learning_no_evidence')).toBe(LearningNextStep.ChangeTopic)
    expect(actionOf('learning_unavailable', 500)).toBe(LearningNextStep.None)
  })

  it('answers a 404 with one sentence that cannot be used to probe', () => {
    const mapped = learningError({ code: 'not_found', status: 404, message: 'resource not found' })
    expect(mapped.copy).toBe(LEARNING_ERROR_COPY.notFound)
    // "not yours" and "never stored" are the same answer on this surface, so the
    // client must not render a sentence that tells them apart.
    expect(mapped.copy).not.toContain('无权')
    expect(mapped.copy).not.toContain('不允许')
  })

  it('routes a 401 by code even when the status says something else', () => {
    expect(learningError({ code: 'unauthorized', status: 403 }).code).toBe('unauthorized')
    expect(learningError({ code: 'whatever', status: 401 }).code).toBe('unauthorized')
  })

  it('keeps a 400 that carries a server sentence to itself', () => {
    const mapped = learningError({
      code: 'invalid_request',
      status: 400,
      message: 'topic must be 160 runes or fewer',
    })
    expect(mapped.copy).toBe(LEARNING_ERROR_COPY.invalidRequest)
    expect(mapped.copy).not.toContain('160')
  })

  it('falls back to the service sentence and never echoes', () => {
    const mapped = learningError({
      code: 'internal_error',
      status: 500,
      message: 'sql: connection refused 10.0.3.7',
    })
    expect(mapped.copy).toBe(LEARNING_ERROR_COPY.server)
    expect(mapped.copy).not.toContain('10.0.3.7')
    expect(learningError(undefined).copy).toBe(LEARNING_ERROR_COPY.server)
    expect(learningError({}).code).toBe('http_0')
  })
})

describe('client-side bounds', () => {
  it('counts runes, not UTF-16 units, so a Chinese topic is not cut short', () => {
    expect(runeCount('Redis 持久化')).toBe(9) // 5 + a space + 3 hanzi
    expect(runeCount('😀abc')).toBe(4)
    expect(validateTopic('主'.repeat(MAX_TOPIC_RUNES))).toBeNull()
    expect(validateTopic('主'.repeat(MAX_TOPIC_RUNES + 1))).toContain(String(MAX_TOPIC_RUNES))
  })

  it('names an empty topic and an empty answer differently from a long one', () => {
    expect(validateTopic('   ')).toBe('先写上要练的主题。')
    expect(validateAnswer('')).toContain('跳过')
    expect(validateAnswer('答'.repeat(MAX_ANSWER_RUNES + 1))).toContain(String(MAX_ANSWER_RUNES))
  })

  it('labels the three question types and nothing else', () => {
    expect(questionTypeLabel('short_answer')).toBe('用自己的话解释')
    expect(questionTypeLabel('concept_comparison')).toBe('说明两者的区别')
    expect(questionTypeLabel('code_analysis')).toBe('读一段代码')
    expect(questionTypeLabel('')).toBe('')
  })
})

describe('the progress read', () => {
  it('asks for the topic the learner wrote, encoded', async () => {
    const stub = installFetchStub(() =>
      jsonResponse({
        data: { topic_key: 'k', topic: 'Redis 持久化', level: 'developing', valid_attempts: 4 },
        error: null,
      }),
    )
    await readLearningProgress({ topic: 'Redis 持久化' })
    expect(stub.calls[0].url).toBe(
      '/api/v1/learning/progress?topic=' + encodeURIComponent('Redis 持久化'),
    )
  })
})
