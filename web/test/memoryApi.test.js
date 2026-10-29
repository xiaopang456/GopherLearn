import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  archiveMemory,
  assertMemoryAdapter,
  confirmDraft,
  addMemory,
  createHttpMemoryAdapter,
  createUnboundMemoryAdapter,
  isMockMemoryAdapter,
  listDrafts,
  listMemories,
  MANUAL_MEMORY_SCOPES,
  MEMORY_CREATE_LIMITS,
  MEMORY_ERROR_COPY,
  MEMORY_FORM_COPY,
  MEMORY_PATH,
  memoryActionPath,
  memoryAdapterName,
  memoryAddBody,
  memoryError,
  memoryListPath,
  MemoryAdapterMode,
  MemoryListState,
  MemoryScope,
  MemoryStatus,
  normalizeDraft,
  normalizeDraftList,
  normalizeMemoryList,
  normalizeMemoryResponse,
  normalizeScope,
  normalizeStatus,
  rejectDraft,
  resetMemoryAdapter,
  resolveMemoryAdapterMode,
  scopeChoiceLabel,
  scopeLabel,
  setMemoryAdapter,
  UNBOUND_CODE,
  validateMemoryForm,
} from '../src/api/memory.js'
import { createMockMemoryAdapter } from '../src/api/memoryMock.js'
import { setTokenProvider } from '../src/api/http.js'
import { installFetchStub, jsonResponse } from './helpers/transport.js'
import { clearDevIssues, devIssues } from '../src/lib/devlog.js'

/**
 * The memory API module is the only place that knows what a memory record looks
 * like, so these cases are the fence around that knowledge: the scope vocabulary,
 * the response shapes the components are allowed to receive, and — most
 * importantly — the failure copy, which must never be the server's own words.
 */

let fetchSpy

beforeEach(() => {
  clearDevIssues()
  resetMemoryAdapter()
  fetchSpy = vi.fn(() => {
    throw new Error('this module must not reach the network')
  })
  globalThis.fetch = fetchSpy
})

describe('the scope vocabulary', () => {
  it('names the three scopes the page has to tell apart', () => {
    expect(scopeLabel(MemoryScope.Global)).toBe('全局画像')
    expect(scopeLabel(MemoryScope.Topic)).toBe('主题记忆')
    expect(scopeLabel(MemoryScope.Episodic)).toBe('情景记忆')
  })

  it('accepts case and padding but never invents a scope', () => {
    expect(normalizeScope('  GLOBAL ')).toBe(MemoryScope.Global)
    expect(normalizeScope('episodic')).toBe(MemoryScope.Episodic)
    expect(normalizeScope('session_wide')).toBe('')
    expect(scopeLabel('')).toBe('类型未识别')
  })

  it('records an unrecognised scope where a developer can find it', () => {
    normalizeScope('mastery')
    expect(devIssues().map((entry) => entry.kind)).toContain('memory.scope.unknown')
  })

  it('treats an unknown status as "not active" rather than as active', () => {
    expect(normalizeStatus(MemoryStatus.Archived)).toBe('archived')
    expect(normalizeStatus('pending_review')).toBe('')
    expect(normalizeStatus('')).toBe('')
  })
})

