import { beforeEach, describe, expect, it } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory } from 'vue-router'

import ChatView from '../src/views/ChatView.vue'
import { createAppRouter } from '../src/router/index.js'
import { authStore } from '../src/auth/store.js'
import { setTokenProvider } from '../src/api/http.js'
import { installFetchStub, jsonResponse, lateDeliverySSEResponse, sseFrame, sseResponse } from './helpers/transport.js'

/**
 * A watched Agent turn, seen through the screen.
 *
 * These cases exist because every interesting property here is about what the DOM is
 * allowed to claim: the trail is the server's frames in the server's order, a step is
 * closed by a frame and not by a timer, a tool's result sentence is the only thing shown
 * about what it found, and nothing on screen is a name the backend never sent. Chat and
 * knowledge turns are included on purpose — the panel must stay absent there and the
 * request body must stay exactly as it was.
 */

const AGENT = { id: 'ag-1', title: '学习会话', mode: 'learning_agent' }
const CHAT = { id: 'ch-1', title: '普通对话', mode: 'chat' }

let bodies

function watchedTurn(extra = []) {
  return [
    sseFrame('message.started', { message_id: 'a-1', run_id: 'run-4' }),
    sseFrame('step_started', { run_id: 'run-4', sequence: 1, step_type: 'plan', message: 'model generation 1' }),
    sseFrame('step_started', { run_id: 'run-4', sequence: 2, step_type: 'tool', message: 'tool call 1' }),
    sseFrame('tool_started', { run_id: 'run-4', sequence: 2, tool_name: 'memory_search' }),
    sseFrame('tool_result', {
      run_id: 'run-4',
      sequence: 2,
      tool_name: 'memory_search',
      content: '本轮已查阅长期记忆。',
      duration_millis: 7,
    }),
    sseFrame('message.delta', { run_id: 'run-4', message_id: 'a-1', delta: '最终回答正文' }),
    sseFrame('message.completed', { run_id: 'run-4', message_id: 'a-1', content: '最终回答正文' }),
    ...extra,
  ]
}

function serve(frames) {
  installFetchStub((url, init = {}) => {
    const path = new URL(String(url), 'https://api.test').pathname
    bodies.push({ path, method: init.method ?? 'GET', body: init.body ? JSON.parse(init.body) : null })
    if (path.endsWith('/stream')) {
      // A function hands back a response the test drives by hand; an array is the
      // whole stream, read to the end at once.
      const given = typeof frames === 'function' ? frames() : frames
      return given && given.body ? given : sseResponse(given)
    }
    if (path === '/api/v1/sessions') return jsonResponse({ data: { items: [AGENT, CHAT] }, error: null })
    if (/\/messages$/.test(path)) return jsonResponse({ data: { items: [] }, error: null })
    return jsonResponse({ data: null, error: { code: 'not_found', message: 'no' } }, { status: 404 })
  })
}

async function openAndSend(session, send = '讲讲 RDB 与 AOF') {
  const router = createAppRouter({ authStore, history: createMemoryHistory() })
  await router.push(`/chat/${session.id}`)
  await router.isReady()
  const app = mount(ChatView, { attachTo: document.body, global: { plugins: [router] } })
  await flushPromises()
  await flushPromises()
  await app.find('textarea').setValue(send)
  await app.find('[data-test="send"]').trigger('click')
  await flushPromises()
  await flushPromises()
  return { app, router }
}

async function expand(app) {
  await app.find('[data-test="agent-steps-toggle"]').trigger('click')
  await flushPromises()
}

beforeEach(() => {
  bodies = []
  authStore.adopt('tok.ui', { id: 'user-1', email: 'ui@example.test', displayName: '' })
  setTokenProvider(() => 'tok.ui')
})

