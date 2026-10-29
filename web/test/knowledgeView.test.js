import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import KnowledgeView from '../src/views/KnowledgeView.vue'
import { setTokenProvider } from '../src/api/http.js'
import { useKnowledge } from '../src/composables/useKnowledge.js'
import { jsonResponse } from './helpers/transport.js'

const mocks = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn() }))

vi.mock('vue-router', () => ({
  useRoute: () => ({ params: {}, query: {}, path: '/knowledge', name: 'knowledge' }),
  useRouter: () => ({ push: mocks.push, replace: mocks.replace }),
}))

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

const COLLECTIONS = [
  { id: 'c-1', title: '操作系统笔记', created_at: '', updated_at: '' },
  { id: 'c-2', title: '并发编程', created_at: '', updated_at: '' },
]

function documentsFor(collectionId, titles) {
  return jsonResponse({
    data: {
      items: titles.map((title, index) => ({
        id: `${collectionId}-d${index}`,
        collection_id: collectionId,
        title,
        format: title.endsWith('.txt') ? 'text' : 'markdown',
        version: index + 1,
        chunk_count: (index + 1) * 3,
        updated_at: '2026-09-25T03:04:05Z',
      })),
    },
  })
}

async function mountPage(handlers) {
  route(handlers)
  const app = mount(KnowledgeView, { attachTo: document.body })
  await flushPromises()
  await flushPromises()
  return app
}

beforeEach(async () => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'tok.kv')
  calls = []
  mocks.push.mockReset()
  route({
    '/api/v1/collections': jsonResponse({ data: { items: [] } }),
  })
  await knowledge.loadCollections()
  await knowledge.selectCollection('')
})

describe('the collection list', () => {
  it('starts honestly empty rather than showing sample material', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: [] } }),
    })

    expect(app.find('[data-test="collections-empty"]').exists()).toBe(true)
    expect(app.find('[data-test="collection-row"]').exists()).toBe(false)
    expect(app.text()).not.toContain('示例')
    app.unmount()
  })

  it('renders exactly what the server returned', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: COLLECTIONS } }),
    })

    const rows = app.findAll('[data-test="collection-row"]')
    expect(rows.map((node) => node.text().trim())).toEqual([
      '操作系统笔记',
      '并发编程',
    ])
    app.unmount()
  })

  it('shows a load failure in the panel, not just the console', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse(
        { data: null, error: { code: 'internal_error', message: 'db: dial tcp 10.0.3.7' } },
        { status: 500 },
      ),
    })

    expect(app.find('[data-test="collections-error"]').exists()).toBe(true)
    expect(app.text()).not.toContain('10.0.3.7')
    app.unmount()
  })
})

describe('creating a collection', () => {
  it('sends the title and selects the result', async () => {
    const app = await mountPage({
      '/api/v1/collections': (url, init) =>
        init.method === 'POST'
          ? jsonResponse({ data: { id: 'c-new', title: '网络' } })
          : jsonResponse({ data: { items: [] } }),
      '/api/v1/collections/c-new/documents': jsonResponse({ data: { items: [] } }),
    })

    await app.find('.kb__input').setValue('网络')
    await app.find('[data-test="create-collection"]').trigger('click')
    await flushPromises()

    const posted = calls.find((call) => call.init.method === 'POST')
    expect(JSON.parse(posted.init.body)).toEqual({ title: '网络' })
    expect(app.findAll('[data-test="collection-row"]').map((node) => node.text())).toEqual([
      '网络',
    ])
    expect(knowledge.activeCollectionId.value).toBe('c-new')
    app.unmount()
  })

  it('keeps the button inert until a name exists', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: [] } }),
    })
    expect(app.find('[data-test="create-collection"]').attributes('disabled')).toBeDefined()

    await app.find('.kb__input').setValue('  ')
    expect(app.find('[data-test="create-collection"]').attributes('disabled')).toBeDefined()
    app.unmount()
  })
})

