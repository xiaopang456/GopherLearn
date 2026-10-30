import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory } from 'vue-router'

import {
  resetKnowledgeState,
  useKnowledge,
} from '../src/composables/useKnowledge.js'
import { useChat } from '../src/composables/useChat.js'
import { authStore } from '../src/auth/store.js'
import { createAppRouter } from '../src/router/index.js'
import { setTokenProvider } from '../src/api/http.js'
import { deferred, jsonResponse } from './helpers/transport.js'

/**
 * The knowledge store is shared across the whole tab, so it must not outlive the
 * account it was filled for. Every case here hands a request over a boundary —
 * a sign-out — and checks which side of it the response is allowed to write on.
 */

const knowledge = useKnowledge()

let calls

function route(handlers) {
  globalThis.fetch = async (url, init = {}) => {
    const path = new URL(String(url), 'https://api.test').pathname
    calls.push({ path, init })
    const handler = handlers[path]
    if (!handler) throw new Error(`unrouted: ${init?.method ?? 'GET'} ${path}`)
    return typeof handler === 'function' ? handler(url, init) : handler
  }
}

const A_COLLECTION = { id: 'c-alice', title: '艾丽丝的笔记' }
const A_DOCUMENT = {
  id: 'd-alice',
  collection_id: 'c-alice',
  title: 'paging.md',
  format: 'markdown',
  version: 1,
  chunk_count: 2,
}

async function settle(times = 5) {
  for (let i = 0; i < times; i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0))
  }
}

function initialState() {
  return {
    collections: knowledge.collections.value,
    active: knowledge.activeCollectionId.value,
    documents: knowledge.documents.value,
    collectionsError: knowledge.collectionsError.value,
    documentsError: knowledge.documentsError.value,
    collectionsLoading: knowledge.collectionsLoading.value,
    documentsLoading: knowledge.documentsLoading.value,
    mutating: knowledge.mutating.value,
  }
}

function expectEmptiedForA(state) {
  expect(state.collections).toEqual([])
  expect(state.active).toBe('')
  expect(state.documents).toEqual([])
  expect(state.collectionsError).toBe('')
  expect(state.documentsError).toBe('')
  expect(state.collectionsLoading).toBe(false)
  expect(state.documentsLoading).toBe(false)
  expect(state.mutating).toBe(false)
}

beforeEach(async () => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  calls = []
  resetKnowledgeState()
  authStore.signOut()
  setTokenProvider(() => 'tok.reset')
  await knowledge.loadCollections()
})

describe('a plain sign-out after a normal browse', () => {
  it('clears collections, documents, selection and every transient flag', async () => {
    route({
      '/api/v1/collections': jsonResponse({ data: { items: [A_COLLECTION] } }),
      '/api/v1/collections/c-alice/documents': jsonResponse({
        data: { items: [A_DOCUMENT] },
      }),
    })
    authStore.adopt('tok.alice', { id: 'u-a', email: 'alice@x.test' })

    await knowledge.loadCollections()
    await knowledge.selectCollection('c-alice')
    expect(knowledge.collections.value).toHaveLength(1)
    expect(knowledge.documents.value).toHaveLength(1)
    expect(knowledge.activeCollectionId.value).toBe('c-alice')

    authStore.signOut()

    expectEmptiedForA(initialState())
    expect(knowledge.activeCollection.value).toBe(null)
    expect(knowledge.collectionIds.value).toEqual([])
  })
})

