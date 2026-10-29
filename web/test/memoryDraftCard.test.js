import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory } from 'vue-router'

import ChatView from '../src/views/ChatView.vue'
import MemoryDraftCard from '../src/components/MemoryDraftCard.vue'
import MessageBubble from '../src/components/MessageBubble.vue'
import { createAppRouter } from '../src/router/index.js'
import { authStore } from '../src/auth/store.js'
import {
  createHttpMemoryAdapter,
  MEMORY_ERROR_COPY,
  resetMemoryAdapter,
  setMemoryAdapter,
} from '../src/api/memory.js'
import { setTokenProvider } from '../src/api/http.js'
import {
  deferred,
  installFetchStub,
  jsonResponse,
  sseFrame,
  sseResponse,
} from './helpers/transport.js'

/**
 * The confirmation card: the decision a `waiting_confirmation` frame asks for, rendered
 * where it was asked — under the reply whose tool call wrote the row.
 *
 * The two properties worth fighting for are placement and singularity. Placement, because
 * a card attached to the wrong reply is a claim about a sentence the server never proposed
 * it beside: the binding is the turn's own `run_id`, and two turns in one session must each
 * keep their own. Singularity, because a draft has one answer — a double click that costs
 * two requests is a second `confirm` on a row the first one already settled, and the second
 * can only come back as a conflict.
 *
 * Everything the card shows comes from the four projected fields, and one of them — the id —
 * is used and never rendered: it is the address of a stored row, which is nothing a learner
 * can check.
 */

const AGENT = { id: 'ag-1', title: '学习会话', mode: 'learning_agent' }

function cardFixture(over = {}) {
  return {
    draftId: 'mem-card-1',
    scope: 'global',
    topicKey: '',
    content: '用户偏好先看代码再读解释',
    ...over,
  }
}

function mountCard(draft = cardFixture()) {
  return mount(MemoryDraftCard, { props: { draft } })
}

/** The writes the card is allowed to make, as the real adapter sends them. */
function memoryWrites(calls) {
  return calls
    .map((record) => {
      const url = new URL(record.url, 'https://api.test')
      return { path: url.pathname, method: record.init.method ?? 'GET' }
    })
    .filter((entry) => entry.method === 'POST')
}

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  // The session list belongs to an account, so opening one needs an identity: without it
  // the route cannot find `ag-1` and the view starts a fresh session instead.
  authStore.adopt('tok.card', { id: 'user-1', email: 'card@example.test', displayName: '' })
  setTokenProvider(() => 'tok.card')
  resetMemoryAdapter()
  setMemoryAdapter(createHttpMemoryAdapter())
})

describe('what the card shows', () => {
  it('names the layer, the subject and the sentence, and no identifier', () => {
    const app = mountCard(cardFixture({ scope: 'topic', topicKey: 'redis-持久化' }))

    expect(app.find('[data-test="memory-draft-scope"]').text()).toBe('主题记忆')
    expect(app.find('[data-test="memory-draft-topic"]').text()).toContain('redis-持久化')
    expect(app.find('[data-test="memory-draft-content"]').text()).toBe('用户偏好先看代码再读解释')
    expect(app.find('[data-test="memory-draft-confirm"]').text()).toBe('确认并生效')
    expect(app.find('[data-test="memory-draft-reject"]').text()).toBe('拒绝')

    // The id is the address of a stored row. A reader cannot check it, and does not need it
    // to decide — so it appears nowhere in what is shown.
    const shown = `${app.text()}${app.html()}`
    for (const internal of ['mem-card-1', 'draft_id', 'draftId', 'user-', 'session-', 'confidence']) {
      expect(shown).not.toContain(internal)
    }
    app.unmount()
  })

  it('labels a global draft without inventing a subject for it', () => {
    const app = mountCard(cardFixture({ scope: 'global', topicKey: '' }))

    expect(app.find('[data-test="memory-draft-scope"]').text()).toBe('全局画像')
    expect(app.find('[data-test="memory-draft-topic"]').exists()).toBe(false)
    app.unmount()
  })

  it('says which of the three layers an episode belongs to', () => {
    const app = mountCard(cardFixture({ scope: 'episodic' }))

    expect(app.find('[data-test="memory-draft-scope"]').text()).toBe('情景记忆')
    expect(app.find('[data-test="memory-draft-topic"]').exists()).toBe(false)
    app.unmount()
  })
})