describe('a watched agent turn', () => {
  it('shows the trail the server reported, in its order, and nothing else', async () => {
    serve(watchedTurn())
    const { app } = await openAndSend(AGENT)

    const line = app.find('[data-test="agent-panel"]')
    expect(line.exists()).toBe(true)
    expect(line.text()).toContain('2 步')
    // Collapsed by default: the list is there to open, not to shout over the answer.
    expect(app.find('[data-test="agent-steps"]').exists()).toBe(false)

    await expand(app)
    const rows = app.findAll('[data-test="agent-steps"] li')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('model generation 1')
    expect(rows[0].text()).toContain('已完成')
    expect(rows[1].text()).toContain('memory_search')
    expect(rows[1].text()).toContain('本轮已查阅长期记忆。')
    expect(rows[1].text()).toContain('7 毫秒')
    expect(app.find('[data-test="send"]').exists()).toBe(true)
    app.unmount()
  })

  it('keeps the answer itself exactly where it was', async () => {
    serve(watchedTurn())
    const { app } = await openAndSend(AGENT)

    expect(app.text()).toContain('最终回答正文')
    expect(app.find('[data-test="agent-panel"]').text()).toContain('本轮已完成')
    app.unmount()
  })

  it('names the step in progress without opening the trail', async () => {
    const late = lateDeliverySSEResponse()
    serve(() => late.response)
    const { app } = await openAndSend(AGENT)

    late.push(sseFrame('message.started', { message_id: 'a-1', run_id: 'run-9' }))
    late.push(
      sseFrame('step_started', { run_id: 'run-9', sequence: 1, step_type: 'plan', message: 'model generation 1' }),
    )
    await flushPromises()
    expect(app.find('[data-test="agent-current-step"]').text()).toBe('model generation 1')
    await expand(app)
    expect(app.findAll('[data-test="agent-steps"] li')[0].text()).toContain('进行中')
    await app.find('[data-test="agent-steps-toggle"]').trigger('click')

    late.push(
      sseFrame('step_started', { run_id: 'run-9', sequence: 2, step_type: 'tool', message: 'tool call 1' }),
    )
    late.push(sseFrame('tool_started', { run_id: 'run-9', sequence: 2, tool_name: 'knowledge_search' }))
    await flushPromises()
    // The previous step closed because this one opened — no client timer involved.
    expect(app.find('[data-test="agent-current-step"]').text()).toContain('knowledge_search')
    await expand(app)
    const rows = app.findAll('[data-test="agent-steps"] li')
    expect(rows[0].text()).toContain('已完成')
    expect(rows[1].text()).toContain('进行中')

    late.push(sseFrame('message.completed', { run_id: 'run-9', message_id: 'a-1', content: '答完了' }))
    await flushPromises()
    await flushPromises()
    // Nothing is running any more, so the line stops claiming that something is.
    expect(app.find('[data-test="agent-current-step"]').exists()).toBe(false)
    late.finish()
    app.unmount()
  })

  it('renders no card for a confirmation frame that carries none', async () => {
    // The frame used to point at the memory page; the decision now lives under the reply,
    // and this frame has nothing a card could be built from: the draft fields are smuggled
    // beside `memory_draft`, not inside it. So the turn reports the tool, and stops there.
    serve([
      sseFrame('message.started', { message_id: 'a-1', run_id: 'run-4' }),
      sseFrame('step_started', { run_id: 'run-4', sequence: 1, step_type: 'plan', message: 'model generation 1' }),
      sseFrame('step_started', { run_id: 'run-4', sequence: 2, step_type: 'tool', message: 'tool call 1' }),
      sseFrame('tool_started', { run_id: 'run-4', sequence: 2, tool_name: 'propose_memory' }),
      sseFrame('tool_result', {
        run_id: 'run-4',
        sequence: 2,
        tool_name: 'propose_memory',
        content: '新增 1 条待确认记忆草稿',
        duration_millis: 4,
      }),
      sseFrame('waiting_confirmation', {
        run_id: 'run-4',
        sequence: 2,
        message: '本回合生成了 1 条待确认记忆草稿，请在当前回复下方的确认卡片中处理。',
        memory_id: 'mem-smuggled',
        draft_text: '草稿正文不该出现在这里',
        topic_key: 'redis-持久化',
      }),
      sseFrame('message.delta', { run_id: 'run-4', message_id: 'a-1', delta: '已记下，等你确认。' }),
      sseFrame('message.completed', { run_id: 'run-4', message_id: 'a-1', content: '已记下，等你确认。' }),
    ])
    const { app } = await openAndSend(AGENT)
    await expand(app)

    // The trail still says what the tool did — that part of the frame is real.
    expect(app.findAll('[data-test="agent-steps"] li')[1].text()).toContain('新增 1 条待确认记忆草稿')
    // And nothing else claims a decision is on offer.
    expect(app.findAll('[data-test="memory-draft-card"]')).toHaveLength(0)
    expect(app.find('[data-test="agent-waiting-confirmation"]').exists()).toBe(false)
    expect(app.find('[data-test="agent-memory-link"]').exists()).toBe(false)
    expect(app.text()).not.toContain('mem-smuggled')
    expect(app.text()).not.toContain('草稿正文不该出现在这里')
    expect(app.text()).not.toContain('redis-持久化')
    app.unmount()
  })

  it('reveals no identifier the protocol does not carry', async () => {
    serve(watchedTurn())
    const { app } = await openAndSend(AGENT)
    await expand(app)

    const dom = app.html()
    for (const internal of ['memory_id', 'source_id', 'session_id', 'user_id', 'run-4', 'a-1', 'chunk_id', 'arguments']) {
      expect(dom).not.toContain(internal)
    }
    // The run identity is used for correlation and then dropped: no id is rendered.
    expect(app.text()).not.toContain('run-')
    app.unmount()
  })
})