describe('normalizeDraft', () => {
  it('reads the server shape, not a convenience shape', () => {
    const draft = normalizeDraft({
      id: 'drf-1',
      scope: 'topic',
      topic_key: 'ctx-cancel',
      content: '取消传播还要再看一次。',
      confidence: 0.64,
      sources: [{ session_id: 'ses-9', message_ids: ['msg-1', 'msg-2'] }],
      created_at: '2026-09-25T09:00:00Z',
    })

    expect(draft).toEqual({
      id: 'drf-1',
      scope: 'topic',
      topicKey: 'ctx-cancel',
      content: '取消传播还要再看一次。',
      confidence: 0.64,
      sources: [{ sessionId: 'ses-9', messageIds: ['msg-1', 'msg-2'] }],
      createdAt: '2026-09-25T09:00:00Z',
    })
  })

  it('accepts the camelCase spelling too', () => {
    const draft = normalizeDraft({
      draftId: 'drf-2',
      scope: 'Global',
      topicKey: 'a-key',
      confidence: '0.5',
      sources: [{ sessionId: 'ses-1', messageId: 'msg-7' }],
      createdAt: '2026-09-25T09:00:00Z',
    })

    expect(draft.id).toBe('drf-2')
    expect(draft.scope).toBe('global')
    expect(draft.confidence).toBe(0.5)
    expect(draft.sources).toEqual([{ sessionId: 'ses-1', messageIds: ['msg-7'] }])
  })

  it('keeps "no confidence given" distinct from zero', () => {
    expect(normalizeDraft({ id: 'a' }).confidence).toBeNull()
    expect(normalizeDraft({ id: 'a', confidence: null }).confidence).toBeNull()
    expect(normalizeDraft({ id: 'a', confidence: 0 }).confidence).toBe(0)
    expect(normalizeDraft({ id: 'a', confidence: 'high' }).confidence).toBeNull()
  })

  it('refuses to turn an out-of-range confidence into a number to display', () => {
    expect(normalizeDraft({ id: 'a', confidence: 1.4 }).confidence).toBeNull()
    expect(normalizeDraft({ id: 'a', confidence: -0.2 }).confidence).toBeNull()
  })

  it('drops provenance entries that carry neither a session nor a message', () => {
    const draft = normalizeDraft({
      id: 'a',
      sources: [{}, { session_id: '' }, null, { session_id: 'ses-2' }],
    })
    expect(draft.sources).toEqual([{ sessionId: 'ses-2', messageIds: [] }])
  })

  it('survives a body that is not an object at all', () => {
    expect(normalizeDraft(null).id).toBe('')
    expect(normalizeDraft('nonsense').content).toBe('')
    expect(normalizeDraftList(undefined)).toEqual([])
    expect(normalizeMemoryList({ items: null })).toEqual([])
  })

  it('reads the flat provenance the endpoint actually sends', () => {
    const draft = normalizeDraft({
      memory_id: 'drf-flat',
      scope: 'episodic',
      content: 'x',
      source_session_id: 'ses-42',
      source_message_ids: ['msg-1', '', 'msg-2'],
    })

    expect(draft.id).toBe('drf-flat')
    expect(draft.sources).toEqual([{ sessionId: 'ses-42', messageIds: ['msg-1', 'msg-2'] }])
  })

  it('reads an empty session and no messages as no provenance at all', () => {
    const draft = normalizeDraft({
      memory_id: 'drf-bare',
      source_session_id: '',
      source_message_ids: [],
    })
    expect(draft.sources).toEqual([])
  })

  it('keeps confidence out of the record when it is not a probability', () => {
    expect(normalizeDraft({ memory_id: 'a', confidence: 1.4 }).confidence).toBeNull()
    expect(normalizeDraft({ memory_id: 'a', confidence: 'high' }).confidence).toBeNull()
    expect(normalizeDraft({ memory_id: 'a', confidence: 0 }).confidence).toBe(0)
  })

  it('drops a record with no id, because nothing can be addressed by it', () => {
    const list = normalizeDraftList({
      items: [{ content: '没有 id' }, { id: 'drf-9', content: '有 id' }],
    })
    expect(list.map((item) => item.id)).toEqual(['drf-9'])
  })
})

describe('write responses', () => {
  it('unwraps a single object, a bare array and an items list alike', () => {
    const row = { id: 'mem-1', scope: 'global', status: 'active' }
    expect(normalizeMemoryResponse(row).id).toBe('mem-1')
    expect(normalizeMemoryResponse([row]).id).toBe('mem-1')
    expect(normalizeMemoryResponse({ items: [row] }).id).toBe('mem-1')
    expect(normalizeMemoryResponse({ items: [{ content: 'x' }] })).toBeNull()
    expect(normalizeMemoryResponse(null)).toBeNull()
  })
})

