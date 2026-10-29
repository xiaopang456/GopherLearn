import { beforeEach, describe, expect, it, vi } from 'vitest'

import { useChat } from '../src/composables/useChat.js'
import { setTokenProvider } from '../src/api/http.js'
import {
  jsonResponse,
  lateDeliverySSEResponse,
  sseFrame,
  sseResponse,
} from './helpers/transport.js'

/**
 * The contract fixes the four SSE event names but not their bodies, so a server
 * is free to stamp every event with its own `run_id`. These cases pin down that
 * a locally minted id and a server-side id are two different namespaces: the
 * reducer must bind the server's first value and reject only a *different*
 * later one, never compare it against the client's.
 */

const TOKEN = 'tok.identity'

let streamQueue
let calls

function pushStream(response) {
  streamQueue.push(response)
}

function frames(list) {
  return sseResponse(list)
}

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => TOKEN)
  streamQueue = []
  calls = []
  globalThis.fetch = async (url, init = {}) => {
    calls.push({ url: String(url), init })
    const path = new URL(String(url), 'https://api.test').pathname
    if (path.endsWith('/stream')) {
      if (streamQueue.length === 0) throw new Error('stream queue empty')
      return streamQueue.shift()
    }
    if (path === '/api/v1/sessions' && init.method === 'POST') {
      return jsonResponse({ data: { id: 's-1' }, error: null })
    }
    return jsonResponse({ data: { items: [] }, error: null })
  }
})

function openedChat() {
  const chat = useChat()
  chat.activeId.value = 's-1'
  return chat
}

describe('server run identity end to end', () => {
  it('completes a started -> delta -> completed chain carrying non-empty run_id', async () => {
    const RUN = 'run_01HXXSERVER'
    const MSG = 'msg_01HYYSERVER'
    pushStream(
      frames([
        sseFrame('message.started', { run_id: RUN, message_id: MSG }),
        sseFrame('message.delta', { run_id: RUN, message_id: MSG, delta: '服务端 ' }),
        sseFrame('message.delta', { run_id: RUN, message_id: MSG, delta: '带身份' }),
        sseFrame('message.completed', { run_id: RUN, message_id: MSG }),
      ]),
    )

    const chat = openedChat()
    await chat.send('问题')

    expect(chat.messages.value[1]).toMatchObject({
      status: 'done',
      content: '服务端 带身份',
    })
    expect(chat.run.value.serverRunId).toBe(RUN)
    expect(chat.run.value.messageId).toBe(MSG)
    expect(chat.run.value.ignoredEvents).toBe(0)
    expect(chat.run.value.clientRunId).not.toBe(RUN)
    expect(chat.run.value.clientRunId).toMatch(/^run-/)
    expect(chat.run.value.placeholderId).toBe(chat.messages.value[1].id)
  })

  it('binds the first server run_id and discards a later, different one', async () => {
    pushStream(
      frames([
        sseFrame('message.started', { run_id: 'run-A' }),
        sseFrame('message.delta', { run_id: 'run-A', delta: '甲' }),
        sseFrame('message.delta', { run_id: 'run-B', delta: '污染' }),
        sseFrame('message.completed', { run_id: 'run-A' }),
      ]),
    )

    const chat = openedChat()
    await chat.send('问题')

    expect(chat.run.value.serverRunId).toBe('run-A')
    expect(chat.messages.value[1].content).toBe('甲')
    expect(chat.messages.value[1].status).toBe('done')
    expect(chat.run.value.ignoredEvents).toBe(1)
  })

  it('accepts an id-less stream just as readily', async () => {
    pushStream(
      frames([
        sseFrame('message.started', {}),
        sseFrame('message.delta', { delta: '无 id' }),
        sseFrame('message.completed', {}),
      ]),
    )

    const chat = openedChat()
    await chat.send('问题')

    expect(chat.messages.value[1].content).toBe('无 id')
    expect(chat.run.value.serverRunId).toBe('')
    expect(chat.run.value.ignoredEvents).toBe(0)
  })

  it('sends only `content`, so no client id is smuggled into the contract', async () => {
    pushStream(frames([sseFrame('message.completed', { content: 'ok' })]))

    const chat = openedChat()
    await chat.send('问题')

    const stream = calls.find((call) => call.url.endsWith('/stream'))
    expect(JSON.parse(stream.init.body)).toEqual({ content: '问题' })
  })

  it('a cancelled request that keeps emitting cannot rewrite the next run', async () => {
    const orphan = lateDeliverySSEResponse()
    pushStream(orphan.response)
    pushStream(frames([sseFrame('message.completed', { content: '正确答案' })]))

    const chat = openedChat()

    const first = chat.send('第一个问题')
    await settle()
    expect(chat.streaming.value).toBe(true)

    chat.cancel()
    const second = chat.send('第二个问题')
    await settle()

    // Frames already in flight when the user pressed stop.
    orphan.push(sseFrame('message.delta', { run_id: 'srv-1', delta: '迟到的污染' }))
    orphan.push(
      sseFrame('message.completed', { run_id: 'srv-1', content: '迟到的完成' }),
    )
    orphan.finish()

    await first
    await second

    const answers = chat.messages.value.filter((m) => m.role === 'assistant')
    expect(answers).toHaveLength(2)
    expect(answers[0]).toMatchObject({ status: 'stopped', content: '' })
    expect(answers[1]).toMatchObject({ status: 'done', content: '正确答案' })
    expect(chat.messages.value.some((m) => m.content.includes('迟到'))).toBe(false)
    expect(chat.run.value.text).toBe('正确答案')
  })
})

async function settle(times = 4) {
  for (let i = 0; i < times; i += 1) await new Promise((resolve) => setTimeout(resolve, 0))
}
