import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import ChatView from '../src/views/ChatView.vue'
import { setTokenProvider } from '../src/api/http.js'
import { jsonResponse, sseFrame, sseResponse } from './helpers/transport.js'

const mocks = vi.hoisted(() => ({
  route: { params: {}, query: {}, path: '/chat', name: 'chat' },
  push: vi.fn(),
  replace: vi.fn(),
}))

vi.mock('vue-router', () => ({
  useRoute: () => mocks.route,
  useRouter: () => ({ push: mocks.push, replace: mocks.replace }),
}))

let calls
let streamFrames

const QUESTION = '请根据我保存的分层摘要主题笔记，说出我用来区分现值和旧值的口诀。'

const SUGGESTION = {
  status_code: 1000,
  maxSelectable: 2,
  candidates: [
    { topicKey: '分层摘要', displayName: '分层摘要' },
    { topicKey: '缓存', displayName: '缓存' },
  ],
}

/**
 * The card belongs where the answer will land: under the question, in the
 * message column, at the width and left edge of the answer itself.
 *
 * These tests drive the real flow - a question typed, the switch on, the
 * server's suggestion answered - and read the page that comes out.
 */
beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'tok.view')
  calls = []
  streamFrames = []
  mocks.route = { params: { sessionId: 's-1' }, query: {}, path: '/chat', name: 'chat' }
  mocks.push.mockReset()
  mocks.replace.mockReset()

  globalThis.fetch = async (url, init = {}) => {
    calls.push({ url: String(url), init: init ?? {} })
    const path = new URL(String(url), 'https://api.test').pathname
    const method = init?.method ?? 'GET'
    if (path === '/api/v1/AI/agent/topic-consent/capability') {
      return jsonResponse({ status_code: 1000, status_msg: 'success', supported: true })
    }
    if (path === '/api/v1/AI/agent/topic-suggestions') {
      return jsonResponse(SUGGESTION)
    }
    if (path === '/api/v1/AI/agent/topic-consent') {
      return jsonResponse({
        status_code: 1000,
        status_msg: 'success',
        ticket: 'ticket-1',
        candidates: SUGGESTION.candidates,
      })
    }
    if (path.endsWith('/stream')) return sseResponse(streamFrames)
    if (path === '/api/v1/sessions/s-1/messages' && method === 'POST') {
      return jsonResponse({ data: { id: 'm-1' }, error: null })
    }
    if (path === '/api/v1/sessions') {
      return jsonResponse({
        data: { items: [{ id: 's-1', mode: 'learning_agent', canUseKnowledge: true, title: '验收会话' }] },
        error: null,
      })
    }
    if (path === '/api/v1/sessions/s-1/messages') {
      return jsonResponse({ data: { items: [] }, error: null })
    }
    return jsonResponse({ data: null, error: { code: 'not_found', message: '没有这个资源' } }, { status: 404 })
  }
})

async function askWithTopic(app, text = QUESTION) {
  const topic = app.find('[data-test="topic-memory-toggle"]')
  await topic.trigger('click')
  await app.find('textarea').setValue(text)
  await app.find('[data-test="send"]').trigger('click')
  await flushPromises()
}

function mountView() {
  return mount(ChatView, { attachTo: document.body })
}