describe('one click, one request', () => {
  it('renders without asking the server for anything, because it already has its draft', async () => {
    // The forbidden implementation is "show the newest pending draft": one read of the
    // listing, and a card is attached to whichever row happens to be first — which in a
    // second conversation is somebody else's decision. A card that issued no read cannot
    // have done that, so the absence of a request is itself the assertion.
    const stub = installFetchStub(() => jsonResponse({ data: null, error: null }))
    const app = mountCard(cardFixture())
    await flushPromises()

    expect(stub.calls).toHaveLength(0)
    expect(app.find('[data-test="memory-draft-content"]').text()).toBe('用户偏好先看代码再读解释')
    app.unmount()
  })

  it('sends confirm for its own draft once, even when the button is hammered', async () => {
    const gate = deferred()
    const stub = installFetchStub((url, init = {}) =>
      String(init.method ?? 'GET') === 'POST' ? gate.promise : jsonResponse({ data: null, error: null }),
    )
    const app = mountCard(cardFixture({ draftId: 'mem-confirm-9' }))

    await app.find('[data-test="memory-draft-confirm"]').trigger('click')
    await app.find('[data-test="memory-draft-confirm"]').trigger('click')
    await app.find('[data-test="memory-draft-confirm"]').trigger('click')
    // The guard is set before the request is made, so the second and third click — all of
    // them long before any response — never reach the transport at all.
    expect(memoryWrites(stub.calls)).toEqual([
      { path: '/api/v1/memories/mem-confirm-9/confirm', method: 'POST' },
    ])

    gate.resolve(jsonResponse({ data: { memory_id: 'mem-confirm-9', status: 'active' }, error: null }))
    await flushPromises()

    expect(app.text()).toContain('已确认并生效')
    expect(app.find('[data-test="memory-draft-confirm"]').exists()).toBe(false)
    expect(app.find('[data-test="memory-draft-reject"]').exists()).toBe(false)
    // Still exactly one write: deciding closes the card rather than arming it again.
    expect(memoryWrites(stub.calls)).toHaveLength(1)
    app.unmount()
  })

  it('sends reject the same way, and says 已拒绝 in place', async () => {
    const stub = installFetchStub(() => jsonResponse({ data: null, error: null }))
    const app = mountCard(cardFixture({ draftId: 'mem-reject-3' }))

    await app.find('[data-test="memory-draft-reject"]').trigger('click')
    await app.find('[data-test="memory-draft-reject"]').trigger('click')
    await flushPromises()

    expect(memoryWrites(stub.calls)).toEqual([
      { path: '/api/v1/memories/mem-reject-3/reject', method: 'POST' },
    ])
    expect(app.text()).toContain('已拒绝')
    expect(app.find('[data-test="memory-draft-confirm"]').exists()).toBe(false)
    app.unmount()
  })

  it('keeps the buttons when the server said no, because the row is still waiting', async () => {
    const stub = installFetchStub(() =>
      jsonResponse(
        { data: null, error: { code: 'memory_already_confirmed', message: 'already confirmed' } },
        { status: 409 },
      ),
    )
    const app = mountCard(cardFixture())

    await app.find('[data-test="memory-draft-confirm"]').trigger('click')
    await flushPromises()

    // The lifecycle sentence is the memory page's own mapping, reused rather than restated.
    expect(app.text()).toContain(MEMORY_ERROR_COPY.alreadyConfirmed)
    expect(app.text()).not.toContain('已确认并生效')
    expect(app.find('[data-test="memory-draft-confirm"]').exists()).toBe(true)
    expect(memoryWrites(stub.calls)).toHaveLength(1)
    app.unmount()
  })

  it('reports a missing row and an expired session with the existing copy', async () => {
    for (const [status, code, copy] of [
      [404, 'not_found', MEMORY_ERROR_COPY.notFound],
      [401, 'unauthorized', MEMORY_ERROR_COPY.unauthorized],
    ]) {
      installFetchStub(() => jsonResponse({ data: null, error: { code, message: 'x' } }, { status }))
      const app = mountCard(cardFixture())
      await app.find('[data-test="memory-draft-reject"]').trigger('click')
      await flushPromises()

      expect(app.text()).toContain(copy)
      expect(app.find('[data-test="memory-draft-result"]').exists()).toBe(false)
      app.unmount()
    }
  })
})

/** A watched turn, with the frames one `propose_memory` call actually produces. */
function proposalTurn(runId, messageId, sequenceBase, draft) {
  const sequence = sequenceBase
  const frames = [
    sseFrame('message.started', { message_id: messageId, run_id: runId }),
    sseFrame('step_started', {
      run_id: runId,
      sequence,
      step_type: 'tool',
      message: 'tool call 1',
    }),
    sseFrame('tool_started', { run_id: runId, sequence, tool_name: 'propose_memory' }),
    sseFrame('tool_result', {
      run_id: runId,
      sequence,
      tool_name: 'propose_memory',
      content: '新增 1 条待确认记忆草稿',
      duration_millis: 4,
    }),
    sseFrame('waiting_confirmation', {
      run_id: runId,
      sequence,
      message: '本回合生成了 1 条待确认记忆草稿，请在当前回复下方的确认卡片中处理。',
      ...(draft ? { memory_draft: draft } : {}),
    }),
    sseFrame('message.delta', { run_id: runId, message_id: messageId, delta: `回答 ${messageId}` }),
    sseFrame('message.completed', {
      run_id: runId,
      message_id: messageId,
      content: `回答 ${messageId}`,
    }),
  ]
  return frames
}

