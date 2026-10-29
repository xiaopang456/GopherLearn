import assert from 'node:assert/strict'
import process from 'node:process'
import { setTokenProvider } from '../../src/api/http.js'
import { createSession } from '../../src/api/sessions.js'
import { startV2Learning, currentV2Learning, answerV2Learning, progressV2Learning, correctV2Learning, cancelV2Learning } from '../../src/api/v2Learning.js'
const realFetch = globalThis.fetch
globalThis.fetch = (url, init) => realFetch(new URL(url, process.env.HYBRID_TEST_URL), init)
const login = await fetch('/api/v1/auth/login', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ email: 'learner@invalid.example', password: 'synthetic-password' }) })
const authenticated = await login.json()
assert.equal(login.status, 200)
setTokenProvider(() => authenticated.data.token)
const session = await createSession({ mode: 'learning_agent', title: '原学习 socket', requestId: 'learning-socket-create' })
const learning = (await startV2Learning({ sessionId: session.id, topic: 'Redis' })).learning
assert.equal(learning.status, 'waiting_answer')
assert.equal(learning.totalQuestions, 3)
assert.ok(learning.question.sources[0].documentId)
assert.equal(learning.question.referenceAnswer, undefined)
assert.equal((await currentV2Learning(session.id)).learning.id, learning.id)
const body = { sessionId: session.id, learningSessionId: learning.id, questionId: learning.question.id, submissionId: 'socket-answer-001', answer: '惰性删除和定期删除' }
const result = (await answerV2Learning(body)).result
assert.equal(result.attempt.effectiveTotal, 80)
assert.equal(result.learning.currentQuestion, 2)
assert.equal((await answerV2Learning(body)).result.attempt.id, result.attempt.id)
assert.equal((await progressV2Learning('Redis')).progress.mastery_score, 80)
assert.equal(typeof (await progressV2Learning('Redis')).progress.id, 'string')
assert.equal((await correctV2Learning({ attemptId: result.attempt.id, effectiveTotal: 25, reason: '明确纠正' })).attempt.effectiveTotal, 25)
assert.equal((await progressV2Learning('Redis')).progress.mastery_score, 25)
assert.equal((await cancelV2Learning({ sessionId: session.id, learningSessionId: learning.id })).learning.status, 'cancelled')
await assert.rejects(() => currentV2Learning(session.id))
console.log(JSON.stringify({ transport: 'actual frontend API, native auth/service/DAO, local socket', task: learning.id, attempt: result.attempt.id }))
