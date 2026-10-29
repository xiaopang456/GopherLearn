import { beforeEach, describe, expect, it, vi } from 'vitest'

import { useLearning } from '../src/composables/useLearning.js'
import { setUnauthorizedHandler } from '../src/api/http.js'
import { LEARNING_ERROR_COPY } from '../src/api/learning.js'
import { LearningPhase } from '../src/lib/learningRun.js'
import { installFetchStub, jsonResponse } from './helpers/transport.js'
import { authStore } from '../src/auth/store.js'

/**
 * The quiz store: sequencing rules the page cannot reach by clicking.
 *
 * The three that matter most here are the ones a UI can get wrong silently — a
 * question number that moves before the server says so, a submission id that is
 * replaced when the outcome is still unknown (which would grade one answer twice),
 * and a countdown that talks the client into declaring a quiz expired.
 */

const learning = useLearning()

const OPEN = {
  id: 'lsn-O',
  session_id: 'session-N',
  topic: 'RDB',
  status: 'waiting_answer',
  current_question: 1,
  total_questions: 3,
  question: { ordinal: 1, type: 'short_answer', question: '两种做法各解决什么问题？' },
  expires_at: '2030-01-01T00:00:00Z',
}
const SECOND = { ...OPEN, current_question: 2, question: { ...OPEN.question, ordinal: 2 } }
const THIRD = { ...OPEN, current_question: 3, question: { ...OPEN.question, ordinal: 3 } }
const DONE = { ...OPEN, status: 'completed', current_question: 4, question: undefined }
const CANCELLED = { ...OPEN, status: 'cancelled', question: undefined }

let calls

function route(handlers) {
  // Pointing the store at a conversation always reads that conversation's topic-memory
  // consent, so the router answers it by default rather than making every fixture
  // repeat a line. A case that cares about the switch overrides this key explicitly —
  // and every other path still throws when nothing is routed for it.
  const routed = {
    'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: false }),
    ...handlers,
  }
  calls = []
  installFetchStub((url, init) => {
    const path = new URL(url, 'https://api.test').pathname
    const key = `${init?.method ?? 'GET'} ${path}`
    calls.push({ key, path, body: init?.body ? JSON.parse(init.body) : null, url: String(url) })
    const handler = routed[key] ?? routed[path]
    if (!handler) throw new Error(`unrouted: `)
    if (Array.isArray(handler)) {
      // A list of responses lets one route answer differently per call, which is how
      // "the refresh after a refusal sees the moved quiz" gets expressed.
      const nth = calls.filter((call) => call.key === key).length - 1
      return handler[Math.min(nth, handler.length - 1)]
    }
    return typeof handler === 'function' ? handler(calls.length) : handler
  })
}

function viewOf(data) {
  return jsonResponse({ data, error: null })
}

async function openWith(handlers) {
  route(handlers)
  await learning.openSession('session-N')
}

beforeEach(() => {
  learning.resetLearningState()
  authStore.adopt('tok.learn', { id: 'user-1', email: 'learn@example.test', displayName: '' })
  setUnauthorizedHandler(null)
  route({ 'GET /api/v1/learning/sessions': viewOf(OPEN) })
})

describe('resume', () => {
  it('treats an idle conversation as a state, not an error', async () => {
    await openWith({ 'GET /api/v1/learning/sessions': viewOf(null) })

    expect(learning.view.value).toBeNull()
    expect(learning.error.value).toBe('')
    expect(learning.resumeState.value).toBe('ready')
    expect(learning.canAct.value).toBe(false)
  })

  it('keeps "nothing open" and "not yours" two different answers', async () => {
    await openWith({ 'GET /api/v1/learning/sessions': viewOf(null) })
    const idleError = learning.error.value

    await learning.resume()
    route({
      'GET /api/v1/learning/sessions': () =>
        jsonResponse(
          { data: null, error: { code: 'not_found', message: 'resource not found' } },
          { status: 404 },
        ),
    })
    await learning.resume()

    expect(idleError).toBe('')
    expect(learning.error.value).toBe(LEARNING_ERROR_COPY.notFound)
  })

  it('asks once per session and reuses what it already has', async () => {
    await openWith({ 'GET /api/v1/learning/sessions': viewOf(OPEN) })
    const reads = calls.filter((call) => call.key.startsWith('GET')).length

    await learning.openSession('session-N')
    expect(calls.filter((call) => call.key.startsWith('GET')).length).toBe(reads)
  })

  it('drops a resume that lands after the account changed', async () => {
    let release
    route({
      'GET /api/v1/learning/sessions': () =>
        new Promise((resolve) => {
          release = resolve
        }),
    })
    const pending = learning.openSession('session-N')
    learning.resetLearningState()
    release(viewOf(OPEN))
    await pending

    expect(learning.view.value).toBeNull()
    expect(learning.sessionId.value).toBe('')
  })
})

