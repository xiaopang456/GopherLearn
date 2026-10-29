import { ApiError, request } from './http.js'
import { recordDevIssue } from '../lib/devlog.js'

/**
 * The memory surface: endpoints, request bodies, response shapes, scope
 * vocabulary and user-visible failure copy — all of it in this file.
 *
 * `MemoryView` must not learn a path, a field name or a status code. It asks the
 * store for drafts and memories, and the store asks an *adapter* for them. Three
 * adapters implement the same port:
 *
 *   - `createHttpMemoryAdapter()` — the same port over `fetch`, and what the app
 *     installs: the server's contract, pinned row by row by
 *     `internal/httpapi/testdata/http_contract.golden`.
 *   - `createMockMemoryAdapter()` — a local, in-memory stand-in used by the dev
 *     server and the tests (see `memoryMock.js`), labelled on screen as demo data
 *     wherever it appears.
 *   - `createUnboundMemoryAdapter()` — the module's starting point and the honest
 *     failure a build falls back to if nothing installs an adapter: a memory list
 *     that cannot be read must not look empty by accident.
 *
 * Nothing here guesses at the wire. The two listings are one path and a `state`
 * query; the three lifecycle transitions are POSTs whose verb lives in the path, so
 * an action carries no body at all; and the one write a person starts themselves
 * carries three fields and no provenance, because that request *is* the decision.
 * What the server refuses is refused here too by omission: a draft's status,
 * identity and timestamps belong to the gate, and a request that tried to send them
 * is answered 400, never applied.
 */

export const MEMORY_PATH = '/api/v1/memories'

/**
 * The two listings this surface offers, and the only two. The state arrives as a
 * query and has no server-side default, so a call that forgets to name one is a
 * bug here rather than a request for somebody else's list.
 *
 * There is deliberately no `archived` state: retired rows are not on offer from
 * this API at all, which is why the page counts what it archived rather than
 * claiming to know the history it cannot read.
 */
export const MemoryListState = {
  Pending: 'pending',
  Active: 'active',
}

export function memoryListPath(state) {
  if (!Object.values(MemoryListState).includes(state)) {
    recordDevIssue('memory.list_state.unknown', { received: state })
    throw new Error('记忆列表只接受 pending 或 active。')
  }
  return `${MEMORY_PATH}?state=${encodeURIComponent(state)}`
}

export function memoryPath(memoryId) {
  return `${MEMORY_PATH}/${encodeURIComponent(String(memoryId))}`
}

/** The verb travels in the path, and an action carries no body. */
export function memoryActionPath(memoryId, action) {
  return `${memoryPath(memoryId)}/${encodeURIComponent(String(action))}`
}

/**
 * Scopes are server-owned vocabulary, and these three spellings are the ones
 * `domain.MemoryScope` stores and the contract golden replays: a global fact, a
 * topic-scoped statement keyed by `topic_key`, an episode. An unrecognised scope is
 * never mapped onto a recognised one — guessing would hand a record an authority
 * nobody gave it, and `topic` is exactly the scope whose recall authorisation this
 * build has not opened.
 */
export const MemoryScope = {
  Global: 'global',
  Topic: 'topic',
  Episodic: 'episodic',
}

export const MEMORY_SCOPES = [MemoryScope.Global, MemoryScope.Topic, MemoryScope.Episodic]

const SCOPE_LABELS = {
  [MemoryScope.Global]: '全局画像',
  [MemoryScope.Topic]: '主题记忆',
  [MemoryScope.Episodic]: '情景记忆',
}

export const UNKNOWN_SCOPE_LABEL = '类型未识别'

export function normalizeScope(raw) {
  const value = typeof raw === 'string' ? raw.trim().toLowerCase() : ''
  if (MEMORY_SCOPES.includes(value)) return value
  if (value !== '') recordDevIssue('memory.scope.unknown', { received: raw })
  return ''
}

export function scopeLabel(scope) {
  return SCOPE_LABELS[scope] ?? UNKNOWN_SCOPE_LABEL
}

/**
 * What a person may state about themselves, and what the server will hold.
 *
 * `episodic` is absent on purpose and the server refuses it too: an episode is a
 * statement about what happened inside one conversation, and only something that
 * watched that conversation can make it. A form filled in later can assert a habit
 * or a rule for a topic, which is exactly what these two scopes are.
 */
export const MANUAL_MEMORY_SCOPES = [MemoryScope.Global, MemoryScope.Topic]

const SCOPE_CHOICE_LABELS = {
  [MemoryScope.Global]: '全局记忆',
  [MemoryScope.Topic]: '主题记忆',
}

