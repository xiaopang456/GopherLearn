import { beforeEach, describe, expect, it, vi } from 'vitest'

import { useChat } from '../src/composables/useChat.js'
import { useKnowledge } from '../src/composables/useKnowledge.js'
import { setTokenProvider } from '../src/api/http.js'
import { jsonResponse, sseFrame, sseResponse } from './helpers/transport.js'

/**
 * Per-turn grounding is the one piece of Chat state that must not travel. The
 * collection list itself must be shared, so these cases drive the real
 * `useKnowledge` store over a stubbed transport and assert what reaches the
 * wire.
 */

const SERVER_COLLECTIONS = [
  { id: 'collection-1', title: '操作系统笔记' },
  { id: 'collection-2', title: '并发编程' },
]

const CHAT_A = { id: 'a', title: '对话 A', mode: 'chat' }
const CHAT_B = { id: 'b', title: '对话 B', mode: 'chat' }
const AGENT = { id: 'g', title: '学习 Agent', mode: 'learning_agent' }
const MYSTERY = { id: 'm', title: '旧会话', mode: 'voice_beta' }

let streamBodies
let collectionPayload
const knowledge = useKnowledge()

function serve(sessions) {
  globalThis.fetch = async (url, init = {}) => {
    const path = new URL(String(url), 'https://api.test').pathname
    if (path.endsWith('/stream')) {
      streamBodies.push(JSON.parse(init.body))
      return sseResponse([sseFrame('message.completed', { content: '回声' })])
    }
    if (path === '/api/v1/collections') {
      return jsonResponse({ data: { items: collectionPayload }, error: null })
    }
    if (path === '/api/v1/sessions') {
      return jsonResponse({ data: { items: sessions }, error: null })
    }
    if (/^\/api\/v1\/sessions\/[^/]+\/messages$/.test(path)) {
      return jsonResponse({ data: { items: [] }, error: null })
    }
    if (init.method === 'DELETE') return jsonResponse(null, { status: 204 })
    return jsonResponse(
      { data: null, error: { code: 'not_found', message: 'no' } },
      { status: 404 },
    )
  }
}

async function seed(ids) {
  collectionPayload = ids
  await knowledge.loadCollections()
}

async function open(chat, id) {
  await chat.openSession(id)
}

beforeEach(async () => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'tok.knowledge')
  streamBodies = []
  collectionPayload = []
  serve([])
  await knowledge.loadCollections()
  expect(knowledge.collections.value).toEqual([])
})

describe('grounding reaches the request only where it was chosen', () => {
  it('omits the block while nothing is selected', async () => {
    serve([CHAT_A])
    const chat = useChat()
    await open(chat, 'a')

    await chat.send('问题')

    expect(streamBodies[0]).toEqual({ content: '问题' })
  })

  it('carries the chosen collections for a chat turn', async () => {
    await seed(SERVER_COLLECTIONS)
    serve([CHAT_A])
    const chat = useChat()
    await open(chat, 'a')

    chat.setKnowledge('a', { enabled: true, selectedCollectionIds: ['collection-2'] })
    await chat.send('问题')

    expect(streamBodies[0]).toEqual({
      content: '问题',
      knowledge: { enabled: true, collection_ids: ['collection-2'] },
    })
  })

  it('refuses to arm grounding on an agent session', async () => {
    await seed(SERVER_COLLECTIONS)
    serve([AGENT])
    const chat = useChat()
    await open(chat, 'g')

    chat.setKnowledge('g', { enabled: true, selectedCollectionIds: ['collection-1'] })
    expect(chat.knowledgeFor('g')).toEqual({ enabled: false, selectedCollectionIds: [] })

    await chat.send('问题')
    // An agent turn does ask to watch its own progress, and grounding is still not on
    // the wire: `stream_steps` is the only addition, and it belongs to the mode, not to
    // the option the server just refused.
    expect(streamBodies[0]).toEqual({ content: '问题', stream_steps: true, requestId: expect.stringMatching(/^send-/) })
  })

  it('refuses it for an unrecognised mode too', async () => {
    await seed(SERVER_COLLECTIONS)
    serve([MYSTERY])
    const chat = useChat()
    await open(chat, 'm')

    chat.setKnowledge('m', { enabled: true, selectedCollectionIds: ['collection-1'] })
    await chat.send('问题')

    expect(streamBodies[0]).toEqual({ content: '问题' })
  })

  it('ignores a collection id the server never issued', async () => {
    await seed(SERVER_COLLECTIONS)
    serve([CHAT_A])
    const chat = useChat()
    await open(chat, 'a')

    chat.setKnowledge('a', {
      enabled: true,
      selectedCollectionIds: ['collection-of-someone-else'],
    })
    await chat.send('问题')

    expect(streamBodies[0]).toEqual({ content: '问题' })
  })

  it('requires a selection before it will claim to be grounded', async () => {
    await seed(SERVER_COLLECTIONS)
    serve([CHAT_A])
    const chat = useChat()
    await open(chat, 'a')

    chat.setKnowledge('a', { enabled: true, selectedCollectionIds: [] })
    await chat.send('问题')

    expect(streamBodies[0]).toEqual({ content: '问题' })
  })
})

