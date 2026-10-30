import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AgentRunPanel from '../src/components/AgentRunPanel.vue'
import AuthLayout from '../src/components/AuthLayout.vue'
import ChatView from '../src/views/ChatView.vue'
import SessionPanel from '../src/components/SessionPanel.vue'
import {
  agentStageDetail,
  agentStageTitle,
  AgentStage,
  deriveAgentStage,
  isStageActive,
} from '../src/lib/agentRun.js'
import {
  abortRun,
  createRunState,
  reduceRun,
  RunPhase,
} from '../src/lib/chatStream.js'
import { useChat } from '../src/composables/useChat.js'
import { setTokenProvider } from '../src/api/http.js'
import {
  jsonResponse,
  lateDeliverySSEResponse,
  sseFrame,
  sseResponse,
} from './helpers/transport.js'

// ChatView resolves the route through the router, so the screen-level cases
// below drive it with a stub whose params a test can set.
const routerMock = vi.hoisted(() => ({
  route: { params: {}, query: {}, path: '/chat', name: 'chat' },
  push: vi.fn(),
  replace: vi.fn(),
}))

vi.mock('vue-router', () => ({
  useRoute: () => routerMock.route,
  useRouter: () => ({ push: routerMock.push, replace: routerMock.replace }),
}))

const AGENT = { id: 'ag-1', title: '学习会话', mode: 'learning_agent' }
const AGENT_B = { id: 'ag-2', title: '另一个学习会话', mode: 'learning_agent' }
const CHAT = { id: 'ch-1', title: '普通对话', mode: 'chat' }

/** Builds a run state parked in the given phase, through the real reducer. */
function run(phase) {
  let state = reduceRun(createRunState({ clientRunId: 'r', placeholderId: 'p' }), {
    kind: 'started',
    runId: '',
    messageId: 'a-1',
  })
  if (phase === RunPhase.Idle) return createRunState({ clientRunId: 'r', placeholderId: 'p' })
  if (phase === RunPhase.Streaming) return reduceRun(state, streaming('内容'))
  if (phase === RunPhase.Completed) {
    return reduceRun(state, {
      kind: 'completed',
      content: '内容',
      runId: '',
      messageId: 'a-1',
      sources: [],
    })
  }
  if (phase === RunPhase.Failed) {
    return reduceRun(state, {
      kind: 'failed',
      runId: '',
      messageId: 'a-1',
      code: 'model_timeout',
      message: '模型超时',
    })
  }
  state = reduceRun(state, streaming('半'))
  return abortRun(state)
}

function streaming(delta) {
  return { kind: 'delta', delta, runId: '', messageId: 'a-1' }
}

describe('deriveAgentStage maps the existing state machine', () => {
  it('has no stage at all before a turn is sent', () => {
    expect(deriveAgentStage(null)).toBe(AgentStage.None)
    expect(agentStageTitle(AgentStage.None)).toBe('')
  })

  it('maps every phase onto exactly one stage', () => {
    expect(deriveAgentStage(createRunState({ clientRunId: 'r', placeholderId: 'p' }))).toBe(
      AgentStage.Waiting,
    )
    expect(deriveAgentStage(run(RunPhase.Streaming))).toBe(AgentStage.Generating)
    expect(deriveAgentStage(run(RunPhase.Completed))).toBe(AgentStage.Completed)
    expect(deriveAgentStage(run(RunPhase.Failed))).toBe(AgentStage.Failed)
  })

  it('reports a cancelled run as cancelled, not as a failure', () => {
    expect(deriveAgentStage(run(RunPhase.Aborted))).toBe(AgentStage.Cancelled)
  })

  it('treats an unknown phase as no stage rather than guessing', () => {
    expect(deriveAgentStage({ phase: 'teleporting' })).toBe(AgentStage.None)
  })

  it('only calls the two pre-terminal stages active', () => {
    expect(isStageActive(AgentStage.Waiting)).toBe(true)
    expect(isStageActive(AgentStage.Generating)).toBe(true)
    expect(isStageActive(AgentStage.Completed)).toBe(false)
    expect(isStageActive(AgentStage.Failed)).toBe(false)
    expect(isStageActive(AgentStage.Cancelled)).toBe(false)
    expect(isStageActive(AgentStage.None)).toBe(false)
  })

  it('words each stage without claiming work that did not happen', () => {
    const all = Object.values(AgentStage).map((stage) => agentStageTitle(stage)).join(' | ')
    for (const claim of ['已检索', '调用了工具', 'knowledge_search', '已出题', '记忆']) {
      expect(all).not.toContain(claim)
    }
    expect(agentStageTitle(AgentStage.Waiting)).toContain('等待')
  })

  it('surfaces the real error text on failure and cancellation', () => {
    const failed = run(RunPhase.Failed)
    expect(agentStageDetail(AgentStage.Failed, failed)).toBe('模型超时')
    const cancelled = { phase: RunPhase.Aborted, error: { code: 'aborted', message: '已停止本次回答的生成。' } }
    expect(agentStageDetail(AgentStage.Cancelled, cancelled)).toContain('已停止')
  })

  it('adds no detail line when there is nothing to add', () => {
    expect(agentStageDetail(AgentStage.Failed, null)).toBe('')
    expect(agentStageDetail(AgentStage.Completed, run(RunPhase.Completed))).toBe('')
    expect(agentStageDetail(AgentStage.None, null)).toBe('')
  })
})

