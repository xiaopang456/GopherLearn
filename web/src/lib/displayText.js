/**
 * The wording of the states, levels and outcomes the server names.
 *
 * The server answers in its own words — `ready`, `active`, `waiting_answer`,
 * `time_bound` — and those words are correct there and unreadable here. This
 * module is where they become Chinese: one table per domain, all in one file.
 *
 * One file per set of tables is the point. A state that two components show has
 * to be called the same thing by both, and a table written inside a component is
 * a table that only that component obeys — the day the two disagree, the wrong
 * one wins wherever it was written last. The domains are kept apart on purpose:
 * a document's index state and a quiz's state are different things that happen
 * to be words, and merging them into one table would let one answer the other.
 *
 * A value no table knows is never shown as it arrived. It gets the neutral
 * wording for its domain ("状态未识别"), because a learner reading
 * `cleanup_pending` learns nothing, and a learner reading a raw key may well
 * take it for the name of a feature.
 *
 * Where the raw value is still worth having — a developer looking at a state
 * the table does not know — it is shown by the component behind the debug
 * switch, not here.
 */

/**
 * The state a status tag shows.
 *
 * The tag is used by the document list and by the memory list, so this one
 * table is what makes a document and a memory read as the same kind of thing.
 * The tone travels with the wording: a state is one fact, and its colour and
 * its name belong together.
 */
export const STATUS_TAG = Object.freeze({
  // document index states
  ready: { text: '就绪', tone: 'success' },
  failed: { text: '失败', tone: 'error' },
  pending: { text: '等待处理', tone: 'warning' },
  indexing: { text: '索引中', tone: 'info' },
  cleanup_pending: { text: '待清理', tone: 'warning' },
  deleting: { text: '删除中', tone: 'info' },
  deleted: { text: '已删除', tone: 'default' },
  // memory states
  active: { text: '生效中', tone: 'success' },
  archived: { text: '已归档', tone: 'default' },
  superseded: { text: '已被替代', tone: 'warning' },
})

/** The state of a quiz or a learning session. */
export const LEARNING_STATUS = Object.freeze({
  created: '刚创建',
  asking: '出题中',
  waiting_answer: '等你作答',
  completed: '已完成',
  cancelled: '已取消',
  expired: '已过期',
})

/** How well a topic is known. */
export const LEARNING_LEVEL = Object.freeze({
  insufficient: '尚不足',
  basic: '入门',
  intermediate: '进阶',
  advanced: '熟练',
})

/** Which layer a memory belongs to. */
export const MEMORY_SCOPE = Object.freeze({
  topic: '主题',
  global: '全局',
})

/** How long a memory lasts. */
export const MEMORY_DURABILITY = Object.freeze({
  time_bound: '有期限',
  stable: '长期有效',
})

/** What the three buttons on a draft card do. */
export const DRAFT_ACTION = Object.freeze({
  add: '确认新增',
  replace: '确认替换',
  cancel: '拒绝草稿',
})

/**
 * The wording for one value, or the neutral wording when the table does not know it.
 *
 * The lookup ignores case and surrounding spaces, the way the server resolves a
 * name: a value that arrived padded or capitalised is the same value.
 */
function wording(table, value, fallback) {
  if (typeof value !== 'string') return fallback
  const key = value.trim().toLowerCase()
  if (key === '') return fallback
  return Object.hasOwn(table, key) ? table[key] : fallback
}

/** The tag a status is shown with. */
export function statusTag(status) {
  return wording(STATUS_TAG, status, { text: '状态未识别', tone: 'default' })
}

/** The wording for a status, for a place that shows the state as a sentence. */
export function statusText(status) {
  return statusTag(status).text
}

/** The state of a quiz. */
export function learningStatusText(status) {
  return wording(LEARNING_STATUS, status, '状态未识别')
}

/** How well a topic is known. */
export function learningLevelText(level) {
  return wording(LEARNING_LEVEL, level, '等级未识别')
}

/** Which layer a memory belongs to. */
export function memoryScopeText(scope) {
  return wording(MEMORY_SCOPE, scope, '范围未说明')
}

/** How long a memory lasts. */
export function memoryDurabilityText(durability) {
  return wording(MEMORY_DURABILITY, durability, '有效期未说明')
}

/** What a draft-card button does. */
export function draftActionText(action) {
  return wording(DRAFT_ACTION, action, '确认')
}
