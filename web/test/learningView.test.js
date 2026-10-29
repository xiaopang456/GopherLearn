import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory } from 'vue-router'

import LearningView from '../src/views/LearningView.vue'
import { createAppRouter } from '../src/router/index.js'
import { authStore } from '../src/auth/store.js'
import { useLearning } from '../src/composables/useLearning.js'
import { LEARNING_ERROR_COPY } from '../src/api/learning.js'
import { installFetchStub, jsonResponse, deferred } from './helpers/transport.js'
import { setTokenProvider } from '../src/api/http.js'

/**
 * The practice page end to end: what each server state puts on screen, what never
 * reaches the screen, and the two paths a quiz page can get wrong in the user's
 * favour — refreshing into a broken-looking idle state, or showing a verdict the
 * contract never sends.
 */

const learning = useLearning()

// A quiz may only be started on a learning-agent conversation, so the fixture the whole
// file shares is one - see the "only a learning agent can be practised on" block below for
// what happens when the account has no such conversation.
const SESSIONS = { items: [{ id: 'session-N', title: 'Redis 持久化', mode: 'learning_agent' }] }

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
const DONE = { ...OPEN, status: 'completed', current_question: 4, question: undefined }
const CANCELLED = { ...OPEN, status: 'cancelled', question: undefined }
const EXPIRED = { ...OPEN, status: 'expired', question: undefined }

let calls

function route(handlers) {
  // The page reads the conversation's topic-memory consent as soon as it points at a
  // session, so that one GET is answered by default. Cases that exercise the switch
  // override the key; every other path still fails loudly when nothing is routed.
  const routed = {
    'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: false }),
    ...handlers,
  }
  calls = []
  installFetchStub((url, init) => {
    const path = new URL(url, 'https://api.test').pathname
    const key = `${init?.method ?? 'GET'} ${path}`
    calls.push({ key, body: init?.body ? JSON.parse(init.body) : null })
    const handler = routed[key] ?? routed[path]
    if (!handler) throw new Error(`unrouted: ${key}`)
    if (Array.isArray(handler)) {
      const nth = calls.filter((call) => call.key === key).length - 1
      return handler[Math.min(nth, handler.length - 1)]
    }
    return typeof handler === 'function' ? handler() : handler
  })
}

function viewOf(data) {
  return jsonResponse({ data, error: null })
}

function refused(status, code, message = 'server detail') {
  return jsonResponse({ data: null, error: { code, message } }, { status })
}

async function openPage(handlers, options = {}) {
  route(handlers)
  const app = mount(LearningView, { attachTo: document.body, ...options })
  await flushPromises()
  await flushPromises()
  return app
}

async function answerWith(app, text) {
  await app.find('[data-test="learn-answer"]').setValue(text)
  await app.find('[data-test="learn-submit"]').trigger('click')
  await flushPromises()
  await flushPromises()
}

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'tok.learn')
  learning.resetLearningState()
  authStore.adopt('tok.learn', { id: 'user-1', email: 'learn@example.test', displayName: '' })
})

describe('the empty answer is a state', () => {
  it('says nothing is open, without an error banner', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(null),
    })

    expect(app.find('[data-test="learn-idle"]').exists()).toBe(true)
    expect(app.text()).toContain('当前没有进行中的练习')
    expect(app.find('[data-test="learn-error"]').exists()).toBe(false)
    expect(app.find('[data-test="learn-card"]').exists()).toBe(false)
    app.unmount()
  })

  it('keeps a 404 visibly different from an idle conversation', async () => {
    // The contract folds "not yours", "never stored" and "unknown action" into one
    // 404 and keeps "nothing open" as a 200, so the page must not render the second
    // case as a failure or the first as an empty inbox.
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': refused(404, 'not_found'),
    })

    expect(app.find('[data-test="learn-error"]').text()).toContain(LEARNING_ERROR_COPY.notFound)
    expect(app.find('[data-test="learn-idle"]').exists()).toBe(false)
    expect(app.text()).not.toContain('当前没有进行中的练习')
    expect(app.text()).not.toContain('server detail')
    app.unmount()
  })

  it('offers the first conversation without a click', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(OPEN),
    })

    expect(app.find('[data-test="learn-session-picker"]').element.value).toBe('session-N')
    app.unmount()
  })
})

