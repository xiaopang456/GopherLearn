import assert from 'node:assert/strict'
import process from 'node:process'
import { setTokenProvider } from '../../src/api/http.js'
import { createV2Memory, listV2Memory, correctV2Memory, archiveV2Memory, deleteV2Memory, revisionsV2Memory } from '../../src/api/v2Memory.js'
const realFetch = globalThis.fetch
globalThis.fetch = (url, options) => realFetch(new URL(url, process.env.HYBRID_MEMORY_URL), options)
const login = await fetch('/api/v1/auth/login', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ email: 'learner@invalid.example', password: 'synthetic-password' }) })
const authenticated = await login.json()
assert.equal(login.status, 200)
assert.equal(authenticated.error, null)
setTokenProvider(() => authenticated.data.token)
const body = { scope: 'topic', topic_key: 'redis', subject: '个人理解', content: '我用队列理解 Redis', durability: 'stable', reason: '用户明确填写', idempotency_key: 'socket-manual-memory-001' }
const created = await createV2Memory(body)
const id = created.memory.id
assert.equal(typeof id, 'string')
assert.equal(created.memory.status, 'active')
assert.equal((await createV2Memory(body)).replay, true)
assert.equal((await listV2Memory()).memories[0].id, id)
const corrected = await correctV2Memory(id, { subject: body.subject, content: '我用任务队列理解 Redis', tags: ['redis'], reason: '用户修正' })
assert.equal(corrected.memory.content, '我用任务队列理解 Redis')
assert.equal((await revisionsV2Memory(id)).revisions.length, 2)
assert.equal((await archiveV2Memory(id, '用户归档')).memory.status, 'archived')
assert.equal((await listV2Memory({ status: 'active' })).memories.length, 0)
assert.equal((await listV2Memory({ status: 'archived' })).memories[0].id, id)
await deleteV2Memory(id, '用户删除')
const deleted = (await listV2Memory({ status: 'deleted' })).memories[0]
assert.equal(deleted.id, id)
assert.equal(deleted.content, '')
await assert.rejects(() => correctV2Memory(id, { subject: '理解', content: '不能复活', tags: ['redis'], reason: '旧页面操作' }), (error) => error.status === 404)
console.log(JSON.stringify({ id, final_status: deleted.status, transport: 'local socket, original DAO, no provider' }))
