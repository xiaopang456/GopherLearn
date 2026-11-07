import { beforeEach, describe, expect, it, vi } from 'vitest'

import { useChat } from '../src/composables/useChat.js'
import { setTokenProvider } from '../src/api/http.js'
import { jsonResponse } from './helpers/transport.js'

const SESSIONS = [
  { id: 'chat-new', title: '新对话', mode: 'chat', updated_at: '2026-09-25T10:00:00Z' },
  { id: 'agent-old', title: '旧 Agent', mode: 'learning_agent', updated_at: '2026-09-20T10:00:00Z' },
  { id: 'odd', title: '未知类型', mode: 'voice_beta', updated_at: '2026-09-26T10:00:00Z' },
  { id: 'agent-new', title: '新 Agent', mode: 'learning_agent', updated_at: '2026-09-24T10:00:00Z' },
  { id: 'chat-old', title: '旧对话', mode: 'chat', updated_at: '2026-09-19T10:00:00Z' },
]

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'tok.order.test')
  globalThis.fetch = async (url) => {
    const path = new URL(String(url), 'https://api.test').pathname
    if (path === '/api/v1/sessions') {
      return jsonResponse({ data: { items: SESSIONS }, error: null })
    }
    throw new Error(`unrouted request: ${path}`)
  }
})

describe('the session list pins agent sessions above the rest', () => {
  it('orders agent sessions first, then the rest, newest first inside each group', async () => {
    const chat = useChat()
    await chat.loadSessions()

    expect(chat.orderedSessions.value.map((session) => session.id)).toEqual([
      'agent-new',
      'agent-old',
      'odd',
      'chat-new',
      'chat-old',
    ])
  })

  it('leaves the server order it was given untouched', async () => {
    const chat = useChat()
    await chat.loadSessions()

    expect(chat.sessions.value.map((session) => session.id)).toEqual(
      SESSIONS.map((session) => session.id),
    )
  })

  it('sorts a session with no usable timestamp last within its group', async () => {
    globalThis.fetch = async (url) => {
      const path = new URL(String(url), 'https://api.test').pathname
      if (path === '/api/v1/sessions') {
        return jsonResponse({
          data: {
            items: [
              { id: 'stamped', mode: 'chat', updated_at: '2026-09-20T10:00:00Z' },
              { id: 'unstamped', mode: 'chat' },
            ],
          },
          error: null,
        })
      }
      throw new Error(`unrouted request: ${path}`)
    }

    const chat = useChat()
    await chat.loadSessions()

    expect(chat.orderedSessions.value.map((session) => session.id)).toEqual([
      'stamped',
      'unstamped',
    ])
  })
})
