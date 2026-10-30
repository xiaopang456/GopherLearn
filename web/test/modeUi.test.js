import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import ChatView from '../src/views/ChatView.vue'
import { setTokenProvider } from '../src/api/http.js'
import { useKnowledge } from '../src/composables/useKnowledge.js'
import { DESKTOP_QUERY, installMedia } from './helpers/media.js'
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

const CHAT = { id: 's-chat', title: '我的对话', mode: 'chat', updated_at: '2026-09-24T08:00:00Z' }
const AGENT = {
  id: 's-agent',
  title: '学习 Agent',
  mode: 'learning_agent',
  updated_at: '2026-09-24T09:00:00Z',
}
const MYSTERY = { id: 's-odd', title: '旧会话', mode: 'voice_beta', updated_at: '' }

let calls

function serve({ sessions = [], history = {}, stream = [], collections = [] } = {}) {
  globalThis.fetch = async (url, init = {}) => {
    calls.push({ url: String(url), init })
    const path = new URL(String(url), 'https://api.test').pathname

    if (path === '/api/v1/collections') {
      return jsonResponse({ data: { items: collections }, error: null })
    }
    if (path.endsWith('/stream')) {
      return sseResponse(stream.length ? stream : [sseFrame('message.completed', { content: '回声' })])
    }
    if (path === '/api/v1/sessions' && init.method === 'POST') {
      const body = JSON.parse(init.body)
      return jsonResponse({
        data: { id: `s-made-${calls.length}`, title: body.title, mode: body.mode },
        error: null,
      })
    }
    if (path === '/api/v1/sessions') {
      return jsonResponse({ data: { items: sessions }, error: null })
    }
    const history_ = /^\/api\/v1\/sessions\/([^/]+)\/messages$/.exec(path)
    if (history_) {
      return jsonResponse({ data: { items: history[history_[1]] ?? [] }, error: null })
    }
    return jsonResponse({ data: null, error: { code: 'not_found', message: 'no' } }, { status: 404 })
  }
}

async function mountAt(path, params) {
  mocks.route = { params, query: {}, path }
  const app = mount(ChatView, { attachTo: document.body })
  await flushPromises()
  await flushPromises()
  return app
}

let media

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'tok.modeui')
  // A desktop viewport keeps the session column resident so its rows are
  // inspectable; the drawer behaviour itself is covered in responsivePanel.
  media = installMedia({ [DESKTOP_QUERY]: true })
  calls = []
  mocks.push.mockReset()
  mocks.replace.mockReset()
})

afterEach(() => {
  media.restore()
})

describe('the composer follows the stored mode', () => {
  it('offers the grounding toolbar in a chat session that has collections', async () => {
    serve({
      sessions: [CHAT, AGENT],
      collections: [{ id: 'col-os', title: '操作系统笔记' }],
    })
    await useKnowledge().loadCollections()
    const app = await mountAt('/chat/s-chat', { sessionId: 's-chat' })

    expect(app.find('[data-test="knowledge-tools"]').exists()).toBe(true)
    app.unmount()
  })

  it('offers no grounding toolbar in a learning agent session', async () => {
    serve({
      sessions: [CHAT, AGENT],
      history: { 's-agent': [{ id: 'm-1', role: 'user', content: '帮我出一题' }] },
    })
    const app = await mountAt('/chat/s-agent', { sessionId: 's-agent' })

    expect(app.find('[data-test="knowledge-tools"]').exists()).toBe(false)
    expect(app.text()).not.toContain('使用知识库')
    app.unmount()
  })

  it('marks an agent transcript with a plain-language status, not a claim', async () => {
    serve({
      sessions: [AGENT],
      history: { 's-agent': [{ id: 'm-1', role: 'user', content: '帮我出一题' }] },
    })
    const app = await mountAt('/chat/s-agent', { sessionId: 's-agent' })

    const header = app.find('[data-test="agent-header"]')
    expect(header.exists()).toBe(true)
    expect(header.text()).toContain('学习 Agent')
    // Honest about where the capability actually stands: it defers to the
    // server-side capability set and promises nothing about steps.
    expect(header.text()).toContain('服务端当前启用的能力')
    expect(header.text()).toContain('执行步骤仅在真实发生并由服务端返回后显示')
    expect(app.find('[data-test="agent-panel"]').exists()).toBe(false)
    expect(app.find('[data-test="agent-steps"]').exists()).toBe(false)
    // No developer vocabulary, and no suggestion that tools or retrieval ran.
    for (const banned of ['Phase', 'learning_agent', 'answer_strategy', '/api/v1/']) {
      expect(app.text()).not.toContain(banned)
    }
    app.unmount()
  })

  it('says so when a stored mode could not be recognised', async () => {
    serve({
      sessions: [MYSTERY],
      history: { 's-odd': [{ id: 'm-9', role: 'assistant', content: '旧内容' }] },
    })
    const app = await mountAt('/chat/s-odd', { sessionId: 's-odd' })

    expect(app.text()).toContain('没能识别出来')
    expect(app.find('[data-test="knowledge-tools"]').exists()).toBe(false)
    app.unmount()
  })

  it('labels each list row with its own mode', async () => {
    serve({ sessions: [CHAT, AGENT, MYSTERY] })
    const app = await mountAt('/chat', {})

    const marks = app.findAll('[data-test="session-mode"]')
    expect(marks).toHaveLength(3)
    expect(marks.map((node) => node.attributes('title'))).toEqual([
      '对话',
      '学习 Agent',
      '模式未识别',
    ])
    app.unmount()
  })
})

