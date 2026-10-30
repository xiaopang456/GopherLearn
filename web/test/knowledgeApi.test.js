import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  ACCEPT_ATTRIBUTE,
  addDocument,
  collectionDocumentsPath,
  createCollection,
  deleteDocument,
  documentPath,
  formatForFile,
  listCollections,
  listDocuments,
  MAX_CONTENT_BYTES,
  normalizeCollectionList,
  normalizeDocumentList,
  replaceDocument,
  utf8Length,
  validateDocumentContent,
} from '../src/api/knowledge.js'
import { useKnowledge } from '../src/composables/useKnowledge.js'
import { useChat } from '../src/composables/useChat.js'
import { setTokenProvider } from '../src/api/http.js'
import { createAppRouter } from '../src/router/index.js'
import { createMemoryHistory } from 'vue-router'
import { jsonResponse } from './helpers/transport.js'

let calls

function route(handlers) {
  globalThis.fetch = async (url, init = {}) => {
    const path = new URL(String(url), 'https://api.test').pathname
    calls.push({ url: String(url), path, init })
    const handler = handlers[path] ?? handlers['*']
    if (!handler) throw new Error(`unrouted: ${init?.method ?? 'GET'} ${path}`)
    return typeof handler === 'function' ? handler(url, init) : handler
  }
}

beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'tok.kb')
  calls = []
})

describe('knowledge endpoints', () => {
  it('builds paths without query strings and escapes every id', () => {
    expect(collectionDocumentsPath('c 1/x')).toBe(
      '/api/v1/collections/c%201%2Fx/documents',
    )
    expect(documentPath('doc?2')).toBe('/api/v1/documents/doc%3F2')
  })

  it('GETs the collection list', async () => {
    route({ '/api/v1/collections': jsonResponse({ data: { items: [] }, error: null }) })

    await listCollections()

    expect(calls[0].path).toBe('/api/v1/collections')
    expect(calls[0].init.method).toBe('GET')
    expect(calls[0].url).not.toContain('tok.kb')
    expect(calls[0].init.headers.authorization).toBe('Bearer tok.kb')
  })

  it('POSTs a collection title', async () => {
    route({ '/api/v1/collections': jsonResponse({ data: { id: 'c-1' }, error: null }) })

    await createCollection('  操作系统  ')

    expect(calls[0].init.method).toBe('POST')
    expect(JSON.parse(calls[0].init.body)).toEqual({ title: '操作系统' })
  })

  it('lists documents under the collection path', async () => {
    route({
      '/api/v1/collections/c-1/documents': jsonResponse({ data: { items: [] } }),
    })

    await listDocuments('c-1')

    expect(calls[0].path).toBe('/api/v1/collections/c-1/documents')
  })

  it('POSTs a document as JSON text, not a file part', async () => {
    route({
      '/api/v1/collections/c-1/documents': jsonResponse({ data: { id: 'd-1' } }),
    })

    await addDocument('c-1', {
      title: 'notes.md',
      format: 'markdown',
      content: '# 标题\n正文',
    })

    expect(calls[0].init.headers['content-type']).toBe('application/json')
    expect(JSON.parse(calls[0].init.body)).toEqual({
      title: 'notes.md',
      format: 'markdown',
      content: '# 标题\n正文',
    })
  })

  it('PUTs a replacement and DELETEs by document id', async () => {
    route({ '/api/v1/documents/d-1': jsonResponse({ data: { id: 'd-1' } }) })

    await replaceDocument('d-1', { title: 'a.md', format: 'markdown', content: 'x' })
    expect(calls[0].init.method).toBe('PUT')

    await deleteDocument('d-1')
    expect(calls[1].init.method).toBe('DELETE')
  })

  it('leaves an unroutable id unable to escape its path segment', async () => {
    route({ '*': jsonResponse({ data: { items: [] } }) })

    await listDocuments('../../admin')

    expect(calls[0].path).not.toBe('/api/v1/collections/../../admin/documents')
    expect(calls[0].path).toBe('/api/v1/collections/..%2F..%2Fadmin/documents')
  })
})

