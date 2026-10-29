import { beforeEach, describe, expect, it, vi } from 'vitest'

import { useChat } from '../src/composables/useChat.js'
import { setTokenProvider } from '../src/api/http.js'
import {
  abortableSSEResponse,
  jsonResponse,
  sseFrame,
  sseResponse,
} from './helpers/transport.js'

const TOKEN = 'tok.stream.test'
const SESSION = { id: 's-1', title: '未命名会话', updated_at: '2026-09-24T08:00:00Z' }

let streamScript
let calls
let sessionSeq

function handler(url, init) {
  const path = new URL(String(url), 'https://api.test').pathname

  if (path.endsWith('/stream')) {
    const body = JSON.parse(init.body)
    return streamScript(body, init.signal)
  }
  if (path === '/api/v1/sessions' && init.method === 'POST') {
    sessionSeq += 1
    return jsonResponse({
      data: { id: `s-new-${sessionSeq}`, title: null },
      error: null,
    })
  }
  if (path === '/api/v1/sessions') {
    return jsonResponse({ data: { items: [SESSION] }, error: null })
  }
  if (/\/messages$/.test(path)) {
    return jsonResponse({
      data: {
        items: [
          { id: 'h-1', role: 'user', content: '历史提问', created_at: '2026-09-24T07:00:00Z' },
          { id: 'h-2', role: 'assistant', content: '历史回答', created_at: '2026-09-24T07:00:05Z' },
        ],
      },
      error: null,
    })
  }
  if (/\/api\/v1\/sessions\/[^/]+$/.test(path) && init.method === 'DELETE') {
    return jsonResponse(null, { status: 204 })
  }
  throw new Error(`unrouted request: ${init?.method} ${path}`)
}

async function flush(times = 8) {
  for (let i = 0; i < times; i += 1) await new Promise((resolve) => setTimeout(resolve, 0))
}

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => TOKEN)
  sessionSeq = 0
  calls = []
  globalThis.fetch = async (url, init = {}) => {
    calls.push({ url: String(url), init })
    return handler(url, init)
  }
})

function scriptedStream(frames, options = {}) {
  return () => sseResponse(frames.map((frame) => frame), options)
}

describe('optimistic transcript', () => {
  it('appends the question and a placeholder before the response arrives', async () => {
    streamScript = scriptedStream([
      sseFrame('message.started', { message_id: 'a-1' }),
      sseFrame('message.delta', { message_id: 'a-1', delta: '你好' }),
      sseFrame('message.completed', { message_id: 'a-1' }),
    ])

    const chat = useChat()
    chat.activeId.value = 's-1'
    const pending = chat.send('你好吗')

    expect(chat.messages.value).toHaveLength(2)
    expect(chat.messages.value[0]).toMatchObject({ role: 'user', content: '你好吗' })
    expect(chat.messages.value[1]).toMatchObject({ role: 'assistant', status: 'pending' })

    await pending
    expect(chat.messages.value[1]).toMatchObject({ status: 'done', content: '你好' })
    expect(chat.streaming.value).toBe(false)
  })

  it('creates a session implicitly when the composer is used on a blank route', async () => {
    streamScript = scriptedStream([sseFrame('message.completed', { content: 'ok' })])
    const chat = useChat()

    await chat.send('第一个问题')

    expect(calls[0].url).toBe('https://api.test/api/v1/sessions')
    expect(chat.activeId.value).toBe('s-new-1')
    expect(chat.messages.value[1].content).toBe('ok')
  })

  it('clears the draft and refuses to send while a run is in flight', async () => {
    streamScript = (_body, signal) =>
      abortableSSEResponse([sseFrame('message.started', {})], signal)
    const chat = useChat()
    chat.activeId.value = 's-1'
    chat.draft.value = '要发送的问题'

    const pending = chat.send()
    await flush(2)
    await chat.send('第二个问题')

    expect(chat.draft.value).toBe('')
    expect(chat.messages.value.filter((m) => m.role === 'user')).toHaveLength(1)

    chat.cancel()
    await pending
  })
})

describe('SSE delta assembly', () => {
  it('concatenates every delta into the current placeholder only', async () => {
    const pieces = ['Gopher', 'Learn ', '是一个', '学习助手', '\n第二行']
    streamScript = scriptedStream([
      sseFrame('message.started', { message_id: 'a-9' }),
      ...pieces.map((delta) =>
        sseFrame('message.delta', { message_id: 'a-9', delta }),
      ),
      sseFrame('message.completed', { message_id: 'a-9' }),
    ])

    const chat = useChat()
    chat.activeId.value = 's-1'
    chat.messages.value.push({
      id: 'pre-existing',
      role: 'assistant',
      content: '不要改我',
      status: 'done',
      error: null,
    })

    await chat.send('讲点什么')

    const placeholder = chat.messages.value.at(-1)
    expect(placeholder.content).toBe(pieces.join(''))
    expect(chat.messages.value[0].content).toBe('不要改我')
    expect(chat.run.value.ignoredEvents).toBe(0)
  })

  it('reassembles a multi-byte character split across network chunks', async () => {
    const frame = sseFrame('message.delta', { delta: '数据流' })
    const encoded = new TextEncoder().encode(frame)
    // Cut in the middle of a 3-byte sequence.
    const split = encoded.indexOf(0xe6) + 1
    streamScript = () =>
      sseResponse([encoded.slice(0, split), encoded.slice(split)])

    const chat = useChat()
    chat.activeId.value = 's-1'
    await chat.send('测试')

    expect(chat.messages.value.at(-1).content).toBe('数据流')
  })
})

