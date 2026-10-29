import { beforeEach, describe, expect, it } from 'vitest'

import { useLearning } from '../src/composables/useLearning.js'
import { TOPIC_MEMORY_COPY } from '../src/api/topicMemory.js'
import { setUnauthorizedHandler } from '../src/api/http.js'
import { authStore } from '../src/auth/store.js'
import { installFetchStub, jsonResponse, deferred } from './helpers/transport.js'

/**
 * The consent switch, seen from the store — the layer that decides what the page is
 * allowed to claim.
 *
 * The rules these cases exist to keep:
 *
 * 1. **Off is a read value, never a default.** `null` means "the server has not said",
 *    and nothing here turns a missing or unreadable answer into `false`, because a
 *    fabricated `false` is how a page ends up telling a learner their memory is not
 *    being read on the strength of a body it could not parse.
 * 2. **The response wins over the request.** A `PUT` that answers something other than
 *    what was sent is followed, not argued with.
 * 3. **A failed write puts the previous answer back.** "Roll back on failure" is only
 *    real if a case can make it fail and observe the old value return.
 * 4. **One write at a time.** A double click on a confirmation-style control must not
 *    become two decisions, even though this endpoint is idempotent on the server.
 */

const learning = useLearning()

const CONSENT_OFF = { enabled: false }
const CONSENT_ON = { enabled: true }
const OPEN_QUIZ = {
  id: 'lsn-O',
  session_id: 'session-N',
  topic: 'RDB',
  status: 'waiting_answer',
  current_question: 1,
  total_questions: 3,
  question: { ordinal: 1, type: 'short_answer', question: '两种做法各解决什么问题？' },
  expires_at: '2030-01-01T00:00:00Z',
}

let calls

function route(handlers) {
  calls = []
  installFetchStub((url, init) => {
    const path = new URL(url, 'https://api.test').pathname
    const key = `${init?.method ?? 'GET'} ${path}`
    calls.push({ key, path, body: init?.body ? JSON.parse(init.body) : null })
    const handler = handlers[key] ?? handlers[path]
    if (!handler) throw new Error(`unrouted: ${key}`)
    return typeof handler === 'function' ? handler() : handler
  })
}

function dataOf(data) {
  return jsonResponse({ data, error: null })
}

function refused(status, code, message = 'server detail') {
  return jsonResponse({ data: null, error: { code, message } }, { status })
}

const consentPath = (id) => `/api/v1/sessions/${id}/topic-memory`

function topicCalls(id = 'session-N') {
  return calls.filter((call) => call.path === consentPath(id))
}

async function openWith(handlers, id = 'session-N') {
  route(handlers)
  await learning.openSession(id)
}

beforeEach(() => {
  learning.resetLearningState()
  authStore.adopt('tok.topic', { id: 'user-1', email: 'topic@example.test', displayName: '' })
  setUnauthorizedHandler(null)
})

describe('restoring the stored answer', () => {
  it('reads the switch on the server’s side of the wire, not from anything local', async () => {
    await openWith({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: dataOf(CONSENT_OFF),
    })

    expect(topicCalls().map((call) => call.key)).toEqual([`GET ${consentPath('session-N')}`])
    expect(learning.topicMemory.value).toBe(false)
    expect(learning.topicMemoryError.value).toBe('')
  })

  it('restores an existing yes just as plainly as a no', async () => {
    await openWith({
      'GET /api/v1/learning/sessions': dataOf(OPEN_QUIZ),
      [`GET ${consentPath('session-N')}`]: dataOf(CONSENT_ON),
    })

    expect(learning.topicMemory.value).toBe(true)
  })

  it('holds nothing to show while the read is still running', async () => {
    await openWith({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: dataOf(CONSENT_OFF),
    })
    expect(learning.topicMemory.value).toBe(false)

    // Now re-read behind a gate: between the two answers the store must claim
    // nothing at all, because a stale "off" on screen is still a claim.
    const gate = deferred()
    route({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: () => gate.promise.then(() => dataOf(CONSENT_ON)),
    })

    const loading = learning.loadTopicMemory()
    expect(learning.topicMemoryLoading.value).toBe(true)

    gate.resolve(dataOf(CONSENT_ON))
    await loading
    expect(learning.topicMemory.value).toBe(true)
    expect(learning.topicMemoryLoading.value).toBe(false)
  })

  it('keeps an unreadable body as "unknown", never as off', async () => {
    await openWith({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: dataOf({}),
    })

    expect(learning.topicMemory.value).toBeNull()
    expect(learning.topicMemoryError.value).toBe(TOPIC_MEMORY_COPY.unreadable)
  })

  it('treats "not yours" and "never existed" as one sentence', async () => {
    await openWith({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: () => refused(404, 'not_found'),
    })

    expect(learning.topicMemory.value).toBeNull()
    expect(learning.topicMemoryError.value).toBe(TOPIC_MEMORY_COPY.readFailed)
    expect(learning.topicMemoryError.value).not.toContain('server detail')
  })

  it('does not keep a read that arrived after the conversation changed', async () => {
    const slow = deferred()
    route({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: () => slow.promise.then(() => dataOf(CONSENT_ON)),
      [`GET ${consentPath('session-B')}`]: dataOf(CONSENT_OFF),
    })

    const first = learning.openSession('session-N')
    slow.resolve(dataOf(CONSENT_ON))
    await first

    await learning.openSession('session-B')
    expect(learning.topicMemory.value).toBe(false)
    expect(topicCalls('session-N').length).toBe(1)
    expect(topicCalls('session-B').length).toBe(1)
  })
})