describe('responses that land after the account is gone', () => {
  it('a collections load in flight at sign-out cannot repopulate the list', async () => {
    const gate = deferred()
    route({ '/api/v1/collections': () => gate.promise })
    authStore.adopt('tok.alice', { id: 'u-a', email: 'alice@x.test' })

    const inFlight = knowledge.loadCollections()
    await settle()
    expect(knowledge.collectionsLoading.value).toBe(true)

    authStore.signOut()
    expect(knowledge.collectionsLoading.value).toBe(false)

    // A's data arrives a beat later and is now nobody's to show.
    gate.resolve(jsonResponse({ data: { items: [A_COLLECTION] } }))
    const result = await inFlight
    await settle()

    expect(result).toEqual([])
    expectEmptiedForA(initialState())
  })

  it('aborts a documents load that is still open at sign-out', async () => {
    const gate = deferred()
    route({
      '/api/v1/collections': jsonResponse({ data: { items: [A_COLLECTION] } }),
      '/api/v1/collections/c-alice/documents': (url, init) => {
        gate.signal = init.signal
        return gate.promise
      },
    })
    authStore.adopt('tok.alice', { id: 'u-a', email: 'alice@x.test' })

    await knowledge.loadCollections()
    const inFlight = knowledge.selectCollection('c-alice')
    await settle()

    authStore.signOut()
    expect(gate.signal.aborted).toBe(true)

    gate.resolve(jsonResponse({ data: { items: [A_DOCUMENT] } }))
    await inFlight
    await settle()
    expectEmptiedForA(initialState())
  })

  it('a createCollection that succeeds post-sign-out is not appended', async () => {
    const gate = deferred()
    route({ '/api/v1/collections': (url, init) => (init.method === 'POST' ? gate.promise : jsonResponse({ data: { items: [] } })) })
    authStore.adopt('tok.alice', { id: 'u-a', email: 'alice@x.test' })

    const inFlight = knowledge.createCollection('艾丽丝私有')
    await settle()
    expect(knowledge.mutating.value).toBe(true)

    authStore.signOut()

    gate.resolve(jsonResponse({ data: A_COLLECTION }))
    expect(await inFlight).toBe(null)
    await settle()
    expectEmptiedForA(initialState())
  })

  it('an upload result cannot write into a reset store', async () => {
    const gate = deferred()
    route({
      '/api/v1/collections': jsonResponse({ data: { items: [A_COLLECTION] } }),
      '/api/v1/collections/c-alice/documents': (url, init) =>
        init.method === 'POST' ? gate.promise : jsonResponse({ data: { items: [A_DOCUMENT] } }),
    })
    authStore.adopt('tok.alice', { id: 'u-a', email: 'alice@x.test' })
    await knowledge.loadCollections()
    await knowledge.selectCollection('c-alice')

    const inFlight = knowledge.uploadDocument('c-alice', {
      name: 'new.md',
      text: async () => '# 新内容',
    })
    await settle()
    authStore.signOut()
    gate.resolve(jsonResponse({ data: { ...A_DOCUMENT, id: 'd-new' } }))

    expect(await inFlight).toBe(null)
    await settle()
    expectEmptiedForA(initialState())
  })

  it('a failed replace after sign-out cannot leave an error behind', async () => {
    const gate = deferred()
    route({
      '/api/v1/documents/d-alice': () => gate.promise,
      '/api/v1/collections': jsonResponse({ data: { items: [A_COLLECTION] } }),
      '/api/v1/collections/c-alice/documents': jsonResponse({ data: { items: [A_DOCUMENT] } }),
    })
    authStore.adopt('tok.alice', { id: 'u-a', email: 'alice@x.test' })
    await knowledge.loadCollections()
    await knowledge.selectCollection('c-alice')

    const inFlight = knowledge.replaceDocument('d-alice', {
      title: 'a.md',
      format: 'markdown',
      content: '改一下',
    })
    await settle()
    authStore.signOut()
    gate.resolve(
      jsonResponse(
        { data: null, error: { code: 'internal_error', message: 'db down' } },
        { status: 500 },
      ),
    )

    expect(await inFlight).toBe(null)
    await settle()
    expect(knowledge.documentsError.value).toBe('')
    expect(knowledge.mutating.value).toBe(false)
  })

  it('a delete that resolves after sign-out reports no success', async () => {
    const gate = deferred()
    route({
      '/api/v1/documents/d-alice': () => gate.promise,
      '/api/v1/collections': jsonResponse({ data: { items: [A_COLLECTION] } }),
      '/api/v1/collections/c-alice/documents': jsonResponse({ data: { items: [A_DOCUMENT] } }),
    })
    authStore.adopt('tok.alice', { id: 'u-a', email: 'alice@x.test' })
    await knowledge.loadCollections()
    await knowledge.selectCollection('c-alice')

    const inFlight = knowledge.removeDocument('d-alice')
    await settle()
    authStore.signOut()
    gate.resolve(jsonResponse({ data: null, error: null }))

    expect(await inFlight).toBe(false)
    await settle()
    expectEmptiedForA(initialState())
  })
})