describe('SSE failure keeps the question', () => {
  it('marks only the assistant turn as failed and shows the server reason', async () => {
    streamScript = scriptedStream([
      sseFrame('message.started', { message_id: 'a-1' }),
      sseFrame('message.delta', { message_id: 'a-1', delta: '写了一半' }),
      sseFrame('message.failed', {
        message_id: 'a-1',
        code: 'invalid_request',
        message: '上下文超出模型长度限制',
      }),
    ])

    const chat = useChat()
    chat.activeId.value = 's-1'
    await chat.send('一个很长的问题')

    const [question, answer] = chat.messages.value
    expect(question.role).toBe('user')
    expect(question.content).toBe('一个很长的问题')
    expect(question.status).toBe('done')
    expect(question.error).toBe(null)

    expect(answer.status).toBe('error')
    expect(answer.content).toBe('写了一半')
    expect(answer.error).toEqual({
      code: 'invalid_request',
      message: '上下文超出模型长度限制',
    })
  })

  it('never echoes an internal server detail into the card', async () => {
    streamScript = scriptedStream([
      sseFrame('message.failed', {
        code: 'internal_error',
        message: 'sql: connect tcp 10.0.3.7:3306 failed',
      }),
    ])

    const chat = useChat()
    chat.activeId.value = 's-1'
    await chat.send('问题')

    expect(chat.messages.value[1].error.message).not.toContain('10.0.3.7')
    expect(chat.messages.value[1].error.message).not.toContain('sql')
  })

  it('fails the turn when the socket closes without a terminal event', async () => {
    streamScript = scriptedStream([
      sseFrame('message.delta', { delta: '半句话' }),
    ])

    const chat = useChat()
    chat.activeId.value = 's-1'
    await chat.send('问题')

    expect(chat.messages.value[1]).toMatchObject({
      status: 'error',
      content: '半句话',
    })
    expect(chat.messages.value[1].error.code).toBe('stream_closed')
  })

  it('surfaces the question even when the transport never connected', async () => {
    streamScript = () => {
      throw new TypeError('Failed to fetch')
    }

    const chat = useChat()
    chat.activeId.value = 's-1'
    await chat.send('问题')

    expect(chat.messages.value[0]).toMatchObject({ role: 'user', content: '问题' })
    expect(chat.messages.value[1].status).toBe('error')
    expect(chat.messages.value[1].error.code).toBe('stream_connect_failed')
    expect(chat.messages.value[1].error.message).not.toContain('TypeError')
  })
})

describe('a completed answer is final', () => {
  it('ignores a failure that arrives after message.completed', async () => {
    streamScript = scriptedStream([
      sseFrame('message.delta', { message_id: 'a-1', delta: '完整答案' }),
      sseFrame('message.completed', { message_id: 'a-1' }),
      sseFrame('message.failed', {
        message_id: 'a-1',
        code: 'internal_error',
        message: 'late failure',
      }),
      sseFrame('message.delta', { message_id: 'a-1', delta: 'TRAILING' }),
    ])

    const chat = useChat()
    chat.activeId.value = 's-1'
    await chat.send('问题')

    const answer = chat.messages.value[1]
    expect(answer.status).toBe('done')
    expect(answer.content).toBe('完整答案')
    expect(answer.error).toBe(null)
    expect(chat.run.value.ignoredEvents).toBe(2)
  })

  it('ignores a second completion that would replace the text', async () => {
    streamScript = scriptedStream([
      sseFrame('message.completed', { message_id: 'a-1', content: '第一版' }),
      sseFrame('message.completed', { message_id: 'a-1', content: '第二版' }),
    ])

    const chat = useChat()
    chat.activeId.value = 's-1'
    await chat.send('问题')

    expect(chat.messages.value[1].content).toBe('第一版')
  })

  it('drops events that name a different assistant message', async () => {
    streamScript = scriptedStream([
      sseFrame('message.started', { message_id: 'mine' }),
      sseFrame('message.delta', { message_id: 'mine', delta: '我的答案' }),
      sseFrame('message.delta', { message_id: 'someone-elses-run', delta: '污染' }),
      sseFrame('message.completed', { message_id: 'mine' }),
    ])

    const chat = useChat()
    chat.activeId.value = 's-1'
    await chat.send('问题')

    expect(chat.messages.value[1].content).toBe('我的答案')
    expect(chat.run.value.ignoredEvents).toBe(1)
  })
})

