import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import ClarificationCard from '../src/components/ClarificationCard.vue'
import MessageBubble from '../src/components/MessageBubble.vue'
import ChatView from '../src/views/ChatView.vue'
import { setTokenProvider } from '../src/api/http.js'
import { jsonResponse, sseFrame, sseResponse } from './helpers/transport.js'

const clarification = {
  clarificationId: 'clar-1',
  question: '上次你给我排的那三道题里，第二道练的是什么？',
  prompt: '你在问哪一次？点一个，或者直接告诉我时间、标题或主题。',
  candidates: [
    { sessionId: 'l4r2-2026am-a1-03', title: '洄漪', topics: ['慢日志与锁'], lastTalkedAt: '2026-04-03T14:36:00Z' },
    { sessionId: 'l4r2-2026pm-b2-07', title: '浔漳', topics: [], lastTalkedAt: '2026-03-28T09:05:00Z' },
  ],
  allowFreeText: true,
}

const mocks = vi.hoisted(() => ({
  route: { params: {}, query: {}, path: '/chat', name: 'chat' },
  push: vi.fn(),
  replace: vi.fn(),
}))

vi.mock('vue-router', () => ({
  useRoute: () => mocks.route,
  useRouter: () => ({ push: mocks.push, replace: mocks.replace }),
}))

let streamFrames
let calls

function routeRequests(handler) {
  globalThis.fetch = async (url, init = {}) => {
    calls.push({ url: String(url), init })
    return handler(new URL(String(url), 'https://api.test').pathname, init)
  }
}

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'tok.view')
  calls = []
  streamFrames = []
  mocks.route = { params: {}, query: {}, path: '/chat', name: 'chat' }
  mocks.push.mockReset()
  mocks.replace.mockReset()
  routeRequests((path, init) => {
    if (path.endsWith('/stream')) return sseResponse(streamFrames)
    if (path === '/api/v1/sessions' && init.method === 'POST') {
      return jsonResponse({ data: { id: 's-1', title: '未命名会话' }, error: null })
    }
    if (path === '/api/v1/sessions') {
      return jsonResponse({ data: { items: [] }, error: null })
    }
    return jsonResponse({ data: null, error: { code: 'not_found', message: '会话不存在' } }, { status: 404 })
  })
})