export function scopeChoiceLabel(scope) {
  return SCOPE_CHOICE_LABELS[scope] ?? UNKNOWN_SCOPE_LABEL
}

/**
 * The server's own ceilings, mirrored so the form can say "too long" before a
 * request is spent rather than after. There is deliberately no source-message
 * limit here: this request carries no provenance at all (see `memoryAddBody`).
 */
export const MEMORY_CREATE_LIMITS = {
  contentRunes: 4000,
  topicKeyRunes: 96,
}

/** Copy for the form's own refusals. No server string reaches this table. */
export const MEMORY_FORM_COPY = {
  contentEmpty: '请填写要记住的内容。',
  contentTooLong: (limit) => `内容最多 ${limit} 个字符，请删短一些。`,
  topicKeyEmpty: '主题记忆需要填写主题标识。',
  topicKeyTooLong: (limit) => `主题标识最多 ${limit} 个字符。`,
  submitting: '正在添加…',
  added: '已添加，这条记忆现在开始生效。',
}

/** Code points, which is what the server counts when it says "characters". */
function runeLength(value) {
  return Array.from(typeof value === 'string' ? value : '').length
}

/**
 * @param {{scope?: string, topicKey?: string, content?: string}} form
 * @returns {{valid: boolean, errors: Record<string, string>}}
 */
export function validateMemoryForm(form = {}) {
  const errors = {}
  const scope = normalizeScope(form.scope)
  const content = typeof form.content === 'string' ? form.content.trim() : ''
  const topicKey = typeof form.topicKey === 'string' ? form.topicKey.trim() : ''

  if (content === '') errors.content = MEMORY_FORM_COPY.contentEmpty
  else if (runeLength(content) > MEMORY_CREATE_LIMITS.contentRunes) {
    errors.content = MEMORY_FORM_COPY.contentTooLong(MEMORY_CREATE_LIMITS.contentRunes)
  }

  // A key belongs to the topic scope only. When the scope is global the field is
  // not shown, so an old value sitting in the form neither blocks the submit nor
  // reaches the wire — the server answers 400 to "global with a topic key",
  // because the uniqueness key is (user, scope, topic_key) and a stray key would
  // split the global namespace into two slots, each keeping its own answer.
  if (scope === MemoryScope.Topic) {
    if (topicKey === '') errors.topicKey = MEMORY_FORM_COPY.topicKeyEmpty
    else if (runeLength(topicKey) > MEMORY_CREATE_LIMITS.topicKeyRunes) {
      errors.topicKey = MEMORY_FORM_COPY.topicKeyTooLong(MEMORY_CREATE_LIMITS.topicKeyRunes)
    }
  }

  return { valid: Object.keys(errors).length === 0, errors }
}

/**
 * The whole request body: three fields at most.
 *
 * Provenance is not merely omitted for convenience — the endpoint *rejects*
 * `source_session_id` / `source_message_ids`, and that refusal is the reason this
 * path can skip the confirmation gate: a hand-written fact is judged on what it
 * says, while an agent's claim has to point at a real message so it can be checked.
 * Letting a client supply a session would let it invent the evidence that makes
 * somebody else's claim auditable. `confidence`, `status`, ids and timestamps are
 * refused for the same reason: the author's assertion *is* the certainty stored.
 */
export function memoryAddBody(form = {}) {
  const scope = normalizeScope(form.scope)
  const body = {
    scope,
    content: typeof form.content === 'string' ? form.content.trim() : '',
  }
  if (scope === MemoryScope.Topic) {
    body.topic_key = typeof form.topicKey === 'string' ? form.topicKey.trim() : ''
  }
  return body
}

export const MemoryStatus = {
  Active: 'active',
  Archived: 'archived',
  Superseded: 'superseded',
}

const MEMORY_STATUSES = [
  MemoryStatus.Active,
  MemoryStatus.Archived,
  MemoryStatus.Superseded,
]

/** '' means "not an active memory", which is the safe reading of a stranger. */
export function normalizeStatus(raw) {
  const value = typeof raw === 'string' ? raw.trim().toLowerCase() : ''
  if (MEMORY_STATUSES.includes(value)) return value
  if (value !== '') recordDevIssue('memory.status.unknown', { received: raw })
  return ''
}

function firstString(...values) {
  for (const value of values) {
    if (typeof value === 'string' && value.trim() !== '') return value.trim()
  }
  return ''
}

function numberOrNull(value) {
  if (value === null || value === undefined || value === '') return null
  const parsed = Number(value)
  if (!Number.isFinite(parsed) || parsed < 0 || parsed > 1) return null
  return parsed
}