describe('starting a quiz', () => {
  it('posts the topic on the addressed conversation and takes the server card', async () => {
    learning.setTopic('  Redis 持久化 ')
    await learning.openSession('session-N')
    route({ 'POST /api/v1/learning/sessions': () => viewOf(OPEN) })

    expect(await learning.start()).toBe(true)
    const post = calls.find((call) => call.key === 'POST /api/v1/learning/sessions')
    expect(post.url).toContain('session_id=session-N')
    expect(post.body).toEqual({ topic: 'Redis 持久化', collection_ids: [] })
    expect(learning.view.value.topic).toBe('RDB')
    expect(learning.canAct.value).toBe(true)
  })

  it('keeps the card the server sent instead of moving the number itself', async () => {
    await openWith({
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/answer': viewOf(SECOND),
    })
    learning.draft.value = 'A'
    await learning.submitAnswer()

    expect(learning.view.value.currentQuestion).toBe(2)
    expect(learning.question.value.ordinal).toBe(2)
  })

  it('reports a refusal about the topic without inventing a quiz', async () => {
    learning.setTopic('无关主题')
    await learning.openSession('session-N')
    route({
      'POST /api/v1/learning/sessions': () =>
        jsonResponse(
          { data: null, error: { code: 'learning_no_evidence', message: 'too little' } },
          { status: 409 },
        ),
    })

    expect(await learning.start()).toBe(false)
    expect(learning.error.value).toBe(LEARNING_ERROR_COPY.noEvidence)
    expect(learning.view.value?.id).toBe('lsn-O')
  })
})

describe('submission_id', () => {
  it('reuses one id across a lost response, so a retry replays the verdict', async () => {
    let attempt = 0
    await learning.openSession('session-N')
    route({
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/answer': () => {
        attempt += 1
        if (attempt === 1) return new TypeError('Failed to fetch')
        return viewOf(SECOND)
      },
    })

    learning.draft.value = '两种机制我都说明了。'
    expect(await learning.submitAnswer()).toBe(false)
    expect(learning.error.value).toBe(LEARNING_ERROR_COPY.network)

    // The answer is still on the page, because a lost response is not a lost turn.
    expect(learning.draft.value).toBe('两种机制我都说明了。')
    learning.draft.value = '两种机制我都说明了。'
    expect(await learning.submitAnswer()).toBe(true)

    const bodies = calls.filter((call) => call.key.endsWith('/answer')).map((c) => c.body)
    expect(bodies).toHaveLength(2)
    expect(bodies[0].submission_id).toBe(bodies[1].submission_id)
    expect(bodies[0].ordinal).toBe(1)
  })

  it('reuses the id after a 5xx too, since the server may already have graded it', async () => {
    await learning.openSession('session-N')
    let attempt = 0
    route({
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/answer': () => {
        attempt += 1
        if (attempt === 1) {
          return jsonResponse(
            { data: null, error: { code: 'internal_error', message: 'db down' } },
            { status: 500 },
          )
        }
        return viewOf(SECOND)
      },
    })

    learning.draft.value = '答'
    await learning.submitAnswer()
    learning.draft.value = '答'
    await learning.submitAnswer()

    const ids = calls.filter((c) => c.key.endsWith('/answer')).map((c) => c.body.submission_id)
    expect(new Set(ids).size).toBe(1)
  })

  it('mints a fresh id only when the server said this attempt was not scored', async () => {
    await learning.openSession('session-N')
    let attempt = 0
    route({
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/answer': () => {
        attempt += 1
        if (attempt === 1) {
          return jsonResponse(
            { data: null, error: { code: 'learning_grading_failed', message: 'model' } },
            { status: 503 },
          )
        }
        return viewOf(SECOND)
      },
    })

    learning.draft.value = '答'
    expect(await learning.submitAnswer()).toBe(false)
    expect(learning.error.value).toBe(LEARNING_ERROR_COPY.gradingFailed)
    learning.draft.value = '答'
    expect(await learning.submitAnswer()).toBe(true)

    const ids = calls.filter((c) => c.key.endsWith('/answer')).map((c) => c.body.submission_id)
    expect(new Set(ids).size).toBe(2)
  })

  it('starts a new id when the question changes', async () => {
    await openWith({
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/answer': () => {
        const n = calls.filter((c) => c.key.endsWith('/answer')).length
        return viewOf(n === 1 ? SECOND : THIRD)
      },
    })
    learning.draft.value = '第一题'
    await learning.submitAnswer()
    learning.draft.value = '第二题'
    await learning.submitAnswer()

    const ids = calls.filter((c) => c.key.endsWith('/answer')).map((c) => c.body.submission_id)
    expect(new Set(ids).size).toBe(2)
  })

  it('uses a skip id of its own for the same question', async () => {
    await learning.openSession('session-N')
    route({
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/skip': viewOf(SECOND),
    })
    await learning.skip()
    const skip = calls.find((call) => call.key.endsWith('/skip'))
    expect(skip.body).toEqual({ submission_id: expect.any(String) })
    expect(skip.body.submission_id).toMatch(/^sub-/)
  })

  it('refuses a second write while one is in flight', async () => {
    let release
    await learning.openSession('session-N')
    route({
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/answer': () =>
        new Promise((resolve) => {
          release = resolve
        }),
    })
    learning.draft.value = '答'
    const first = learning.submitAnswer()
    const second = await learning.submitAnswer()
    release(viewOf(SECOND))
    await first

    expect(second).toBe(false)
    expect(calls.filter((c) => c.key.endsWith('/answer'))).toHaveLength(1)
  })
})