describe('AgentRunPanel rendering rules', () => {
  function mountPanel(props = {}) {
    return mount(AgentRunPanel, {
      props: { stage: AgentStage.Generating, ...props },
    })
  }

  it('renders nothing at all when there is no turn', () => {
    const app = mountPanel({ stage: AgentStage.None })
    expect(app.find('[data-test="agent-panel"]').exists()).toBe(false)
    expect(app.text()).toBe('')
  })

  it('shows one sentence per stage, with no step-shaped decoration', () => {
    const app = mountPanel({ stage: AgentStage.Waiting })
    expect(app.find('[data-test="agent-panel"]').exists()).toBe(true)
    // The detail line only exists to add something the title did not say.
    expect(app.find('[data-test="agent-stage-detail"]').exists()).toBe(false)
    expect(app.text()).toBe('已发出请求，等待服务开始回答。')
    expect(app.find('[data-test="agent-steps"]').exists()).toBe(false)
    expect(app.find('[data-test="agent-steps-toggle"]').exists()).toBe(false)
  })

  it('does not print the stage title twice', () => {
    for (const stage of [AgentStage.Waiting, AgentStage.Completed]) {
      const text = mountPanel({ stage }).text()
      const title = agentStageTitle(stage)
      expect(text.split(title)).toHaveLength(2) // once, then nothing
    }
  })

  it('adds the progressive note only while text is actually arriving', () => {
    const bare = mountPanel({ stage: AgentStage.Generating })
    expect(bare.find('[data-test="agent-stage-detail"]').exists()).toBe(false)

    const streaming = mountPanel({
      stage: AgentStage.Generating,
      run: { text: '已经到了一段', error: null },
    })
    expect(streaming.find('[data-test="agent-stage-detail"]').text()).toContain('逐步显示')
  })

  it('announces itself while a turn is in progress only', () => {
    expect(mountPanel({ stage: AgentStage.Generating }).attributes('aria-live')).toBe('polite')
    expect(mountPanel({ stage: AgentStage.Completed }).attributes('aria-live')).toBeUndefined()
  })

  it('reveals the step section only when steps are actually supplied', async () => {
    const app = mountPanel({
      stage: AgentStage.Generating,
      steps: [{ id: 's1', label: '来自真实事件的步骤' }],
      expanded: false,
    })
    const toggle = app.find('[data-test="agent-steps-toggle"]')
    expect(toggle.exists()).toBe(true)
    expect(toggle.attributes('aria-expanded')).toBe('false')
    expect(app.find('[data-test="agent-steps"]').exists()).toBe(false)

    await toggle.trigger('click')
    expect(app.emitted('update:expanded').at(-1)).toEqual([true])
  })

  it('renders supplied steps once expanded, as text', () => {
    const app = mountPanel({
      stage: AgentStage.Completed,
      steps: [{ id: 's1', label: '步骤甲' }, { id: 's2', label: '<img src=x>' }],
      expanded: true,
    })
    const items = app.findAll('.agent__step-label')
    expect(items).toHaveLength(2)
    expect(items[1].text()).toBe('<img src=x>')
    expect(app.find('img').exists()).toBe(false)
  })

  it('carries the failure and cancellation notice', () => {
    const failed = mountPanel({
      stage: AgentStage.Failed,
      run: { error: { code: 'model_timeout', message: '模型超时' } },
    })
    expect(failed.text()).toContain('模型超时')
    expect(failed.text()).toContain('model_timeout')

    const cancelled = mountPanel({
      stage: AgentStage.Cancelled,
      run: { error: { code: 'aborted', message: '已停止本次回答的生成。' } },
    })
    expect(cancelled.text()).toContain('已停止')
  })
})