describe('the card', () => {
  it('renders the question as text and the position the server sent', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(OPEN),
    })

    expect(app.find('[data-test="learn-question"]').text()).toBe('两种做法各解决什么问题？')
    expect(app.text()).toContain('第 1 / 3 题')
    expect(app.find('[data-test="learn-kind"]').text()).toBe('用自己的话解释')
    expect(app.find('[data-test="learn-status"]').text()).toBe('等你作答')
    app.unmount()
  })

  it('never lets question text become markup', async () => {
    const nasty = {
      ...OPEN,
      question: {
        ordinal: 1,
        type: 'short_answer',
        question: '<img src=x onerror="alert(1)"> 说明这两种做法',
      },
    }
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(nasty),
    })

    expect(app.find('[data-test="learn-question"]').text()).toContain('<img src=x')
    expect(app.find('img').exists()).toBe(false)
    app.unmount()
  })

  it('shows the preparing state without a card when no question is open', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf({ ...OPEN, status: 'asking', question: undefined }),
    })

    expect(app.find('[data-test="learn-no-question"]').text()).toContain('题目还在准备')
    expect(app.find('[data-test="learn-answer"]').exists()).toBe(false)
    // …but the quiz can still be ended, which is the only way out of a stuck one.
    expect(app.find('[data-test="learn-cancel"]').attributes('disabled')).toBeUndefined()
    app.unmount()
  })

  it('locks both submits while an answer is being scored', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf({ ...OPEN, status: 'grading' }),
    })

    expect(app.text()).toContain('答案已经收到，正在评分')
    expect(app.find('[data-test="learn-submit"]').attributes('disabled')).toBeDefined()
    expect(app.find('[data-test="learn-skip"]').attributes('disabled')).toBeDefined()
    app.unmount()
  })
})

describe('starting and answering', () => {
  it('starts on the addressed conversation and takes the first card from the server', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(null),
      'POST /api/v1/learning/sessions': viewOf(OPEN),
    })

    await app.find('[data-test="learn-topic"]').setValue('RDB 与 AOF')
    expect(app.find('[data-test="learn-start"]').attributes('disabled')).toBeUndefined()
    await app.find('[data-test="learn-start"]').trigger('click')
    await flushPromises()
    await flushPromises()

    const post = calls.find((call) => call.key === 'POST /api/v1/learning/sessions')
    expect(post.body).toEqual({ topic: 'RDB 与 AOF', collection_ids: [] })
    expect(app.find('[data-test="learn-question"]').exists()).toBe(true)
    app.unmount()
  })

  it('sends one answer per double click', async () => {
    let release
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/answer': () =>
        new Promise((resolve) => {
          release = () => resolve(viewOf(SECOND))
        }),
    })

    await app.find('[data-test="learn-answer"]').setValue('说完了')
    await app.find('[data-test="learn-submit"]').trigger('click')
    await app.find('[data-test="learn-submit"]').trigger('click')
    expect(calls.filter((call) => call.key.endsWith('/answer'))).toHaveLength(1)

    release()
    await flushPromises()
    await flushPromises()
    expect(app.find('[data-test="learn-question"]').exists()).toBe(true)
    app.unmount()
  })

  it('reports an accepted answer as counted, and only as counted', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/answer': viewOf(SECOND),
    })
    await answerWith(app, 'RDB 做快照，AOF 做追加。')

    expect(app.find('[data-test="learn-flash"]').text()).toBe(
      '上一题已经计入你的掌握度记录。',
    )
    expect(app.text()).toContain('第 2 / 3 题')
    // no invented verdict anywhere next to it
    expect(app.text()).not.toContain('正确')
    expect(app.text()).not.toContain('得分')
    app.unmount()
  })

  it('keeps the card and the draft when the send fails', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/answer': () =>
        new TypeError('Failed to fetch'),
    })
    await answerWith(app, '我的一半答案')

    expect(app.find('[data-test="learn-error"]').text()).toContain(LEARNING_ERROR_COPY.network)
    expect(app.find('[data-test="learn-question"]').exists()).toBe(true)
    expect(app.find('[data-test="learn-answer"]').element.value).toBe('我的一半答案')
    app.unmount()
  })

  it('shows the counting sentence once the quiz is finished', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf({ ...DONE, topic: 'RDB' }),
    })

    expect(app.find('[data-test="learn-counted"]').exists()).toBe(true)
    expect(app.find('[data-test="learn-cancel"]').attributes('disabled')).toBeDefined()
    // a terminal quiz must not lock the topic field: a new round is lawful
    expect(app.find('[data-test="learn-topic"]').attributes('disabled')).toBeUndefined()
    app.unmount()
  })

  it('names a cancelled and an expired quiz differently, by what the server said', async () => {
    const cancelled = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(CANCELLED),
    })
    expect(cancelled.find('[data-test="learn-cancelled-note"]').exists()).toBe(true)
    expect(cancelled.find('[data-test="learn-expired-note"]').exists()).toBe(false)
    cancelled.unmount()

    const expired = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(EXPIRED),
    })
    expect(expired.find('[data-test="learn-expired-note"]').exists()).toBe(true)
    expect(expired.find('[data-test="learn-status"]').text()).toBe('已超时')
    expired.unmount()
  })
})