describe('failure copy', () => {
  const cases = [
    [{ code: 'unauthorized', status: 401, message: 'token has expired' }, 'unauthorized'],
    [{ code: 'forbidden', status: 403, message: 'belongs to user 42' }, 'crossAccount'],
    [{ code: 'cross_user', status: 400, message: 'not yours' }, 'crossAccount'],
    [{ code: 'not_found', status: 404, message: 'row missing' }, 'notFound'],
    [{ code: 'invalid_request', status: 400, message: 'scope required' }, 'invalidRequest'],
    [{ code: 'already_processed', status: 409, message: 'twice' }, 'conflict'],
    [{ code: 'internal_error', status: 500, message: 'redis: -ERR' }, 'unavailable'],
    [{ code: 'network_error', status: 0, message: 'Failed to fetch' }, 'network'],
    [{ code: UNBOUND_CODE, status: 503, message: 'no adapter' }, 'notWired'],
  ]

  it.each(cases)('maps %j onto one owned sentence', (error, key) => {
    const mapped = memoryError(error)
    expect(mapped.copy).toBe(MEMORY_ERROR_COPY[key])
    expect(typeof mapped.code).toBe('string')
  })

  it('never repeats the server sentence, even for a status it understands', () => {
    const mapped = memoryError({
      code: 'invalid_request',
      status: 400,
      message: 'column memory_drafts.topic_key violates not-null constraint',
    })
    expect(mapped.copy).not.toContain('topic_key')
    expect(mapped.copy).toBe(MEMORY_ERROR_COPY.invalidRequest)
  })

  it('reads an unknown failure as the service sentence with a machine code', () => {
    const mapped = memoryError({ code: 'teapot', status: 418, message: 'nope' })
    expect(mapped.copy).toBe(MEMORY_ERROR_COPY.unavailable)
    expect(mapped.code).toBe('teapot')
  })

  it('reads a missing error as the service sentence rather than as nothing', () => {
    expect(memoryError(undefined).copy).toBe(MEMORY_ERROR_COPY.unavailable)
    expect(memoryError({}).code).toBe('http_0')
  })

  /**
   * The server keeps five lifecycle answers distinct at 409 because each one has a
   * different next move. Collapsing them here would undo that work at the last
   * step, so each code must reach the screen with its own sentence.
   */
  const lifecycle = [
    ['memory_already_confirmed', MEMORY_ERROR_COPY.alreadyConfirmed],
    ['memory_archived', MEMORY_ERROR_COPY.archivedRow],
    ['memory_superseded', MEMORY_ERROR_COPY.supersededRow],
    ['memory_not_active', MEMORY_ERROR_COPY.notActiveRow],
    ['memory_evidence_missing', MEMORY_ERROR_COPY.evidenceMissing],
  ]

  it.each(lifecycle)('keeps %s apart from the other conflicts', (code, copy) => {
    const mapped = memoryError({ code, status: 409, message: 'lifecycle detail' })
    expect(mapped.copy).toBe(copy)
    expect(mapped.code).toBe(code)
  })

  it('keeps the five lifecycle sentences apart from each other', () => {
    const copies = lifecycle.map(([code]) => memoryError({ code, status: 409 }).copy)
    expect(new Set(copies).size).toBe(lifecycle.length)
    expect(copies).not.toContain(MEMORY_ERROR_COPY.conflict)
  })

  it('falls back to the shared conflict sentence for a 409 it does not know', () => {
    expect(memoryError({ code: 'version_mismatch', status: 409 }).copy).toBe(
      MEMORY_ERROR_COPY.conflict,
    )
  })
})

