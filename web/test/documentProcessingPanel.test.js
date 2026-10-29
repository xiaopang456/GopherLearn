import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import DocumentProcessingPanel from '../src/components/DocumentProcessingPanel.vue'
import V2DocumentsView from '../src/views/V2DocumentsView.vue'
import { setTokenProvider } from '../src/api/http.js'

const at = (ms) => new Date(Date.UTC(2026, 9, 21, 1, 2, 3, 0) + ms).toISOString()
const stage = (name, ms, label, extra = {}) => ({ stage: name, at: at(ms), label, ...extra })

const READY = {
  mode: 'candidate', status: 'ready', attempts: 2, progress: { completed: 8, total: 8 }, error: null,
  stages: [
    stage('uploaded', 0, '已上传'), stage('queued', 100, '等待处理'),
    stage('retried', 15100, '第 1 次尝试中断，已自动重试', { attempt: 1 }),
    stage('assigned', 15220, '已分配处理节点'), stage('processing', 15700, '处理中'),
    stage('verifying', 16000, '处理完成，校验中'), stage('ready', 16130, '可检索'),
  ],
}
const PROCESSING = {
  mode: 'candidate', status: 'indexing', attempts: 1, progress: { completed: 39, total: 79 }, error: null,
  stages: [stage('uploaded', 0, '已上传'), stage('queued', 120, '等待处理'), stage('assigned', 240, '已分配处理节点'), stage('processing', 400, '处理中')],
}
const FAILED = {
  mode: 'candidate', status: 'failed', attempts: 1, error: { code: 'source_open_failed', label: '源文件读取失败' },
  stages: [stage('uploaded', 0, '已上传'), stage('queued', 50, '等待处理'), stage('failed', 900, '处理失败')],
}

let requests
function serve(...bodies) {
  requests = []
  let n = 0
  globalThis.fetch = vi.fn(async (url, init = {}) => {
    const body = bodies[Math.min(n++, bodies.length - 1)]
    requests.push({ url: String(url), signal: init.signal, headers: init.headers })
    return { ok: true, status: 200, text: async () => JSON.stringify(body) }
  })
}

beforeEach(() => { vi.useFakeTimers(); setTokenProvider(() => 'tok') })
afterEach(() => { vi.useRealTimers(); setTokenProvider(null) })

const mountPanel = (props = {}) => mount(DocumentProcessingPanel, { props: { documentId: 'doc-1', documentStatus: 'indexing', ...props } })
const settle = async () => { await vi.advanceTimersByTimeAsync(0); await flushPromises() }

