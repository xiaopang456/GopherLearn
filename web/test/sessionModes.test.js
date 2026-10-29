import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  createSession,
  normalizeSession,
} from '../src/api/sessions.js'
import { streamMessage } from '../src/api/stream.js'
import { setTokenProvider } from '../src/api/http.js'
import {
  createSessionBody,
  isDefaultTitle,
  isKnownMode,
  modeLabel,
  normalizeMode,
  SessionMode,
  UNKNOWN_MODE_LABEL,
} from '../src/lib/modes.js'
import {
  emptyKnowledgeDraft,
  isKnowledgeUsable,
  toWireKnowledge,
} from '../src/lib/knowledge.js'
import { clearDevIssues, devIssues } from '../src/lib/devlog.js'
import { useChat } from '../src/composables/useChat.js'
import { jsonResponse, sseFrame, sseResponse } from './helpers/transport.js'

let calls

function route(handler) {
  globalThis.fetch = async (url, init = {}) => {
    calls.push({ url: String(url), init })
    return handler(url, init)
  }
}

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'tok.modes')
  clearDevIssues()
  calls = []
})

describe('createSessionBody', () => {
  it('names the chat mode and its default title', () => {
    expect(
      createSessionBody({ mode: SessionMode.Chat, title: '新对话' }),
    ).toEqual({ mode: 'chat', title: '新对话' })
  })

  it('names the learning agent mode', () => {
    expect(
      createSessionBody({ mode: SessionMode.LearningAgent, title: '学习 Agent' }),
    ).toEqual({ mode: 'learning_agent', title: '学习 Agent' })
  })

  it('drops a blank title rather than sending an empty string', () => {
    expect(createSessionBody({ mode: 'chat', title: '   ' })).toEqual({
      mode: 'chat',
    })
  })

  it('refuses to create a session without a known mode', () => {
    expect(() => createSessionBody({ mode: '' })).toThrow()
    expect(() => createSessionBody({ mode: 'knowledge_qa' })).toThrow()
    expect(() => createSessionBody({})).toThrow()
  })

  it('treats knowledge_qa as a mode it must never accept', () => {
    // Grounding is a per-turn Chat option, not a third session mode.
    expect(isKnownMode('knowledge_qa')).toBe(false)
    expect(normalizeMode('knowledge_qa')).toBe('')
  })
})

describe('createSession wire shape', () => {
  it('POSTs {title, mode} for a chat session', async () => {
    route(() => jsonResponse({ data: { id: 's-1', mode: 'chat' }, error: null }))

    await createSession({ mode: 'chat', title: '新对话' })

    expect(calls[0].url).toBe('https://api.test/api/v1/sessions')
    expect(calls[0].init.method).toBe('POST')
    expect(JSON.parse(calls[0].init.body)).toEqual({ mode: 'chat', title: '新对话' })
  })

  it('POSTs {title, mode} for a learning agent session', async () => {
    route(() =>
      jsonResponse({ data: { id: 's-2', mode: 'learning_agent' }, error: null }),
    )

    await createSession({ mode: 'learning_agent', title: '学习 Agent' })

    expect(JSON.parse(calls[0].init.body)).toEqual({
      mode: 'learning_agent',
      title: '学习 Agent',
    })
  })
})

describe('normalizeSession keeps the mode', () => {
  it('reads chat and learning_agent', () => {
    expect(normalizeSession({ id: '1', mode: 'chat' })).toEqual({
      id: '1',
      title: '未命名会话',
      mode: 'chat',
      updatedAt: '',
    })
    expect(normalizeSession({ id: '2', mode: 'learning_agent' }).mode).toBe(
      'learning_agent',
    )
  })

  it('accepts the alternate spellings a Go server may use', () => {
    expect(normalizeSession({ session_mode: 'chat' }).mode).toBe('chat')
    expect(normalizeSession({ sessionMode: 'LEARNING_AGENT' }).mode).toBe(
      'learning_agent',
    )
  })

  it('never guesses a mode from a missing or foreign value', () => {
    expect(normalizeSession({ id: '3' }).mode).toBe('')
    expect(normalizeSession({ mode: null }).mode).toBe('')
    expect(normalizeSession({ mode: 'agent' }).mode).toBe('')
  })

  it('labels an unknown mode honestly instead of as an agent', () => {
    expect(modeLabel('')).toBe(UNKNOWN_MODE_LABEL)
    expect(modeLabel('learning_agent')).toBe('学习 Agent')
    expect(modeLabel('chat')).toBe('对话')
  })

  it('records an unrecognised mode where a developer can find it', () => {
    normalizeSession({ id: 'x', mode: 'voice_beta' })

    const issues = devIssues()
    expect(issues).toHaveLength(1)
    expect(issues[0].kind).toBe('session.mode.unknown')
    expect(issues[0].detail.received).toBe('voice_beta')
  })

  it('does not log a merely absent mode', () => {
    normalizeSession({ id: 'x' })
    expect(devIssues()).toEqual([])
  })

  it('recognises which titles still stand in for "not named yet"', () => {
    expect(isDefaultTitle('chat', '新对话')).toBe(true)
    expect(isDefaultTitle('chat', '未命名会话')).toBe(true)
    expect(isDefaultTitle('learning_agent', '学习 Agent')).toBe(true)
    expect(isDefaultTitle('learning_agent', '为什么需要索引')).toBe(false)
    expect(isDefaultTitle('chat', '学习 Agent')).toBe(false)
  })
})