describe('paths and the adapter seam', () => {
  it('asks each listing by state, because the endpoint has no default', () => {
    expect(memoryListPath(MemoryListState.Pending)).toBe(`${MEMORY_PATH}?state=pending`)
    expect(memoryListPath(MemoryListState.Active)).toBe(`${MEMORY_PATH}?state=active`)
  })

  it('refuses to build a listing for a state this API does not offer', () => {
    expect(() => memoryListPath('archived')).toThrow(/只接受 pending 或 active/)
    expect(() => memoryListPath(undefined)).toThrow(/只接受/)
    expect(devIssues().map((entry) => entry.kind)).toContain('memory.list_state.unknown')
  })

  it('puts the verb in the path and escapes the id', () => {
    expect(memoryActionPath('drf 1', 'confirm')).toBe(`${MEMORY_PATH}/drf%201/confirm`)
    // An id cannot add a segment: the separator is escaped, so `a/b` stays one
    // path parameter rather than becoming a two-level route.
    expect(memoryActionPath('a/b', 'archive')).toBe(`${MEMORY_PATH}/a%2Fb/archive`)
  })

  it('refuses an adapter that cannot answer the port', () => {
    expect(() => assertMemoryAdapter({ name: 'half', listDrafts: () => {} })).toThrow(
      /缺少操作/,
    )
    expect(() => assertMemoryAdapter(null)).toThrow(/必须是一个对象/)
  })

  it('ships unbound, so a build without a memory server cannot claim records', async () => {
    expect(memoryAdapterName()).toBe('unbound')
    expect(isMockMemoryAdapter()).toBe(false)
    await expect(listDrafts()).rejects.toMatchObject({ code: UNBOUND_CODE, status: 503 })
    await expect(listMemories()).rejects.toMatchObject({ code: UNBOUND_CODE })
    await expect(confirmDraft('drf-1')).rejects.toMatchObject({ code: UNBOUND_CODE })
    // Nothing reached the network: the failure is decided before a request is
    // ever built, so an unwired build cannot half-call a real origin.
    expect(fetchSpy).not.toHaveBeenCalled()
  })

  it('routes the unbound failure to the sentence that says so', async () => {
    const error = await listDrafts().catch((caught) => caught)
    expect(memoryError(error).copy).toBe(MEMORY_ERROR_COPY.notWired)
  })

  it('never touches the network from an unwired build, whatever it is asked to do', async () => {
    const spy = vi.fn(() => {
      throw new Error('this module must not reach the network')
    })
    globalThis.fetch = spy

    await archiveMemory('mem-1').catch(() => {})
    await rejectDraft('drf-1').catch(() => {})
    await confirmDraft('drf-1').catch(() => {})
    await listMemories().catch(() => {})
    await listDrafts().catch(() => {})

    expect(spy).not.toHaveBeenCalled()
  })

  it('lets an installed adapter take over every operation and be reported by name', async () => {
    const adapter = createMockMemoryAdapter()
    setMemoryAdapter(adapter)
    expect(memoryAdapterName()).toBe('mock')
    expect(isMockMemoryAdapter()).toBe(true)
    const payload = await listDrafts()
    expect(normalizeDraftList(payload)).toHaveLength(3)

    resetMemoryAdapter()
    expect(memoryAdapterName()).toBe('unbound')
  })

  it('records which operation reached an unbound adapter', async () => {
    await createUnboundMemoryAdapter().archiveMemory('mem-1').catch(() => {})
    expect(devIssues().map((entry) => entry.detail.operation)).toContain('archiveMemory')
  })
})