describe('documents of the selected collection', () => {
  it('shows the fields the contract actually carries', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: COLLECTIONS } }),
      '/api/v1/collections/c-1/documents': documentsFor('c-1', ['页表.md']),
    })
    await app.findAll('[data-test="collection-row"]')[0].trigger('click')
    await flushPromises()

    const doc = app.find('[data-test="document-list"] .kb__doc')
    expect(doc.text()).toContain('页表.md')
    expect(doc.text()).toContain('markdown')
    expect(doc.text()).toContain('版本')
    expect(doc.text()).toContain('切片数')
    expect(doc.text()).toContain('3')
    app.unmount()
  })

  it('says the collection is empty instead of inventing a document', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: COLLECTIONS } }),
      '/api/v1/collections/c-1/documents': jsonResponse({ data: { items: [] } }),
    })
    await app.findAll('[data-test="collection-row"]')[0].trigger('click')
    await flushPromises()

    expect(app.find('[data-test="documents-empty"]').exists()).toBe(true)
    expect(app.find('[data-test="document-list"]').exists()).toBe(false)
    app.unmount()
  })

  it('switching collections replaces the document set', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: COLLECTIONS } }),
      '/api/v1/collections/c-1/documents': documentsFor('c-1', ['甲.md']),
      '/api/v1/collections/c-2/documents': documentsFor('c-2', ['乙.txt']),
    })
    const rows = app.findAll('[data-test="collection-row"]')
    await rows[0].trigger('click')
    await flushPromises()
    expect(app.find('.kb__doc-title').text()).toBe('甲.md')

    await app.findAll('[data-test="collection-row"]')[1].trigger('click')
    await flushPromises()
    expect(app.find('.kb__doc-title').text()).toBe('乙.txt')
    app.unmount()
  })
})

async function chooseFile(app, { name, content }) {
  const input = app.find('[data-test="file-input"]')
  Object.defineProperty(input.element, 'files', {
    value: [{ name, size: content.length, text: async () => content }],
    configurable: true,
  })
  await input.trigger('change')
  await flushPromises()
  await flushPromises()
}

describe('uploading', () => {
  it('sends a markdown file as JSON with the filename as title', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: COLLECTIONS } }),
      '/api/v1/collections/c-1/documents': (url, init) =>
        init.method === 'POST'
          ? jsonResponse({
              data: {
                id: 'c-1-d0',
                collection_id: 'c-1',
                title: 'swap.md',
                format: 'markdown',
                version: 1,
                chunk_count: 2,
              },
            })
          : jsonResponse({ data: { items: [] } }),
    })
    await app.findAll('[data-test="collection-row"]')[0].trigger('click')
    await flushPromises()

    await chooseFile(app, { name: 'swap.md', content: '# 页面置换\n正文' })

    const posted = calls.find((call) => call.init.method === 'POST')
    expect(JSON.parse(posted.init.body)).toEqual({
      title: 'swap.md',
      format: 'markdown',
      content: '# 页面置换\n正文',
    })
    expect(app.find('.kb__doc-title').text()).toBe('swap.md')
    app.unmount()
  })

  it('maps a .txt file onto the text format', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: COLLECTIONS } }),
      '/api/v1/collections/c-1/documents': (url, init) =>
        init.method === 'POST'
          ? jsonResponse({
              data: { id: 'd-txt', collection_id: 'c-1', title: 'a.txt', format: 'text', version: 1, chunk_count: 1 },
            })
          : jsonResponse({ data: { items: [] } }),
    })
    await app.findAll('[data-test="collection-row"]')[0].trigger('click')
    await flushPromises()

    await chooseFile(app, { name: 'a.txt', content: '纯文本内容' })

    const posted = calls.find((call) => call.init.method === 'POST')
    expect(JSON.parse(posted.init.body).format).toBe('text')
    expect(app.find('.kb__doc').text()).toContain('text')
    app.unmount()
  })

  it('blocks an oversized file before the request leaves', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: COLLECTIONS } }),
      '/api/v1/collections/c-1/documents': jsonResponse({ data: { items: [] } }),
    })
    await app.findAll('[data-test="collection-row"]')[0].trigger('click')
    await flushPromises()
    const before = calls.length

    await chooseFile(app, {
      name: 'huge.md',
      content: 'a'.repeat(2 * 1024 * 1024 + 8),
    })

    expect(calls).toHaveLength(before)
    expect(app.find('[data-test="upload-error"]').text()).toContain('2 MiB')
    app.unmount()
  })

  it('names the accepted types in the picker', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: COLLECTIONS } }),
      '/api/v1/collections/c-1/documents': jsonResponse({ data: { items: [] } }),
    })
    expect(app.find('[data-test="file-input"]').attributes('accept')).toBe(
      '.md,.markdown,.txt',
    )
    app.unmount()
  })

  it('shows a rejected file type next to the control', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: COLLECTIONS } }),
      '/api/v1/collections/c-1/documents': jsonResponse({ data: { items: [] } }),
    })
    await app.findAll('[data-test="collection-row"]')[0].trigger('click')
    await flushPromises()
    const before = calls.length

    await chooseFile(app, { name: 'plan.pdf', content: 'x' })

    expect(calls).toHaveLength(before)
    expect(app.find('[data-test="upload-error"]').text()).toContain('只支持')
    app.unmount()
  })

  it('disables uploading while there is no collection at all', async () => {
    // With collections present the page selects the first one, so "nothing
    // selected" only happens when there is nothing to select.
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: [] } }),
    })
    expect(knowledge.activeCollectionId.value).toBe('')
    expect(app.find('[data-test="file-input"]').attributes('disabled')).toBeDefined()
    expect(app.find('[data-test="upload-button"]').classes()).toContain('kb__upload--off')
    app.unmount()
  })

  it('exposes the file control to assistive technology', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: COLLECTIONS } }),
      '/api/v1/collections/c-1/documents': jsonResponse({ data: { items: [] } }),
    })
    const input = app.find('[data-test="file-input"]')
    // A visible label bound by `for`, and a named input: not a hidden control
    // reachable only by a JavaScript click.
    expect(input.attributes('id')).toBe('knowledge-file-input')
    expect(input.attributes('aria-label')).toContain('Markdown')
    expect(app.find('[data-test="upload-button"]').attributes('for')).toBe(
      'knowledge-file-input',
    )
    expect(input.attributes('disabled')).toBeUndefined()
    app.unmount()
  })
})