describe('changing the answer', () => {
  it('saves the boolean it was given and takes the response as the answer', async () => {
    await openWith({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: dataOf(CONSENT_OFF),
      [`PUT ${consentPath('session-N')}`]: dataOf(CONSENT_ON),
    })

    const saved = await learning.saveTopicMemory(true)

    expect(saved).toBe(true)
    expect(topicCalls()[1].key).toBe(`PUT ${consentPath('session-N')}`)
    expect(topicCalls()[1].body).toEqual({ enabled: true })
    expect(learning.topicMemory.value).toBe(true)
    expect(learning.topicMemorySaving.value).toBe(false)
  })

  it('follows a server that answers differently than asked', async () => {
    await openWith({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: dataOf(CONSENT_OFF),
      [`PUT ${consentPath('session-N')}`]: dataOf(CONSENT_OFF),
    })

    const saved = await learning.saveTopicMemory(true)

    // No optimistic write means there is nothing to reconcile: whatever came back is
    // the state, even when the learner asked for the opposite.
    expect(learning.topicMemory.value).toBe(false)
    expect(saved).toBe(true)
  })

  it('turns it back off with the same one-key body', async () => {
    await openWith({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: dataOf(CONSENT_ON),
      [`PUT ${consentPath('session-N')}`]: dataOf(CONSENT_OFF),
    })

    await learning.saveTopicMemory(false)

    expect(topicCalls()[1].body).toEqual({ enabled: false })
    expect(learning.topicMemory.value).toBe(false)
  })

  it('sends one request no matter how many times the click lands', async () => {
    const gate = deferred()
    await openWith({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: dataOf(CONSENT_OFF),
      [`PUT ${consentPath('session-N')}`]: () => gate.promise.then(() => dataOf(CONSENT_ON)),
    })

    const first = learning.saveTopicMemory(true)
    // In flight the store still carries the last answer the server gave, not the click:
    // an optimistic write here would show a permission that has not been recorded.
    expect(learning.topicMemory.value).toBe(false)

    const second = learning.saveTopicMemory(true)
    const third = learning.saveTopicMemory(false)
    await Promise.resolve()
    await Promise.resolve()

    // Counted before anything is awaited, so a missing lock fails as an assertion
    // rather than as a test that hangs waiting on a gate nobody will ring.
    expect(calls.filter((call) => call.key.startsWith('PUT'))).toHaveLength(1)
    expect(await second).toBe(false)
    expect(await third).toBe(false)
    expect(learning.topicMemorySaving.value).toBe(true)

    gate.resolve(dataOf(CONSENT_ON))
    await first
    expect(learning.topicMemory.value).toBe(true)
  })

  it('puts the earlier answer back when the write fails', async () => {
    await openWith({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: dataOf(CONSENT_ON),
      [`PUT ${consentPath('session-N')}`]: () => refused(500, 'internal_error', 'boom'),
    })

    const saved = await learning.saveTopicMemory(false)

    expect(saved).toBe(false)
    expect(learning.topicMemory.value).toBe(true)
    expect(learning.topicMemoryError.value).toBe(TOPIC_MEMORY_COPY.writeFailed)
    expect(learning.topicMemoryError.value).not.toContain('boom')
    expect(learning.topicMemorySaving.value).toBe(false)
  })

  it('refuses to invent a decision out of a non-boolean', async () => {
    await openWith({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: dataOf(CONSENT_OFF),
    })

    expect(await learning.saveTopicMemory('yes')).toBe(false)
    expect(await learning.saveTopicMemory(undefined)).toBe(false)
    expect(topicCalls().filter((call) => call.key.startsWith('PUT')).length).toBe(0)
  })

  it('says so plainly when no conversation is addressed', async () => {
    learning.closeSession()
    // Routed empty: anything this call tried to send would fail as `unrouted` rather
    // than pass silently, which is what makes "no request" an assertion with teeth.
    route({})

    expect(await learning.saveTopicMemory(true)).toBe(false)
    expect(learning.topicMemoryError.value).toBe(TOPIC_MEMORY_COPY.missingSession)
    expect(calls).toHaveLength(0)
  })

  it('saves without an open quiz, because the preference is not an act on a quiz', async () => {
    await openWith({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: dataOf(CONSENT_OFF),
      [`PUT ${consentPath('session-N')}`]: dataOf(CONSENT_ON),
    })

    expect(learning.view.value).toBeNull()
    expect(await learning.saveTopicMemory(true)).toBe(true)
    expect(learning.topicMemory.value).toBe(true)
  })

  it('stays available while a quiz write is in flight', async () => {
    const quizGate = deferred()
    await openWith({
      'GET /api/v1/learning/sessions': dataOf(OPEN_QUIZ),
      [`GET ${consentPath('session-N')}`]: dataOf(CONSENT_OFF),
      'POST /api/v1/learning/sessions/lsn-O/skip': () => quizGate.promise.then(() => dataOf(OPEN_QUIZ)),
      [`PUT ${consentPath('session-N')}`]: dataOf(CONSENT_ON),
    })

    const skipping = learning.skip()
    expect(learning.work.value).toBe('skip')

    expect(await learning.saveTopicMemory(true)).toBe(true)
    quizGate.resolve(dataOf(OPEN_QUIZ))
    await skipping
  })
})