describe('format mapping and the size ceiling', () => {
  it('maps accepted extensions onto the server’s format table', () => {
    expect(formatForFile('a.md')).toEqual({ format: 'markdown' })
    expect(formatForFile('a.MARKDOWN')).toEqual({ format: 'markdown' })
    expect(formatForFile('a.mkd')).toEqual({ format: 'markdown' })
    expect(formatForFile('a.txt')).toEqual({ format: 'text' })
  })

  it('refuses anything else instead of guessing a format', () => {
    for (const name of ['a.pdf', 'a.docx', 'a', 'no-extension', 'a.', 'a.md.txt.exe']) {
      expect(formatForFile(name).error).toBeTruthy()
    }
  })

  it('advertises only the extensions the browser picker should allow', () => {
    expect(ACCEPT_ATTRIBUTE).toBe('.md,.markdown,.txt')
  })

  it('counts 2 MiB the way the server does, in UTF-8 bytes', () => {
    expect(MAX_CONTENT_BYTES).toBe(2 << 20)
    expect(utf8Length('学习')).toBe(6)
    expect(validateDocumentContent('a'.repeat(MAX_CONTENT_BYTES))).toBe(null)
    expect(validateDocumentContent('a'.repeat(MAX_CONTENT_BYTES + 1))).toMatch(/2 MiB/)
  })

  it('measures multibyte content against the byte ceiling, not the character count', () => {
    // 700k CJK characters is under 2 MiB of *characters* and over 2 MiB of bytes.
    const heavy = '学'.repeat(700_001)
    expect(heavy.length).toBeLessThan(MAX_CONTENT_BYTES)
    expect(validateDocumentContent(heavy)).toMatch(/超过/)
  })

  it('rejects an empty document before the server is asked', () => {
    expect(validateDocumentContent('')).toBe('文件内容是空的。')
    expect(validateDocumentContent('   ')).not.toBeNull()
  })
})

describe('list normalisation', () => {
  it('drops entries the client could never address', () => {
    const list = normalizeCollectionList({
      items: [{ id: 'c-1', title: '甲' }, { title: '无 id' }, null],
    })
    expect(list).toEqual([
      { id: 'c-1', title: '甲', createdAt: '', updatedAt: '' },
    ])
  })

  it('reads a missing title as a placeholder, not an invention', () => {
    expect(normalizeCollectionList({ items: [{ id: 'c-2' }] })[0].title).toBe(
      '未命名知识库',
    )
  })

  it('coerces document counters to numbers', () => {
    const [doc] = normalizeDocumentList({
      items: [
        {
          id: 'd-1',
          collection_id: 'c-1',
          title: 'n.md',
          format: 'markdown',
          version: '3',
          chunk_count: '12',
          updated_at: '2026-09-25T00:00:00Z',
        },
      ],
    })
    expect(doc).toMatchObject({ version: 3, chunkCount: 12, format: 'markdown' })
  })

  it('treats a malformed envelope as empty rather than throwing', () => {
    expect(normalizeCollectionList(null)).toEqual([])
    expect(normalizeDocumentList({ data: 'nope' })).toEqual([])
  })
})