describe('every exit route performs the same reset', () => {
  it('a 401 from a knowledge call signs out and clears the store', async () => {
    authStore.adopt('tok.stale', { id: 'u-a', email: 'alice@x.test' })
    const router = createAppRouter({ authStore, history: createMemoryHistory() })
    await router.push('/knowledge/v2-documents')
    await router.isReady()

    route({
      '/api/v1/collections': jsonResponse({ data: { items: [A_COLLECTION] } }),
      '/api/v1/collections/c-alice/documents': jsonResponse({ data: { items: [A_DOCUMENT] } }),
    })
    await knowledge.loadCollections()
    await knowledge.selectCollection('c-alice')
    expect(knowledge.collections.value).toHaveLength(1)

    // The next call is rejected by the server.
    route({
      '/api/v1/collections': jsonResponse(
        { data: null, error: { code: 'unauthorized', message: 'authentication is required' } },
        { status: 401 },
      ),
    })
    await knowledge.loadCollections()
    await settle()

    expect(authStore.isAuthenticated()).toBe(false)
    expect(router.currentRoute.value.name).toBe('login')
    expectEmptiedForA(initialState())
  })

  it('resetKnowledgeState is callable on its own and is idempotent', () => {
    resetKnowledgeState()
    const first = { ...initialState(), collections: [...initialState().collections] }
    resetKnowledgeState()
    expectEmptiedForA(first)
    expectEmptiedForA(initialState())
  })
})

describe('account A out, account B in', () => {
  it('B sees nothing of A even when B own load fails', async () => {
    route({
      '/api/v1/collections': jsonResponse({ data: { items: [A_COLLECTION] } }),
      '/api/v1/collections/c-alice/documents': jsonResponse({ data: { items: [A_DOCUMENT] } }),
    })
    authStore.adopt('tok.alice', { id: 'u-a', email: 'alice@x.test' })
    await knowledge.loadCollections()
    await knowledge.selectCollection('c-alice')
    authStore.signOut()

    // B signs in and their very first read fails.
    authStore.adopt('tok.bob', { id: 'u-b', email: 'bob@x.test' })
    route({
      '/api/v1/collections': jsonResponse(
        { data: null, error: { code: 'internal_error', message: 'db down' } },
        { status: 500 },
      ),
    })
    await knowledge.loadCollections()

    expect(knowledge.collections.value).toEqual([])
    expect(knowledge.documents.value).toEqual([])
    expect(knowledge.activeCollectionId.value).toBe('')
    const text = JSON.stringify([
      knowledge.collections.value,
      knowledge.documents.value,
    ])
    expect(text).not.toContain('艾丽丝')
    expect(text).not.toContain('paging.md')
    // The failure is reported, but against an empty store: nothing of A's leaks.
    expect(knowledge.collectionsError.value).toBe('知识库列表加载失败。')
  })

  it('B keeps an empty store rather than A list when a transient failure follows', async () => {
    // Same user, same account: a failed refresh must not wipe what they had.
    authStore.adopt('tok.alice', { id: 'u-a', email: 'alice@x.test' })
    route({
      '/api/v1/collections': jsonResponse({ data: { items: [A_COLLECTION] } }),
    })
    await knowledge.loadCollections()
    expect(knowledge.collections.value).toHaveLength(1)

    route({
      '/api/v1/collections': jsonResponse(
        { data: null, error: { code: 'internal_error', message: 'blip' } },
        { status: 500 },
      ),
    })
    await knowledge.loadCollections()

    expect(knowledge.collections.value).toHaveLength(1)
  })
})

describe('a refresh that drops the open collection', () => {
  it('clears the selection, its documents, and the Chat draft naming them', async () => {
    authStore.adopt('tok.alice', { id: 'u-a', email: 'alice@x.test' })
    const chat = useChat()
    route({
      '/api/v1/collections': jsonResponse({
        data: { items: [A_COLLECTION, { id: 'c-keep', title: '保留' }] },
      }),
      '/api/v1/collections/c-alice/documents': jsonResponse({ data: { items: [A_DOCUMENT] } }),
      '/api/v1/sessions': jsonResponse({
        data: { items: [{ id: 's-1', title: '对话', mode: 'chat' }] },
      }),
    })
    await knowledge.loadCollections()
    await chat.openSession('s-1')
    chat.setKnowledge('s-1', { enabled: true, selectedCollectionIds: ['c-alice'] })
    await knowledge.selectCollection('c-alice')
    expect(knowledge.activeCollectionId.value).toBe('c-alice')
    expect(knowledge.documents.value).toHaveLength(1)

    // The server no longer lists c-alice.
    route({
      '/api/v1/collections': jsonResponse({ data: { items: [{ id: 'c-keep', title: '保留' }] } }),
    })
    await knowledge.loadCollections()
    await settle()

    expect(knowledge.activeCollectionId.value).toBe('')
    expect(knowledge.documents.value).toEqual([])
    expect(chat.knowledgeFor('s-1')).toEqual({ enabled: false, selectedCollectionIds: [] })
  })
})
