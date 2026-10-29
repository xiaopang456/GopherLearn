import { ApiError } from './http.js'
import { MemoryScope, MemoryStatus } from './memory.js'

/**
 * The mock side of the memory port, used by the dev server and by the tests.
 *
 * Two rules keep it useful rather than decorative:
 *
 * 1. It answers in the *server's* shape, field for field: `memory_id`, flat
 *    `source_session_id` / `source_message_ids`, a single object from a write and
 *    `{ items: [...] }` from a list. Not a convenience object shaped the way the
 *    component would like, because then `normalizeDraft()` would only meet the
 *    real spelling in production. The shape mirrors `internal/httpapi/memory.go`
 *    as read in this phase; the port is still not installed, so nothing here is
 *    a live call.
 * 2. It has no capability the listing does not have. It returns pending and active
 *    rows only — retired rows exist in the table but are never listed — and it
 *    stores, extracts and recalls nothing across a reload.
 */

const STAMP = '2026-09-25T09:00:00Z'

/**
 * The keys `POST /api/v1/memories` accepts. Everything else — a status, an id, a
 * confidence, and above all a source session or message — is the client trying to
 * decide its own gate or to invent evidence, so the mock refuses it exactly like
 * `decodeJSON` on the server does.
 */
const ADD_FIELDS = new Set(['scope', 'topic_key', 'content'])

function draft(id, scope, content, confidence, session, messages, topicKey = '') {
  return {
    memory_id: id,
    scope,
    topic_key: topicKey,
    content,
    confidence,
    source_session_id: session,
    source_message_ids: messages,
    status: 'pending',
    created_at: STAMP,
    updated_at: STAMP,
  }
}

function memory(id, scope, content, confidence, session, messages, topicKey = '') {
  return {
    ...draft(id, scope, content, confidence, session, messages, topicKey),
    status: MemoryStatus.Active,
  }
}

export function mockDraftSeed() {
  return [
    draft(
      'drf-profile-1',
      MemoryScope.Global,
      '解释概念时先给结论，再给一个最小例子。',
      0.82,
      'ses-104',
      ['msg-104-7'],
    ),
    draft(
      'drf-topic-1',
      MemoryScope.Topic,
      '有缓冲 channel 的丢弃语义还需要再确认一次。',
      0.64,
      'ses-118',
      ['msg-118-3', 'msg-118-4'],
      'channel-buffer',
    ),
    // No provenance at all: the page must say so rather than print a blank.
    draft(
      'drf-episode-1',
      MemoryScope.Episodic,
      '这一段对话里你两次要求把例子换成真实代码。',
      0.55,
      '',
      [],
    ),
  ]
}

export function mockMemorySeed() {
  return [
    memory(
      'mem-profile-1',
      MemoryScope.Global,
      '回答控制在一段以内，除非我让你展开。',
      0.91,
      'ses-77',
      ['msg-77-2'],
    ),
    memory(
      'mem-topic-1',
      MemoryScope.Topic,
      'Gin 的路由分组和中间件顺序你已经讲清楚了。',
      0.74,
      'ses-83',
      ['msg-83-11'],
      'gin-routing',
    ),
    memory(
      'mem-episode-1',
      MemoryScope.Episodic,
      '上次讲 Redis TTL 时你要求先讲删除策略再讲命令。',
      0.68,
      'ses-90',
      ['msg-90-1'],
    ),
    // Retired, and therefore never listed: it exists only so an archive action
    // has something to move a row into.
    {
      ...memory(
        'mem-archived-1',
        MemoryScope.Topic,
        '这条已经不再带入回答。',
        0.6,
        'ses-61',
        ['msg-61-4'],
        'gorm-soft-delete',
      ),
      status: MemoryStatus.Archived,
    },
  ]
}

/** What a listing never shows: the states the lifecycle has retired. */
function isListed(row) {
  return row.status !== MemoryStatus.Archived && row.status !== MemoryStatus.Superseded
}

function clone(record) {
  return { ...record, source_message_ids: [...(record.source_message_ids ?? [])] }
}

function fail(status, code, message) {
  throw new ApiError(message, { code, status })
}

/**
 * @param {{drafts?: object[], memories?: object[], latency?: number,
 *   now?: () => number}} [options]
 */
