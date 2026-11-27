import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import ChatView from '../src/views/ChatView.vue'
import ComposerBox from '../src/components/ComposerBox.vue'
import { setTokenProvider } from '../src/api/http.js'
import { jsonResponse } from './helpers/transport.js'

const mocks = vi.hoisted(() => ({
  route: { params: {}, query: {}, path: '/chat', name: 'chat' },
  push: vi.fn(),
  replace: vi.fn(),
}))

vi.mock('vue-router', () => ({
  useRoute: () => mocks.route,
  useRouter: () => ({ push: mocks.push, replace: mocks.replace }),
}))

function mountComposer(overrides = {}) {
  return mount(ComposerBox, {
    props: {
      modelValue: '一个问题',
      mode: 'learning_agent',
      knowledgeEnabled: false,
      canUseKnowledge: true,
      topicMemoryEnabled: false,
      canUseTopicMemory: false,
      ...overrides,
    },
  })
}

describe('where the topic switch sits', () => {
  it('lives in the composer, to the right of the library switch and left of the send button', () => {
    const app = mountComposer({ canUseTopicMemory: true })
    const html = app.html()
    expect(app.find('[data-test="topic-memory-toggle"]').exists()).toBe(true)
    expect(app.find('[data-test="topic-memory-toggle"]').text()).toContain('使用主题记忆')
    expect(html.indexOf('data-test="knowledge-tools"')).toBeLessThan(html.indexOf('data-test="topic-memory-toggle"'))
    expect(html.indexOf('data-test="topic-memory-toggle"')).toBeLessThan(html.indexOf('data-test="send"'))
  })

  it('takes the place of the shortcut hint it replaced', () => {
    const app = mountComposer({ canUseTopicMemory: true })
    expect(app.text()).not.toContain('Enter 发送')
  })

  it('leaves the shortcut hint alone where there is no topic switch', () => {
    const app = mountComposer({ canUseTopicMemory: false })
    expect(app.text()).toContain('Enter 发送 · Shift + Enter 换行')
  })

  it('is withheld on a deployment that never asks for a topic', () => {
    const app = mountComposer({ canUseTopicMemory: false })
    expect(app.find('[data-test="topic-memory-toggle"]').exists()).toBe(false)
    expect(app.emitted('update:topicMemoryEnabled')).toBeUndefined()
  })
})

describe('the topic switch reports only the on/off choice', () => {
  it('is off unless the caller says it is on', () => {
    const app = mountComposer({ canUseTopicMemory: true, topicMemoryEnabled: false })
    expect(app.find('[data-test="topic-memory-toggle"]').attributes('aria-pressed')).toBe('false')
  })

  it('turns on from off', async () => {
    const app = mountComposer({ canUseTopicMemory: true, topicMemoryEnabled: false })
    await app.find('[data-test="topic-memory-toggle"]').trigger('click')
    expect(app.emitted('update:topicMemoryEnabled').at(-1)).toEqual([true])
  })

  it('turns off from on', async () => {
    const app = mountComposer({ canUseTopicMemory: true, topicMemoryEnabled: true })
    await app.find('[data-test="topic-memory-toggle"]').trigger('click')
    expect(app.emitted('update:topicMemoryEnabled').at(-1)).toEqual([false])
  })

  it('does nothing while the switch is inert', async () => {
    const app = mountComposer({ canUseTopicMemory: true, topicMemoryDisabled: true })
    await app.find('[data-test="topic-memory-toggle"]').trigger('click')
    expect(app.emitted('update:topicMemoryEnabled')).toBeUndefined()
  })

  it('is a switch of its own: the library switch is untouched by it', async () => {
    const app = mountComposer({ canUseTopicMemory: true, knowledgeEnabled: false })
    await app.find('[data-test="topic-memory-toggle"]').trigger('click')
    expect(app.emitted('update:knowledgeEnabled')).toBeUndefined()
    expect(app.emitted('update:topicMemoryEnabled').at(-1)).toEqual([true])
  })
})

describe('sending behaviour is unchanged by the topic switch', () => {
  it('keeps Enter-to-send and Shift+Enter for a newline', async () => {
    const app = mountComposer({ canUseTopicMemory: true })
    const textarea = app.find('textarea')

    await textarea.trigger('keydown', { key: 'Enter', shiftKey: true })
    expect(app.emitted('submit')).toBeUndefined()

    await textarea.trigger('keydown', { key: 'Enter' })
    expect(app.emitted('submit')).toHaveLength(1)
  })
})

describe('the chat view', () => {
  let calls

  beforeEach(() => {
    vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
    setTokenProvider(() => 'tok.view')
    calls = []
    mocks.route = { params: { sessionId: 's-1' }, query: {}, path: '/chat', name: 'chat' }
    mocks.push.mockReset()
    mocks.replace.mockReset()

    globalThis.fetch = async (url, init = {}) => {
      calls.push({ url: String(url), init })
      const path = new URL(String(url), 'https://api.test').pathname
      if (path === '/api/v1/AI/agent/topic-consent/capability') {
        return jsonResponse({ status_code: 1000, status_msg: 'success', supported: true })
      }
      if (path === '/api/v1/sessions') {
        return jsonResponse({
          data: {
            items: [{ id: 's-1', mode: 'learning_agent', canUseKnowledge: true, title: '验收会话' }],
          },
          error: null,
        })
      }
      if (path === '/api/v1/sessions/s-1/messages') {
        return jsonResponse({ data: { items: [] }, error: null })
      }
      return jsonResponse({ data: null, error: { code: 'not_found', message: '没有这个资源' } }, { status: 404 })
    }
  })

  it('shows the topic switch inside the composer, not in a strip above it', async () => {
    const app = mount(ChatView, { attachTo: document.body })
    await flushPromises()

    const composer = app.find('[data-test="composer"]')
    expect(composer.find('[data-test="topic-memory-toggle"]').exists()).toBe(true)
    expect(composer.text()).toContain('使用主题记忆')
    // The old strip is gone; the switch is the composer\'s own control now.
    expect(app.text()).not.toContain('使用我的学习记录')
    expect(app.find('[data-test="learning-records-toggle"]').exists()).toBe(false)
    app.unmount()
  })

  it('keeps the two switches independent: the topic switch does not touch the library switch', async () => {
    const app = mount(ChatView, { attachTo: document.body })
    await flushPromises()

    const knowledgeBefore = app.find('[data-test="knowledge-toggle"]').attributes('aria-pressed')
    const topic = app.find('[data-test="topic-memory-toggle"]')
    expect(topic.attributes('aria-pressed')).toBe('false')

    await topic.trigger('click')
    expect(app.find('[data-test="topic-memory-toggle"]').attributes('aria-pressed')).toBe('true')
    expect(app.find('[data-test="knowledge-toggle"]').attributes('aria-pressed')).toBe(knowledgeBefore)
    app.unmount()
  })
})