describe('cancellation', () => {
  it('aborts the request and keeps the partial answer visible', async () => {
    streamScript = (_body, signal) =>
      abortableSSEResponse([sseFrame('message.delta', { delta: '正在写' })], signal)

    const chat = useChat()
    chat.activeId.value = 's-1'
    const pending = chat.send('问题')
    await flush(4)

    chat.cancel()
    await pending

    const answer = chat.messages.value[1]
    expect(answer.status).toBe('stopped')
    expect(answer.content).toBe('正在写')
    expect(answer.error.code).toBe('aborted')
    expect(chat.streaming.value).toBe(false)
  })

  it('stops a later failure from overwriting a completed answer', async () => {
    streamScript = scriptedStream([
      sseFrame('message.completed', { content: '已经写完' }),
    ])
    const chat = useChat()
    chat.activeId.value = 's-1'
    await chat.send('问题')

    chat.cancel()
    expect(chat.messages.value[1]).toMatchObject({ status: 'done', content: '已经写完' })
  })
})

describe('credential placement on the stream', () => {
  it('carries the bearer token in a header and never in the URL', async () => {
    streamScript = scriptedStream([sseFrame('message.completed', { content: 'ok' })])
    const chat = useChat()
    chat.activeId.value = 's-1'

    await chat.send('问题')

    const stream = calls.at(-1)
    expect(stream.url).toBe('https://api.test/api/v1/sessions/s-1/stream')
    expect(stream.url).not.toContain(TOKEN)
    expect(stream.url).not.toContain('?')
    expect(stream.init.headers.authorization).toBe(`Bearer ${TOKEN}`)
    expect(stream.init.headers.accept).toBe('text/event-stream')
  })

  it('escapes the session id used in the path', async () => {
    streamScript = scriptedStream([sseFrame('message.completed', { content: 'ok' })])
    const chat = useChat()
    chat.activeId.value = 'a/b?c'

    await chat.send('问题')

    expect(calls.at(-1).url).toBe(
      'https://api.test/api/v1/sessions/a%2Fb%3Fc/stream',
    )
  })
})

describe('session and history loading', () => {
  it('reads the session list through the envelope', async () => {
    streamScript = scriptedStream([])
    const chat = useChat()

    await chat.loadSessions()

    expect(chat.sessions.value).toEqual([
      {
        id: 's-1',
        title: '未命名会话',
        mode: '',
        updatedAt: '2026-09-24T08:00:00Z',
      },
    ])
    expect(chat.sessionsError.value).toBe('')
  })

  it('labels an untitled session with the first question for display only', async () => {
    streamScript = scriptedStream([sseFrame('message.completed', { content: 'ok' })])
    const chat = useChat()

    await chat.send('为什么    goroutine 会泄漏')

    expect(chat.sessions.value[0].title).toBe('为什么 goroutine 会泄漏')
  })

  it('loads history for the opened session', async () => {
    streamScript = scriptedStream([])
    const chat = useChat()

    await chat.openSession('s-1')

    expect(chat.messages.value.map((item) => item.role)).toEqual([
      'user',
      'assistant',
    ])
    expect(chat.messages.value[0]).toMatchObject({
      id: 'h-1',
      content: '历史提问',
      status: 'done',
    })
  })

  it('reports a history failure without clearing the session', async () => {
    streamScript = scriptedStream([])
    const chat = useChat()
    globalThis.fetch = async () =>
      jsonResponse(
        { data: null, error: { code: 'not_found', message: '会话不存在' } },
        { status: 404 },
      )

    await chat.openSession('missing')

    expect(chat.historyError.value).toBe('会话不存在')
    expect(chat.messages.value).toEqual([])
  })

  it('deletes a session and closes it when it was open', async () => {
    streamScript = scriptedStream([])
    const chat = useChat()
    await chat.loadSessions()
    await chat.openSession('s-1')

    await chat.removeSession('s-1')

    expect(chat.sessions.value).toEqual([])
    expect(chat.activeId.value).toBe('')
    expect(chat.messages.value).toEqual([])
  })

  it('aborts an in-flight answer before switching sessions', async () => {
    streamScript = (_body, signal) =>
      abortableSSEResponse([sseFrame('message.delta', { delta: '旧会话' })], signal)
    const chat = useChat()
    chat.activeId.value = 's-1'
    const pending = chat.send('问题')
    await flush(4)

    await chat.openSession('s-2')
    await pending

    expect(chat.streaming.value).toBe(false)
    expect(chat.activeId.value).toBe('s-2')
    // The abandoned answer must not follow the user into the next transcript.
    expect(chat.messages.value.some((m) => m.content === '旧会话')).toBe(false)
    expect(chat.messages.value.map((m) => m.id)).toEqual(['h-1', 'h-2'])
  })
})
