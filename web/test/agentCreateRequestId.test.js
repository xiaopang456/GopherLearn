import { beforeEach, expect, it, vi } from 'vitest'
import { createSession } from '../src/api/sessions.js'
import { setTokenProvider } from '../src/api/http.js'
import { jsonResponse } from './helpers/transport.js'

let bodies
beforeEach(() => {
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'synthetic.token')
  bodies = []
  globalThis.fetch = async (_url, init) => {
    bodies.push(JSON.parse(init.body))
    return jsonResponse({ data: { id: 'agent-session', mode: 'learning_agent' }, error: null })
  }
})

it('passes the caller operation key unchanged across retries and new operations', async () => {
  const operation = { mode: 'learning_agent', title: '学习 Agent', requestId: 'create-operation-001' }
  await createSession(operation)
  await createSession(operation)
  await createSession({ ...operation, requestId: 'create-operation-002' })
  expect(bodies).toEqual([
    operation, operation, { ...operation, requestId: 'create-operation-002' },
  ])
})

it('does not generate a missing key and leaves ordinary creation unchanged', async () => {
  await createSession({ mode: 'learning_agent', title: 'Agent' })
  await createSession({ mode: 'chat', title: '普通', requestId: 'not-used-by-chat' })
  expect(bodies).toEqual([
    { mode: 'learning_agent', title: 'Agent' },
    { mode: 'chat', title: '普通' },
  ])
})