export function createMockMemoryAdapter({
  drafts = mockDraftSeed(),
  memories = mockMemorySeed(),
  latency = 0,
  now = () => Date.now(),
} = {}) {
  let draftRows = drafts.map(clone)
  let memoryRows = memories.map(clone)

  /** Per-operation faults to inject, with the number of calls left to fail. */
  const armed = new Map()
  const calls = []

  /**
   * Every operation passes through here: it records the call, spends the
   * configured latency, then honours either an injected fault or an already
   * aborted signal — the two things a test needs to reach states the happy path
   * never produces.
   */
  async function settle(operation, { signal } = {}, id = '') {
    calls.push({ operation, id })
    const fault = armed.get(operation)
    if (fault) {
      fault.left -= 1
      if (fault.left <= 0) armed.delete(operation)
    }
    if (latency > 0) await new Promise((resolve) => setTimeout(resolve, latency))
    if (signal?.aborted) {
      const error = new Error('The user aborted a request.')
      error.name = 'AbortError'
      throw error
    }
    if (fault) throw new ApiError(fault.message, { code: fault.code, status: fault.status })
  }

  const adapter = {
    name: 'mock',
    calls,

    snapshot() {
      return { drafts: draftRows.map(clone), memories: memoryRows.map(clone) }
    },

    /**
     * Make the next `times` calls of `operation` fail with this transport shape.
     * A repeating arm exists so a test can hold a list in a failed state while a
     * write fails differently — the page renders those two notices separately, and
     * a one-shot fault would be swept away by the refresh after the write.
     */
    failNext(
      operation,
      { status = 500, code = 'internal_error', message = 'db down', times = 1 } = {},
    ) {
      armed.set(operation, { status, code, message, left: Math.max(1, times) })
      return adapter
    },

    seedRows({ drafts: nextDrafts, memories: nextMemories }) {
      if (nextDrafts) draftRows = nextDrafts.map(clone)
      if (nextMemories) memoryRows = nextMemories.map(clone)
      return adapter
    },

    async listDrafts(options = {}) {
      await settle('listDrafts', options)
      return { items: draftRows.map(clone) }
    },

    async listMemories(options = {}) {
      await settle('listMemories', options)
      return { items: memoryRows.filter(isListed).map(clone) }
    },

    /**
     * A memory the person stated themselves: active on arrival, confidence pinned to
     * certain, an id the caller did not choose, and the previous active row of the
     * same subject superseded by the same write — the submit button *is* the
     * decision, so there is no second request to make.
     *
     * The refusals are the server's. Provenance in particular is rejected rather
     * than ignored: a hand-written fact has no conversation behind it, and accepting
     * one would let this request fabricate the evidence that makes an agent's claim
     * checkable.
     */
    async addMemory(body = {}) {
      await settle('addMemory', {})
      const forbidden = Object.keys(body).filter((key) => !ADD_FIELDS.has(key))
      if (forbidden.length > 0) fail(400, 'invalid_request', 'fields are not writable here')
      const scope = String(body.scope ?? '')
      if (scope !== MemoryScope.Global && scope !== MemoryScope.Topic) {
        fail(400, 'invalid_request', 'only global and topic can be stated by hand')
      }
      const content = String(body.content ?? '').trim()
      if (content === '') fail(400, 'invalid_request', 'content is required')
      const topicKey = String(body.topic_key ?? '').trim()
      if (scope === MemoryScope.Topic && topicKey === '') {
        fail(400, 'invalid_request', 'topic key is required')
      }
      if (scope === MemoryScope.Global && topicKey !== '') {
        fail(400, 'invalid_request', 'a global memory carries no topic key')
      }
      const stamp = new Date(now()).toISOString()
      const created = {
        memory_id: `mem-authored-${memoryRows.length + 1}`,
        scope,
        topic_key: topicKey,
        content,
        confidence: 1,
        source_session_id: '',
        source_message_ids: null,
        status: MemoryStatus.Active,
        created_at: stamp,
        updated_at: stamp,
      }
      // Same subject = same (scope, topic key): the new statement replaces the old
      // one rather than leaving two live answers to the same question.
      memoryRows = memoryRows.map((row) =>
        row.status === MemoryStatus.Active && row.scope === scope && (row.topic_key ?? '') === topicKey
          ? { ...row, status: MemoryStatus.Superseded, updated_at: stamp }
          : row,
      )
      memoryRows = [...memoryRows, clone(created)]
      return clone(created)
    },

    /**
     * Confirming moves the row out of pending and into the listed set, which is
     * the transition the page is built around, and answers with the new memory as
     * one object rather than a list.
     */
    async confirmDraft(draftId) {
      await settle('confirmDraft', {}, draftId)
      const index = draftRows.findIndex((row) => row.memory_id === draftId)
      if (index < 0) fail(404, 'not_found', 'draft is gone')
      const [draft] = draftRows.splice(index, 1)
      const stamp = new Date(now()).toISOString()
      const created = {
        ...draft,
        memory_id: `mem-confirmed-${draftId}`,
        status: MemoryStatus.Active,
        created_at: stamp,
        updated_at: stamp,
      }
      memoryRows = [...memoryRows, created]
      return clone(created)
    },

    async rejectDraft(draftId) {
      await settle('rejectDraft', {}, draftId)
      const index = draftRows.findIndex((row) => row.memory_id === draftId)
      if (index < 0) fail(404, 'not_found', 'draft is gone')
      const [rejected] = draftRows.splice(index, 1)
      // A rejected draft stops being pending and is never listed again.
      return { ...clone(rejected), status: MemoryStatus.Archived }
    },

    async archiveMemory(memoryId) {
      await settle('archiveMemory', {}, memoryId)
      const row = memoryRows.find((item) => item.memory_id === memoryId)
      if (!row) fail(404, 'not_found', 'memory is gone')
      if (row.status !== MemoryStatus.Active) {
        fail(409, 'memory_not_active', 'memory is not in the state the action needs')
      }
      const archived = {
        ...row,
        status: MemoryStatus.Archived,
        updated_at: new Date(now()).toISOString(),
      }
      memoryRows = memoryRows.map((item) => (item.memory_id === memoryId ? archived : item))
      return clone(archived)
    },
  }

  return adapter
}
