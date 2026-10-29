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

let streamFrames
let calls

function routeRequests(handler) {
  globalThis.fetch = async (url, init = {}) => {
    calls.push({ url: String(url), init })
    return handler(new URL(String(url), 'https://api.test').pathname, init)
  }
}

async function typeAndSend(app, text) {
  await app.find('textarea').setValue(text)
  await app.find('[data-test="send"]').trigger('click')
  await flushPromises()
}

function mountView() {
  return mount(ChatView, { attachTo: document.body })
}

function bubbles(app) {
  return app.findAll('[data-test^="message-"]')
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
    return jsonResponse(
      { data: null, error: { code: 'not_found', message: '会话不存在' } },
      { status: 404 },
    )
  })
})

describe('chat view wiring', () => {
  it('shows the empty state and a disabled send button', async () => {
    const app = mountView()
    await flushPromises()

    expect(app.text()).toContain('从一个具体的问题开始')
    expect(app.find('[data-test="composer"]').exists()).toBe(true)
    expect(app.find('[data-test="send"]').attributes('disabled')).toBeDefined()
    app.unmount()
  })

  it('enables sending only for a non-blank draft', async () => {
    const app = mountView()
    await flushPromises()

    await app.find('textarea').setValue('   ')
    expect(app.find('[data-test="send"]').attributes('disabled')).toBeDefined()

    await app.find('textarea').setValue('有内容了')
    expect(app.find('[data-test="send"]').attributes('disabled')).toBeUndefined()
    app.unmount()
  })

  it('streams deltas into the assistant bubble and settles as done', async () => {
    streamFrames = [
      sseFrame('message.started', { message_id: 'a-1' }),
      sseFrame('message.delta', { message_id: 'a-1', delta: '第一段 ' }),
      sseFrame('message.delta', { message_id: 'a-1', delta: '第二段' }),
      sseFrame('message.completed', { message_id: 'a-1' }),
    ]
    const app = mountView()
    await flushPromises()

    await typeAndSend(app, '讲讲 SSE')

    const turns = bubbles(app)
    expect(turns).toHaveLength(2)
    expect(turns[0].text()).toContain('讲讲 SSE')
    expect(turns[1].text()).toContain('第一段 第二段')
    expect(app.text()).not.toContain('正在生成')
    expect(app.find('textarea').element.value).toBe('')
    app.unmount()
  })

  it('keeps the question visible with an error card when the run fails', async () => {
    streamFrames = [
      sseFrame('message.delta', { delta: '只写了一半' }),
      sseFrame('message.failed', { code: 'invalid_request', message: '上下文过长' }),
    ]
    const app = mountView()
    await flushPromises()

    await typeAndSend(app, '一个很长的问题')

    const turns = bubbles(app)
    expect(turns).toHaveLength(2)
    expect(turns[0].text()).toContain('一个很长的问题')
    expect(turns[1].text()).toContain('只写了一半')
    expect(turns[1].text()).toContain('上下文过长')
    expect(turns[1].text()).toContain('invalid_request')
    app.unmount()
  })

  it('shows the stop control during a run and the stopped state after', async () => {
    const app = mountView()
    await flushPromises()

    // `closeAfterChunks: false` leaves the run open, the way a slow model does.
    routeRequests((path, init) => {
      if (path.endsWith('/stream')) {
        return sseResponse([sseFrame('message.delta', { delta: '正在输出' })], {
          closeAfterChunks: false,
        })
      }
      if (path === '/api/v1/sessions' && init.method === 'POST') {
        return jsonResponse({ data: { id: 's-1', title: '未命名会话' }, error: null })
      }
      return jsonResponse({ data: { items: [] }, error: null })
    })

    await app.find('textarea').setValue('问题')
    await app.find('[data-test="send"]').trigger('click')
    await flushPromises()

    expect(app.find('[data-test="abort"]').exists()).toBe(true)
    expect(app.text()).toContain('正在生成')

    await app.find('[data-test="abort"]').trigger('click')
    await flushPromises()

    expect(app.find('[data-test="abort"]').exists()).toBe(false)
    expect(app.text()).toContain('已停止')
    expect(app.text()).toContain('正在输出')
    expect(app.text()).toContain('问题')
    app.unmount()
  })

  it('reports a failed history load instead of showing a blank transcript', async () => {
    mocks.route = { params: { sessionId: 'gone' }, query: {}, path: '/chat' }
    const app = mountView()
    await flushPromises()

    expect(app.text()).toContain('会话不存在')
    app.unmount()
  })

  it('creates the session through POST and writes it back to the router', async () => {
    streamFrames = [sseFrame('message.completed', { content: 'ok' })]
    const app = mountView()
    await flushPromises()

    await typeAndSend(app, '新会话的第一个问题')

    const created = calls.find((call) => call.init.method === 'POST')
    expect(created.url).toBe('https://api.test/api/v1/sessions')
    expect(mocks.replace).toHaveBeenCalledWith({
      name: 'session',
      params: { sessionId: 's-1' },
    })
    expect(app.text()).toContain('新会话的第一个问题')
    app.unmount()
  })

  it('puts the bearer token in headers only', async () => {
    streamFrames = [sseFrame('message.completed', { content: 'ok' })]
    const app = mountView()
    await flushPromises()

    await typeAndSend(app, '问题')

    expect(calls.length).toBeGreaterThan(1)
    for (const call of calls) {
      expect(call.url).not.toContain('tok.view')
      expect(call.url).not.toMatch(/\?/)
    }
    const stream = calls.find((call) => call.url.endsWith('/stream'))
    expect(stream.init.headers.authorization).toBe('Bearer tok.view')
    expect(stream.init.headers.accept).toBe('text/event-stream')
    app.unmount()
  })
})