/**
 * Provenance arrives flat: one citing session and the message ids inside it. It is
 * read into the one-row-per-source shape the page renders, and a record with no
 * provenance at all stays empty rather than being given a placeholder session —
 * "no source recorded" is itself information a confirmation decision needs.
 */
function normalizeSources(source) {
  const sessionId = firstString(
    source.source_session_id,
    source.sourceSessionId,
    source.session_id,
    source.sessionId,
  )
  const rawIds = source.source_message_ids ?? source.sourceMessageIds ?? source.message_ids ?? []
  const messageIds = Array.isArray(rawIds)
    ? rawIds.map((id) => String(id ?? '')).filter((id) => id !== '')
    : []
  const single = firstString(source.message_id, source.messageId)
  if (single && !messageIds.includes(single)) messageIds.push(single)

  const rows = []
  if (sessionId !== '' || messageIds.length > 0) {
    rows.push({ sessionId, messageIds })
  }
  // A nested list is not what this API sends, but reading one costs nothing here
  // and keeps a future shape change from silently dropping provenance instead.
  for (const entry of Array.isArray(source.sources) ? source.sources : []) {
    const nested = normalizeSources(entry ?? {})
    rows.push(...nested)
  }
  return rows
}

export function normalizeDraft(raw) {
  const source = raw && typeof raw === 'object' ? raw : {}
  return {
    id: firstString(source.id, source.memory_id, source.memoryId, source.draft_id, source.draftId),
    scope: normalizeScope(source.scope),
    topicKey: firstString(source.topic_key, source.topicKey),
    content: firstString(source.content, source.text),
    confidence: numberOrNull(source.confidence),
    sources: normalizeSources(source),
    createdAt: firstString(source.created_at, source.createdAt),
  }
}

export function normalizeMemory(raw) {
  const source = raw && typeof raw === 'object' ? raw : {}
  return {
    id: firstString(source.id, source.memory_id, source.memoryId),
    scope: normalizeScope(source.scope),
    topicKey: firstString(source.topic_key, source.topicKey),
    content: firstString(source.content, source.text),
    confidence: numberOrNull(source.confidence),
    status: normalizeStatus(source.status),
    sources: normalizeSources(source),
    createdAt: firstString(source.created_at, source.createdAt),
    updatedAt: firstString(source.updated_at, source.updatedAt),
  }
}

function itemsOf(payload) {
  if (Array.isArray(payload)) return payload
  if (payload && Array.isArray(payload.items)) return payload.items
  return []
}

/** Records without an id cannot be addressed, so they are dropped, not shown. */
export function normalizeDraftList(payload) {
  return itemsOf(payload)
    .map(normalizeDraft)
    .filter((item) => item.id !== '')
}

export function normalizeMemoryList(payload) {
  return itemsOf(payload)
    .map(normalizeMemory)
    .filter((item) => item.id !== '')
}

/**
 * A write may answer with the object it stored or with a one-item list, the same
 * two shapes the read endpoints use. Unwrapping both here keeps the store from
 * having to know which kind of call it is finishing, and a body with no
 * addressable id still reads as "nothing came back" rather than as a record with
 * an empty key.
 */
function asRecord(payload) {
  if (Array.isArray(payload)) return payload[0] ?? null
  if (payload && Array.isArray(payload.items)) return payload.items[0] ?? null
  return payload && typeof payload === 'object' ? payload : null
}

export function normalizeMemoryResponse(payload) {
  const record = asRecord(payload)
  const normalized = record ? normalizeMemory(record) : null
  return normalized && normalized.id ? normalized : null
}

/**
 * Failure copy, decided here rather than in the component.
 *
 * `http.js` whitelists which *server* messages may be shown. This page declines
 * that option entirely: a memory write is a durable change, and a server string
 * such as a constraint violation or a Redis error would read like an instruction
 * about data the user can see. Every branch below is a sentence this file owns,
 * keyed on the machine code and the status alone.
 */
export const MEMORY_ERROR_COPY = {
  unauthorized: '登录状态已经失效，请重新登入后再处理记忆。',
  crossAccount: '这条记忆不属于当前账号，页面没有替你操作它。',
  notFound: '这条记忆已经不在了，可能已经被处理过，列表已刷新为最新状态。',
  invalidRequest: '这个操作没有被接受，内容没有被改动，请刷新后重试。',
  conflict: '这条记忆已经被处理过了，本次操作没有生效，列表已刷新。',
  unavailable: '服务暂时没有完成这个操作，你的输入没有被丢弃，请稍后重试。',
  network: '无法连接到服务，本次操作没有发出。',
  notWired: '这个环境还没有开通记忆接口，因此列表里没有内容。',
  alreadyConfirmed: '这条草稿已经确认过了，不需要再点一次，列表已刷新。',
  archivedRow: '这条记忆已经归档，不再是可操作的生效记忆。',
  supersededRow: '这条记忆已经被更新的一条替代，页面显示的是最新状态。',
  notActiveRow: '这条记忆当前的状态不允许这个操作，列表已刷新为最新状态。',
  evidenceMissing:
    '这条记忆引用的对话已经不在存储里了，需要先新建一条草稿才能再次确认。',
}