function draftOnWire(draftId, content) {
  return { draft_id: draftId, scope: 'global', content }
}

describe('which reply a card belongs to', () => {
  let turns

  function serveTurns() {
    installFetchStub((url) => {
      const path = new URL(String(url), 'https://api.test').pathname
      if (path.endsWith('/stream')) return sseResponse(turns.shift() ?? [])
      if (path === '/api/v1/sessions') {
        return jsonResponse({ data: { items: [AGENT] }, error: null })
      }
      if (/\/messages$/.test(path)) return jsonResponse({ data: { items: [] }, error: null })
      return jsonResponse({ data: null, error: { code: 'not_found', message: 'no' } }, { status: 404 })
    })
  }

  async function open() {
    const router = createAppRouter({ authStore, history: createMemoryHistory() })
    await router.push(`/chat/${AGENT.id}`)
    await router.isReady()
    const app = mount(ChatView, { attachTo: document.body, global: { plugins: [router] } })
    await flushPromises()
    await flushPromises()
    return { app, router }
  }

  async function send(app, text) {
    await app.find('textarea').setValue(text)
    await app.find('[data-test="send"]').trigger('click')
    await flushPromises()
    await flushPromises()
  }

  it('puts each turn draft under that turn answer, and never in the panel', async () => {
    turns = [
      proposalTurn('run-1', 'a-1', 1, draftOnWire('mem-first', '第一条草稿')),
      proposalTurn('run-2', 'a-2', 1, draftOnWire('mem-second', '第二条草稿')),
    ]
    serveTurns()
    const { app } = await open()

    const replies = () =>
      app
        .findAllComponents(MessageBubble)
        .filter((bubble) => bubble.props('message').role === 'assistant')

    await send(app, '第一个问题')
    expect(replies()).toHaveLength(1)
    expect(replies()[0].text()).toContain('回答 a-1')
    expect(replies()[0].find('[data-test="memory-draft-content"]').text()).toBe('第一条草稿')
    // The old affordance is gone: no pointer up in the progress strip, no route away.
    expect(app.find('[data-test="agent-waiting-confirmation"]').exists()).toBe(false)
    expect(app.find('[data-test="agent-memory-link"]').exists()).toBe(false)

    await send(app, '第二个问题')
    const cards = app.findAll('[data-test="memory-draft-card"]')
    expect(cards).toHaveLength(2)
    expect(replies()).toHaveLength(2)
    // The first reply still holds its own draft: a second turn cannot move it, and the
    // second card is not a copy of the first.
    expect(replies()[0].find('[data-test="memory-draft-content"]').text()).toBe('第一条草稿')
    expect(replies()[1].find('[data-test="memory-draft-content"]').text()).toBe('第二条草稿')
    // Each card sits inside its own assistant turn, below that answer's text.
    expect(replies()[1].text()).toContain('回答 a-2')
    // Neither id is on screen.
    expect(app.text()).not.toContain('mem-first')
    expect(app.text()).not.toContain('mem-second')
    app.unmount()
  })

  it('shows no card at all when the frame names no draft to decide', async () => {
    // The server only sends this frame with a card, so a frame without one is a protocol
    // disagreement. Filling it in from the newest pending draft is the bug this case exists
    // for: in a second conversation that row is somebody else's decision.
    turns = [proposalTurn('run-3', 'a-3', 1, null)]
    serveTurns()
    const { app } = await open()
    await send(app, '记一条')

    // The turn really ran, so the absence below is a decision and not a broken harness.
    expect(app.text()).toContain('回答 a-3')
    expect(app.findAll('[data-test="memory-draft-card"]')).toHaveLength(0)
    expect(app.find('[data-test="agent-waiting-confirmation"]').exists()).toBe(false)
    app.unmount()
  })

  it('drops a card from a frame that belongs to another turn', async () => {
    // A straggler from a run the page never started, arriving inside this turn's stream.
    turns = [
      [
        ...proposalTurn('run-4', 'a-4', 1, null).slice(0, -2),
        sseFrame('waiting_confirmation', {
          run_id: 'run-foreign',
          sequence: 9,
          message: '本回合生成了 1 条待确认记忆草稿，请在当前回复下方的确认卡片中处理。',
          memory_draft: draftOnWire('mem-foreign', '别轮的草稿'),
        }),
        sseFrame('message.delta', { run_id: 'run-4', message_id: 'a-4', delta: '回答 a-4' }),
        sseFrame('message.completed', { run_id: 'run-4', message_id: 'a-4', content: '回答 a-4' }),
      ],
    ]
    serveTurns()
    const { app } = await open()
    await send(app, '讲讲 RDB')

    expect(app.text()).toContain('回答 a-4')
    expect(app.findAll('[data-test="memory-draft-card"]')).toHaveLength(0)
    expect(app.text()).not.toContain('别轮的草稿')
    app.unmount()
  })
})