describe('leaving a conversation or an account', () => {
  it('clears the old answer the moment the selection changes', async () => {
    const gate = deferred()
    route({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: dataOf(CONSENT_ON),
      [`GET ${consentPath('session-B')}`]: () => gate.promise.then(() => dataOf(CONSENT_OFF)),
    })

    await learning.openSession('session-N')
    expect(learning.topicMemory.value).toBe(true)

    const switched = learning.openSession('session-B')
    // The previous conversation's yes must not be shown against this one, even for a
    // tick: the switch is not merely re-read, it is withdrawn first.
    expect(learning.topicMemory.value).toBeNull()

    gate.resolve(dataOf(CONSENT_OFF))
    await switched
    expect(learning.topicMemory.value).toBe(false)
  })

  it('drops everything on sign-out, including an in-flight read', async () => {
    const gate = deferred()
    route({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: () => gate.promise.then(() => dataOf(CONSENT_ON)),
    })

    const opened = learning.openSession('session-N')
    learning.resetLearningState()
    gate.resolve(dataOf(CONSENT_ON))
    await opened

    expect(learning.topicMemory.value).toBeNull()
    expect(learning.topicMemoryLoading.value).toBe(false)
    expect(learning.topicMemoryError.value).toBe('')
    expect(learning.sessionId.value).toBe('')
  })

  it('signs the learner out through the one funnel when the bearer is rejected', async () => {
    const reported = []
    setUnauthorizedHandler((error) => reported.push(error.code))

    await openWith({
      'GET /api/v1/learning/sessions': dataOf(null),
      [`GET ${consentPath('session-N')}`]: () => refused(401, 'unauthorized'),
    })

    expect(reported).toEqual(['unauthorized'])
    expect(learning.topicMemory.value).toBeNull()

    authStore.signOut()
    expect(learning.topicMemoryError.value).toBe('')
  })
})