describe('deleting a document', () => {
  it('removes the row after the server confirms', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: COLLECTIONS } }),
      '/api/v1/collections/c-1/documents': documentsFor('c-1', ['甲.md', '乙.md']),
      '/api/v1/documents/c-1-d0': jsonResponse({ data: null, error: null }),
    })
    await app.findAll('[data-test="collection-row"]')[0].trigger('click')
    await flushPromises()
    expect(app.findAll('[data-test="delete-document"]')).toHaveLength(2)

    await app.find('[data-test="delete-document"]').trigger('click')
    await flushPromises()

    expect(app.findAll('.kb__doc-title').map((node) => node.text())).toEqual(['乙.md'])
    app.unmount()
  })

  it('keeps the row when the delete failed', async () => {
    const app = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: COLLECTIONS } }),
      '/api/v1/collections/c-1/documents': documentsFor('c-1', ['甲.md']),
      '/api/v1/documents/c-1-d0': jsonResponse(
        { data: null, error: { code: 'not_found', message: '文档不存在' } },
        { status: 404 },
      ),
    })
    await app.findAll('[data-test="collection-row"]')[0].trigger('click')
    await flushPromises()

    await app.find('[data-test="delete-document"]').trigger('click')
    await flushPromises()

    expect(app.findAll('.kb__doc-title')).toHaveLength(1)
    expect(app.find('[data-test="documents-error"]').text()).toContain('文档不存在')
    app.unmount()
  })
})

describe('the way back to a conversation', () => {
  it('offers a route to chat only once there is something to ground on', async () => {
    const empty = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: [] } }),
    })
    expect(empty.find('[data-test="goto-chat"]').exists()).toBe(false)
    empty.unmount()

    const filled = await mountPage({
      '/api/v1/collections': jsonResponse({ data: { items: COLLECTIONS } }),
      '/api/v1/collections/c-1/documents': jsonResponse({ data: { items: [] } }),
    })
    expect(filled.find('[data-test="goto-chat"]').exists()).toBe(true)
    await filled.find('[data-test="goto-chat"]').trigger('click')
    expect(mocks.push).toHaveBeenCalledWith({ name: 'chat' })
    filled.unmount()
  })
})
