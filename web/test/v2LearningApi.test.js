import { beforeEach, afterEach, expect, it, vi } from 'vitest'
import { setTokenProvider } from '../src/api/http.js'
import { startV2Learning, currentV2Learning, answerV2Learning, actionV2Learning, cancelV2Learning, progressV2Learning, correctV2Learning, parseLearningResponse } from '../src/api/v2Learning.js'
let calls
beforeEach(() => {
  calls = []
  vi.stubEnv('VITE_API_BASE_URL', 'https://api.test')
  setTokenProvider(() => 'synthetic-only')
  vi.stubGlobal('fetch', async (url, init) => {
    calls.push({ url: String(url), init })
    return new Response(JSON.stringify({ status_code: 1000, learning: { id: 'original-uuid', status: 'waiting_answer' } }))
  })
})
afterEach(() => { vi.unstubAllGlobals(); vi.unstubAllEnvs() })

it('uses native nonstream routes and exact explicit bodies without clean fields/defaults', async () => {
  const sessionId = '9007199254740993123'
  const start = { sessionId, topic: 'Redis' }
  const answer = { sessionId, learningSessionId: 'original-uuid', questionId: 'question-uuid', submissionId: 'explicit-operation-key', answer: '惰性删除' }
  await startV2Learning(start); await currentV2Learning(sessionId)
  await answerV2Learning(answer); await answerV2Learning(answer)
  await actionV2Learning({ ...answer, action: 'skip' })
  await cancelV2Learning({ sessionId, learningSessionId: 'original-uuid' })
  await progressV2Learning('Redis'); await correctV2Learning({ attemptId: 'attempt-uuid', effectiveTotal: 25, reason: '明确纠正' })
  expect(JSON.parse(calls[0].init.body)).toEqual(start)
  expect(JSON.parse(calls[2].init.body)).toEqual(answer)
  expect(calls[2].init.body).toBe(calls[3].init.body)
  expect(calls.map((c) => new URL(c.url).pathname.split('/').at(-1))).toEqual(['start', 'current', 'answer', 'answer', 'action', 'cancel', 'progress', 'correct'])
  expect(calls[1].url).toContain(sessionId)
  expect(calls[0].init.headers.authorization).toBe('Bearer synthetic-only')
  expect(calls).toHaveLength(8)
})

it('preserves native identities, scores, sources and text without numeric rounding', () => {
  const response = parseLearningResponse('{"status_code":1000,"progress":{"id":9007199254740993123,"user_id":9007199254740993124,"mastery_score":25,"confidence":0.9},"learning":{"id":"uuid","sessionId":"9007199254740993125","question":{"question":"文本 \\"id\\":9007199254740993123","sources":[{"documentId":"doc-uuid","documentVersion":2,"startLine":5,"endLine":9}]}}}')
  expect(response.progress).toEqual({ id: '9007199254740993123', user_id: '9007199254740993124', mastery_score: 25, confidence: 0.9 })
  expect(response.learning.question.sources[0]).toEqual({ documentId: 'doc-uuid', documentVersion: 2, startLine: 5, endLine: 9 })
  expect(response.learning.question.question).toBe('文本 "id":9007199254740993123')
  expect(() => parseLearningResponse('{bad')).toThrow()
})

it('rejects native HTTP-200 business failure and preserves cancellation; never retries', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response('{"status_code":5001,"status_msg":"原状态冲突"}')))
  await expect(startV2Learning({ sessionId: '1', topic: 'Redis' })).rejects.toMatchObject({ nativeCode: 5001, status: 200, message: '原状态冲突' })
  expect(fetch).toHaveBeenCalledTimes(1)
  const aborted = new DOMException('cancelled', 'AbortError')
  vi.stubGlobal('fetch', async () => { throw aborted })
  await expect(answerV2Learning({})).rejects.toBe(aborted)
})