describe('where the topic card sits', () => {
  it('is inside the message column, not in a strip above the composer', async () => {
    const app = mountView()
    await flushPromises()
    await askWithTopic(app)

    const card = app.find('[data-test="topic-consent"]')
    expect(card.exists()).toBe(true)
    // The card is a child of the transcript column, which is the column the
    // answers are in.
    const list = app.find('.chat__list')
    expect(list.exists()).toBe(true)
    expect(list.find('[data-test="topic-consent"]').exists()).toBe(true)
    // It is not a sibling of the column, above the composer.
    expect(app.find('.chat__main > [data-test="topic-consent"]').exists()).toBe(false)
    app.unmount()
  })

  it('carries the title the review asked for', async () => {
    const app = mountView()
    await flushPromises()
    await askWithTopic(app)

    expect(app.find('[data-test="topic-consent-title"]').text()).toBe('选择本轮使用的主题')
    app.unmount()
  })

  it('keeps the candidates, the count, the search entry and the buttons inside the card', async () => {
    const app = mountView()
    await flushPromises()
    await askWithTopic(app)

    const card = app.find('[data-test="topic-consent"]')
    expect(card.find('[data-test="topic-consent-count"]').exists()).toBe(true)
    expect(card.find('[data-test="topic-consent-options"]').exists()).toBe(true)
    expect(card.find('[data-test="topic-option-分层摘要"]').exists()).toBe(true)
    expect(card.find('[data-test="topic-open-catalogue"]').exists()).toBe(true)
    expect(card.find('[data-test="topic-confirm"]').exists()).toBe(true)
    expect(card.find('[data-test="topic-skip"]').exists()).toBe(true)
    app.unmount()
  })

  it('shows the question the card is about', async () => {
    const app = mountView()
    await flushPromises()
    await askWithTopic(app)

    // The learner can see what they asked while the card is up.
    expect(app.text()).toContain(QUESTION)
    app.unmount()
  })
})

describe('what happens when the learner decides', () => {
  it('sends the request once and shows the question once', async () => {
    streamFrames = [sseFrame('message.completed', { content: '现值看白板，旧值翻档案。' })]
    const app = mountView()
    await flushPromises()
    await askWithTopic(app)

    await app.find('[data-test="topic-option-分层摘要"]').setValue(true)
    await app.find('[data-test="topic-confirm"]').trigger('click')
    await flushPromises()

    // The card has given way to the answer.
    expect(app.find('[data-test="topic-consent"]').exists()).toBe(false)

    // The question is on the page once, not twice: the pending bubble was
    // replaced by the message the server received.
    const questionTurns = app
      .findAll('[data-test^="message-"]')
      .filter((node) => node.text().includes(QUESTION))
    expect(questionTurns).toHaveLength(1)

    // One request, and it carries the ticket.
    const sends = calls.filter((call) => call.url.includes('/stream'))
    expect(sends).toHaveLength(1)
    app.unmount()
  })

  it('sends the request once when the learner declines the topic', async () => {
    streamFrames = [sseFrame('message.completed', { content: '好。' })]
    const app = mountView()
    await flushPromises()
    await askWithTopic(app)

    await app.find('[data-test="topic-skip"]').trigger('click')
    await flushPromises()

    expect(app.find('[data-test="topic-consent"]').exists()).toBe(false)
    const sends = calls.filter((call) => call.url.includes('/stream'))
    expect(sends).toHaveLength(1)
    const questionTurns = app
      .findAll('[data-test^="message-"]')
      .filter((node) => node.text().includes(QUESTION))
    expect(questionTurns).toHaveLength(1)
    app.unmount()
  })

  it('asks the server for the suggestion once and issues one ticket', async () => {
    streamFrames = [sseFrame('message.completed', { content: '好。' })]
    const app = mountView()
    await flushPromises()
    await askWithTopic(app)

    await app.find('[data-test="topic-option-分层摘要"]').setValue(true)
    await app.find('[data-test="topic-confirm"]').trigger('click')
    await flushPromises()

    const suggestions = calls.filter((call) => call.url.includes('/topic-suggestions'))
    const tickets = calls.filter((call) => call.url.includes('/topic-consent') && !call.url.includes('/capability'))
    expect(suggestions).toHaveLength(1)
    expect(tickets).toHaveLength(1)
    app.unmount()
  })
})

describe('the composer keeps its own switch', () => {
  it('leaves the topic switch in the composer', async () => {
    const app = mountView()
    await flushPromises()

    const composer = app.find('[data-test="composer"]')
    expect(composer.find('[data-test="topic-memory-toggle"]').exists()).toBe(true)
    app.unmount()
  })
})