describe('what must never reach the screen', () => {
  it('ignores grading internals a server might one day send', async () => {
    const polluted = {
      ...OPEN,
      topic_key: 'rdb-secret-key',
      user_id: 'user-7',
      submission_id: 'sub-from-server',
      reference_answer: '参考答案：RDB 面向恢复速度。',
      rubric: ['指出恢复速度', '指出写入开销'],
      evidence: [{ chunk_id: 'chk-9', snapshot: '证据原文片段' }],
      score: 88,
      feedback: '这道题答得不错',
      mastery: 91,
      confidence: 0.95,
      lease_token: 'lease-42',
    }
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(polluted),
    })
    // The countdown is the one legitimately numeric thing on this page, and its digits
    // are a function of the wall clock: scanning the whole page for a bare `91` would
    // pass at one hour and fail at the next (it did — "本轮剩余 28591:37:28"). So the
    // clock node is excluded from the scan rather than the check being weakened.
    const scanned = app.element.cloneNode(true)
    scanned.querySelector('[data-test="learn-clock"]')?.remove()
    const text = scanned.textContent.trim()
    expect(app.find('[data-test="learn-clock"]').text()).toMatch(/\d/)

    for (const leak of [
      'rdb-secret-key',
      'user-7',
      'sub-from-server',
      '参考答案：RDB 面向恢复速度。',
      '指出恢复速度',
      'chk-9',
      '证据原文片段',
      '这道题答得不错',
      'lease-42',
      'reference_answer',
      'rubric',
      'lease_token',
    ]) {
      expect(text).not.toContain(leak)
    }
    // a bare score must not appear either, in any of the shapes it could take
    expect(text).not.toMatch(/88\s*分/)
    expect(text).not.toMatch(/88\s*\/\s*100/)
    expect(text).not.toContain('91')
    app.unmount()
  })

  it('does not show a projection that rode along on a quiz response', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf({
        ...OPEN,
        progress: {
          topic_key: 'rdb',
          topic: 'RDB',
          mastery: 64,
          confidence: 0.6,
          valid_attempts: 7,
          level: 'mastered',
          weak_points: ['不该出现的缺口'],
        },
      }),
      'POST /api/v1/learning/sessions/lsn-O/skip': viewOf(SECOND),
    })

    expect(app.text()).not.toContain('已经掌握')
    expect(app.text()).not.toContain('不该出现的缺口')

    // Skipping answers with the same shape, and still must not display it.
    await app.find('[data-test="learn-skip"]').trigger('click')
    await flushPromises()
    await flushPromises()
    expect(app.text()).not.toContain('不该出现的缺口')
    expect(app.text()).not.toContain('已经掌握')
    app.unmount()
  })

  it('keeps developer vocabulary off the page', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(OPEN),
    })
    const text = app.text()

    for (const banned of ['Phase', 'waiting_answer', '/api/v1/', 'ordinal', 'session_id', 'lsn-O']) {
      expect(text).not.toContain(banned)
    }
    app.unmount()
  })

  it('claims no capability the contract does not publish', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(OPEN),
    })
    const text = app.text()

    for (const claim of [
      '会自动安排练习',
      '自动出题',
      '逐题反馈',
      '评分理由如下',
      '这道题你答对了',
      '跨会话',
      '主题授权',
      '历史测验列表',
    ]) {
      expect(text).not.toContain(claim)
    }
    app.unmount()
  })
})