describe('the agent stage follows the four SSE events through useChat', () => {
  beforeEach(() => {
    vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
    setTokenProvider(() => 'tok.agent')
  })

  function serve(frames) {
    globalThis.fetch = async (url, init = {}) => {
      const path = new URL(String(url), 'https://api.test').pathname
      if (path.endsWith('/stream')) return sseResponse(frames)
      if (path === '/api/v1/sessions') return jsonResponse({ data: { items: [AGENT] }, error: null })
      if (/\/messages$/.test(path)) return jsonResponse({ data: { items: [] }, error: null })
      if (init.method === 'DELETE') return jsonResponse(null, { status: 204 })
      return jsonResponse({ data: null, error: { code: 'not_found', message: 'no' } }, { status: 404 })
    }
  }

  it('walks none -> waiting -> generating -> completed', async () => {
    serve([
      sseFrame('message.started', { message_id: 'a-1' }),
      sseFrame('message.delta', { message_id: 'a-1', delta: '先讲一半' }),
      sseFrame('message.completed', { message_id: 'a-1', content: '先讲一半，后讲一半' }),
    ])
    const chat = useChat()
    expect(chat.agentStage.value).toBe(AgentStage.None)

    await chat.openSession('ag-1')
    expect(chat.agentStage.value).toBe(AgentStage.None) // opened, nothing sent

    await chat.send('讲讲上下文预算')
    expect(chat.agentStage.value).toBe(AgentStage.Completed)
    expect(chat.messages.value.at(-1).content).toBe('先讲一半，后讲一半')
  })

  it('reads a failed stream as the failed stage', async () => {
    serve([
      sseFrame('message.started', { message_id: 'a-1' }),
      sseFrame('message.delta', { message_id: 'a-1', delta: '写到这' }),
      sseFrame('message.failed', { message_id: 'a-1', code: 'invalid_request', message: '上下文过长' }),
    ])
    const chat = useChat()
    await chat.openSession('ag-1')
    await chat.send('问题')

    expect(chat.agentStage.value).toBe(AgentStage.Failed)
    expect(chat.messages.value.at(-1).content).toBe('写到这')
  })

  it('reads a cancellation as the cancelled stage', async () => {
    const orphan = lateDeliverySSEResponse()
    let streamSignal = null
    globalThis.fetch = async (url, init = {}) => {
      const path = new URL(String(url), 'https://api.test').pathname
      if (path.endsWith('/stream')) {
        streamSignal = init.signal
        return orphan.response
      }
      if (path === '/api/v1/sessions') return jsonResponse({ data: { items: [AGENT] }, error: null })
      return jsonResponse({ data: { items: [] }, error: null })
    }
    const chat = useChat()
    await chat.openSession('ag-1')

    const pending = chat.send('问题')
    await settle()
    // Request out, nothing back yet: waiting, not "generating".
    expect(chat.agentStage.value).toBe(AgentStage.Waiting)

    orphan.push(sseFrame('message.delta', { message_id: 'a-1', delta: '正在写' }))
    await settle()
    expect(chat.agentStage.value).toBe(AgentStage.Generating)

    chat.cancel()
    expect(chat.agentStage.value).toBe(AgentStage.Cancelled)
    // Cancelling the view also stops reading the socket.
    expect(streamSignal.aborted).toBe(true)

    orphan.finish()
    await pending
    expect(chat.agentStage.value).toBe(AgentStage.Cancelled)
  })
})