describe('the adapter choice', () => {
  /**
   * The real backend is the default, so every row that is not an explicit opt-in
   * must land on `http`. A missing or misspelled flag reading as "show me the demo
   * data" would put invented memories in front of a user, which is the one failure
   * this table is not allowed to have.
   */
  const matrix = [
    ['dev, flag absent', { DEV: true }, 'http'],
    ['dev, flag empty', { DEV: true, VITE_MEMORY_MOCK: '' }, 'http'],
    ['dev, flag whitespace', { DEV: true, VITE_MEMORY_MOCK: '   ' }, 'http'],
    ['dev, flag off', { DEV: true, VITE_MEMORY_MOCK: 'off' }, 'http'],
    ['dev, flag false', { DEV: true, VITE_MEMORY_MOCK: 'false' }, 'http'],
    ['dev, flag mock', { DEV: true, VITE_MEMORY_MOCK: 'mock' }, 'http'],
    ['dev, flag yes', { DEV: true, VITE_MEMORY_MOCK: 'yes' }, 'http'],
    ['dev, flag 1', { DEV: true, VITE_MEMORY_MOCK: '1' }, 'http'],
    ['dev, flag true as a boolean', { DEV: true, VITE_MEMORY_MOCK: true }, 'http'],
    ['dev, flag ON spelled exactly', { DEV: true, VITE_MEMORY_MOCK: 'on' }, 'mock'],
    ['dev, flag padded and upper case', { DEV: true, VITE_MEMORY_MOCK: '  ON ' }, 'mock'],
    ['prod, flag on', { DEV: false, VITE_MEMORY_MOCK: 'on' }, 'http'],
    ['test mode, flag on', { VITE_MEMORY_MOCK: 'on' }, 'http'],
    ['no environment at all', undefined, 'http'],
    ['no flags in the environment', {}, 'http'],
  ]

  it.each(matrix)('%s → %s', (_label, env, expected) => {
    expect(resolveMemoryAdapterMode(env)).toBe(expected)
  })

  it('names the two outcomes, and they are the adapter names', () => {
    expect(MemoryAdapterMode.Http).toBe('http')
    expect(MemoryAdapterMode.Mock).toBe('mock')
    expect(resolveMemoryAdapterMode({ DEV: true, VITE_MEMORY_MOCK: 'on' })).toBe(
      MemoryAdapterMode.Mock,
    )
  })

  it('decides the same way twice, and never consults anything but its argument', () => {
    const env = { DEV: true, VITE_MEMORY_MOCK: 'on' }
    expect(resolveMemoryAdapterMode(env)).toBe('mock')
    expect(resolveMemoryAdapterMode(env)).toBe('mock')
    expect(env.VITE_MEMORY_MOCK).toBe('on')
  })

  it('is what the boot sequence actually uses', async () => {
    // The bug this pins is specific and already happened once: `main.js` had the
    // inverted condition `VITE_MEMORY_MOCK !== 'off'`, which made the demo adapter
    // the dev default. The rule now lives in the resolver above, so boot has to
    // call it and must not re-derive the flag itself.
    const source = readFileSync(resolve(process.cwd(), 'src/main.js'), 'utf8')

    expect(source).toContain('resolveMemoryAdapterMode')
    expect(source).toContain('MemoryAdapterMode.Mock')
    expect(source).not.toMatch(/VITE_MEMORY_MOCK\s*!==?\s*['"]off['"]/)
    expect(source).not.toMatch(/import\.meta\.env\.DEV[^)]*&&[^)]*VITE_MEMORY_MOCK/)
  })

  it('reports the installed adapter by name, so the label follows the transport', () => {
    expect(memoryAdapterName()).toBe('unbound')
    setMemoryAdapter(createHttpMemoryAdapter())
    expect(memoryAdapterName()).toBe('http')
    expect(isMockMemoryAdapter()).toBe(false)
    setMemoryAdapter(createMockMemoryAdapter())
    expect(memoryAdapterName()).toBe('mock')
    expect(isMockMemoryAdapter()).toBe(true)
    resetMemoryAdapter()
    expect(memoryAdapterName()).toBe('unbound')
  })
})