describe('the projection on request', () => {
  it('reads level, attempts and gaps from the progress endpoint only', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'GET /api/v1/learning/progress': viewOf({
        topic_key: 'rdb',
        topic: 'RDB',
        mastery: 64,
        confidence: 0.5,
        valid_attempts: 5,
        level: 'mastered',
        weak_points: ['没说清 fork 时的写入放大'],
        last_attempt: '2026-09-26T03:00:00Z',
      }),
    })

    expect(app.find('[data-test="learn-progress-text"]').exists()).toBe(false)
    await app.find('[data-test="learn-progress-load"]').trigger('click')
    await flushPromises()
    await flushPromises()

    const text = app.find('[data-test="learn-progress-text"]').text()
    expect(text).toContain('已经掌握')
    expect(text).toContain('5 次有效作答')
    expect(text).toContain('没说清 fork 时的写入放大')
    expect(text).not.toContain('64')
    expect(text).not.toContain('0.5')
    app.unmount()
  })

  it('says "no record yet" for a topic nobody has practised', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'GET /api/v1/learning/progress': refused(404, 'not_found'),
    })
    await app.find('[data-test="learn-progress-load"]').trigger('click')
    await flushPromises()
    await flushPromises()

    expect(app.find('[data-test="learn-progress-absent"]').text()).toContain('还没有可展示的掌握度记录')
    expect(app.find('[data-test="learn-progress-error"]').exists()).toBe(false)
    app.unmount()
  })
})

describe('the clock only stops the page', () => {
  it('past the deadline it disables actions without declaring an expiry', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf({
        ...OPEN,
        expires_at: '2026-01-01T00:00:00Z',
      }),
    })

    learning.tick(Date.parse('2026-01-01T00:00:01Z'))
    await flushPromises()

    expect(app.find('[data-test="learn-clock"]').text()).toContain('服务端')
    expect(app.find('[data-test="learn-submit"]').attributes('disabled')).toBeDefined()
    expect(app.find('[data-test="learn-skip"]').attributes('disabled')).toBeDefined()
    expect(app.find('[data-test="learn-cancel"]').attributes('disabled')).toBeDefined()
    // the server still says it is waiting; the page must not overwrite that
    expect(app.find('[data-test="learn-status"]').text()).toBe('等你作答')
    expect(app.find('[data-test="learn-expired-note"]').exists()).toBe(false)
    app.unmount()
  })

  it('warns while there is time left', async () => {
    const soon = new Date(Date.now() + 60 * 1000).toISOString()
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf({ ...OPEN, expires_at: soon }),
    })
    learning.tick(Date.now())
    await flushPromises()

    expect(app.find('[data-test="learn-clock"]').text()).toContain('作答还来得及')
    expect(app.find('[data-test="learn-submit"]').attributes('disabled')).toBeDefined() // empty draft
    await app.find('[data-test="learn-answer"]').setValue('答')
    expect(app.find('[data-test="learn-submit"]').attributes('disabled')).toBeUndefined()
    app.unmount()
  })
})

