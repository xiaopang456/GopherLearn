import { beforeEach, afterEach, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { setTokenProvider } from '../src/api/http.js'
import V2MemoryDraftCard from '../src/components/V2MemoryDraftCard.vue'
import MemoryDraftCard from '../src/components/MemoryDraftCard.vue'
import MessageBubble from '../src/components/MessageBubble.vue'
import { jsonResponse } from './helpers/transport.js'
const draft = { draftId: 'actual-draft-uuid', scope: 'topic', topicKey: 'redis', subject: '理解', content: '队列比喻', durability: 'stable', conflictIds: ['900719925474099312345'] }
let calls, reply
beforeEach(() => {
  calls = []
  reply = () => ({ status_code: 1000, draft: { id: draft.draftId, status: 'confirmed' }, memory: { id: 'actual-memory-uuid', status: 'active' }, revision: { id: 'actual-revision' } })
  setTokenProvider(() => 'synthetic-only')
  vi.stubGlobal('fetch', async (url, init) => { calls.push({ url: String(url), init }); return jsonResponse(reply()) })
})
afterEach(() => vi.unstubAllGlobals())
async function reason(wrapper, value = '用户明确决定') { await wrapper.get('[data-test=v2-draft-reason]').setValue(value) }
it('requires explicit reason/target; sends original add/replace/cancel and large identities', async () => {
  const wrapper = mount(V2MemoryDraftCard, { props: { draft } })
  expect(wrapper.get('[data-test=v2-draft-add]').element.disabled).toBe(true)
  await reason(wrapper)
  expect(wrapper.get('[data-test=v2-draft-replace]').element.disabled).toBe(true)
  expect(wrapper.get('[data-test=v2-draft-target]').element.value).toBe('')
  await wrapper.get('[data-test=v2-draft-target]').setValue(draft.conflictIds[0])
  await wrapper.get('[data-test=v2-draft-replace]').trigger('click'); await flushPromises()
  expect(JSON.parse(calls[0].init.body)).toEqual({ action: 'replace', reason: '用户明确决定', target_memory_id: draft.conflictIds[0] })
  expect(calls[0].url).toContain(draft.draftId)
  await wrapper.get('[data-test=v2-draft-add]').trigger('click'); await flushPromises()
  expect(JSON.parse(calls[1].init.body).action).toBe('add')
  wrapper.unmount()
})
it('confirmed then cancel displays original confirmed/replay instead of rejected/archived', async () => {
  const wrapper = mount(V2MemoryDraftCard, { props: { draft } }); await reason(wrapper)
  await wrapper.get('[data-test=v2-draft-add]').trigger('click'); await flushPromises()
  reply = () => ({ status_code: 1000, draft: { id: draft.draftId, status: 'confirmed' }, memory: { id: 'actual-memory-uuid', status: 'active' }, replay: true })
  await wrapper.get('[data-test=v2-draft-cancel]').trigger('click'); await flushPromises()
  expect(JSON.parse(calls[1].init.body).action).toBe('cancel')
  expect(wrapper.get('[data-test=v2-draft-outcome]').text()).toContain('draft.status=confirmed')
  expect(wrapper.get('[data-test=v2-draft-outcome]').text()).toContain('replay=true')
  expect(wrapper.text()).not.toContain('status=archived'); wrapper.unmount()
})
it('cancelled and deleted replay never claim active', async () => {
  const wrapper = mount(V2MemoryDraftCard, { props: { draft } })
  reply = () => ({ status_code: 1000, draft: { id: draft.draftId, status: 'cancelled' } })
  await wrapper.get('[data-test=v2-draft-cancel]').trigger('click'); await flushPromises()
  expect(wrapper.get('[data-test=v2-draft-outcome]').text()).toContain('draft.status=cancelled')
  expect(wrapper.find('[data-test=v2-active-confirmation]').exists()).toBe(false)
  reply = () => ({ status_code: 1000, draft: { id: draft.draftId, status: 'confirmed' }, memory: { id: 'actual-memory-uuid', status: 'deleted' }, replay: true })
  await reason(wrapper); await wrapper.get('[data-test=v2-draft-add]').trigger('click'); await flushPromises()
  expect(wrapper.get('[data-test=v2-draft-memory-state]').text()).toContain('status=deleted')
  expect(wrapper.find('[data-test=v2-active-confirmation]').exists()).toBe(false); wrapper.unmount()
})
it.each([404, 409, 500])('original HTTP %i, missing/cross-user/conflict/expiry never publish success', async (status) => {
  const wrapper = mount(V2MemoryDraftCard, { props: { draft } }); await reason(wrapper)
  vi.stubGlobal('fetch', async () => jsonResponse({ status_code: 5009, status_msg: '原公开错误' }, { status }))
  await wrapper.get('[data-test=v2-draft-add]').trigger('click'); await flushPromises()
  expect(wrapper.text()).toContain('原公开错误')
  expect(wrapper.find('[data-test=v2-draft-outcome]').exists()).toBe(false)
  await reason(wrapper, '编辑不会改旧请求')
  vi.stubGlobal('fetch', async (url, init) => { calls.push({ url, init }); return jsonResponse({ status_code: 1000, draft: { id: draft.draftId, status: 'cancelled' } }) })
  await wrapper.get('[data-test=v2-draft-retry]').trigger('click'); await flushPromises()
  expect(JSON.parse(calls[0].init.body).reason).toBe('用户明确决定'); wrapper.unmount()
})
it('duplicate clicks/cancel and changed card ignore old response and reset old input', async () => {
  const wrapper = mount(V2MemoryDraftCard, { props: { draft, runId: 'first-run' } }); await reason(wrapper)
  let complete
  vi.stubGlobal('fetch', (url, init) => { calls.push({ url, init }); return new Promise((resolve) => { complete = resolve }) })
  await wrapper.get('[data-test=v2-draft-add]').trigger('click')
  await wrapper.get('[data-test=v2-draft-add]').trigger('click')
  expect(calls).toHaveLength(1)
  await wrapper.get('[data-test=v2-draft-abort]').trigger('click')
  complete(jsonResponse(reply())); await flushPromises()
  expect(wrapper.find('[data-test=v2-draft-outcome]').exists()).toBe(false)
  await wrapper.setProps({ draft: { ...draft, draftId: 'other-card' }, runId: 'other-run' })
  expect(wrapper.get('[data-test=v2-draft-reason]').element.value).toBe('')
  expect(wrapper.find('[data-test=v2-draft-retry]').exists()).toBe(false); wrapper.unmount()
})
it('response from previous card while request in flight cannot affect the next card', async () => {
  const wrapper = mount(V2MemoryDraftCard, { props: { draft } }); await reason(wrapper)
  let complete
  vi.stubGlobal('fetch', () => new Promise((resolve) => { complete = resolve }))
  await wrapper.get('[data-test=v2-draft-add]').trigger('click')
  await wrapper.setProps({ draft: { ...draft, draftId: 'another-draft' } })
  complete(jsonResponse(reply())); await flushPromises()
  expect(wrapper.find('[data-test=v2-draft-outcome]').exists()).toBe(false); wrapper.unmount()
})
it('native RunResult/event provenance uses V2 card; history without draft never creates one; clean card remains separate', async () => {
  const message = { id: 'local', role: 'assistant', content: '真实正文', status: 'done', originalV2: { memoryDraft: draft }, memoryDraft: draft }
  const wrapper = mount(MessageBubble, { props: { message } })
  expect(wrapper.findComponent(V2MemoryDraftCard).exists()).toBe(true)
  expect(wrapper.findComponent(MemoryDraftCard).exists()).toBe(false)
  await wrapper.setProps({ message: { ...message, originalV2: null, memoryContract: 'v2' } })
  expect(wrapper.findComponent(V2MemoryDraftCard).exists()).toBe(true)
  await wrapper.setProps({ message: { id: 'history', role: 'assistant', content: '只有历史正文', status: 'done' } })
  expect(wrapper.findComponent(V2MemoryDraftCard).exists()).toBe(false)
  await wrapper.setProps({ message: { id: 'clean', role: 'assistant', content: 'clean 正文', status: 'done', memoryDraft: draft } })
  expect(wrapper.findComponent(MemoryDraftCard).exists()).toBe(true)
  expect(wrapper.findComponent(V2MemoryDraftCard).exists()).toBe(false); wrapper.unmount()
})