describe('knowledge draft and wire conversion', () => {
  it('starts disabled with no invented collection', () => {
    expect(emptyKnowledgeDraft()).toEqual({ enabled: false, selectedCollectionIds: [] })
  })

  it('is only usable when enabled with at least one id', () => {
    expect(isKnowledgeUsable({ enabled: true, selectedCollectionIds: [] })).toBe(false)
    expect(isKnowledgeUsable({ enabled: false, selectedCollectionIds: ['c-1'] })).toBe(false)
    expect(isKnowledgeUsable({ enabled: true, selectedCollectionIds: ['c-1'] })).toBe(true)
    expect(isKnowledgeUsable(null)).toBe(false)
  })

  it('emits nothing at all when disabled — the one representation we standardise on', () => {
    expect(toWireKnowledge(emptyKnowledgeDraft())).toBe(null)
    expect(toWireKnowledge({ enabled: true, selectedCollectionIds: [] })).toBe(null)
  })

  it('emits the contract key names when enabled', () => {
    expect(
      toWireKnowledge({ enabled: true, selectedCollectionIds: ['collection-1'] }),
    ).toEqual({ enabled: true, collection_ids: ['collection-1'] })
  })

  it('dedupes and de-noises the selected ids', () => {
    expect(
      toWireKnowledge({
        enabled: true,
        selectedCollectionIds: ['a', 'a', ' b ', '', 'c'],
      }).collection_ids,
    ).toEqual(['a', 'b', 'c'])
  })
})

describe('streamMessage body', () => {
  const base = {
    sessionId: 's-1',
    content: '问题',
    signal: new AbortController().signal,
    onEvent: () => {},
  }

  it('omits knowledge entirely when the option is off', async () => {
    route(() => sseResponse([sseFrame('message.completed', {})]))

    await streamMessage({ ...base, knowledge: emptyKnowledgeDraft() })

    expect(JSON.parse(calls[0].init.body)).toEqual({ content: '问题' })
  })

  it('carries no legacy `message`, `mode` or `answer_strategy` field', async () => {
    route(() => sseResponse([sseFrame('message.completed', {})]))

    await streamMessage({ ...base, knowledge: null })

    expect(Object.keys(JSON.parse(calls[0].init.body))).toEqual(['content'])
  })

  it('adds the knowledge block once a real collection is selected', async () => {
    route(() => sseResponse([sseFrame('message.completed', {})]))

    await streamMessage({
      ...base,
      knowledge: { enabled: true, selectedCollectionIds: ['collection-1'] },
    })

    expect(JSON.parse(calls[0].init.body)).toEqual({
      content: '问题',
      knowledge: { enabled: true, collection_ids: ['collection-1'] },
    })
  })

  it('still sends no credential in the URL', async () => {
    route(() => sseResponse([sseFrame('message.completed', {})]))

    await streamMessage({ ...base })

    expect(calls[0].url).not.toContain('tok.modes')
    expect(calls[0].init.headers.authorization).toBe('Bearer tok.modes')
  })
})

describe('useChat session creation policy', () => {
  function createHandler(mode) {
    return (url, init) => {
      if (String(url).endsWith('/stream')) {
        return sseResponse([sseFrame('message.completed', { content: '回声' })])
      }
      if (init?.method === 'POST' && String(url).endsWith('/sessions')) {
        return jsonResponse({
          data: { id: 's-new', title: '未命名会话', mode },
          error: null,
        })
      }
      return jsonResponse({ data: { items: [] }, error: null })
    }
  }

  it('asks for chat when a question is typed with no session yet', async () => {
    route(createHandler('chat'))
    const chat = useChat()

    await chat.send('第一个问题')

    const created = calls.find((c) => c.init.method === 'POST')
    expect(JSON.parse(created.init.body)).toEqual({
      mode: 'chat',
      title: '新对话',
    })
    expect(chat.activeMode.value).toBe('chat')
  })

  it('asks for learning_agent when the user picks that option', async () => {
    route(createHandler('learning_agent'))
    const chat = useChat()

    const id = await chat.createSession(SessionMode.LearningAgent)

    const created = calls.find((c) => c.init.method === 'POST')
    expect(JSON.parse(created.init.body)).toEqual({
      mode: 'learning_agent',
      title: '学习 Agent',
      requestId: expect.stringMatching(/^create-/),
    })
    expect(id).toBe('s-new')
    expect(chat.activeMode.value).toBe('learning_agent')
    expect(chat.isAgentSession.value).toBe(true)
    expect(chat.isChatSession.value).toBe(false)
  })

  it('refuses to create a third kind of session', async () => {
    route(createHandler('chat'))
    const chat = useChat()

    expect(await chat.createSession('knowledge_qa')).toBe('')
    expect(calls.filter((c) => c.init.method === 'POST')).toHaveLength(0)
    expect(chat.sessionsError.value).not.toBe('')
  })

  it('adopts the mode the server echoed, not the one requested', async () => {
    // The mode is a server-owned fact: if the stored session says otherwise,
    // every downstream decision has to follow the stored session.
    route(() =>
      jsonResponse({
        data: { id: 's-echo', title: '未命名会话', mode: 'learning_agent' },
        error: null,
      }),
    )
    const chat = useChat()

    await chat.createSession(SessionMode.Chat)

    expect(chat.activeSession.value.mode).toBe('learning_agent')
    expect(chat.isChatSession.value).toBe(false)
    expect(chat.canUseKnowledge.value).toBe(false)
  })
})
