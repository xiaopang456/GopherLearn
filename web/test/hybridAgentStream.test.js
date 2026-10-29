import { beforeEach, expect, it } from 'vitest'
import { streamMessage } from '../src/api/stream.js'
import { installFetchStub, sseFrame, sseResponse } from './helpers/transport.js'

let bodies
beforeEach(() => {
  bodies = []
  installFetchStub((url, init) => {
    bodies.push(JSON.parse(init.body))
    return sseResponse([sseFrame('message.completed', { message_id: '9007199254740993', content: 'answer' })])
  })
})
it('passes explicit original Agent operation fields without generating a key', async () => {
  const topicConsent = { ticket: 'explicit-ticket', selectedTopicKeys: ['jwt'] }
  const input = { sessionId: 'agent-session', content: 'question', token: 'synthetic', requestId: 'same-operation-001', knowledgePolicy: 'strict', topicConsent, streamSteps: true, onEvent() {} }
  await streamMessage(input)
  await streamMessage(input)
  expect(bodies).toEqual([0, 1].map(() => ({ content: 'question', requestId: input.requestId, knowledgePolicy: 'strict', topicConsent, stream_steps: true })))
})
it('leaves an ordinary request unchanged', async () => {
  await streamMessage({ sessionId: 'ordinary-session', content: 'question', token: 'synthetic', onEvent() {} })
  expect(bodies).toEqual([{ content: 'question' }])
})