describe('the mock adapter', () => {
  it('answers in the server shape so the normalizers are the ones under test', async () => {
    const adapter = createMockMemoryAdapter()
    const drafts = await adapter.listDrafts()
    const memories = await adapter.listMemories()

    expect(Object.keys(drafts)).toEqual(['items'])
    expect(drafts.items[0]).not.toHaveProperty('topicKey')
    expect(Object.keys(drafts.items[0])).toEqual(
      expect.arrayContaining(['memory_id', 'topic_key', 'confidence', 'source_session_id']),
    )
    // Only pending and active rows are ever listed; the retired seed row is in
    // the table but not on the list, exactly like the endpoint.
    expect(memories.items.map((row) => row.status)).toEqual([
      MemoryStatus.Active,
      MemoryStatus.Active,
      MemoryStatus.Active,
    ])
    expect(adapter.snapshot().memories).toHaveLength(4)
  })

  it('covers all three scopes in the demo rows, and one row without provenance', async () => {
    const adapter = createMockMemoryAdapter()
    const scopes = (await adapter.listDrafts()).items.map((row) => row.scope)
    expect(new Set(scopes)).toEqual(
      new Set([MemoryScope.Global, MemoryScope.Topic, MemoryScope.Episodic]),
    )

    const normalized = normalizeDraftList(await adapter.listDrafts())
    const noSource = normalized.find((row) => row.sources.length === 0)
    expect(noSource?.id).toBe('drf-episode-1')
  })

  it('moves a confirmed draft into the memory set and answers with the new row', async () => {
    const adapter = createMockMemoryAdapter()
    const before = normalizeDraftList(await adapter.listDrafts())
    const draft = before.find((row) => row.id === 'drf-topic-1')
    const response = await adapter.confirmDraft('drf-topic-1')
    const created = normalizeMemoryResponse(response)

    expect(created).not.toBeNull()
    expect(created.content).toBe(draft.content)
    expect(created.scope).toBe(MemoryScope.Topic)
    expect(created.topicKey).toBe(draft.topicKey)
    expect(created.confidence).toBe(draft.confidence)
    expect(created.status).toBe(MemoryStatus.Active)
    expect((await adapter.listDrafts()).items.map((row) => row.memory_id)).not.toContain(
      'drf-topic-1',
    )
    expect((await adapter.listMemories()).items.map((row) => row.memory_id)).toContain(created.id)
  })

  it('rejects, archives, and reports a vanished or already-retired row', async () => {
    const adapter = createMockMemoryAdapter()
    await adapter.rejectDraft('drf-profile-1')
    expect((await adapter.listDrafts()).items).toHaveLength(2)

    const archived = await adapter.archiveMemory('mem-topic-1')
    expect(archived.status).toBe(MemoryStatus.Archived)
    // An archived row is never listed again, so the only honest statement about
    // history is the count of what this page archived.
    expect((await adapter.listMemories()).items.map((row) => row.memory_id)).not.toContain(
      'mem-topic-1',
    )

    await expect(adapter.archiveMemory('mem-topic-1')).rejects.toMatchObject({
      status: 409,
      code: 'memory_not_active',
    })
    await expect(adapter.rejectDraft('drf-gone')).rejects.toMatchObject({
      status: 404,
      code: 'not_found',
    })
  })

  it('hands a test exactly one injected failure per arming', async () => {
    const adapter = createMockMemoryAdapter()
    adapter.failNext('listDrafts', { status: 500, code: 'internal_error', message: 'db down' })
    await expect(adapter.listDrafts()).rejects.toMatchObject({ status: 500 })
    await expect(adapter.listDrafts()).resolves.toHaveProperty('items')
  })

  it('honours an already aborted signal the way fetch does', async () => {
    const adapter = createMockMemoryAdapter()
    const controller = new AbortController()
    controller.abort()
    await expect(adapter.listMemories({ signal: controller.signal })).rejects.toMatchObject({
      name: 'AbortError',
    })
  })

  it('records each call by operation so a test can count requests', async () => {
    const adapter = createMockMemoryAdapter()
    await adapter.confirmDraft('drf-1').catch(() => {})
    expect(adapter.calls.map((call) => call.operation)).toEqual(['confirmDraft'])
  })

  it('does not mutate the seed it was handed', async () => {
    const seed = [
      { memory_id: 'drf-x', scope: 'topic', content: 'one', source_message_ids: ['msg-1'] },
    ]
    const adapter = createMockMemoryAdapter({ drafts: seed, memories: [] })
    await adapter.rejectDraft('drf-x')
    expect(seed[0].source_message_ids).toEqual(['msg-1'])
    expect(await adapter.listDrafts()).toEqual({ items: [] })
  })
})

/**
 * The HTTP adapter is what `main.js` installs, so these cases are the only place
 * the client is checked against the wire the server actually answers with — the
 * paths and methods `internal/httpapi/testdata/http_contract.golden` replays, and
 * the absence of a body that the confirmation gate depends on.
 */