describe('creating a session is an explicit mode choice', () => {
  it('opens a two-item menu rather than one ambiguous button', async () => {
    serve({ sessions: [CHAT] })
    const app = await mountAt('/chat', {})

    expect(app.find('[data-test="new-session-menu"]').exists()).toBe(false)
    await app.find('[data-test="new-session"]').trigger('click')
    const items = app.findAll('[data-test="new-session-choice"]')
    expect(items).toHaveLength(2)
    expect(items.map((node) => node.text()).join('|')).toContain('学习 Agent')
    app.unmount()
  })

  it('creates a chat session from the menu', async () => {
    serve({ sessions: [] })
    const app = await mountAt('/chat', {})
    await app.find('[data-test="new-session"]').trigger('click')

    await app.findAll('[data-test="new-session-choice"]')[0].trigger('click')
    await flushPromises()

    const created = calls.find((call) => call.init.method === 'POST')
    expect(JSON.parse(created.init.body)).toEqual({ mode: 'chat', title: '新对话' })
    expect(mocks.replace).toHaveBeenCalled()
    app.unmount()
  })

  it('creates a learning agent session from the menu', async () => {
    serve({ sessions: [] })
    const app = await mountAt('/chat', {})
    await app.find('[data-test="new-session"]').trigger('click')

    await app.findAll('[data-test="new-session-choice"]')[1].trigger('click')
    await flushPromises()

    const created = calls.find((call) => call.init.method === 'POST')
    expect(JSON.parse(created.init.body)).toEqual({
      mode: 'learning_agent',
      title: '学习 Agent',
      requestId: expect.stringMatching(/^create-/),
    })
    // and the main region switches to the agent chrome immediately
    expect(app.find('[data-test="knowledge-tools"]').exists()).toBe(false)
    app.unmount()
  })

  it('offers both modes from the empty state without a dead expanse', async () => {
    serve({ sessions: [] })
    const app = await mountAt('/chat', {})

    expect(app.find('[data-test="empty-state"]').exists()).toBe(true)
    expect(app.find('[data-test="empty-new-chat"]').exists()).toBe(true)
    expect(app.find('[data-test="empty-new-agent"]').exists()).toBe(true)

    await app.find('[data-test="empty-new-agent"]').trigger('click')
    await flushPromises()
    const created = calls.find((call) => call.init.method === 'POST')
    expect(JSON.parse(created.init.body).mode).toBe('learning_agent')
    app.unmount()
  })
})

describe('the grounding picker mirrors the shared collection list', () => {
  it('lists a real collection with no invented entry', async () => {
    serve({
      sessions: [CHAT],
      collections: [
        { id: 'col-os', title: '操作系统笔记' },
        { id: 'col-concurrency', title: '并发编程' },
      ],
    })
    // The shell loads collections in the app; this stands in for it.
    await useKnowledge().loadCollections()

    const app = await mountAt('/chat/s-chat', { sessionId: 's-chat' })
    const toggle = app.find('[data-test="knowledge-toggle"]')
    expect(toggle.attributes('disabled')).toBeUndefined()

    await toggle.trigger('click')
    await app.find('[data-test="knowledge-pick"]').trigger('click')

    const options = app.findAll('.tool__option')
    expect(options.map((node) => node.text())).toEqual(['操作系统笔记', '并发编程'])
    app.unmount()
  })

  it('shows no grounding controls at all when there is nothing to ground with', async () => {
    serve({ sessions: [CHAT], collections: [] })
    await useKnowledge().loadCollections()

    const app = await mountAt('/chat/s-chat', { sessionId: 's-chat' })
    // A switch that can never be turned on, pointing at a page that cannot
    // enable it, reads as a broken screen. The controls come back on their own
    // once the backend serves a collection.
    expect(app.find('[data-test="knowledge-tools"]').exists()).toBe(false)
    expect(app.text()).not.toContain('使用知识库')
    expect(app.text()).not.toContain('添加知识库后可启用')
    app.unmount()
  })

  it('sends the selection chosen through the composer on the wire', async () => {
    serve({
      sessions: [CHAT],
      collections: [{ id: 'col-os', title: '操作系统笔记' }],
      stream: [sseFrame('message.completed', { message_id: 'a-1', content: '回声' })],
    })
    await useKnowledge().loadCollections()

    const app = await mountAt('/chat/s-chat', { sessionId: 's-chat' })

    // Armed but nothing picked yet: the turn must go out ungrounded.
    await app.find('[data-test="knowledge-toggle"]').trigger('click')
    await app.find('textarea').setValue('先不引用')
    await app.find('[data-test="send"]').trigger('click')
    await flushPromises()
    let stream = calls.filter((call) => call.url.endsWith('/stream')).at(-1)
    expect(JSON.parse(stream.init.body)).toEqual({ content: '先不引用' })

    // Now pick the collection through the picker and ask again.
    await app.find('[data-test="knowledge-toggle"]').trigger('click')
    await app.find('[data-test="knowledge-toggle"]').trigger('click')
    await app.find('[data-test="knowledge-pick"]').trigger('click')
    await app.find('.tool__option input[type="checkbox"]').setValue(true)

    await app.find('textarea').setValue('页表怎么工作')
    await app.find('[data-test="send"]').trigger('click')
    await flushPromises()
    stream = calls.filter((call) => call.url.endsWith('/stream')).at(-1)

    expect(JSON.parse(stream.init.body)).toEqual({
      content: '页表怎么工作',
      knowledge: { enabled: true, collection_ids: ['col-os'] },
    })
    app.unmount()
  })
})
