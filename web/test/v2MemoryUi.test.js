import { beforeEach, afterEach, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { setTokenProvider } from '../src/api/http.js'
import { useV2MemoryOperation } from '../src/composables/useV2MemoryOperation.js'
import V2MemoryView from '../src/views/V2MemoryView.vue'
import { jsonResponse } from './helpers/transport.js'
let calls, response
const memory = { id: '900719925474099312345', subject: '个人理解', content: '队列比喻', scope: 'topic', topic_key: 'redis', durability: 'stable', status: 'active', tags: ['redis'], user_confirmed: true }
beforeEach(() => {
  calls = []; response = () => ({ status_code: 1000, memory, memories: [memory], pendingDrafts: [], revisions: [{ id: 'actual-revision', reason: '用户决定' }] })
  setTokenProvider(() => 'synthetic-only')
  vi.stubGlobal('fetch', async (url, init) => { calls.push({ url: String(url), init }); return jsonResponse(response(url, init)) })
})
afterEach(() => vi.unstubAllGlobals())
async function fill(wrapper) {
  for (const [field, value] of Object.entries({ scope: 'topic', subject: '个人理解', content: '队列比喻', 'topic-key': 'redis', durability: 'stable', tags: 'redis', reason: '用户明确填写' })) await wrapper.get(`[data-test=${field}]`).setValue(value)
}
it('starts without implicit scope/durability, submits actual fields, keeps old clean boundary and 100 limits', async () => {
  const wrapper = mount(V2MemoryView); await flushPromises()
  expect(wrapper.get('[data-test=scope]').element.value).toBe('')
  expect(wrapper.get('[data-test=durability]').element.value).toBe('')
  expect(wrapper.text()).toContain('各最多 100 条')
  expect(wrapper.text()).toContain('旧 clean')
  await fill(wrapper); await wrapper.get('[data-test=v2-create-form]').trigger('submit'); await flushPromises()
  const request = JSON.parse(calls.find((c) => c.init.method === 'POST').init.body)
  expect(request).toMatchObject({ scope: 'topic', subject: '个人理解', content: '队列比喻', topic_key: 'redis', durability: 'stable', reason: '用户明确填写', tags: ['redis'] })
  expect(request.idempotency_key).toMatch(/^v2-memory-/)
  for (const field of ['decision','valid_until','user_id','userName']) expect(request).not.toHaveProperty(field)
  expect(wrapper.get('[data-test=v2-operation-result]').text()).toContain('active')
  wrapper.unmount()
})
it('explicit retry reuses immutable key/body; changed form/new operation get a new key', async () => {
  const wrapper = mount(V2MemoryView); await flushPromises(); await fill(wrapper)
  response = (url, init) => init.method === 'POST' ? { status_code: 1002, status_msg: '原错误' } : { status_code: 1000, memories: [], pendingDrafts: [] }
  await wrapper.get('[data-test=v2-create-form]').trigger('submit'); await flushPromises()
  const first = calls.find((c) => c.init.method === 'POST').init.body
  await wrapper.get('[data-test=content]').setValue('新输入不能改旧请求')
  await wrapper.get('[data-test=v2-retry]').trigger('click'); await flushPromises()
  expect(calls.filter((c) => c.init.method === 'POST')[1].init.body).toBe(first)
  await wrapper.get('[data-test=v2-create-form]').trigger('submit'); await flushPromises()
  const next = JSON.parse(calls.filter((c) => c.init.method === 'POST')[2].init.body)
  expect(next.content).toBe('新输入不能改旧请求')
  expect(next.idempotency_key).not.toBe(JSON.parse(first).idempotency_key)
  expect(wrapper.text()).toContain('原错误'); wrapper.unmount()
})
it('conflict does not choose a winner or implicit target', async () => {
  const wrapper = mount(V2MemoryView); await flushPromises(); await fill(wrapper)
  response = () => ({ status_code: 1000, conflicts: [memory], memories: [], pendingDrafts: [] })
  await wrapper.get('[data-test=v2-create-form]').trigger('submit'); await flushPromises()
  expect(calls.filter((c) => c.init.method === 'POST')).toHaveLength(1)
  expect(wrapper.get('[data-test=create-decision]').element.value).toBe('')
  await wrapper.get('[data-test=create-decision]').setValue('replace')
  expect(wrapper.get('[data-test=create-target]').element.value).toBe('')
  await wrapper.get('[data-test=create-target]').setValue(memory.id)
  await wrapper.get('[data-test=v2-conflicts] button').trigger('click'); await flushPromises()
  const posts = calls.filter((c) => c.init.method === 'POST').map((c) => JSON.parse(c.init.body))
  expect(posts[1]).toMatchObject({ decision: 'replace', target_memory_id: memory.id, content: posts[0].content })
  expect(posts[1].idempotency_key).not.toBe(posts[0].idempotency_key); wrapper.unmount()
})
it('uses original correction/archive/delete/revision fields with string identities', async () => {
  const wrapper = mount(V2MemoryView); await flushPromises()
  await wrapper.get('[data-test=v2-revisions]').trigger('click'); await flushPromises()
  expect(wrapper.get('[data-test=v2-revision-result]').text()).toContain('actual-revision')
  await wrapper.get('[data-test=v2-edit]').trigger('click')
  await wrapper.get('[aria-label="修改 reason"]').setValue('用户修正')
  await wrapper.get('[data-test=v2-edit-form]').trigger('submit'); await flushPromises()
  const correction = calls.find((c) => c.init.method === 'PATCH')
  expect(correction.url).toContain(memory.id)
  expect(JSON.parse(correction.init.body)).toEqual({ subject: memory.subject, content: memory.content, tags: ['redis'], reason: '用户修正' })
  await wrapper.get('[data-test=manage-reason]').setValue('用户明确操作')
  await wrapper.get('[data-test=v2-archive]').trigger('click'); await flushPromises()
  await wrapper.get('[data-test=v2-delete]').trigger('click'); await flushPromises()
  expect(calls.find((c) => c.init.method === 'DELETE').url).toContain(memory.id)
  wrapper.unmount()
})
it('duplicate clicks, cancellation and stale responses do not publish success; retry stays explicit', async () => {
  const op = useV2MemoryOperation(); let complete
  vi.stubGlobal('fetch', () => new Promise((resolve) => { complete = resolve }))
  const running = op.start('create', { scope: 'topic', content: '旧请求' })
  expect(await op.start('create', { content: '重复点击' })).toBeNull()
  const oldKey = op.pending.value.body.idempotency_key
  op.cancel(); complete(jsonResponse({ status_code: 1000, memory }))
  expect(await running).toBeNull(); expect(op.result.value).toBeNull()
  expect(op.error.value.code).toBe('AbortError')
  vi.stubGlobal('fetch', async () => jsonResponse({ status_code: 1000, memory }))
  await op.retry(); expect(op.pending.value.body.idempotency_key).toBe(oldKey)
})
it('time_bound/global are explicit, valid_until is exact, and length counts Unicode runes', async () => {
  const wrapper = mount(V2MemoryView); await flushPromises(); await fill(wrapper)
  await wrapper.get('[data-test=scope]').setValue('global')
  await wrapper.get('[data-test=topic-key]').setValue('')
  await wrapper.get('[data-test=durability]').setValue('time_bound')
  const deadline = '2030-01-01T00:00:00+08:00'
  await wrapper.get('[data-test=valid-until]').setValue(deadline)
  await wrapper.get('[data-test=content]').setValue('🙂'.repeat(400))
  await wrapper.get('[data-test=v2-create-form]').trigger('submit'); await flushPromises()
  const sent = JSON.parse(calls.find((c) => c.init.method === 'POST').init.body)
  expect(sent).toMatchObject({ scope: 'global', durability: 'time_bound', valid_until: deadline, content: '🙂'.repeat(400) })
  expect(sent).not.toHaveProperty('topic_key')
  await wrapper.get('[data-test=content]').setValue('🙂'.repeat(401))
  await wrapper.get('[data-test=v2-create-form]').trigger('submit'); await flushPromises()
  expect(calls.filter((c) => c.init.method === 'POST')).toHaveLength(1)
  expect(wrapper.text()).toContain('content 超过原 400 字上限')
  wrapper.unmount()
})