describe('the http adapter, against the pinned contract', () => {
  const draftRow = {
    memory_id: 'mem-7',
    scope: 'global',
    content: '回答控制在一段以内。',
    status: 'pending',
    confidence: 1,
    source_session_id: 'session-3',
    source_message_ids: ['message-4', 'message-5'],
    created_at: '2026-09-26T02:00:00Z',
    updated_at: '2026-09-26T02:00:00Z',
  }

  beforeEach(() => {
    setTokenProvider(() => 'jwt-under-test')
    setMemoryAdapter(createHttpMemoryAdapter())
    expect(memoryAdapterName()).toBe('http')
  })

  afterEach(() => {
    setTokenProvider(() => null)
  })

  it('asks for the two listings by state, with the bearer in a header', async () => {
    const { calls } = installFetchStub(() =>
      jsonResponse({ data: { items: [draftRow] }, error: null }),
    )

    const drafts = normalizeDraftList(await listDrafts())
    const memories = normalizeMemoryList(await listMemories())

    expect(calls.map((call) => [call.init.method ?? 'GET', call.url])).toEqual([
      ['GET', `${MEMORY_PATH}?state=pending`],
      ['GET', `${MEMORY_PATH}?state=active`],
    ])
    for (const call of calls) {
      expect(call.init.headers.authorization).toBe('Bearer jwt-under-test')
      // A read carries nothing: no body, and so no JSON content type either.
      expect(call.init.body).toBeUndefined()
      expect(call.init.headers['content-type']).toBeUndefined()
    }
    // The row is addressed by the id the server actually sends, and its provenance
    // arrives flat rather than nested.
    expect(drafts).toHaveLength(1)
    expect(drafts[0]).toMatchObject({
      id: 'mem-7',
      scope: MemoryScope.Global,
      sources: [{ sessionId: 'session-3', messageIds: ['message-4', 'message-5'] }],
    })
    expect(memories[0].id).toBe('mem-7')
  })

  it('names the verb in the path and sends no body to go with it', async () => {
    const { calls } = installFetchStub(() =>
      jsonResponse({ data: { ...draftRow, status: 'active' }, error: null }),
    )

    await confirmDraft('mem 7')
    await rejectDraft('mem-7')
    await archiveMemory('mem-7')

    expect(calls.map((call) => [call.init.method, call.url])).toEqual([
      ['POST', `${MEMORY_PATH}/mem%207/confirm`],
      ['POST', `${MEMORY_PATH}/mem-7/reject`],
      ['POST', `${MEMORY_PATH}/mem-7/archive`],
    ])
    for (const call of calls) {
      expect(call.init.body).toBeUndefined()
      expect(call.init.headers['content-type']).toBeUndefined()
    }
  })

  it('keeps a write that answers with the stored row readable by id', async () => {
    installFetchStub(() => jsonResponse({ data: { ...draftRow, status: 'active' }, error: null }))
    const confirmed = normalizeMemoryResponse(await confirmDraft('mem-7'))
    expect(confirmed).toMatchObject({ id: 'mem-7', status: MemoryStatus.Active })
  })

  it('renders a lifecycle conflict as the sentence naming the state it found', async () => {
    installFetchStub(() =>
      jsonResponse(
        {
          data: null,
          error: {
            code: 'memory_evidence_missing',
            message: 'the conversation this memory cites is no longer stored',
          },
        },
        { status: 409 },
      ),
    )

    await expect(confirmDraft('mem-7')).rejects.toMatchObject({
      code: 'memory_evidence_missing',
      status: 409,
    })
    const shown = memoryError({ code: 'memory_evidence_missing', status: 409 })
    expect(shown.copy).toBe(MEMORY_ERROR_COPY.evidenceMissing)
    // The server's English is never the user's sentence, and this one says what
    // happened to the evidence rather than blaming the reader.
    expect(shown.copy).not.toMatch(/conversation/i)
    expect(shown.copy).toMatch(/对话/)
  })

  it('treats a foreign row and a vanished row as the same not-found', async () => {
    installFetchStub(() =>
      jsonResponse({ data: null, error: { code: 'not_found', message: 'resource not found' } }, { status: 404 }),
    )
    const shown = await archiveMemory('mem-other').then(
      () => null,
      (error) => memoryError(error),
    )
    expect(shown).toMatchObject({ code: 'not_found' })
    expect(shown.copy).toBe(MEMORY_ERROR_COPY.notFound)
  })
})