describe('DocumentProcessingPanel', () => {
  it('shows the stages in order with labels, local clock times and the time since the previous stage', async () => {
    serve(READY)
    const wrapper = mountPanel({ documentStatus: 'ready' })
    await settle()
    const items = wrapper.findAll('[data-test="processing-stages"] li')
    expect(items.map((li) => li.attributes('data-stage'))).toEqual(['uploaded', 'queued', 'retried', 'assigned', 'processing', 'verifying', 'ready'])
    expect(items.map((li) => li.find('.proc__label').text())).toEqual(['已上传', '等待处理', '第 1 次尝试中断，已自动重试', '已分配处理节点', '处理中', '处理完成，校验中', '可检索'])
    expect(items[0].find('.proc__time').text()).toMatch(/^\d{2}:\d{2}:\d{2}\.\d{3}$/)
    expect(items.map((li) => li.find('.proc__delta').text())).toEqual(['', '+0.10s', '+15.00s', '+0.12s', '+0.48s', '+0.30s', '+0.13s'])
    expect(items[2].classes()).toContain('proc__item--retried')
    expect(wrapper.get('[data-test="processing-attempts"]').text()).toBe('共 2 次尝试')
    expect(wrapper.find('[data-test="processing-progress"]').exists()).toBe(false) // finished: no bar
    expect(requests[0].url).toBe('/api/v1/knowledge/documents/doc-1/processing')
    wrapper.unmount()
  })

  it('shows a progress bar while processing and no attempts note for a single attempt', async () => {
    serve(PROCESSING)
    const wrapper = mountPanel()
    await settle()
    const progress = wrapper.get('[data-test="processing-progress"]')
    expect(progress.text()).toContain('39 / 79')
    expect(progress.get('[role="progressbar"]').attributes('aria-valuenow')).toBe('39')
    expect(wrapper.find('[data-test="processing-attempts"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('shows the failure label for a failed document', async () => {
    serve(FAILED)
    const wrapper = mountPanel({ documentStatus: 'failed' })
    await settle()
    expect(wrapper.get('[data-test="processing-failure"]').text()).toBe('源文件读取失败')
    expect(wrapper.find('li[data-stage="failed"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('shows only the mode and status for local processing, and keeps polling until the document is ready', async () => {
    serve({ mode: 'legacy', status: 'indexing' }, { mode: 'legacy', status: 'ready' })
    const wrapper = mountPanel()
    await settle()
    expect(wrapper.get('[data-test="processing-legacy"]').text()).toContain('当前为本地处理模式')
    expect(wrapper.text()).toContain('indexing')
    expect(wrapper.find('[data-test="processing-stages"]').exists()).toBe(false)
    await vi.advanceTimersByTimeAsync(1000)
    expect(requests).toHaveLength(2) // local processing is polled while the document is in progress
    await flushPromises()
    await vi.advanceTimersByTimeAsync(5000)
    expect(requests).toHaveLength(2) // and it stops once the document is ready
    wrapper.unmount()
  })

  it('polls every second while processing, stops once ready, and never shows scheduling detail', async () => {
    serve(PROCESSING, PROCESSING, READY, READY)
    const wrapper = mountPanel()
    await settle()
    expect(requests).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(999)
    expect(requests).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(requests).toHaveLength(2)
    await vi.advanceTimersByTimeAsync(1000)
    expect(requests).toHaveLength(3) // this answer says ready
    await flushPromises()
    await vi.advanceTimersByTimeAsync(10000)
    expect(requests).toHaveLength(3)
    const text = wrapper.text().toLowerCase()
    for (const word of ['node', 'boot', 'execution', 'generation', 'owner', '节点名']) expect(text).not.toContain(word)
    wrapper.unmount()
  })

  it('does one more request when the list reports a new status, then stops', async () => {
    serve(READY)
    const wrapper = mountPanel({ documentStatus: 'indexing' })
    await settle()
    expect(requests).toHaveLength(1)
    await wrapper.setProps({ documentStatus: 'ready' })
    await settle()
    expect(requests).toHaveLength(2)
    await vi.advanceTimersByTimeAsync(5000)
    expect(requests).toHaveLength(2)
    wrapper.unmount()
  })

  it('stops polling and aborts the request in flight when unmounted', async () => {
    serve(PROCESSING)
    const wrapper = mountPanel()
    await settle()
    await vi.advanceTimersByTimeAsync(1000) // second request is now in flight or done
    const last = requests[requests.length - 1]
    wrapper.unmount()
    expect(last.signal.aborted).toBe(true)
    const count = requests.length
    await vi.advanceTimersByTimeAsync(10000)
    expect(requests).toHaveLength(count)
  })
})

describe('the documents page', () => {
  it('does not poll documents whose panel is closed, and polls an opened one', async () => {
    requests = []
    globalThis.fetch = vi.fn(async (url) => {
      requests.push(String(url))
      if (String(url).endsWith('/processing')) return { ok: true, status: 200, text: async () => JSON.stringify(PROCESSING) }
      return { ok: true, status: 200, text: async () => JSON.stringify({ status_code: 1000, documents: [
        { id: 'a', original_name: 'a.md', version: 1, active_version: 0, index_status: 'indexing', file_size: 3 },
        { id: 'b', original_name: 'b.md', version: 1, active_version: 0, index_status: 'indexing', file_size: 3 }] }) }
    })
    const wrapper = mount(V2DocumentsView, { global: { mocks: { $router: {} }, stubs: { SectionFrame: { template: '<div><slot /></div>' } }, provide: {} } })
    await settle()
    await vi.advanceTimersByTimeAsync(5000)
    expect(requests.filter((u) => u.endsWith('/processing'))).toEqual([])
    await wrapper.findAll('[data-test="v2-processing-toggle"]')[1].trigger('click')
    await settle()
    await vi.advanceTimersByTimeAsync(2000)
    const processing = requests.filter((u) => u.endsWith('/processing'))
    expect(processing.length).toBeGreaterThanOrEqual(2)
    expect(new Set(processing)).toEqual(new Set(['/api/v1/knowledge/documents/b/processing']))
    await wrapper.findAll('[data-test="v2-processing-toggle"]')[1].trigger('click') // collapse
    await settle()
    const count = requests.length
    await vi.advanceTimersByTimeAsync(5000)
    expect(requests.length).toBe(count)
    wrapper.unmount()
  })
})