describe('a rejected bearer', () => {
  async function mountWithRouter(handlers) {
    const router = createAppRouter({ authStore, history: createMemoryHistory() })
    await router.push('/learning')
    await router.isReady()
    const app = await openPage(handlers, { global: { plugins: [router] } })
    return { app, router }
  }

  it('ends the session on a failed read and moves to the sign-in screen', async () => {
    const { app, router } = await mountWithRouter({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': refused(401, 'unauthorized', 'token expired'),
    })

    expect(authStore.isAuthenticated()).toBe(false)
    expect(router.currentRoute.value.name).toBe('login')
    expect(router.currentRoute.value.query.redirect).toBe('/learning')
    expect(app.text()).not.toContain('token expired')
    app.unmount()
  })

  it('ends it the same way when a write is what got refused', async () => {
    const { app, router } = await mountWithRouter({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'POST /api/v1/learning/sessions/lsn-O/answer': refused(401, 'unauthorized'),
    })
    await answerWith(app, '答')

    expect(authStore.isAuthenticated()).toBe(false)
    expect(router.currentRoute.value.name).toBe('login')
    app.unmount()
  })

  it('leaves no quiz behind for whoever signs in next', async () => {
    const first = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(OPEN),
    })
    expect(first.find('[data-test="learn-question"]').exists()).toBe(true)
    first.unmount()

    authStore.signOut()
    expect(learning.view.value).toBeNull()
    expect(learning.sessionId.value).toBe('')

    authStore.adopt('tok.next', { id: 'user-2', email: 'b@example.test', displayName: '' })
    const next = await openPage({
      'GET /api/v1/sessions': viewOf({ items: [{ id: 'session-B', title: '另一个账号的对话', mode: 'chat' }] }),
      'GET /api/v1/learning/sessions': viewOf(null),
    })

    expect(next.text()).toContain('当前没有进行中的练习')
    expect(next.text()).not.toContain('两种做法各解决什么问题？')
    // What this account owns is one plain conversation, and a plain conversation cannot
    // hold a quiz - so the picker offers nothing and says why, instead of pre-selecting a
    // conversation the server would refuse.
    expect(next.find('[data-test="learn-session-picker"]').element.value).toBe('')
    expect(next.find('[data-test="learn-no-agent-session"]').exists()).toBe(true)
    next.unmount()
  })
})

describe('accessibility of the quiz form', () => {
  it('gives every control a real label and a name', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(OPEN),
    })

    for (const id of ['learn-session', 'learn-topic', 'learn-answer']) {
      const control = app.find(`#${id}`)
      expect(control.exists()).toBe(true)
      const label = app.find(`label[for="${id}"]`)
      expect(label.exists()).toBe(true)
      expect(label.text().trim().length).toBeGreaterThan(0)
    }
    for (const button of app.findAll('button')) {
      expect(button.attributes('type')).toBe('button')
      expect(button.text().trim().length).toBeGreaterThan(0)
    }
    expect(app.findAll('h2').length).toBeGreaterThanOrEqual(3)
    app.unmount()
  })

  it('announces the state and the clock in a live region', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf({ ...OPEN, expires_at: '2026-01-01T00:00:00Z' }),
    })

    expect(app.find('[data-test="learn-clock"]').attributes('role')).toBe('status')
    learning.tick(Date.parse('2027-01-01T00:00:00Z'))
    await flushPromises()
    expect(app.find('[data-test="learn-clock"]').attributes('role')).toBe('status')
    app.unmount()
  })
})

