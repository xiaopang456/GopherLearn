import { beforeEach, describe, expect, it, vi } from 'vitest'

import { useChat } from '../src/composables/useChat.js'
import { setTokenProvider } from '../src/api/http.js'
import {
  deferred,
  jsonResponse,
  sseFrame,
  sseResponse,
} from './helpers/transport.js'

/**
 * Opening A then B fires two history reads. If A is slower it resolves last and
 * — without a generation guard — replaces B's transcript. Every case here makes
 * the arrival order deliberately wrong.
 */

let gates
let calls
let streamContent

function installGatedFetch() {
  globalThis.fetch = async (url, init = {}) => {
    calls.push({ url: String(url), init })
    const path = new URL(String(url), 'https://api.test').pathname

    const history = /^\/api\/v1\/sessions\/([^/]+)\/messages$/.exec(path)
    if (history) return gates[history[1]].promise

    if (path.endsWith('/stream')) {
      return sseResponse([sseFrame('message.completed', { content: streamContent })])
    }
    if (path === '/api/v1/sessions' && init.method === 'POST') {
      return jsonResponse({ data: { id: 's-new' }, error: null })
    }
    return jsonResponse({ data: { items: [] }, error: null })
  }
}

/** Records which fetch carried which signal, so aborts are observable. */
let signals

function okMessages(id, content) {
  return jsonResponse({
    data: { items: [{ id, role: 'user', content }] },
    error: null,
  })
}

function failMessages() {
  return jsonResponse(
    { data: null, error: { code: 'not_found', message: '会话不存在' } },
    { status: 404 },
  )
}

async function settle(times = 4) {
  for (let i = 0; i < times; i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0))
  }
}

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'tok.race')
  gates = { A: deferred(), B: deferred(), C: deferred() }
  calls = []
  streamContent = '流式答案'
  signals = []
  installGatedFetch()
})

function transcript(chat) {
  return chat.messages.value.map((item) => item.content)
}

describe('out-of-order history responses', () => {
  it('keeps showing B when A is requested first but answers last', async () => {
    const chat = useChat()

    const openingA = chat.openSession('A')
    await settle(1)
    const openingB = chat.openSession('B')
    await settle(1)

    gates.B.resolve(okMessages('B-1', 'B 的消息'))
    await openingB
    expect(transcript(chat)).toEqual(['B 的消息'])

    gates.A.resolve(okMessages('A-1', 'A 的消息'))
    await openingA
    await settle()

    expect(chat.activeId.value).toBe('B')
    expect(transcript(chat)).toEqual(['B 的消息'])
    expect(chat.messages.value.some((m) => m.id === 'A-1')).toBe(false)
    expect(chat.historyLoading.value).toBe(false)
    expect(chat.historyError.value).toBe('')
  })

  it('a late failure for A cannot mark the open session B as broken', async () => {
    const chat = useChat()

    const openingA = chat.openSession('A')
    await settle(1)
    const openingB = chat.openSession('B')
    await settle(1)

    gates.B.resolve(okMessages('B-1', 'B 的消息'))
    await openingB

    gates.A.resolve(failMessages())
    await openingA
    await settle()

    expect(chat.activeId.value).toBe('B')
    expect(chat.historyError.value).toBe('')
    expect(chat.historyLoading.value).toBe(false)
    expect(transcript(chat)).toEqual(['B 的消息'])
  })

  it('answers arriving in the wrong order across three sessions still settle on the last opened', async () => {
    const chat = useChat()

    const openingA = chat.openSession('A')
    await settle(1)
    const openingB = chat.openSession('B')
    await settle(1)
    const openingC = chat.openSession('C')
    await settle(1)

    gates.A.resolve(okMessages('A-1', 'A 的消息'))
    await openingA
    gates.C.resolve(okMessages('C-1', 'C 的消息'))
    await openingC
    gates.B.resolve(okMessages('B-1', 'B 的消息'))
    await openingB
    await settle()

    expect(chat.activeId.value).toBe('C')
    expect(transcript(chat)).toEqual(['C 的消息'])
  })

  it('aborts the superseded history request instead of leaving it running', async () => {
    const chat = useChat()

    const openingA = chat.openSession('A')
    await settle(1)
    const aSignal = calls.at(-1).init.signal
    signals.push(aSignal)

    const openingB = chat.openSession('B')
    await settle(1)

    expect(aSignal.aborted).toBe(true)
    expect(calls.at(-1).init.signal).not.toBe(aSignal)

    gates.B.resolve(okMessages('B-1', 'B 的消息'))
    await openingB
    gates.A.resolve(failMessages())
    await openingA
  })
})

describe('other paths that must not leave a stale history request', () => {
  it('closing a session discards a response still on its way', async () => {
    const chat = useChat()

    const openingA = chat.openSession('A')
    await settle()
    chat.closeSession()
    const pendingSignal = calls.at(-1).init.signal

    gates.A.resolve(okMessages('A-1', 'A 的消息'))
    await openingA
    await settle()

    expect(pendingSignal.aborted).toBe(true)
    expect(chat.activeId.value).toBe('')
    expect(chat.messages.value).toEqual([])
    expect(chat.historyLoading.value).toBe(false)
  })

  it('deleting the open session discards its in-flight history too', async () => {
    const chat = useChat()
    chat.sessions.value = [{ id: 'A', title: '会话 A', updatedAt: '' }]

    const openingA = chat.openSession('A')
    await settle()

    await chat.removeSession('A')
    gates.A.resolve(okMessages('A-1', 'A 的消息'))
    await openingA
    await settle()

    expect(chat.activeId.value).toBe('')
    expect(chat.messages.value).toEqual([])
    expect(chat.sessions.value).toEqual([])
  })

  it('starting an answer is not overwritten by a history load still running', async () => {
    const chat = useChat()

    const openingA = chat.openSession('A')
    await settle()

    const sending = chat.send('当场提问')
    await settle()

    gates.A.resolve(okMessages('A-1', 'A 的消息'))
    await openingA
    await sending
    await settle()

    expect(chat.messages.value.map((m) => m.role)).toEqual(['user', 'assistant'])
    expect(chat.messages.value[0].content).toBe('当场提问')
    expect(chat.messages.value[1].content).toBe('流式答案')
    expect(chat.messages.value.some((m) => m.id === 'A-1')).toBe(false)
    expect(chat.historyLoading.value).toBe(false)
  })
})