describe('the hand-written memory request', () => {
  it('offers only the two scopes a person can assert about themselves', () => {
    expect(MANUAL_MEMORY_SCOPES).toEqual([MemoryScope.Global, MemoryScope.Topic])
    expect(scopeChoiceLabel(MemoryScope.Global)).toBe('全局记忆')
    expect(scopeChoiceLabel(MemoryScope.Episodic)).toBe('类型未识别')
  })

  it('sends topic_key for a topic memory and not a word of it for a global one', () => {
    expect(
      Object.keys(
        memoryAddBody({
          scope: MemoryScope.Global,
          topicKey: 'go',
          content: '讲代码用 Go 语法。',
        }),
      ).sort(),
    ).toEqual(['content', 'scope'])

    expect(
      memoryAddBody({
        scope: MemoryScope.Topic,
        topicKey: ' go ',
        content: '  这个主题用 Go 示例。  ',
      }),
    ).toEqual({ scope: MemoryScope.Topic, topic_key: 'go', content: '这个主题用 Go 示例。' })
  })

  it('carries no provenance, because the endpoint refuses it rather than ignoring it', () => {
    // A hand-written fact has no conversation behind it. Accepting a session id
    // here would let this request invent the evidence that makes an agent's claim
    // checkable, so the field must not exist in the body at all.
    const body = memoryAddBody({
      scope: MemoryScope.Global,
      content: 'x',
      sourceSessionId: 'ses-1',
      sourceMessageId: 'msg-1',
      status: 'active',
      memory_id: 'mem-9',
      confidence: 0,
      user_id: 'usr-9',
    })

    expect(Object.keys(body).sort()).toEqual(['content', 'scope'])
  })

  it('refuses the two incomplete shapes the server also refuses', () => {
    expect(validateMemoryForm({ scope: 'global', content: '   ' }).errors.content).toBeTruthy()
    expect(validateMemoryForm({ scope: 'global', content: '' }).valid).toBe(false)
    expect(validateMemoryForm({ scope: 'topic', content: 'ok' }).errors.topicKey).toBeTruthy()
    // An old key left in the form while the scope is global neither blocks nor ships.
    expect(validateMemoryForm({ scope: 'global', content: 'ok', topicKey: 'go' }).valid).toBe(true)
    expect(validateMemoryForm({ scope: 'nonsense', content: 'ok' }).valid).toBe(true)
  })

  it('counts characters the way the server does, in code points', () => {
    const at = (n) => '😀'.repeat(n)

    expect(
      validateMemoryForm({ scope: 'global', content: at(MEMORY_CREATE_LIMITS.contentRunes) }).valid,
    ).toBe(true)
    expect(
      validateMemoryForm({
        scope: 'global',
        content: at(MEMORY_CREATE_LIMITS.contentRunes + 1),
      }).errors.content,
    ).toBe(MEMORY_FORM_COPY.contentTooLong(MEMORY_CREATE_LIMITS.contentRunes))
    expect(
      validateMemoryForm({
        scope: 'topic',
        content: 'ok',
        topicKey: 'a'.repeat(MEMORY_CREATE_LIMITS.topicKeyRunes + 1),
      }).errors.topicKey,
    ).toBeTruthy()
  })

  it('posts the built body to /api/v1/memories and nothing else', async () => {
    const stub = installFetchStub(() =>
      jsonResponse({
        data: {
          memory_id: 'mem-new-1',
          scope: 'global',
          content: '讲代码用 Go 语法。',
          status: 'active',
          confidence: 1,
          source_session_id: '',
          source_message_ids: null,
          created_at: '2026-09-28T04:00:00Z',
          updated_at: '2026-09-28T04:00:00Z',
        },
        error: null,
      }),
    )
    setMemoryAdapter(createHttpMemoryAdapter())

    const created = await addMemory({
      scope: MemoryScope.Global,
      topicKey: 'leftover',
      content: '讲代码用 Go 语法。',
    })

    expect(stub.calls).toHaveLength(1)
    expect(stub.calls[0].url).toBe(MEMORY_PATH)
    expect(stub.calls[0].init.method).toBe('POST')
    expect(JSON.parse(stub.calls[0].init.body)).toEqual({
      scope: 'global',
      content: '讲代码用 Go 语法。',
    })
    // The answer is a live memory, not a draft waiting for a second click.
    expect(normalizeMemoryResponse(created)).toMatchObject({
      id: 'mem-new-1',
      scope: MemoryScope.Global,
      status: MemoryStatus.Active,
      confidence: 1,
    })
  })

  it('fails through the unbound adapter without reaching the network', async () => {
    await expect(
      addMemory({ scope: MemoryScope.Global, content: 'x' }),
    ).rejects.toMatchObject({ code: UNBOUND_CODE, status: 503 })
    expect(fetchSpy).not.toHaveBeenCalled()
  })

  it('requires the add operation on every adapter', () => {
    const without = { ...createHttpMemoryAdapter() }
    delete without.addMemory
    expect(() => assertMemoryAdapter(without)).toThrow(/addMemory/)
  })
})