describe('useKnowledge store', () => {
  const fresh = () => useKnowledge()

  it('loads collections into the shared state', async () => {
    route({
      '/api/v1/collections': jsonResponse({
        data: { items: [{ id: 'c-1', title: '操作系统笔记' }] },
      }),
    })
    const knowledge = fresh()

    await knowledge.loadCollections()

    expect(knowledge.collections.value).toHaveLength(1)
    expect(knowledge.collectionsError.value).toBe('')
  })

  it('makes a created collection immediately selectable by Chat', async () => {
    route({
      '/api/v1/collections': (url, init) =>
        init.method === 'POST'
          ? jsonResponse({ data: { id: 'c-new', title: '并发' } })
          : jsonResponse({ data: { items: [] } }),
    })
    const knowledge = fresh()
    await knowledge.loadCollections()
    const chat = useChat()

    const created = await knowledge.createCollection('并发')

    expect(created.id).toBe('c-new')
    expect(knowledge.collections.value.map((item) => item.id)).toEqual(['c-new'])
    // The composer reads the same list, so it becomes usable without a refresh.
    expect(knowledge.collectionIds.value).toEqual(['c-new'])
    expect(chat.canUseKnowledge.value).toBe(false) // still no session open
  })

  it('reports a failed create without pretending a collection exists', async () => {
    route({
      '/api/v1/collections': (url, init) =>
        init.method === 'POST'
          ? jsonResponse(
              { data: null, error: { code: 'invalid_request', message: 'request is invalid' } },
              { status: 400 },
            )
          : jsonResponse({ data: { items: [] } }),
    })
    const knowledge = fresh()
    const before = knowledge.collections.value.length

    expect(await knowledge.createCollection('   ')).toBeNull()
    expect(knowledge.collectionsError.value).toBe('请填写知识库名称。')

    expect(await knowledge.createCollection('坏的')).toBeNull()
    expect(knowledge.collections.value).toHaveLength(before)
    expect(knowledge.collectionsError.value).toBe('request is invalid')
  })

  it('shows documents of the selected collection', async () => {
    route({
      '/api/v1/collections/c-1/documents': jsonResponse({
        data: {
          items: [
            { id: 'd-1', collection_id: 'c-1', title: 'a.md', format: 'markdown', version: 1, chunk_count: 4 },
          ],
        },
      }),
      '/api/v1/collections/c-2/documents': jsonResponse({ data: { items: [] } }),
    })
    const knowledge = fresh()

    await knowledge.selectCollection('c-1')
    expect(knowledge.documents.value.map((item) => item.id)).toEqual(['d-1'])

    await knowledge.selectCollection('c-2')
    expect(knowledge.documents.value).toEqual([])
  })

  it('lets a slow response for the old collection lose to the new one', async () => {
    const gates = { 'c-1': deferredGate(), 'c-2': deferredGate() }
    route({
      '/api/v1/collections/c-1/documents': () => gates['c-1'].promise,
      '/api/v1/collections/c-2/documents': () => gates['c-2'].promise,
    })
    const knowledge = fresh()

    const opening = knowledge.selectCollection('c-1')
    await settle()
    const second = knowledge.selectCollection('c-2')
    await settle()

    gates['c-2'].resolve(
      jsonResponse({ data: { items: [{ id: 'd-new', collection_id: 'c-2', title: '新.md' }] } }),
    )
    await second
    expect(knowledge.documents.value.map((item) => item.id)).toEqual(['d-new'])

    // The superseded request answers last and must change nothing.
    gates['c-1'].resolve(
      jsonResponse({ data: { items: [{ id: 'd-old', collection_id: 'c-1', title: '旧.md' }] } }),
    )
    await opening
    await settle()

    expect(knowledge.documents.value.map((item) => item.id)).toEqual(['d-new'])
    expect(knowledge.documentsLoading.value).toBe(false)
    expect(knowledge.documentsError.value).toBe('')
  })

  it('aborts the superseded document request instead of letting it drain', async () => {
    const signals = []
    const gates = { 'c-1': deferredGate(), 'c-2': deferredGate() }
    route({
      '/api/v1/collections/c-1/documents': (url, init) => {
        signals.push(init.signal)
        return gates['c-1'].promise
      },
      '/api/v1/collections/c-2/documents': (url, init) => {
        signals.push(init.signal)
        return gates['c-2'].promise
      },
    })
    const knowledge = fresh()

    const first = knowledge.selectCollection('c-1')
    await settle()
    expect(signals).toHaveLength(1)
    expect(signals[0].aborted).toBe(false)

    // Browsing away while A is still in flight must stop reading A.
    const second = knowledge.selectCollection('c-2')
    await settle()
    expect(signals[1]).not.toBe(signals[0])
    expect(signals[0].aborted).toBe(true)

    gates['c-2'].resolve(jsonResponse({ data: { items: [] } }))
    await second
    gates['c-1'].resolve(jsonResponse({ data: { items: [] } }))
    await first
  })

  it('uploads a chosen file as a document', async () => {
    route({
      '/api/v1/collections/c-1/documents': (url, init) =>
        init.method === 'POST'
          ? jsonResponse({
              data: {
                id: 'd-up',
                collection_id: 'c-1',
                title: '笔记.md',
                format: 'markdown',
                version: 1,
                chunk_count: 2,
              },
            })
          : jsonResponse({ data: { items: [] } }),
    })
    const knowledge = fresh()
    await knowledge.selectCollection('c-1')

    const created = await knowledge.uploadDocument('c-1', {
      name: '笔记.md',
      text: async () => '# 标题\n内容',
    })

    expect(created.id).toBe('d-up')
    expect(JSON.parse(calls.at(-1).init.body)).toEqual({
      title: '笔记.md',
      format: 'markdown',
      content: '# 标题\n内容',
    })
    expect(knowledge.documents.value.map((item) => item.id)).toEqual(['d-up'])
    expect(knowledge.documentsError.value).toBe('')
  })

  it('refuses an unsupported file before any request leaves', async () => {
    route({ '*': jsonResponse({ data: { items: [] } }) })
    const knowledge = fresh()
    await knowledge.selectCollection('c-1')
    const before = calls.length

    const created = await knowledge.uploadDocument('c-1', {
      name: 'manual.pdf',
      text: async () => 'x',
    })

    expect(created).toBeNull()
    expect(calls).toHaveLength(before)
    expect(knowledge.documentsError.value).toContain('只支持')
  })

  it('refuses an over-limit file before any request leaves', async () => {
    route({ '*': jsonResponse({ data: { items: [] } }) })
    const knowledge = fresh()
    await knowledge.selectCollection('c-1')
    const before = calls.length

    const created = await knowledge.uploadDocument('c-1', {
      name: 'big.txt',
      text: async () => 'a'.repeat(MAX_CONTENT_BYTES + 1),
    })

    expect(created).toBeNull()
    expect(calls).toHaveLength(before)
    expect(knowledge.documentsError.value).toMatch(/2 MiB/)
  })

  it('surfaces an upload failure where the user can see it', async () => {
    route({
      '/api/v1/collections/c-1/documents': (url, init) =>
        init.method === 'POST'
          ? jsonResponse(
              { data: null, error: { code: 'invalid_request', message: '内容为空' } },
              { status: 400 },
            )
          : jsonResponse({ data: { items: [] } }),
    })
    const knowledge = fresh()
    await knowledge.selectCollection('c-1')

    await knowledge.uploadDocument('c-1', { name: 'a.md', text: async () => 'x' })

    expect(knowledge.documentsError.value).toBe('内容为空')
  })

  it('removes a document from the visible list on success', async () => {
    route({
      '/api/v1/documents/d-1': jsonResponse({ data: null, error: null }),
      '/api/v1/collections/c-1/documents': jsonResponse({
        data: {
          items: [
            { id: 'd-1', collection_id: 'c-1', title: 'a.md' },
            { id: 'd-2', collection_id: 'c-1', title: 'b.md' },
          ],
        },
      }),
    })
    const knowledge = fresh()
    await knowledge.selectCollection('c-1')
    expect(knowledge.documents.value).toHaveLength(2)

    expect(await knowledge.removeDocument('d-1')).toBe(true)

    expect(knowledge.documents.value.map((item) => item.id)).toEqual(['d-2'])
    expect(knowledge.documentsError.value).toBe('')
  })

  it('keeps the document listed when the delete failed', async () => {
    route({
      '/api/v1/documents/d-1': jsonResponse(
        { data: null, error: { code: 'not_found', message: '文档不存在' } },
        { status: 404 },
      ),
      '/api/v1/collections/c-1/documents': jsonResponse({
        data: { items: [{ id: 'd-1', collection_id: 'c-1', title: 'a.md' }] },
      }),
    })
    const knowledge = fresh()
    await knowledge.selectCollection('c-1')

    expect(await knowledge.removeDocument('d-1')).toBe(false)

    expect(knowledge.documents.value.map((item) => item.id)).toEqual(['d-1'])
    expect(knowledge.documentsError.value).toBe('文档不存在')
  })

  it('clears documents when the selection is dropped', async () => {
    route({ '*': jsonResponse({ data: { items: [{ id: 'd-1' }] } }) })
    const knowledge = fresh()
    await knowledge.selectCollection('c-1')
    expect(knowledge.documents.value).toHaveLength(1)

    await knowledge.selectCollection('')

    expect(knowledge.documents.value).toEqual([])
    expect(knowledge.activeCollectionId.value).toBe('')
  })
})

