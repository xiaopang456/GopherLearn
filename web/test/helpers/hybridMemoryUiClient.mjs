import assert from 'node:assert/strict'
import process from 'node:process'
import { ref } from 'vue'
import { setTokenProvider, request } from '../../src/api/http.js'
import { createSession } from '../../src/api/sessions.js'
import { useV2MemoryPage } from '../../src/composables/useV2MemoryPage.js'
import { useV2MemoryDraft } from '../../src/composables/useV2MemoryDraft.js'
const realFetch = globalThis.fetch
globalThis.fetch = (url, options) => realFetch(new URL(url, process.env.HYBRID_MEMORY_UI_URL), options)
async function login(email) {
  const response = await fetch('/api/v1/auth/login', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ email, password: 'synthetic-password' }) })
  const authenticated = await response.json(); assert.equal(authenticated.error, null)
  return authenticated.data.token
}
const ownerToken = await login('learner@invalid.example'), otherToken = await login('other@invalid.example')
let token = ownerToken; setTokenProvider(() => token)
const page = useV2MemoryPage()
const fields = { scope: 'topic', topic_key: 'redis', subject: '个人理解', content: '我用队列理解 Redis', tags: ['redis'], durability: 'stable', reason: '用户明确填写' }
const written = await page.operation.start('create', fields)
assert.equal(written.memory.status, 'active')
const originalKey = page.operation.pending.value.body.idempotency_key
assert.equal((await page.operation.retry()).replay, true)
assert.equal(page.operation.pending.value.body.idempotency_key, originalKey)
const originalID = written.memory.id
await page.refresh(); assert.equal(page.memories.value[0].id, originalID)
await page.showRevisions(originalID); assert.equal(page.revisions.value.items.length, 1)
const session = await createSession({ mode: 'learning_agent', title: '治理测试', requestId: 'create-memory-ui-001' })
async function run(key) { return request(`/api/v1/sessions/${session.id}/messages`, { method: 'POST', body: { requestId: key, content: '请记住我用队列理解 Redis' } }) }
const result = await run('send-memory-ui-001')
assert.equal(typeof result.memoryDraft.draftId, 'string')
assert.ok(result.memoryDraft.conflictIds.includes(originalID))
const current = ref(result.memoryDraft), runID = ref(result.runId)
const card = useV2MemoryDraft(() => current.value, () => runID.value)
assert.equal(await card.decide('replace', '没有明确选目标'), null)
const replaced = await card.decide('replace', '用户明确选择原目标', originalID)
assert.equal(replaced.draft.status, 'confirmed'); assert.equal(replaced.memory.status, 'active')
assert.notEqual(replaced.memory.id, originalID)
const newID = replaced.memory.id
const oldCancel = await card.decide('cancel', '旧卡明确操作')
assert.equal(oldCancel.draft.status, 'confirmed'); assert.equal(oldCancel.replay, true)
await page.operation.start('correct', { subject: '理解修正', content: '我用任务队列理解 Redis', tags: ['redis'], reason: '用户修正' }, newID)
await page.showRevisions(newID); assert.ok(page.revisions.value.items.length >= 2)
assert.equal((await page.operation.start('archive', { reason: '用户归档' }, newID)).memory.status, 'archived')
await page.operation.start('delete', { reason: '用户删除' }, newID)
const deletedReplay = await card.decide('add', '旧卡明确重放')
assert.equal(deletedReplay.memory.status, 'deleted'); assert.equal(deletedReplay.replay, true)
const agentReplay = await run('send-memory-ui-001')
assert.equal(agentReplay.replayed, true); assert.equal(agentReplay.memoryDraft.draftId, result.memoryDraft.draftId)
const second = await run('send-memory-ui-002')
current.value = second.memoryDraft; runID.value = second.runId
token = otherToken
assert.equal(await card.decide('add', '跨用户操作'), null)
assert.ok(card.operation.error.value)
token = ownerToken
assert.equal((await card.decide('cancel', '用户明确拒绝')).draft.status, 'cancelled')
const third = await run('send-memory-ui-003')
current.value = third.memoryDraft; runID.value = third.runId
assert.equal(await card.decide('add', '旧过期卡片'), null)
assert.equal(card.outcome.value, null); assert.ok(card.operation.error.value)
await page.refresh(); assert.equal(page.memories.value.length, 0)
// Native global path remains reachable; original SQL lock cannot succeed on SQLite.
assert.equal(await page.operation.start('create', { scope: 'global', subject: '个人偏好', content: '我喜欢短讲解', durability: 'stable', reason: '用户明确填写' }), null)
assert.ok(page.operation.error.value)
card.dispose(); page.dispose()
console.log(JSON.stringify({ originalID, newID, draftID: result.memoryDraft.draftId, states: ['confirmed', 'cancelled', 'deleted'], transport: 'actual frontend composables/native API/auth/Service/DAO, local socket only' }))
