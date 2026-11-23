import assert from 'node:assert/strict'
import process from 'node:process'

import { setTokenProvider } from '../../src/api/http.js'
import { useChat } from '../../src/composables/useChat.js'
import { useTopicSelection } from '../../src/composables/useTopicSelection.js'

// The real chat page chain: the actual useChat and useTopicSelection
// composables, the actual HTTP/SSE routes and the actual context assembly.
// The server runs a scripted model, so nothing is paid for and the Go test can
// assert exactly which prompt each turn assembled.
const server = process.env.HYBRID_CHAT_TOPIC_URL
assert.ok(server, 'HYBRID_CHAT_TOPIC_URL is required')

const realFetch = globalThis.fetch
globalThis.fetch = (url, options) => realFetch(new URL(url, server), options)

async function login(email) {
  const response = await fetch('/api/v1/auth/login', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ email, password: 'synthetic-password' }),
  })
  const authenticated = await response.json()
  assert.equal(authenticated.error, null)
  return authenticated.data.token
}

async function waitFor(probe, label, timeoutMs = 15000) {
  const started = Date.now()
  for (;;) {
    if (await probe()) return
    if (Date.now() - started > timeoutMs) throw new Error(`timed out waiting for ${label}`)
    await new Promise((resolve) => setTimeout(resolve, 50))
  }
}

const token = await login('learner@invalid.example')
setTokenProvider(() => token)

const chat = useChat()
const flow = useTopicSelection(chat)
chat.loadSessions()
const sessionA = await chat.createSession('learning_agent')
assert.ok(sessionA, 'agent session A')
const capability = await flow.refreshCapability()
assert.equal(capability.supported, true, 'confirmed deployment must offer the flow')

const lastAssistant = () => [...chat.messages.value].reverse().find((item) => item.role === 'assistant')
const settle = async () => {
  await waitFor(() => !chat.streaming.value, 'the turn to settle')
  await waitFor(() => {
    const message = lastAssistant()
    return message && message.status === 'done'
  }, 'the answer to be final')
}
async function askAndWait(question, expectPanel) {
  chat.draft.value = question
  const submit = flow.submit()
  if (expectPanel) {
    await waitFor(() => flow.prompt.value.visible, 'the consent panel')
  }
  await submit
  return submit
}

// Turn 1 — select: one topic confirmed, the answer must carry its receipt.
flow.enabled.value = true
chat.draft.value = '请讲讲鉴权里的过期时间校验'
flow.submit()
await waitFor(() => flow.prompt.value.visible, 'the consent panel for turn 1')
assert.ok(flow.prompt.value.candidates.some((item) => item.topicKey === 'jwt-auth'), 'jwt-auth must be suggested')
flow.toggleSelection('jwt-auth')
await flow.confirmSelection()
await settle()
const selectedReceipt = lastAssistant()?.usedTopicKeys ?? []
assert.deepEqual(selectedReceipt, ['jwt-auth'], 'the selected topic must be the receipt')

// Turn 2 — skip: the same request still asks, with no topic personalisation.
chat.draft.value = '鉴权 token 过期怎么处理'
flow.submit()
await waitFor(() => flow.prompt.value.visible, 'the consent panel for turn 2')
flow.skipSelection()
await settle()
const skipReceipt = lastAssistant()?.usedTopicKeys ?? []
assert.deepEqual(skipReceipt, [], 'a skipped turn must not use topic records')

// Turn 3 — switch A -> B -> A with the panel open: the panel must not survive
// and the pending question must not be sent into either session.
const sessionB = await chat.createSession('learning_agent')
assert.ok(sessionB, 'agent session B')
await chat.openSession(sessionA)
chat.draft.value = '鉴权里的会话过期怎么排查'
flow.submit()
await waitFor(() => flow.prompt.value.visible, 'the consent panel for turn 3')
await chat.openSession(sessionB)
await chat.openSession(sessionA)
const panelAfterSwitch = flow.prompt.value.visible
const staleQuestionSent = chat.messages.value.some((item) => item.role === 'user' && item.content === '鉴权里的会话过期怎么排查')
assert.equal(panelAfterSwitch, false, 'the panel must not survive a session switch')
assert.equal(staleQuestionSent, false, 'the retired question must not be sent')

// A fresh question after the switch keeps its own identity and loads nothing.
flow.enabled.value = false
chat.draft.value = '鉴权里的签名校验怎么做'
await chat.send()
await settle()
const freshReceipt = lastAssistant()?.usedTopicKeys ?? []
assert.deepEqual(freshReceipt, [], 'an unconfirmed turn after the switch must not use records')

console.log(JSON.stringify({
  selectedReceipt,
  skipReceipt,
  panelAfterSwitch,
  staleQuestionSent,
  freshReceipt,
  transport: 'actual useChat/useTopicSelection composables, actual HTTP/SSE and context assembly, scripted model only',
}))