describe('agent state never crosses a session boundary', () => {
  beforeEach(() => {
    vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
    setTokenProvider(() => 'tok.agent2')
  })

  function serve(sessions, history = {}) {
    globalThis.fetch = async (url, init = {}) => {
      const path = new URL(String(url), 'https://api.test').pathname
      if (init.method === 'DELETE') return jsonResponse(null, { status: 204 })
      if (path.endsWith('/stream')) {
        return sseResponse([sseFrame('message.completed', { message_id: 'a-1', content: '答案' })])
      }
      if (path === '/api/v1/sessions') return jsonResponse({ data: { items: sessions }, error: null })
      const historyMatch = /^\/api\/v1\/sessions\/([^/]+)\/messages$/.exec(path)
      if (historyMatch) {
        return jsonResponse({ data: { items: history[historyMatch[1]] ?? [] }, error: null })
      }
      return jsonResponse({ data: null, error: { code: 'not_found', message: 'no' } }, { status: 404 })
    }
  }

  it('switching agent sessions drops the finished stage and the expansion', async () => {
    serve([AGENT, AGENT_B], {
      'ag-2': [{ id: 'h-9', role: 'assistant', content: 'B 的旧回答' }],
    })
    const chat = useChat()
    await chat.openSession('ag-1')
    await chat.send('问题')
    expect(chat.agentStage.value).toBe(AgentStage.Completed)

    chat.setAgentPanelOpen(true)
    expect(chat.agentPanelOpen.value).toBe(true)

    await chat.openSession('ag-2')

    expect(chat.agentStage.value).toBe(AgentStage.None)
    expect(chat.agentPanelOpen.value).toBe(false)
    // and B genuinely re-reads its own history
    expect(chat.messages.value.map((m) => m.id)).toEqual(['h-9'])
  })

  it('closing and deleting drop it too', async () => {
    serve([AGENT])
    const chat = useChat()
    await chat.openSession('ag-1')
    await chat.send('问题')
    chat.setAgentPanelOpen(true)

    chat.closeSession()
    expect(chat.agentStage.value).toBe(AgentStage.None)
    expect(chat.agentPanelOpen.value).toBe(false)

    await chat.openSession('ag-1')
    await chat.send('再来一次')
    chat.setAgentPanelOpen(true)
    await chat.removeSession('ag-1')
    expect(chat.agentStage.value).toBe(AgentStage.None)
    expect(chat.agentPanelOpen.value).toBe(false)
  })

  it('a second send in the same session does not keep the old panel open', async () => {
    serve([AGENT])
    const chat = useChat()
    await chat.openSession('ag-1')
    await chat.send('第一问')
    chat.setAgentPanelOpen(true)

    await chat.send('第二问')

    expect(chat.agentPanelOpen.value).toBe(false)
    expect(chat.agentStage.value).toBe(AgentStage.Completed)
  })

  it('keeps Chat and Agent controls apart on the same screen', async () => {
    serve([CHAT, AGENT])
    const chat = useChat()

    await chat.openSession('ch-1')
    expect(chat.isChatSession.value).toBe(true)
    expect(chat.isAgentSession.value).toBe(false)
    // A chat session has no agent stage of its own to display.
    await chat.send('对话问题')
    expect(chat.agentStage.value).toBe(AgentStage.Completed) // the run exists...
    expect(chat.isAgentSession.value).toBe(false) // ...but the agent region is gated off by mode

    await chat.openSession('ag-1')
    expect(chat.isAgentSession.value).toBe(true)
    expect(chat.canUseKnowledge.value).toBe(false) // Agent never gets the grounding toggle
    expect(chat.activeKnowledge.value).toEqual({ enabled: false, selectedCollectionIds: [] })
  })
})

async function settle(times = 5) {
  for (let i = 0; i < times; i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0))
  }
}