describe('refusals that mean the server already moved', () => {
  it('re-reads the quiz after an out-of-order answer', async () => {
    await learning.openSession('session-N')
    route({
      // The refresh after the refusal is the only read left in the log, and it
      // answers with the quiz that already moved.
      'GET /api/v1/learning/sessions': viewOf(SECOND),
      'POST /api/v1/learning/sessions/lsn-O/answer': () =>
        jsonResponse(
          { data: null, error: { code: 'learning_question_order', message: 'nope' } },
          { status: 400 },
        ),
    })
    learning.draft.value = '答'
    expect(await learning.submitAnswer()).toBe(false)

    expect(learning.error.value).toBe(LEARNING_ERROR_COPY.questionOrder)
    // `route()` resets the log, so the single GET below is the refresh the sentence
    // promised — and the card on screen is the server's moved quiz, not the client's
    // guess about what question comes next.
    expect(calls.filter((c) => c.key.startsWith('GET'))).toHaveLength(1)
    expect(learning.view.value.currentQuestion).toBe(2)
    expect(learning.question.value.ordinal).toBe(2)
  })

  it('leaves the card alone when the quiz closed under the learner', async () => {
    await learning.openSession('session-N')
    route({
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/answer': () =>
        jsonResponse(
          { data: null, error: { code: 'learning_session_closed', message: 'over' } },
          { status: 409 },
        ),
    })
    learning.draft.value = '答'
    await learning.submitAnswer()

    expect(learning.error.value).toBe(LEARNING_ERROR_COPY.sessionClosed)
    expect(learning.view.value.status).toBe('waiting_answer')
  })
})

describe('ending a quiz', () => {
  it('sends no body for cancel and takes the terminal view', async () => {
    await learning.openSession('session-N')
    route({
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/cancel': viewOf(CANCELLED),
    })

    expect(await learning.cancel()).toBe(true)
    const cancel = calls.find((call) => call.key.endsWith('/cancel'))
    expect(cancel.body).toBeNull()
    expect(learning.view.value.status).toBe('cancelled')
    expect(learning.canAct.value).toBe(false)
    expect(learning.canCancel.value).toBe(false)
    expect(learning.phase.value).toBe(LearningPhase.Cancelled)
  })

  it('keeps a card submittable while it is being prepared, for cancel only', async () => {
    await learning.openSession('session-N')
    route({
      'GET /api/v1/learning/sessions': viewOf({ ...OPEN, status: 'asking', question: undefined }),
      'POST /api/v1/learning/sessions/lsn-O/cancel': viewOf(CANCELLED),
    })
    await learning.resume({ silent: true })

    expect(learning.canAct.value).toBe(false)
    expect(learning.canCancel.value).toBe(true)
    expect(learning.phase.value).toBe(LearningPhase.Starting)
  })
})

