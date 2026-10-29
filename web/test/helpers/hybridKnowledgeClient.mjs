import assert from 'node:assert/strict'
import process from 'node:process'
import { setTimeout } from 'node:timers/promises'
import { setTokenProvider } from '../../src/api/http.js'
import { uploadV2Document, getV2Document, updateV2Document, listV2Documents,
  readV2Source, deleteV2Document } from '../../src/api/v2Documents.js'

const base = process.env.HYBRID_TEST_URL
const nativeFetch = globalThis.fetch
globalThis.fetch = (url, init) => nativeFetch(new URL(url, base), init)
const response = await nativeFetch(`${base}/api/v1/auth/login`, { method: 'POST',
  headers: { 'content-type': 'application/json' },
  body: JSON.stringify({ email: 'learner@invalid.example', password: 'synthetic-password' }) })
assert.equal(response.status, 200)
setTokenProvider(() => null)
const auth = await response.json()
setTokenProvider(() => auth.data.token)
const file = new File(['# 原标题\r\n真实原文\r\n'], 'socket.md', { type: 'text/markdown' })
const uploaded = await uploadV2Document(file)
const id = uploaded.document.id
assert.equal(uploaded.document.index_status, 'pending')
assert.equal(uploaded.document.active_version, 0)
const until = Date.now() + 8000
let document
do {
  document = (await getV2Document(id)).document
  if (document.index_status === 'ready') break
  await setTimeout(20)
} while (Date.now() < until)
assert.equal(document.index_status, 'ready')
const source = await readV2Source(id, String(document.active_version), '1', '2')
assert.equal(source.document_id, id)
assert.equal(source.document_version, 1)
assert.deepEqual(source.lines, [{ number: 1, text: '# 原标题' }, { number: 2, text: '真实原文' }])
const unchanged = await updateV2Document(id, file)
assert.equal(unchanged.unchanged, true)
assert.equal(unchanged.document.version, 1)
await assert.rejects(() => readV2Source(id, '2', '1', '2'), { code: 'document_version_not_active' })
assert.ok((await listV2Documents()).documents.some((item) => item.id === id))
await deleteV2Document(id)
assert.ok(!(await listV2Documents()).documents.some((item) => item.id === id))
console.log(JSON.stringify({ id, version: 1, source: source.lines, unchanged: true, deleteSubmitted: true }))