const CROSS_ACCOUNT_CODES = new Set(['forbidden', 'cross_user', 'ownership_mismatch'])
const NOT_FOUND_CODES = new Set(['not_found'])
const INVALID_CODES = new Set(['invalid_request'])
const CONFLICT_CODES = new Set(['conflict', 'already_processed', 'version_mismatch'])

/**
 * The lifecycle answers all arrive as 409, and the server keeps them distinct on
 * purpose: each one has a different next move for the person reading the screen.
 * Collapsing them into "conflict" here would throw that work away at the last
 * step, so each code gets its own sentence and the shared conflict copy is only
 * the fallback for a conflict this table does not know.
 */
const LIFECYCLE_COPY = {
  memory_already_confirmed: MEMORY_ERROR_COPY.alreadyConfirmed,
  memory_archived: MEMORY_ERROR_COPY.archivedRow,
  memory_superseded: MEMORY_ERROR_COPY.supersededRow,
  memory_not_active: MEMORY_ERROR_COPY.notActiveRow,
  memory_evidence_missing: MEMORY_ERROR_COPY.evidenceMissing,
}

/** @returns {{copy: string, code: string}} `code` is machine-readable only. */
export function memoryError(error) {
  const code = typeof error?.code === 'string' ? error.code : ''
  const status = Number(error?.status) || 0

  if (code === UNBOUND_CODE) return { copy: MEMORY_ERROR_COPY.notWired, code: UNBOUND_CODE }
  if (LIFECYCLE_COPY[code]) return { copy: LIFECYCLE_COPY[code], code }
  if (code === 'unauthorized' || status === 401) {
    return { copy: MEMORY_ERROR_COPY.unauthorized, code: 'unauthorized' }
  }
  if (CROSS_ACCOUNT_CODES.has(code) || status === 403) {
    return { copy: MEMORY_ERROR_COPY.crossAccount, code: 'forbidden' }
  }
  if (NOT_FOUND_CODES.has(code) || status === 404) {
    return { copy: MEMORY_ERROR_COPY.notFound, code: 'not_found' }
  }
  if (INVALID_CODES.has(code) || status === 400) {
    return { copy: MEMORY_ERROR_COPY.invalidRequest, code: 'invalid_request' }
  }
  if (CONFLICT_CODES.has(code) || status === 409) {
    return { copy: MEMORY_ERROR_COPY.conflict, code: 'conflict' }
  }
  if (code === 'network_error') {
    return { copy: MEMORY_ERROR_COPY.network, code: 'network_error' }
  }
  return { copy: MEMORY_ERROR_COPY.unavailable, code: code || `http_${status || 0}` }
}

export const MemoryAdapterMode = {
  Http: 'http',
  Mock: 'mock',
}

/**
 * Which transport the memory screen uses, decided from the build environment
 * alone so the rule stays a table a test can pin rather than a conditional buried
 * in the boot sequence.
 *
 * The real backend is the default in every case: an unset, empty, `off`, `false`
 * or otherwise spelled flag all mean HTTP. The mock needs the exact word `on`
 * (case and padding aside) *and* the dev server — an environment slip must not be
 * able to put invented records in front of somebody using a production build.
 *
 * @param {{DEV?: unknown, VITE_MEMORY_MOCK?: unknown}} env
 * @returns {'http'|'mock'}
 */
export function resolveMemoryAdapterMode(env = {}) {
  const requested =
    typeof env?.VITE_MEMORY_MOCK === 'string' ? env.VITE_MEMORY_MOCK.trim().toLowerCase() : ''
  if (env?.DEV === true && requested === 'on') return MemoryAdapterMode.Mock
  return MemoryAdapterMode.Http
}

/**
 * The real transport: this port over `fetch`, installed by `main.js`. The reads are
 * the two listings, the writes are the three transitions, and none of them sends a
 * body that could name a state — `confirm` of a draft the server no longer holds
 * comes back as a 409 with the code that says which state it found.
 */