describe('a rejected credential still ends the session everywhere', () => {
  it('signs out and routes to the sign-in screen from a knowledge call', async () => {
    const store = {
      state: { token: 'tok.kb', user: { email: 'a@b.test' } },
      signedIn: true,
      isAuthenticated: () => store.signedIn,
      signOut: () => {
        store.signedIn = false
      },
    }
    const router = createAppRouter({ authStore: store, history: createMemoryHistory() })
    await router.push('/knowledge/v2-documents')
    await router.isReady()

    route({
      '/api/v1/collections': jsonResponse(
        { data: null, error: { code: 'unauthorized', message: 'authentication is required' } },
        { status: 401 },
      ),
    })

    const knowledge = useKnowledge()
    await knowledge.loadCollections()
    await settle()

    expect(store.signedIn).toBe(false)
    expect(router.currentRoute.value.name).toBe('login')
    expect(router.currentRoute.value.query.redirect).toBe('/knowledge/v2-documents')
    // and the panel says something safe rather than leaking internals
    expect(knowledge.collectionsError.value).toBe('authentication is required')
  })

  it('does not treat a wrong password as an expired session', async () => {
    route({
      '/api/v1/collections': jsonResponse(
        { data: null, error: { code: 'invalid_credentials', message: 'nope' } },
        { status: 401 },
      ),
    })
    const knowledge = useKnowledge()

    await knowledge.loadCollections()

    expect(knowledge.collectionsError.value).toBe('nope')
  })
})

function deferredGate() {
  let resolve
  const promise = new Promise((res) => {
    resolve = res
  })
  return { promise, resolve }
}

async function settle(times = 4) {
  for (let i = 0; i < times; i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0))
  }
}