describe('the clock', () => {
  it('past the deadline it stops the actions without declaring an expiry', async () => {
    await learning.openSession('session-N')
    route({
      'GET /api/v1/learning/sessions': viewOf({
        ...OPEN,
        expires_at: '2026-01-01T00:00:00Z',
      }),
    })
    await learning.resume({ silent: true })
    learning.tick(Date.parse('2026-01-02T00:00:00Z'))

    expect(learning.canAct.value).toBe(false)
    expect(learning.canCancel.value).toBe(false)
    expect(learning.expiry.value.level).toBe('passed')
    expect(learning.phase.value).toBe(LearningPhase.Answering)
    expect(learning.view.value.status).toBe('waiting_answer')
    expect(learning.expiry.value.text).toContain('服务端')
  })

  it('warns while there is still time, and never blocks on an unknown deadline', async () => {
    await learning.openSession('session-N')
    route({
      'GET /api/v1/learning/sessions': viewOf({ ...OPEN, expires_at: 'not-a-time' }),
    })
    await learning.resume({ silent: true })
    learning.tick(Date.now())

    expect(learning.expiry.value.level).toBe('none')
    expect(learning.canAct.value).toBe(true)
  })

  it('says nothing about the clock once the quiz is terminal', async () => {
    await learning.openSession('session-N')
    route({ 'GET /api/v1/learning/sessions': viewOf({ ...DONE, expires_at: '2026-01-01T00:00:00Z' }) })
    await learning.resume({ silent: true })
    learning.tick(Date.parse('2026-02-01T00:00:00Z'))

    expect(learning.expiry.value.text).toBe('')
  })
})

describe('the projection', () => {
  it('renders the level, the attempt count and the gaps, and no raw numbers', async () => {
    await learning.openSession('session-N')
    route({
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'GET /api/v1/learning/progress': viewOf({
        topic_key: 'rdb',
        topic: 'RDB',
        mastery: 64,
        confidence: 0.16666666666666666,
        valid_attempts: 4,
        level: 'developing',
        weak_points: ['没说清写入开销', '漏了触发点'],
        last_attempt: '2026-09-26T03:00:00Z',
      }),
    })
    await learning.loadProgress()
    const text = learning.progressText.value

    expect(text).toContain('正在形成')
    expect(text).toContain('4 次有效作答')
    expect(text).toContain('没说清写入开销')
    expect(text).toContain('漏了触发点')
    expect(text).not.toContain('64')
    expect(text).not.toContain('0.1666')
    // kept as data, because a later screen may need it
    expect(learning.progress.value.mastery).toBe(64)
  })

  it('calls an unpractised topic "no record yet", not a permission failure', async () => {
    await learning.openSession('session-N')
    route({
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'GET /api/v1/learning/progress': () =>
        jsonResponse(
          { data: null, error: { code: 'not_found', message: 'resource not found' } },
          { status: 404 },
        ),
    })
    await learning.loadProgress()

    expect(learning.progress.value).toBe('absent')
    expect(learning.progressText.value).toBe('这个主题还没有可展示的掌握度记录。')
    expect(learning.progressText.value).not.toContain('无权')
    expect(learning.progressError.value).toBe('')
  })

  it('forgets a stale projection as soon as an answer lands', async () => {
    await learning.openSession('session-N')
    route({
      'GET /api/v1/learning/sessions': [viewOf(OPEN), viewOf(SECOND)],
      'POST /api/v1/learning/sessions/lsn-O/answer': viewOf(SECOND),
      'GET /api/v1/learning/progress': viewOf({
        topic_key: 'rdb',
        topic: 'RDB',
        level: 'developing',
        valid_attempts: 4,
        weak_points: [],
        mastery: 64,
        confidence: 0.6,
      }),
    })
    await learning.loadProgress()
    expect(learning.progress.value).not.toBeNull()

    learning.draft.value = '答'
    await learning.submitAnswer()
    expect(learning.progress.value).toBeNull()
  })
})

describe('session facts', () => {
  it('signs the user out through the shared funnel on a 401 write', async () => {
    const handler = vi.fn()
    setUnauthorizedHandler(handler)
    await learning.openSession('session-N')
    route({
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/answer': () =>
        jsonResponse(
          { data: null, error: { code: 'unauthorized', message: 'token expired' } },
          { status: 401 },
        ),
    })
    learning.draft.value = '答'
    await learning.submitAnswer()

    // http.js reports the rejected bearer once and the store reports the failure it
    // mapped, so the count is not the point: the single funnel is. The real
    // end-to-end check (sign-out plus redirect) lives in learningView.test.js.
    expect(handler.mock.calls.length).toBeGreaterThanOrEqual(1)
  })

  it('wipes everything on sign-out, including the addressed conversation', async () => {
    learning.setTopic('RDB')
    await learning.openSession('session-N')
    learning.draft.value = '草稿内容'

    authStore.signOut()

    expect(learning.view.value).toBeNull()
    expect(learning.sessionId.value).toBe('')
    expect(learning.topic.value).toBe('')
    expect(learning.draft.value).toBe('')
    expect(learning.error.value).toBe('')
    expect(learning.progress.value).toBeNull()
    expect(learning.work.value).toBeNull()
  })

  it('refuses to answer for a conversation it is not looking at', async () => {
    learning.setTopic('RDB')
    await learning.openSession('')
    expect(await learning.start()).toBe(false)
    expect(learning.error.value).toContain('先选一段对话')
  })
})
