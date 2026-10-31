import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory } from 'vue-router'

import ChatView from '../src/views/ChatView.vue'
import { createAppRouter } from '../src/router/index.js'
import { authStore } from '../src/auth/store.js'
import { setTokenProvider } from '../src/api/http.js'
import {
  installFetchStub,
  jsonResponse,
  lateDeliverySSEResponse,
  sseFrame,
  sseResponse,
} from './helpers/transport.js'

/**
 * The ids the client makes for itself.
 *
 * The page carries them in its `data-test` attributes (`message-msg-<uuid>`), and a
 * UUID is hex digits and dashes — so it can contain "a-1" by coincidence, about one
 * turn in thirty. The checks below are about the *server's* identifiers, so the
 * client's own ids are fixed here: a coincidence must not decide whether this file
 * passes. The case that tells the two apart is "tells the client's own id apart from
 * the server's identifier".
 */
const clientIds = vi.hoisted(() => ({ suffix: 'fixed-local-id' }))

vi.mock('../src/lib/id.js', () => ({
  localId: (prefix = 'tmp') => `${prefix}-${clientIds.suffix}`,
}))

/**
 * An agent turn whose text arrives in blocks, seen through the screen.
 *
 * The property these cases exist for is that a paragraph the page shows as the answer is a
 * paragraph the server confirmed, and that a paragraph the model wrote on the way to a tool
 * stops being mistaken for one - without disappearing. Everything else here is the same
 * guarantee seen from the other side: text still growing while the provider is mid-sentence,
 * a cancelled turn keeping its words without keeping its claim, and a late fragment having
 * nowhere left to write.
 */

const AGENT = { id: 'ag-1', title: '学习会话', mode: 'learning_agent' }
const CHAT = { id: 'ch-1', title: '普通对话', mode: 'chat' }
const RUN = 'run-4'

function started(blockId, round) {
  return sseFrame('agent.text.started', { run_id: RUN, block_id: blockId, round })
}

function delta(blockId, content) {
  return sseFrame('agent.text.delta', { run_id: RUN, block_id: blockId, content })
}

function ended(blockId, kind) {
  return sseFrame('agent.text.ended', { run_id: RUN, block_id: blockId, kind })
}

function note(blockId, round, fragments) {
  return [
    started(blockId, round),
    ...fragments.map((text) => delta(blockId, text)),
    ended(blockId, 'process'),
  ]
}

function candidate(blockId, round, fragments) {
  return [
    started(blockId, round),
    ...fragments.map((text) => delta(blockId, text)),
    ended(blockId, 'answer_candidate'),
  ]
}

const TOOL_STEP = [
  sseFrame('step_started', {
    run_id: RUN,
    sequence: 1,
    step_type: 'plan',
    message: 'model generation 1',
  }),
  ...note('blk-1', 1, ['我先查一下你的', 'RPC 笔记。']),
  sseFrame('step_started', {
    run_id: RUN,
    sequence: 2,
    step_type: 'tool',
    message: 'tool call 1',
  }),
  sseFrame('tool_started', { run_id: RUN, sequence: 2, tool_name: 'knowledge_search' }),
  sseFrame('tool_result', {
    run_id: RUN,
    sequence: 2,
    tool_name: 'knowledge_search',
    content: '本轮检索了你的笔记。',
    duration_millis: 9,
  }),
]

const ANSWER_STEP = [
  sseFrame('step_started', {
    run_id: RUN,
    sequence: 3,
    step_type: 'plan',
    message: 'model generation 2',
  }),
  ...candidate('blk-2', 2, ['TCP 是字节流，', '不会保留消息边界。']),
]

const ANSWER = 'TCP 是字节流，不会保留消息边界。'

function completedFrame(content = ANSWER, extra = {}) {
  return sseFrame('message.completed', {
    run_id: RUN,
    message_id: 'a-1',
    content,
    final_block_id: 'blk-2',
    ...extra,
  })
}

let bodies

function serve(frames) {
  installFetchStub((url, init = {}) => {
    const path = new URL(String(url), 'https://api.test').pathname
    bodies.push({ path, method: init.method ?? 'GET', body: init.body ? JSON.parse(init.body) : null })
    if (path.endsWith('/stream')) {
      const given = typeof frames === 'function' ? frames() : frames
      return given && given.body ? given : sseResponse(given)
    }
    if (path === '/api/v1/sessions') return jsonResponse({ data: { items: [AGENT, CHAT] }, error: null })
    if (/\/messages$/.test(path)) return jsonResponse({ data: { items: [] }, error: null })
    return jsonResponse({ data: null, error: { code: 'not_found', message: 'no' } }, { status: 404 })
  })
}