describe('only a learning agent can be practised on', () => {
  it('lists a learning-agent conversation and leaves a plain chat one out', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf({
        items: [
          { id: 'session-chat', title: '普通对话', mode: 'chat' },
          { id: 'session-agent', title: '练习对话', mode: 'learning_agent' },
        ],
      }),
      'GET /api/v1/learning/sessions': viewOf(null),
    })

    const picker = app.find('[data-test="learn-session-picker"]')
    const options = picker.findAll('option').map((option) => option.text())
    expect(options).toEqual(['练习对话'])
    // The default selection is the agent conversation, so the page cannot ask the server
    // to start on the plain one on the user's behalf.
    expect(picker.element.value).toBe('session-agent')
    expect(learning.sessionId.value).toBe('session-agent')
    expect(app.find('[data-test="learn-no-agent-session"]').exists()).toBe(false)
    app.unmount()
  })

  it('says so and offers no start when the account has no agent conversation', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf({
        items: [{ id: 'session-chat', title: '普通对话', mode: 'chat' }],
      }),
    })

    const picker = app.find('[data-test="learn-session-picker"]')
    expect(picker.findAll('option').map((option) => option.text())).toEqual([
      '没有可练习的学习 Agent 会话',
    ])
    expect(picker.element.value).toBe('')
    expect(app.find('[data-test="learn-no-agent-session"]').text()).toContain('普通对话不能开始练习')
    // Starting is unreachable rather than merely unfriendly: the button stays disabled and
    // no request is sent, so a user cannot be told "invalid request" for a mistake the page
    // offered them.
    const start = app.find('[data-test="learn-start"]')
    expect(start.attributes('disabled')).toBeDefined()
    await start.trigger('click')
    await flushPromises()
    expect(calls.filter((call) => call.key === 'POST /api/v1/learning/sessions')).toEqual([])
    app.unmount()
  })

  it('drops a plain conversation the store still holds from an earlier visit', async () => {
    learning.openSession('session-chat')
    const app = await openPage({
      'GET /api/v1/sessions': viewOf({
        items: [
          { id: 'session-chat', title: '普通对话', mode: 'chat' },
          { id: 'session-agent', title: '练习对话', mode: 'learning_agent' },
        ],
      }),
      'GET /api/v1/learning/sessions': viewOf(null),
      'GET /api/v1/sessions/session-agent/topic-memory': viewOf({ enabled: false }),
    })

    expect(learning.sessionId.value).toBe('session-agent')
    expect(app.find('[data-test="learn-session-picker"]').element.value).toBe('session-agent')
    app.unmount()
  })
})

/**
 * The topic-memory consent switch.
 *
 * What is being guarded here is narrower than "the toggle works": the page may only
 * ever display an answer the server gave, it may not offer the decision for a
 * conversation that cannot hold one, and the sentence it shows must not claim a wider
 * permission than the contract grants — one conversation, the one subject the open quiz
 * names, confirmed records only.
 */
const CONSENT_LABEL = '允许学习 Agent 在当前会话中读取当前测验主题下已确认的记忆'

