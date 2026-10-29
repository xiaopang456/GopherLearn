import { beforeEach, afterEach, expect, it, vi } from 'vitest'
import { setTokenProvider } from '../src/api/http.js'
import { createV2Memory, listV2Memory, correctV2Memory, archiveV2Memory, deleteV2Memory, revisionsV2Memory, resolveV2Draft } from '../src/api/v2Memory.js'
import { jsonResponse } from './helpers/transport.js'
let calls
beforeEach(() => {
  calls = []
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'synthetic-only')
  vi.stubGlobal('fetch', async (url, init) => { calls.push({ url: String(url), init }); return jsonResponse({ status_code: 1000, memories: [], pendingDrafts: [] }) })
})
afterEach(() => { vi.unstubAllGlobals(); vi.unstubAllEnvs() })

it('sends explicit native handwritten fields without clean state/defaults or identity', async () => {
  const body = { scope: 'topic', topic_key: 'redis', subject: '理解', content: '队列比喻', durability: 'time_bound', valid_until: '2030-01-01T00:00:00Z', reason: '明确填写', idempotency_key: 'manual-001' }
  await createV2Memory(body)
  expect(JSON.parse(calls[0].init.body)).toEqual(body)
  expect(calls[0].url).toBe('https://api.test/api/v1/AI/agent/memories')
  expect(calls[0].init.headers.authorization).toBe('Bearer synthetic-only')
})

it('preserves original scope and status without mixing clean archived/cancelled', async () => {
  await listV2Memory({ scope: 'topic', status: 'deleted' })
  expect(calls[0].url).toContain('?scope=topic&status=deleted')
  await listV2Memory()
  expect(calls[1].url).not.toContain('?')
})

it('preserves large string identities and original explicit actions/reasons', async () => {
  const id = '900719925474099312345'
  await correctV2Memory(id, { subject: '理解', content: '正文', tags: ['redis'], reason: '修正' })
  await archiveV2Memory(id, '归档'); await deleteV2Memory(id, '删除'); await revisionsV2Memory(id)
  await resolveV2Draft(id, { action: 'replace', target_memory_id: id, reason: '用户选择原目标' })
  for (const call of calls) expect(call.url).toContain(id)
  expect(calls.map((call) => call.init.method)).toEqual(['PATCH', 'POST', 'DELETE', 'GET', 'POST'])
  expect(JSON.parse(calls[4].init.body)).toEqual({ action: 'replace', target_memory_id: id, reason: '用户选择原目标' })
})

it('does not mark native business error, HTTP conflict or cancellation as success', async () => {
  vi.stubGlobal('fetch', async () => jsonResponse({ status_code: 2006, status_msg: '认证失效' }))
  await expect(listV2Memory()).rejects.toMatchObject({ code: 'unauthorized', nativeCode: 2006 })
  vi.stubGlobal('fetch', async () => jsonResponse({ status_code: 5012, status_msg: '原冲突' }, { status: 409 }))
  await expect(resolveV2Draft('draft', { action: 'add', reason: '确认' })).rejects.toMatchObject({ status: 409, nativeCode: 5012 })
  const aborted = new DOMException('cancelled', 'AbortError')
  vi.stubGlobal('fetch', async () => { throw aborted })
  await expect(createV2Memory({})).rejects.toBe(aborted)
})