describe('the agent region on the real screen', () => {
  // A session with no history is what makes the empty state render, so the
  // copy checks below need to choose it explicitly.
  const ONE_MESSAGE = [{ id: 'h-1', role: 'user', content: '旧问题' }]

  async function mountChat(sessionId, sessions, streamFrames = [], history = ONE_MESSAGE) {
    routerMock.route = {
      params: { sessionId },
      query: {},
      path: `/chat/${sessionId}`,
      name: 'session',
    }
    globalThis.fetch = async (url, init = {}) => {
      const path = new URL(String(url), 'https://api.test').pathname
      if (path.endsWith('/stream')) return sseResponse(streamFrames)
      if (path === '/api/v1/sessions') {
        return jsonResponse({ data: { items: sessions }, error: null })
      }
      if (/\/messages$/.test(path)) {
        return jsonResponse({ data: { items: history }, error: null })
      }
      if (init.method === 'DELETE') return jsonResponse(null, { status: 204 })
      return jsonResponse({ data: { items: [] }, error: null })
    }
    const app = mount(ChatView, { attachTo: document.body })
    await flushPromises()
    await flushPromises()
    return app
  }

  const DONE = [
    sseFrame('message.started', { message_id: 'a-1' }),
    sseFrame('message.delta', { message_id: 'a-1', delta: '回答正文' }),
    sseFrame('message.completed', { message_id: 'a-1', content: '回答正文' }),
  ]

  it('appears for an agent turn and never for a chat turn', async () => {
    const agent = await mountChat('ag-1', [AGENT], DONE)
    expect(agent.find('[data-test="agent-header"]').exists()).toBe(true)
    // History is loaded but no turn has been sent: no status strip at all.
    expect(agent.find('[data-test="agent-panel"]').exists()).toBe(false)

    await agent.find('textarea').setValue('帮我复盘错题')
    await agent.find('[data-test="send"]').trigger('click')
    await flushPromises()

    const panel = agent.find('[data-test="agent-panel"]')
    expect(panel.exists()).toBe(true)
    expect(panel.text()).toContain('本轮已完成')
    // Still no step list, because the backend has not emitted one.
    expect(agent.find('[data-test="agent-steps"]').exists()).toBe(false)
    // And no knowledge toolbar in an agent session.
    expect(agent.find('[data-test="knowledge-tools"]').exists()).toBe(false)
    agent.unmount()

    const chatApp = await mountChat('ch-1', [CHAT], DONE)
    await chatApp.find('textarea').setValue('普通问题')
    await chatApp.find('[data-test="send"]').trigger('click')
    await flushPromises()
    // The chat screen shows no grounding controls either — this build serves
    // no knowledge collection, and a switch that can never be turned on reads
    // as a broken screen. It gains no agent status strip.
    expect(chatApp.find('[data-test="knowledge-tools"]').exists()).toBe(false)
    expect(chatApp.find('[data-test="agent-panel"]').exists()).toBe(false)
    expect(chatApp.find('[data-test="agent-header"]').exists()).toBe(false)
    chatApp.unmount()
  })

  it('does not advertise capabilities that are not wired, and says the approved lines', async () => {
    // Phrases that used to claim memory, exercise generation, review, or a
    // placeholder-only responder. None of them is true of the current build,
    // under either provider, so none of them may appear on these screens.
    const FORBIDDEN = [
      '调用记忆',
      '与记忆的学习助手',
      '出题或复盘',
      '是否查资料由 Agent',
      '当前回答来自基础回复',
      '仍在开通中',
      '正在逐段开通',
      '已检索',
      '已调用工具',
      'knowledge_search',
    ]

    // A loaded transcript is what makes the mode header render.
    const agent = await mountChat('ag-1', [AGENT])
    expect(agent.find('[data-test="agent-header"]').exists()).toBe(true)
    const agentText = agent.text()
    for (const phrase of FORBIDDEN) expect(agentText).not.toContain(phrase)
    expect(agentText).toContain(
      '学习 Agent 会根据服务端当前启用的能力处理问题。执行步骤仅在真实发生并由服务端返回后显示，重新打开这个会话仍能读到。',
    )
    agent.unmount()

    // No history at all is what surfaces the empty state.
    const chatApp = await mountChat('ch-1', [CHAT], [], [])
    const chatText = chatApp.text()
    for (const phrase of FORBIDDEN) expect(chatText).not.toContain(phrase)
    expect(chatText).toContain(
      '直接输入即可开始普通对话；需要使用学习 Agent 时，可单独创建一个会话。',
    )
    chatApp.unmount()

    // The sign-in pitch is the product's first claim about itself, and it used
    // to promise exercise generation, grading, mastery tracking and memory.
    // None of those has an endpoint, so none of those words may appear here.
    const auth = mount(AuthLayout, {
      props: { title: '登入' },
      slots: { default: '<p>表单占位</p>' },
    })
    const authText = auth.text()
    for (const phrase of [...FORBIDDEN, '出题', '判分', '掌握度', '记忆']) {
      expect(authText).not.toContain(phrase)
    }
    expect(authText).toContain('基于你自己的资料回答，并指出依据来自哪里。')
    auth.unmount()
  })

  it('describes the two creation options without claiming tools or memory', async () => {
    const app = mount(SessionPanel, { props: { sessions: [AGENT, CHAT] } })
    await app.find('[data-test="new-session"]').trigger('click')

    const text = app.find('[data-test="new-session-menu"]').text()
    expect(text).not.toContain('调用记忆')
    expect(text).not.toContain('出题')
    expect(text).toContain('学习 Agent')
    expect(text).toContain('由服务端能力决定如何处理问题')
    // The knowledge option is real as of Phase 3B, so it may still be stated.
    expect(text).toContain('可单轮挂知识库')
  })
})