export function createHttpMemoryAdapter() {
  return {
    name: 'http',
    listDrafts: (options) =>
      request(memoryListPath(MemoryListState.Pending), { signal: options?.signal }),
    listMemories: (options) =>
      request(memoryListPath(MemoryListState.Active), { signal: options?.signal }),
    addMemory: (body) => request(MEMORY_PATH, { method: 'POST', body }),
    confirmDraft: (draftId) =>
      request(memoryActionPath(draftId, 'confirm'), { method: 'POST' }),
    rejectDraft: (draftId) =>
      request(memoryActionPath(draftId, 'reject'), { method: 'POST' }),
    archiveMemory: (memoryId) =>
      request(memoryActionPath(memoryId, 'archive'), { method: 'POST' }),
  }
}

export const UNBOUND_CODE = 'memory_service_unavailable'

function unboundError(operation) {
  recordDevIssue('memory.adapter.unbound', { operation })
  return new ApiError(MEMORY_ERROR_COPY.notWired, {
    code: UNBOUND_CODE,
    status: 503,
    path: operation,
  })
}

/**
 * What the page ships with. A memory endpoint that does not exist must not be
 * called, and a memory list that cannot be read must not look empty by accident —
 * so every operation fails with one stable, self-describing error.
 */
export function createUnboundMemoryAdapter() {
  const refuse = (operation) => () => Promise.reject(unboundError(operation))
  return {
    name: 'unbound',
    listDrafts: refuse('listDrafts'),
    addMemory: refuse('addMemory'),
    confirmDraft: refuse('confirmDraft'),
    rejectDraft: refuse('rejectDraft'),
    listMemories: refuse('listMemories'),
    archiveMemory: refuse('archiveMemory'),
  }
}

const REQUIRED_OPERATIONS = [
  'listDrafts',
  'addMemory',
  'confirmDraft',
  'rejectDraft',
  'listMemories',
  'archiveMemory',
]

export function assertMemoryAdapter(adapter) {
  if (!adapter || typeof adapter !== 'object') {
    throw new Error('记忆适配器必须是一个对象。')
  }
  const missing = REQUIRED_OPERATIONS.filter((name) => typeof adapter[name] !== 'function')
  if (missing.length > 0) {
    throw new Error(`记忆适配器缺少操作：${missing.join('、')}。`)
  }
  return adapter
}

let activeAdapter = createUnboundMemoryAdapter()

/**
 * The adapter is chosen once at boot in the real app, but the page still has to
 * label its data honestly if anything swaps it later, so the change is announced
 * rather than left for a reader to poll for. This module stays free of Vue; the
 * store turns these callbacks into reactive state.
 */
const adapterListeners = new Set()

function notifyAdapterChange() {
  const name = memoryAdapterName()
  for (const listener of [...adapterListeners]) listener(name)
}

export function onMemoryAdapterChange(listener) {
  if (typeof listener !== 'function') return () => {}
  adapterListeners.add(listener)
  return () => adapterListeners.delete(listener)
}

/** Installed once at boot; no call site may pick an adapter for itself. */
export function setMemoryAdapter(adapter) {
  assertMemoryAdapter(adapter)
  activeAdapter = adapter
  notifyAdapterChange()
  return activeAdapter
}

export function resetMemoryAdapter() {
  activeAdapter = createUnboundMemoryAdapter()
  notifyAdapterChange()
  return activeAdapter
}

export function memoryAdapter() {
  return activeAdapter
}

export function memoryAdapterName() {
  return typeof activeAdapter?.name === 'string' ? activeAdapter.name : 'unbound'
}

/**
 * The page reads this to label what it is showing. A mock list presented without
 * that label is fabricated data wearing a real UI, which is the one thing this
 * screen must never do.
 */
export function isMockMemoryAdapter() {
  return memoryAdapterName() === 'mock'
}

export function listDrafts(options = {}) {
  return activeAdapter.listDrafts(options)
}

/**
 * The only route from a form to an add request: it builds the body itself, so a
 * caller cannot hand the wire a `topic_key` for a global memory or a provenance
 * field the endpoint refuses. The adapter receives the finished body.
 */
export function addMemory(form) {
  return activeAdapter.addMemory(memoryAddBody(form))
}

export function confirmDraft(draftId, options = {}) {
  return activeAdapter.confirmDraft(String(draftId ?? ''), options)
}

export function rejectDraft(draftId, options = {}) {
  return activeAdapter.rejectDraft(String(draftId ?? ''), options)
}

export function listMemories(options = {}) {
  return activeAdapter.listMemories(options)
}

export function archiveMemory(memoryId, options = {}) {
  return activeAdapter.archiveMemory(String(memoryId ?? ''), options)
}
