import { beforeEach, afterEach, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import { setTokenProvider } from '../src/api/http.js'
import { nativeDocumentRequest, uploadV2Document, updateV2Document, retryV2Document,
  reindexV2Document, deleteV2Document, readV2Source, V2_MAX_FILE_BYTES } from '../src/api/v2Documents.js'
import V2DocumentsView from '../src/views/V2DocumentsView.vue'
import V2SourceView from '../src/views/V2SourceView.vue'
import CitationList from '../src/components/CitationList.vue'
import { normalizeSourceList } from '../src/lib/sources.js'
import { jsonResponse } from './helpers/transport.js'
let calls
const document = { id: 'original-doc', original_name: '笔记.md', version: 2, active_version: 1,
  index_status: 'failed', file_size: 123, error_message: '原索引错误' }
beforeEach(() => {
  calls = []
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'synthetic-only')
  vi.stubGlobal('fetch', async (url, init = {}) => {
    calls.push({ url: String(url), init })
    return jsonResponse({ status_code: 1000, documents: [document], document })
  })
})
afterEach(() => { vi.unstubAllGlobals(); vi.unstubAllEnvs() })

it('native multipart keeps the original file/name and does not carry Collection or invented CAS', async () => {
  const file = new File(['原内容'], '原名.md', { type: 'text/markdown' })
  await uploadV2Document(file); await updateV2Document('original-doc', file)
  expect(V2_MAX_FILE_BYTES).toBe(5 << 20)
  for (const call of calls) {
    expect(call.init.body).toBeInstanceOf(FormData)
    expect(call.init.body.get('file').name).toBe('原名.md')
    expect([...call.init.body.keys()]).toEqual(['file'])
    expect(call.init.headers['content-type']).toBeUndefined()
    expect(call.init.headers.authorization).toBe('Bearer synthetic-only')
    expect(call.url).toContain('/api/v1/knowledge/documents')
    expect(call.url).not.toContain('collections')
  }
  expect(calls.map((c) => c.init.method)).toEqual(['POST', 'PUT'])
})

it('retry/reindex/delete preserve native status/error and never simulate success', async () => {
  await retryV2Document('original-doc'); await reindexV2Document('original-doc'); await deleteV2Document('original-doc')
  expect(calls.map((c) => new URL(c.url).pathname)).toEqual([
    '/api/v1/knowledge/documents/original-doc/retry', '/api/v1/knowledge/documents/original-doc/reindex', '/api/v1/knowledge/documents/original-doc',
  ])
  vi.stubGlobal('fetch', async () => jsonResponse({ status_code: 7010, status_msg: '原 busy' }, { status: 409 }))
  await expect(retryV2Document('original-doc')).rejects.toMatchObject({ status: 409, nativeCode: 7010, code: 'v2_7010' })
  vi.stubGlobal('fetch', async () => jsonResponse({ status_code: 7002, status_msg: '超出原上限' }, { status: 413 }))
  await expect(uploadV2Document(new File(['x'], 'a.txt'))).rejects.toMatchObject({ status: 413, nativeCode: 7002 })
  vi.stubGlobal('fetch', async () => jsonResponse({ status_code: 7009, status_msg: '不可 retry' }))
  await expect(nativeDocumentRequest('/api/v1/knowledge/documents')).rejects.toMatchObject({ nativeCode: 7009 })
})

it('precise source request keeps original document/version/range without snippet fallback', async () => {
  vi.stubGlobal('fetch', async (url, init) => { calls.push({ url: String(url), init }); return jsonResponse({ data: { document_id: 'original-doc', document_version: 7, lines: [{ number: 2, text: '原文' }] }, error: null }) })
  const source = await readV2Source('original-doc', '7', '2', '3')
  const url = new URL(calls[0].url)
  expect(url.searchParams.get('version')).toBe('7')
  expect(url.searchParams.get('start_line')).toBe('2')
  expect(url.searchParams.get('end_line')).toBe('3')
  expect(source.document_version).toBe(7)
  vi.stubGlobal('fetch', async () => jsonResponse({ data: null, error: { code: 'document_version_not_active', message: 'version unavailable' } }, { status: 409 }))
  await expect(readV2Source('original-doc', '1', '2', '3')).rejects.toMatchObject({ code: 'document_version_not_active', status: 409 })
})

it('document screen shows real failed/version state and Collection boundary; retry invokes original route', async () => {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/', component: V2DocumentsView }] })
  await router.push('/'); await router.isReady()
  const view = mount(V2DocumentsView, { global: { plugins: [router] } }); await flushPromises()
  // The page speaks to the person, not to the contract: no Collection wording,
  // a Chinese status and a Chinese version label. The boundary itself is
  // still enforced above, in the multipart request.
  expect(view.text()).toContain('文档')
  expect(view.text()).not.toContain('Collection')
  expect(view.get('[data-test="v2-status"]').text()).toContain('失败')
  expect(view.text()).toContain('版本 1')
  const retry = view.findAll('button').find((b) => b.text() === '重试')
  await retry.trigger('click'); await flushPromises()
  expect(calls.some((c) => c.url.endsWith('/retry'))).toBe(true)
  expect(view.get('[data-test="v2-status"]').text()).toContain('失败')
  expect(view.get('[role="status"]').text()).not.toContain('索引完成')
  view.unmount()
})

it('citation navigates original version/lines and source screen never substitutes latest', async () => {
  const router = createRouter({ history: createMemoryHistory(), routes: [
    { path: '/', component: { template: '<div />' } },
    { path: '/source/:documentId', name: 'v2-source', component: V2SourceView },
  ] })
  await router.push('/'); await router.isReady()
  const citations = mount(CitationList, { props: { sources: normalizeSourceList([{ citation: 1, doc_id: 'original-doc', doc_version: 7, file_name: '原文', start_line: 2, end_line: 3, snippet: '不能充当原文' }]) }, global: { plugins: [router] } })
  await citations.get('[data-test="citation-toggle"]').trigger('click')
  await citations.get('[data-test="read-v2-source"]').trigger('click'); await flushPromises()
  expect(router.currentRoute.value.params.documentId).toBe('original-doc')
  expect(router.currentRoute.value.query).toEqual({ version: '7', start_line: '2', end_line: '3' })
  vi.stubGlobal('fetch', async () => jsonResponse({ data: null, error: { code: 'document_source_missing', message: 'missing' } }, { status: 404 }))
  const sourceView = mount(V2SourceView, { global: { plugins: [router] } }); await flushPromises()
  expect(sourceView.text()).toContain('document_source_missing')
  expect(sourceView.text()).not.toContain('不能充当原文')
  sourceView.unmount(); citations.unmount()
})