describe('a turn that does not end well', () => {
  it('marks the running step as failed and fabricates no result for it', async () => {
    serve([
      sseFrame('message.started', { message_id: 'a-1', run_id: 'run-5' }),
      sseFrame('step_started', { run_id: 'run-5', sequence: 1, step_type: 'plan', message: 'model generation 1' }),
      sseFrame('step_started', { run_id: 'run-5', sequence: 2, step_type: 'tool', message: 'tool call 1' }),
      sseFrame('tool_started', { run_id: 'run-5', sequence: 2, tool_name: 'memory_search' }),
      sseFrame('message.failed', {
        run_id: 'run-5',
        code: 'internal_error',
        message: '模型生成失败',
        termination_reason: 'model_call_budget',
      }),
    ])
    const { app } = await openAndSend(AGENT)
    await expand(app)

    const rows = app.findAll('[data-test="agent-steps"] li')
    expect(rows[1].text()).toContain('未成功')
    expect(rows[1].text()).not.toContain('已完成')
    expect(app.find('[data-test="agent-step-detail"]').exists()).toBe(false)
    expect(app.find('[data-test="agent-panel"]').text()).toContain('本轮未完成')
    app.unmount()
  })

  it('a cancelled turn closes its steps as cancelled, and a late frame cannot reopen them', async () => {
    const late = lateDeliverySSEResponse()
    serve(() => late.response)
    const { app } = await openAndSend(AGENT)

    late.push(sseFrame('message.started', { message_id: 'a-1', run_id: 'run-6' }))
    late.push(sseFrame('step_started', { run_id: 'run-6', sequence: 1, step_type: 'plan', message: 'model generation 1' }))
    await flushPromises()

    await app.find('[data-test="abort"]').trigger('click')
    await flushPromises()
    await expand(app)
    expect(app.findAll('[data-test="agent-steps"] li')[0].text()).toContain('已取消')

    // The socket was still draining: the terminal frames the server does not even send
    // for a cancelled turn, and the ones that arrive late, must not move the trail.
    late.push(sseFrame('tool_started', { run_id: 'run-6', sequence: 2, tool_name: 'memory_search' }))
    late.push(sseFrame('message.completed', { run_id: 'run-6', message_id: 'a-1', content: '不该出现的回答' }))
    await flushPromises()
    await flushPromises()

    expect(app.text()).not.toContain('不该出现的回答')
    expect(app.findAll('[data-test="agent-steps"] li')).toHaveLength(1)
    expect(app.find('[data-test="agent-panel"]').text()).toContain('本轮已取消')
    // Let the drained socket close, or the run this test started never settles.
    late.finish()
    await flushPromises()
    app.unmount()
  })
})

describe('an unwatched turn', () => {
  it('sends the body it always sent and shows no trail', async () => {
    serve([
      sseFrame('message.started', { message_id: 'a-1' }),
      sseFrame('message.completed', { message_id: 'a-1', content: '普通回答' }),
    ])
    const { app } = await openAndSend(CHAT)

    const stream = bodies.filter((call) => call.path.endsWith('/stream'))
    expect(stream).toHaveLength(1)
    expect(stream[0].body).toEqual({ content: '讲讲 RDB 与 AOF' })
    expect(app.find('[data-test="agent-panel"]').exists()).toBe(false)
    expect(app.text()).toContain('普通回答')
    app.unmount()
  })

  it('stays silent even if a server sent step frames it never asked for', async () => {
    serve([
      sseFrame('message.started', { message_id: 'a-1' }),
      sseFrame('step_started', { run_id: 'run-7', sequence: 1, step_type: 'plan', message: 'model generation 1' }),
      sseFrame('message.completed', { message_id: 'a-1', content: '普通回答' }),
    ])
    const { app } = await openAndSend(CHAT)

    const stream = bodies.filter((call) => call.path.endsWith('/stream'))
    expect(stream[0].body).not.toHaveProperty('stream_steps')
    expect(app.find('[data-test="agent-panel"]').exists()).toBe(false)
    expect(app.text()).toContain('普通回答')
    app.unmount()
  })
})