describe('the topic-memory switch', () => {
  it('offers it for a learning-agent conversation and shows the stored answer', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(null),
      'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: false }),
    })

    expect(app.find('[data-test="learn-topic-memory"]').exists()).toBe(true)
    expect(app.text()).toContain(CONSENT_LABEL)
    expect(app.find('[data-test="learn-topic-memory-switch"]').element.checked).toBe(false)
    expect(app.find('[data-test="learn-topic-memory-state"]').text()).toContain('未允许')
    app.unmount()
  })

  it('restores a yes that is already stored, so the default is read not assumed', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(null),
      'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: true }),
    })

    expect(app.find('[data-test="learn-topic-memory-switch"]').element.checked).toBe(true)
    expect(app.find('[data-test="learn-topic-memory-state"]').text()).toContain('已允许')
    app.unmount()
  })

  it('never shows the switch for an account whose conversations are all plain chat', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf({ items: [{ id: 'session-chat', title: '普通对话', mode: 'chat' }] }),
      'GET /api/v1/learning/sessions': viewOf(null),
    })

    expect(app.find('[data-test="learn-no-agent-session"]').exists()).toBe(true)
    expect(app.find('[data-test="learn-topic-memory"]').exists()).toBe(false)
    expect(app.find('[data-test="learn-topic-memory-switch"]').exists()).toBe(false)
    // Nothing was asked about a conversation that cannot hold a consent.
    expect(calls.some((call) => call.key.includes('topic-memory'))).toBe(false)
    app.unmount()
  })

  it('refuses to render an unreadable answer as "off"', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(null),
      'GET /api/v1/sessions/session-N/topic-memory': viewOf({}),
    })

    const box = app.find('[data-test="learn-topic-memory-switch"]')
    expect(box.element.checked).toBe(false)
    expect(box.element.disabled).toBe(true)
    expect(app.find('[data-test="learn-topic-memory-state"]').text()).toContain('还读不到')
    expect(app.find('[data-test="learn-topic-memory-state"]').text()).not.toContain('未允许')
    app.unmount()
  })

  it('saves the click as one boolean and shows the server’s answer', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(null),
      'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: false }),
      'PUT /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: true }),
    })

    await app.find('[data-test="learn-topic-memory-switch"]').setValue(true)
    await flushPromises()
    await flushPromises()

    const put = calls.find((call) => call.key.startsWith('PUT'))
    expect(put.key).toBe('PUT /api/v1/sessions/session-N/topic-memory')
    expect(Object.keys(put.body)).toEqual(['enabled'])
    expect(put.body).toEqual({ enabled: true })
    expect(app.find('[data-test="learn-topic-memory-switch"]').element.checked).toBe(true)
    app.unmount()
  })

  it('follows a server that answers the opposite of what was asked', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(null),
      'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: false }),
      'PUT /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: false }),
    })

    await app.find('[data-test="learn-topic-memory-switch"]').setValue(true)
    await flushPromises()
    await flushPromises()

    // Nothing was applied optimistically, so there is nothing to walk back: the box
    // simply shows the answer. Asserted at both levels on purpose — this is the case
    // where the bound value never moves, so an element left at "clicked" would be a
    // position the server never gave.
    expect(learning.topicMemory.value).toBe(false)
    expect(app.find('[data-test="learn-topic-memory-switch"]').element.checked).toBe(false)
    app.unmount()
  })

  it('locks the box while the write is running and sends one request', async () => {
    const gate = deferred()
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(null),
      'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: false }),
      'PUT /api/v1/sessions/session-N/topic-memory': () => gate.promise.then(() => viewOf({ enabled: true })),
    })

    await app.find('[data-test="learn-topic-memory-switch"]').setValue(true)
    await flushPromises()

    expect(app.find('[data-test="learn-topic-memory-switch"]').element.disabled).toBe(true)
    await app.find('[data-test="learn-topic-memory-switch"]').setValue(false)

    gate.resolve()
    await flushPromises()
    await flushPromises()

    expect(calls.filter((call) => call.key.startsWith('PUT'))).toHaveLength(1)
    expect(app.find('[data-test="learn-topic-memory-switch"]').element.checked).toBe(true)
    app.unmount()
  })

  it('gives the focus back to the switch when the write took it away', async () => {
    // What this pins: a control that is `disabled` while its write is in flight is
    // blurred by the browser, and the focus does not come back on its own when the
    // flag clears. jsdom does not perform that blur, so the case performs it — the
    // point under test is what the component does after focus has been dropped, not
    // whether the environment drops it.
    const gate = deferred()
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(null),
      'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: false }),
      'PUT /api/v1/sessions/session-N/topic-memory': () => gate.promise.then(() => viewOf({ enabled: true })),
    })

    const box = app.find('[data-test="learn-topic-memory-switch"]')
    box.element.focus()
    expect(document.activeElement).toBe(box.element)

    // The blur is issued before the patch lands, which is the ordering the real browser
    // produces: the change handler flips the control to `disabled` on the next tick and
    // the drop to `<body>` follows that patch. Once it is disabled, `blur()` is a no-op
    // in jsdom, so this ordering is also the only way to express the cause here.
    const changing = box.setValue(true)
    box.element.blur()
    await changing
    expect(document.activeElement).toBe(document.body)

    gate.resolve()
    await flushPromises()
    await flushPromises()

    expect(document.activeElement).toBe(box.element)
    expect(box.element.disabled).toBe(false)
    app.unmount()
  })

  it('does not steal focus the learner moved somewhere else mid-write', async () => {
    const gate = deferred()
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(null),
      'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: false }),
      'PUT /api/v1/sessions/session-N/topic-memory': () => gate.promise.then(() => viewOf({ enabled: true })),
    })

    const box = app.find('[data-test="learn-topic-memory-switch"]')
    const other = app.find('#learn-topic')
    box.element.focus()
    await box.setValue(true)
    await flushPromises()

    // The learner tabbed on. Their position wins over the returning response.
    other.element.focus()
    gate.resolve()
    await flushPromises()
    await flushPromises()

    expect(document.activeElement).toBe(other.element)
    expect(box.element.checked).toBe(true)
    app.unmount()
  })

  it('puts the old answer back on a failed write without repeating the server’s text', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(null),
      'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: true }),
      'PUT /api/v1/sessions/session-N/topic-memory': refused(500, 'internal_error'),
    })

    await app.find('[data-test="learn-topic-memory-switch"]').setValue(false)
    await flushPromises()
    await flushPromises()

    // The rollback has to reach the element: the store put `true` back, and a checkbox
    // left at the clicked `false` would show a decision that was never saved.
    expect(learning.topicMemory.value).toBe(true)
    expect(app.find('[data-test="learn-topic-memory-switch"]').element.checked).toBe(true)
    expect(app.find('[data-test="learn-topic-memory-error"]').exists()).toBe(true)
    expect(app.text()).not.toContain('server detail')
    expect(app.find('[data-test="learn-topic-memory-switch"]').element.disabled).toBe(false)
    app.unmount()
  })

  it('says the preference can be saved before any quiz is open, and what that means', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(null),
      'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: false }),
    })

    const note = app.find('[data-test="learn-topic-memory-state"]').text()
    expect(note).toContain('有当前测验主题时才会生效')
    expect(app.find('[data-test="learn-topic-memory-switch"]').element.disabled).toBe(false)
    app.unmount()
  })

  it('drops that note once the server has an open quiz on screen', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(OPEN),
      'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: true }),
    })

    expect(app.find('[data-test="learn-topic-memory-state"]').text()).not.toContain('有当前测验主题时才会生效')
    expect(app.find('[data-test="learn-topic-memory-switch"]').element.checked).toBe(true)
    app.unmount()
  })

  it('claims no wider permission than the contract grants', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf(SESSIONS),
      'GET /api/v1/learning/sessions': viewOf(null),
      'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: false }),
    })

    const region = app.find('[data-test="learn-topic-memory"]').text()
    for (const overreach of ['所有主题', '全部主题', '所有会话', '全部会话', '所有用户', '未确认']) {
      expect(region).not.toContain(overreach)
    }
    // The sentence names the two bounds the backend actually enforces.
    expect(region).toContain('当前会话')
    expect(region).toContain('当前测验主题')
    app.unmount()
  })

  it('withdraws the answer and re-reads when the conversation changes', async () => {
    const app = await openPage({
      'GET /api/v1/sessions': viewOf({
        items: [
          { id: 'session-N', title: '练习甲', mode: 'learning_agent' },
          { id: 'session-B', title: '练习乙', mode: 'learning_agent' },
        ],
      }),
      'GET /api/v1/learning/sessions': viewOf(null),
      'GET /api/v1/sessions/session-N/topic-memory': viewOf({ enabled: true }),
      'GET /api/v1/sessions/session-B/topic-memory': viewOf({ enabled: false }),
    })

    expect(app.find('[data-test="learn-topic-memory-switch"]').element.checked).toBe(true)

    await app.find('[data-test="learn-session-picker"]').setValue('session-B')
    await flushPromises()
    await flushPromises()

    expect(app.find('[data-test="learn-topic-memory-switch"]').element.checked).toBe(false)
    expect(calls.some((call) => call.key === 'GET /api/v1/sessions/session-B/topic-memory')).toBe(true)
    // The two conversations were asked separately; neither answer was reused.
    expect(calls.filter((call) => call.key.includes('topic-memory')).map((call) => call.key)).toEqual([
      'GET /api/v1/sessions/session-N/topic-memory',
      'GET /api/v1/sessions/session-B/topic-memory',
    ])
    app.unmount()
  })
})