async function openAndSend(session, send = 'RPC 为什么没有消息边界？') {
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

// The draining socket takes one frame at a time, so a scripted turn is pushed fragment by
// fragment; a spread of frames would hand it the first and drop the rest.
function pushAll(late, frames) {
  for (const one of frames) late.push(one)
}

function assistantTurn(app) {
  const turns = app.findAll('[data-test^="message-"]')
  return turns[turns.length - 1]
}

/**
 * Whether the markup mentions an identifier as an identifier.
 *
 * A match is a whole identifier: the characters on either side of it are not part of
 * another word. `data-test="message-a-1"` is the server's id on the page, and
 * `消息：a-1` is the server's id in the text; `data-test="message-msg-ba-1c3d-…"` is
 * the client's own id and says nothing about the server's.
 */
function mentionsIdentifier(markup, id) {
  const escaped = id.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  return new RegExp(`(?<![0-9A-Za-z])${escaped}(?![0-9A-Za-z])`).test(markup)
}

/** The reply's own body, with the fold's paragraphs left out. */
function answerBody(turn) {
  const bodies = turn.findAll('[data-test="markdown-body"]')
  return bodies[bodies.length - 1]
}

beforeEach(() => {
  bodies = []
  authStore.adopt('tok.ui', { id: 'user-1', email: 'ui@example.test', displayName: '' })
  setTokenProvider(() => 'tok.ui')
})

describe('text that arrives while the model is still writing', () => {
  it('grows on the screen before the round has finished', async () => {
    const late = lateDeliverySSEResponse()
    serve(() => late.response)
    const { app } = await openAndSend(AGENT)

    late.push(sseFrame('message.started', { message_id: 'a-1', run_id: RUN }))
    pushAll(late, TOOL_STEP)
    await flushPromises()
    expect(assistantTurn(app).text()).toContain('我先查一下你的RPC 笔记。')
    // Nothing has been folded yet, because nothing has been separated yet.
    expect(app.find('[data-test="process-notes"]').exists()).toBe(false)

    // The answer's own round: its opening frame and first fragment, with the round still
    // open - so the learner is reading a sentence the provider has not finished.
    pushAll(late, ANSWER_STEP.slice(0, 3))
    await flushPromises()
    expect(assistantTurn(app).text()).toContain('TCP 是字节流，')
    expect(assistantTurn(app).text()).not.toContain('不会保留消息边界。')
    expect(assistantTurn(app).text()).toContain('正在生成')
    expect(app.find('[data-test="process-notes"]').exists()).toBe(false)

    pushAll(late, ANSWER_STEP.slice(3, 5))
    await flushPromises()
    expect(assistantTurn(app).text()).toContain('不会保留消息边界。')
    // Still nothing folded: the round has not said it was the answer yet.
    expect(app.find('[data-test="process-notes"]').exists()).toBe(false)

    late.push(completedFrame())
    await flushPromises()
    await flushPromises()
    late.finish()
    app.unmount()
  })

  it('separates the note from the answer once the answer is confirmed', async () => {
    serve([sseFrame('message.started', { message_id: 'a-1', run_id: RUN }), ...TOOL_STEP, ...ANSWER_STEP, completedFrame()])
    const { app } = await openAndSend(AGENT)

    const turn = assistantTurn(app)
    // The body is the confirmed answer alone; the note is no longer posing as a reply.
    expect(answerBody(turn).text()).toBe(ANSWER)
    const fold = app.find('[data-test="process-notes"]')
    expect(fold.exists()).toBe(true)
    // Closed by default, and the words inside it are the model's own.
    expect(app.find('[data-test="process-note-list"]').exists()).toBe(false)
    expect(fold.text()).toContain('1 条说明')

    await app.find('[data-test="process-notes-toggle"]').trigger('click')
    await flushPromises()
    const notes = app.findAll('[data-test^="process-note-"]')
    const list = app.find('[data-test="process-note-list"]')
    expect(list.exists()).toBe(true)
    expect(list.text()).toContain('我先查一下你的RPC 笔记。')
    expect(notes.length).toBeGreaterThan(0)
    // The note is readable, but it lives in the fold and nowhere else.
    expect(answerBody(turn).text()).toBe(ANSWER)
    expect(turn.text()).toContain('我先查一下你的RPC 笔记。')
    app.unmount()
  })

  it('reports no identifier or protocol word a learner cannot define', async () => {
    serve([sseFrame('message.started', { message_id: 'a-1', run_id: RUN }), ...TOOL_STEP, ...ANSWER_STEP, completedFrame()])
    const { app } = await openAndSend(AGENT)
    await app.find('[data-test="process-notes-toggle"]').trigger('click')
    await flushPromises()

    // A block id is a correlation handle, and a kind is a server enum: neither is a word a
    // learner can define, so neither may appear on the page at all.
    //
    // Both forms of the check are here. The literal one says none of these characters
    // is anywhere in the markup; the identifier one says the server's identifier is not
    // on the page as an identifier - in an attribute or in the text. The first is only
    // possible because this file fixes the client's own ids (see `clientIds`): a UUID is
    // hex digits and dashes, and it can contain "a-1" by coincidence, which has nothing
    // to do with the server's identifier. The second holds whatever the client does.
    const dom = app.html()
    for (const internal of ['blk-1', 'blk-2', 'block_id', 'final_block_id', 'answer_candidate', 'run-4', 'a-1']) {
      expect(dom, internal).not.toContain(internal)
      expect(mentionsIdentifier(dom, internal), internal).toBe(false)
    }
    for (const jargon of ['round', 'block', 'process', 'candidate', 'failed']) {
      expect(app.text()).not.toContain(jargon)
    }
    app.unmount()
  })

  it("tells the client's own id apart from the server's identifier", async () => {
    // The client generates the id it puts in `data-test`, and a UUID can contain the
    // server's identifier's characters by coincidence. The server's identifier must
    // still be caught when it appears on its own - that is what a leak would look like.
    clientIds.suffix = 'ba-1c3d-4111-8111-111111111111'
    serve([sseFrame('message.started', { message_id: 'a-1', run_id: RUN }), ...TOOL_STEP, ...ANSWER_STEP, completedFrame()])
    const { app } = await openAndSend(AGENT)
    await app.find('[data-test="process-notes-toggle"]').trigger('click')
    await flushPromises()

    const dom = app.html()
    // The client's own id is on the page, and it happens to carry the same characters.
    expect(dom).toContain('message-asst-ba-1c3d-4111-8111-111111111111')
    expect(mentionsIdentifier(dom, 'a-1')).toBe(false)
    // The server's identifier is not on the page either.
    expect(dom).not.toContain('message-a-1')
    expect(app.text()).not.toContain('a-1')

    // And the check still catches it when it does appear - as an attribute, and as text.
    expect(mentionsIdentifier('data-test="message-a-1"', 'a-1')).toBe(true)
    expect(mentionsIdentifier('消息：a-1', 'a-1')).toBe(true)
    app.unmount()
  })

  it('keeps the citations beside the answer that earned them', async () => {
    serve([
      sseFrame('message.started', { message_id: 'a-1', run_id: RUN }),
      ...TOOL_STEP,
      ...ANSWER_STEP,
      completedFrame(ANSWER, {
        sources: [
          {
            source_id: 'src-1',
            document_id: 'doc-1',
            collection_id: 'col-1',
            chunk_id: 'c-1',
            title: 'redis-persistence.md',
            snippet: 'AOF 的 fsync 策略…',
            score: 0.8,
          },
        ],
      }),
    ])
    const { app } = await openAndSend(AGENT)

    expect(app.findAll('[data-test^="citation-"]').length).toBeGreaterThan(0)
    expect(answerBody(assistantTurn(app)).text()).toBe(ANSWER)
    app.unmount()
  })

  it('still puts the memory card under the reply that stored the draft', async () => {
    const withDraft = [
      sseFrame('message.started', { message_id: 'a-1', run_id: RUN }),
      TOOL_STEP[0],
      ...note('blk-1', 1, ['我先把这条记下来。']),
      sseFrame('step_started', { run_id: RUN, sequence: 2, step_type: 'tool', message: 'tool call 1' }),
      sseFrame('tool_started', { run_id: RUN, sequence: 2, tool_name: 'propose_memory' }),
      sseFrame('tool_result', {
        run_id: RUN,
        sequence: 2,
        tool_name: 'propose_memory',
        content: '新增 1 条待确认记忆草稿',
        duration_millis: 4,
      }),
      sseFrame('waiting_confirmation', {
        run_id: RUN,
        sequence: 2,
        message: '本回合生成了 1 条待确认记忆草稿，请在当前回复下方的确认卡片中处理。',
        memory_draft: {
          draft_id: 'mem-draft-3',
          scope: 'global',
          content: '用户偏好先看代码示例',
        },
      }),
      ...ANSWER_STEP,
      completedFrame(),
    ]
    serve(withDraft)
    const { app } = await openAndSend(AGENT)

    const cards = app.findAll('[data-test="memory-draft-card"]')
    expect(cards).toHaveLength(1)
    expect(cards[0].text()).toContain('用户偏好先看代码示例')
    // The card belongs to this reply, not to the panel and not to another turn's bubble.
    expect(assistantTurn(app).find('[data-test="memory-draft-card"]').exists()).toBe(true)
    expect(app.find('[data-test="agent-memory-link"]').exists()).toBe(false)
    app.unmount()
  })
})

describe('a turn that cannot be confirmed', () => {
  it('keeps broken text on screen without ever calling it an answer', async () => {
    serve([
      sseFrame('message.started', { message_id: 'a-1', run_id: RUN }),
      ...TOOL_STEP,
      sseFrame('step_started', { run_id: RUN, sequence: 3, step_type: 'plan', message: 'model generation 2' }),
      started('blk-2', 2),
      delta('blk-2', '写到一半的句子'),
      ended('blk-2', 'failed'),
      sseFrame('message.failed', {
        run_id: RUN,
        code: 'internal_error',
        message: '回答生成失败。',
        termination_reason: 'model_call_limit',
      }),
    ])
    const { app } = await openAndSend(AGENT)

    const turn = assistantTurn(app)
    expect(turn.text()).toContain('写到一半的句子')
    // A failed round is not a candidate, so nothing folds and nothing is confirmed.
    expect(app.find('[data-test="process-notes"]').exists()).toBe(false)
    expect(turn.text()).toContain('回答生成失败')
    app.unmount()
  })

  it('folds nothing when the completed frame names a block it never published', async () => {
    // A terminal frame the reducer took is the body, and an unknown block id confirms nothing -
    // so no paragraph is filed away as a note, and nothing claims a block this page has never
    // seen. The stronger promise, that such a frame cannot be sent at all, belongs to the
    // server and is enforced there: see TestConfirmedBlockMustBeAFinishedCandidate.
    serve([
      sseFrame('message.started', { message_id: 'a-1', run_id: RUN }),
      ...TOOL_STEP,
      ...ANSWER_STEP,
      completedFrame('屏幕上从来没有的一段话', { final_block_id: 'blk-404' }),
    ])
    const { app } = await openAndSend(AGENT)

    expect(app.find('[data-test="process-notes"]').exists()).toBe(false)
    expect(answerBody(assistantTurn(app)).text()).toBe('屏幕上从来没有的一段话')
    app.unmount()
  })

  it('lets a cancelled turn keep its words and drop its claim', async () => {
    const late = lateDeliverySSEResponse()
    serve(() => late.response)
    const { app } = await openAndSend(AGENT)

    late.push(sseFrame('message.started', { message_id: 'a-1', run_id: RUN }))
    pushAll(late, TOOL_STEP)
    await flushPromises()
    await app.find('[data-test="abort"]').trigger('click')
    await flushPromises()

    const written = assistantTurn(app).text()
    expect(written).toContain('我先查一下你的RPC 笔记。')
    // The socket was still draining. A fragment that arrives after the learner stopped the
    // turn cannot lengthen a paragraph that has already been explained as an incomplete one.
    late.push(delta('blk-1', '迟到的一段'))
    late.push(sseFrame('message.completed', { run_id: RUN, message_id: 'a-1', content: '不该出现的回答' }))
    await flushPromises()
    await flushPromises()
    expect(assistantTurn(app).text()).toBe(written)
    expect(app.text()).not.toContain('迟到的一段')
    expect(app.text()).not.toContain('不该出现的回答')
    late.finish()
    app.unmount()
  })
})

describe('a turn that did not ask to be watched', () => {
  it('still reads its answer from the deltas it always used', async () => {
    serve([
      sseFrame('message.started', { message_id: 'a-1' }),
      sseFrame('message.delta', { message_id: 'a-1', delta: '普通对话的' }),
      sseFrame('message.delta', { message_id: 'a-1', delta: '一半答案' }),
      sseFrame('message.completed', { message_id: 'a-1', content: '普通对话的一半答案' }),
    ])
    const { app } = await openAndSend(CHAT)

    const stream = bodies.filter((call) => call.path.endsWith('/stream'))
    expect(stream[0].body).toEqual({ content: 'RPC 为什么没有消息边界？' })
    expect(assistantTurn(app).find('[data-test="markdown-body"]').text()).toBe('普通对话的一半答案')
    expect(app.find('[data-test="process-notes"]').exists()).toBe(false)
    app.unmount()
  })
})
