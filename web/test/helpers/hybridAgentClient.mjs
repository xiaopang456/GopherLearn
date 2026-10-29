// Invoked by hybrid's permanent Go regression against its real local Handler.
import assert from 'node:assert/strict'
import process from 'node:process'
import { useChat } from '../../src/composables/useChat.js'
import { setTokenProvider } from '../../src/api/http.js'
import { createSession } from '../../src/api/sessions.js'
import { streamMessage } from '../../src/api/stream.js'

const base = process.env.HYBRID_TEST_URL
const nativeFetch = globalThis.fetch
globalThis.fetch = (url, init) => nativeFetch(new URL(url, base), init)
const login = await nativeFetch(`${base}/api/v1/auth/login`, {
  method: 'POST', headers: { 'content-type': 'application/json' },
  body: JSON.stringify({ email: 'learner@invalid.example', password: 'synthetic-password' }),
})
const auth = await login.json()
assert.equal(login.status, 200)
setTokenProvider(() => auth.data.token)
const chat = useChat()
const agent = await chat.createSession('learning_agent')
assert.ok(agent)
await chat.send('你好')
const reply = chat.messages.value.at(-1)
assert.equal(reply.status, 'done')
assert.equal(reply.content, '你好，公开片段')
assert.equal(typeof reply.serverId, 'string')
assert.ok(reply.runId)
assert.equal(reply.originalV2.answer, reply.content)
const responseId = reply.serverId
chat.closeSession()
await chat.openSession(agent)
const history = chat.messages.value.filter((message) => message.role === 'assistant')
assert.equal(history.length, 1)
assert.equal(history[0].id, responseId)
assert.equal(history[0].content, reply.content)
// Reuse the original creation operation through the actual API, without minting
// a new operation key on the caller's behalf; send replay is tested server-side.
const ordinary = await createSession({ mode: 'chat', title: '普通回归' })
const events = []
await streamMessage({ sessionId: ordinary.id, content: '普通问题', onEvent: (event) => events.push(event) })
assert.ok(events.some((event) => event.kind === 'completed'))
console.log(JSON.stringify({ agent, runId: reply.runId, messageId: responseId, content: reply.content, historyCount: history.length, ordinaryCompleted: true }))