describe('澄清卡', () => {
  it('shows the question, the prompt and one button per candidate, named by title and date', () => {
    const wrapper = mount(ClarificationCard, { props: { clarification } })

    expect(wrapper.get('[data-test=clarification-card]').exists()).toBe(true)
    expect(wrapper.get('[data-test=clarification-question]').text()).toBe(clarification.question)
    expect(wrapper.get('[data-test=clarification-prompt]').text()).toBe(clarification.prompt)
    const buttons = wrapper.findAll('[data-test^=clarification-candidate-]')
    expect(buttons).toHaveLength(2)
    // The learner reads a title and a date, not a session id.
    expect(buttons[0].text()).toContain('洄漪')
    expect(buttons[0].text()).toContain('2026')
    expect(buttons[0].text()).toContain('慢日志与锁')
    expect(buttons[1].text()).toContain('浔漳')
    wrapper.unmount()
  })

  it('a clicked candidate is answered with its session id, which the server reads exactly', async () => {
    const wrapper = mount(ClarificationCard, { props: { clarification } })

    await wrapper.get('[data-test=clarification-candidate-2]').trigger('click')

    expect(wrapper.emitted('choose')).toEqual([['l4r2-2026pm-b2-07']])
    // The card is spent: it cannot be answered twice.
    expect(wrapper.find('[data-test=clarification-candidates]').exists()).toBe(false)
    expect(wrapper.get('[data-test=clarification-answered]').text()).toContain('已经把你的选择发出去了')
    wrapper.unmount()
  })

  it('the learner may answer in their own words when the card allows it', async () => {
    const wrapper = mount(ClarificationCard, { props: { clarification } })

    expect(wrapper.get('[data-test=clarification-free-text]').exists()).toBe(true)
    expect(wrapper.get('[data-test=clarification-free-submit]').element.disabled).toBe(true)

    await wrapper.get('[data-test=clarification-free-text]').setValue('上周讲 HTTP 的那次')
    expect(wrapper.get('[data-test=clarification-free-submit]').element.disabled).toBe(false)

    await wrapper.get('[data-test=clarification-free-submit]').trigger('click')
    expect(wrapper.emitted('choose')).toEqual([['上周讲 HTTP 的那次']])
    expect(wrapper.find('[data-test=clarification-free]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('a card without free text offers the choices only', () => {
    const wrapper = mount(ClarificationCard, {
      props: { clarification: { ...clarification, allowFreeText: false } },
    })

    expect(wrapper.find('[data-test=clarification-free]').exists()).toBe(false)
    expect(wrapper.findAll('[data-test^=clarification-candidate-]')).toHaveLength(2)
    wrapper.unmount()
  })

  it('the learner may pick from their own conversation list, and the pick is the session id', async () => {
    routeRequests((path, init) => {
      if (path.endsWith('/stream')) return sseResponse(streamFrames)
      if (path === '/api/v1/sessions' && init.method === 'POST') {
        return jsonResponse({ data: { id: 's-1', title: '未命名会话' }, error: null })
      }
      if (path === '/api/v1/sessions') {
        return jsonResponse({
          data: {
            items: [
              { id: 'l4r2-2026pm-b2-07', title: '浔漳', mode: 'learning_agent', updated_at: '2026-03-28T09:05:00Z' },
              { id: 'l4r2-2026am-a1-03', title: '洄漪', mode: 'learning_agent', updated_at: '2026-04-03T14:36:00Z' },
            ],
          },
          error: null,
        })
      }
      return jsonResponse({ data: null, error: { code: 'not_found', message: '会话不存在' } }, { status: 404 })
    })

    const wrapper = mount(ClarificationCard, {
      props: { clarification: { ...clarification, allowHistoryBrowse: true } },
    })

    // The list is the learner's own history, and it is loaded on demand: a
    // clarification answered from the candidates never pays for it.
    expect(wrapper.find('[data-test=clarification-history-toggle]').exists()).toBe(true)
    expect(wrapper.find('[data-test=clarification-history-list]').exists()).toBe(false)

    await wrapper.get('[data-test=clarification-history-toggle]').trigger('click')
    await flushPromises()

    const entries = wrapper.findAll('[data-test^=clarification-history-session-]')
    expect(entries).toHaveLength(2)
    expect(entries[0].text()).toContain('浔漳')

    // The filter narrows the learner's own list.
    await wrapper.get('[data-test=clarification-history-filter]').setValue('洄漪')
    expect(wrapper.findAll('[data-test^=clarification-history-session-]')).toHaveLength(1)

    await wrapper.get('[data-test=clarification-history-session-l4r2-2026am-a1-03]').trigger('click')
    // The answer is the session id, which the server reads exactly.
    expect(wrapper.emitted('choose')).toEqual([['l4r2-2026am-a1-03']])
    wrapper.unmount()
  })

  it('a card that may not browse the history offers the choices only', () => {
    const wrapper = mount(ClarificationCard, { props: { clarification } })

    expect(wrapper.find('[data-test=clarification-history-toggle]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('a reply that was read back still carries its card, so the learner can answer it later', async () => {
    // The learner may answer minutes later, in a conversation they have
    // reloaded in between: a card that only lived on the live turn is gone
    // exactly when it is still answerable.
    const message = {
      id: 'history', role: 'assistant', content: '你在问哪一次？', status: 'done',
      clarification: { ...clarification, allowHistoryBrowse: true },
    }
    const wrapper = mount(MessageBubble, { props: { message } })

    expect(wrapper.findComponent(ClarificationCard).exists()).toBe(true)
    expect(wrapper.get('[data-test=clarification-question]').text()).toBe(clarification.question)
    expect(wrapper.get('[data-test=clarification-candidate-1]').text()).toContain('洄漪')
    expect(wrapper.find('[data-test=clarification-history-toggle]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('a message without originalV2 never grows a card, and the choice leaves the bubble', async () => {
    const message = {
      id: 'local', role: 'assistant', content: '你问的是哪一次？', status: 'done',
      originalV2: { clarification },
    }
    const wrapper = mount(MessageBubble, { props: { message } })
    expect(wrapper.findComponent(ClarificationCard).exists()).toBe(true)

    await wrapper.get('[data-test=clarification-candidate-1]').trigger('click')
    expect(wrapper.emitted('clarify')).toEqual([['l4r2-2026am-a1-03']])

    // A message read back from the store has no originalV2, and then there is
    // nothing to ask.
    await wrapper.setProps({ message: { id: 'history', role: 'assistant', content: '历史正文', status: 'done' } })
    expect(wrapper.findComponent(ClarificationCard).exists()).toBe(false)
    // Nor does a learner's own question.
    await wrapper.setProps({ message: { id: 'mine', role: 'user', content: '我的问题', status: 'done' } })
    expect(wrapper.findComponent(ClarificationCard).exists()).toBe(false)
    wrapper.unmount()
  })
})

describe('澄清卡在聊天里', () => {
  it('a picked candidate goes out as the next user message, over the ordinary send channel', async () => {
    streamFrames = [
      sseFrame('message.started', { message_id: 'a-1' }),
      sseFrame('message.completed', {
        message_id: 'a-1',
        content: '你问的是哪一次？',
        original_v2: { clarification },
      }),
    ]
    const app = mount(ChatView, { attachTo: document.body })
    await flushPromises()

    await app.find('textarea').setValue('上次那三道题')
    await app.find('[data-test="send"]').trigger('click')
    await flushPromises()

    const card = app.findComponent(ClarificationCard)
    expect(card.exists()).toBe(true)
    expect(app.find('[data-test=clarification-question]').text()).toBe(clarification.question)

    await app.get('[data-test=clarification-candidate-1]').trigger('click')
    await flushPromises()

    const streams = calls.filter((call) => call.url.endsWith('/stream'))
    expect(streams).toHaveLength(2)
    // The answer is the candidate's id, sent as the learner's next message.
    expect(JSON.parse(streams[1].init.body).content).toBe('l4r2-2026am-a1-03')
    // The learner sees that message on screen like any other of their own.
    expect(app.findAll('[data-test^="message-"]').length).toBe(4)
    expect(app.text()).toContain('l4r2-2026am-a1-03')
    app.unmount()
  })
})