describe('the draft belongs to one session, not to the screen', () => {
  it('does not follow from one chat session into another', async () => {
    await seed(SERVER_COLLECTIONS)
    serve([CHAT_A, CHAT_B])
    const chat = useChat()

    await open(chat, 'a')
    chat.setKnowledge('a', { enabled: true, selectedCollectionIds: ['collection-1'] })
    expect(chat.activeKnowledge.value.enabled).toBe(true)

    await open(chat, 'b')
    expect(chat.activeKnowledge.value).toEqual({ enabled: false, selectedCollectionIds: [] })

    await chat.send('B 的问题')
    expect(streamBodies[0]).toEqual({ content: 'B 的问题' })

    await open(chat, 'a')
    expect(chat.activeKnowledge.value).toEqual({
      enabled: true,
      selectedCollectionIds: ['collection-1'],
    })
  })

  it('does not leak from a chat session into an agent session', async () => {
    await seed(SERVER_COLLECTIONS)
    serve([CHAT_A, AGENT])
    const chat = useChat()

    await open(chat, 'a')
    chat.setKnowledge('a', { enabled: true, selectedCollectionIds: ['collection-1'] })

    await open(chat, 'g')
    expect(chat.activeKnowledge.value.enabled).toBe(false)
    expect(chat.canUseKnowledge.value).toBe(false)

    await chat.send('Agent 的问题')
    // The chat draft did not cross over; only the agent's own request flag did.
    expect(streamBodies[0]).toEqual({ content: 'Agent 的问题', stream_steps: true, requestId: expect.stringMatching(/^send-/) })
  })

  it('drops the draft with the session', async () => {
    await seed(SERVER_COLLECTIONS)
    serve([CHAT_A])
    const chat = useChat()
    await open(chat, 'a')
    chat.setKnowledge('a', { enabled: true, selectedCollectionIds: ['collection-1'] })

    await chat.removeSession('a')
    expect(chat.knowledgeFor('a')).toEqual({ enabled: false, selectedCollectionIds: [] })
  })
})

describe('the collection list is one shared truth', () => {
  it('prunes a selection whose collection disappeared from the shared list', async () => {
    await seed(SERVER_COLLECTIONS)
    serve([CHAT_A])
    const chat = useChat()
    await open(chat, 'a')
    chat.setKnowledge('a', {
      enabled: true,
      selectedCollectionIds: ['collection-1', 'collection-2'],
    })

    await seed([{ id: 'collection-2', title: '并发编程' }])

    expect(chat.knowledgeFor('a')).toEqual({
      enabled: true,
      selectedCollectionIds: ['collection-2'],
    })
  })

  it('disables the option when the last collection goes away', async () => {
    await seed(SERVER_COLLECTIONS)
    serve([CHAT_A])
    const chat = useChat()
    await open(chat, 'a')
    chat.setKnowledge('a', { enabled: true, selectedCollectionIds: ['collection-1'] })
    expect(chat.canUseKnowledge.value).toBe(true)

    await seed([])

    expect(chat.canUseKnowledge.value).toBe(false)
    expect(chat.activeKnowledge.value).toEqual({ enabled: false, selectedCollectionIds: [] })

    await chat.send('问题')
    expect(streamBodies[0]).toEqual({ content: '问题' })
  })

  it('is visible to the composer and the management screen at the same time', async () => {
    await seed(SERVER_COLLECTIONS)
    serve([CHAT_A])
    const chat = useChat()
    expect(chat.canUseKnowledge.value).toBe(false) // no session open yet

    await open(chat, 'a')
    expect(knowledge.collections.value.map((item) => item.id)).toEqual([
      'collection-1',
      'collection-2',
    ])
    // One list, two consumers: the composer sees the same truth the page shows.
    expect(chat.canUseKnowledge.value).toBe(true)
  })
})

describe('mode state stays correct while typing', () => {
  it('a freshly created chat session can enable grounding immediately', async () => {
    await seed(SERVER_COLLECTIONS)
    globalThis.fetch = async (url, init = {}) => {
      const path = new URL(String(url), 'https://api.test').pathname
      if (path.endsWith('/stream')) {
        streamBodies.push(JSON.parse(init.body))
        return sseResponse([sseFrame('message.completed', { content: '回声' })])
      }
      if (path === '/api/v1/sessions' && init.method === 'POST') {
        return jsonResponse({ data: { id: 'fresh', mode: 'chat' }, error: null })
      }
      return jsonResponse({ data: { items: [] }, error: null })
    }

    const chat = useChat()
    const id = await chat.createSession('chat')

    expect(chat.isChatSession.value).toBe(true)
    expect(chat.canUseKnowledge.value).toBe(true)

    chat.setKnowledge(id, { enabled: true, selectedCollectionIds: ['collection-1'] })
    await chat.send('基于笔记的问题')

    expect(streamBodies[0]).toEqual({
      content: '基于笔记的问题',
      knowledge: { enabled: true, collection_ids: ['collection-1'] },
    })
  })
})
