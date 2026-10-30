import { beforeEach, afterEach, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { setTokenProvider } from '../src/api/http.js'
import { useV2Learning } from '../src/composables/useV2Learning.js'
import V2LearningView from '../src/views/V2LearningView.vue'
import { createAppRouter } from '../src/router/index.js'
import { createMemoryHistory } from 'vue-router'
import { jsonResponse } from './helpers/transport.js'
const sid = '9007199254740993123'
const question = { id: 'question-uuid', type: 'short_answer', question: '公开原题', sources: [{ documentId: 'doc-uuid', documentName: '原文', documentVersion: 2, startLine: 5, endLine: 9 }] }
const task = { id: 'task-uuid', sessionId: sid, topic: 'Redis', status: 'waiting_answer', currentQuestion: 1, totalQuestions: 3, question }
const attempt = { id: 'attempt-uuid', status: 'completed', correctness: 50, completeness: 20, clarity: 10, autoTotal: 80, effectiveTotal: 80, feedback: '原反馈', missingPoints: ['原遗漏'], confidence: 0.9 }
let calls, responder
beforeEach(() => {
  calls = []; setTokenProvider(() => 'synthetic-only')
  responder = (url) => url.endsWith('/sessions') ? { data: { sessions: [{ id: sid, mode: 'learning_agent', title: '真实会话' }, { id: '2', mode: 'chat', title: '普通会话' }] }, error: null } : { status_code: 1000, learning: task }
  vi.stubGlobal('fetch', async (url, init) => { calls.push({ url: String(url), init }); return jsonResponse(await responder(String(url), init)) })
})
afterEach(() => vi.unstubAllGlobals())
const mountPage = () => mount(V2LearningView, { global: { stubs: { RouterLink: { props: ['to'], template: '<a :data-target="JSON.stringify(to)"><slot /></a>' } } } })
async function prepare(wrapper) {
  await flushPromises(); await wrapper.get('[data-test=session]').setValue(sid); await wrapper.get('[data-test=topic]').setValue('Redis')
}
it('binds native operations, original public fields and exact version source route without private fields', async () => {
  const w = mountPage(); await prepare(w)
  expect(w.get('[data-test=session]').text()).not.toContain('普通会话')
  await w.get('[data-test=start-form]').trigger('submit'); await flushPromises()
  expect(JSON.parse(calls.at(-1).init.body)).toEqual({ sessionId: sid, topic: 'Redis' })
  expect(w.text()).toContain('第 1 / 3 题')
  expect(JSON.parse(w.get('[data-test=source]').attributes('data-target'))).toEqual({ name: 'v2-source', params: { documentId: 'doc-uuid' }, query: { version: '2', start_line: '5', end_line: '9' } })
  responder = () => ({ status_code: 1000, result: { learning: { ...task, currentQuestion: 2, question: { ...question, id: 'q2' } }, attempt, progress: { id: '9007199254740993124', topic: 'Redis', topic_key: 'redis', mastery_score: 80, valid_attempts: 1, level: 'insufficient', confidence: 0.9 } } })
  await w.get('[data-test=answer]').setValue('原答案'); await w.get('[data-test=answer-form]').trigger('submit'); await flushPromises()
  const body = JSON.parse(calls.at(-1).init.body)
  expect(body).toMatchObject({ sessionId: sid, learningSessionId: 'task-uuid', questionId: 'question-uuid', answer: '原答案' })
  expect(body.submissionId.length).toBeLessThanOrEqual(64)
  expect(w.get('[data-test=attempt]').text()).toContain('原反馈'); expect(w.text()).toContain('原遗漏')
  expect(w.get('[data-test=mastery-score]').text()).toBe('80')
  responder = () => ({ status_code: 1000, attempt: { ...attempt, effectiveTotal: 25 }, progress: { mastery_score: 25 } })
  await w.get('[data-test=score]').setValue('25'); await w.get('[data-test=reason]').setValue('明确纠正'); await w.get('[data-test=correct-form]').trigger('submit'); await flushPromises()
  expect(JSON.parse(calls.at(-1).init.body)).toEqual({ attemptId: 'attempt-uuid', effectiveTotal: 25, reason: '明确纠正' })
  expect(w.text()).toContain('得分 25'); expect(w.get('[data-test=mastery-score]').text()).toBe('25')
  responder = () => ({ status_code: 1000, result: { learning: task, attempt: { status: 'skipped' } } })
  await w.get('[data-test=skip]').trigger('click'); await flushPromises()
  expect(JSON.parse(calls.at(-1).init.body)).toMatchObject({ action: 'skip', questionId: 'q2', learningSessionId: 'task-uuid' })
  responder = () => ({ status_code: 1000, learning: { ...task, status: 'cancelled', question: null } })
  await w.get('[data-test=cancel]').trigger('click'); await flushPromises(); expect(w.get('[data-test=task]').text()).toContain('已取消')
  responder = () => ({ status_code: 1000, progress: { mastery_score: 25 } })
  await w.get('[data-test=progress]').trigger('click'); await flushPromises(); expect(calls.at(-1).url).toContain('/progress?topic=Redis')
  responder = () => ({ status_code: 8002, status_msg: '当前没有可恢复的学习测试' })
  await w.get('[data-test=current]').trigger('click'); await flushPromises()
  expect(w.find('[data-test=task]').exists()).toBe(false); expect(w.find('[data-test=attempt]').exists()).toBe(false)
  expect(w.text()).toContain('只能继续还没结束的那一次'); expect(w.text()).not.toContain('reference_answer')
  w.unmount()
})

it('explicit retry snapshots answer/action; edits and new operations never reuse an old submission', async () => {
  const page = useV2Learning(); page.selectSession(sid); await page.perform('start', { topic: 'Redis' })
  responder = () => ({ status_code: 8006, status_msg: '原判分失败' })
  await page.perform('answer', { answer: '首次答案' }); const original = calls.at(-1).init.body
  expect(page.error.value.message).toBe('原判分失败'); expect(page.attempt.value).toBe(null)
  await page.retry(); expect(calls.at(-1).init.body).toBe(original)
  responder = () => ({ status_code: 8007, status_msg: '原提交冲突' })
  await page.perform('answer', { answer: '改变的答案' })
  expect(JSON.parse(calls.at(-1).init.body).submissionId).not.toBe(JSON.parse(original).submissionId)
  expect(page.error.value.message).toBe('原提交冲突')
  await page.perform('action', { action: 'skip' }); const skip = calls.at(-1).init.body
  await page.retry(); expect(calls.at(-1).init.body).toBe(skip)
  expect(JSON.parse(skip)).not.toHaveProperty('answer')
  page.dispose()
})

it('does not retry Start or invent a task on HTTP-200 error, expiry or lost response', async () => {
  const w = mountPage(); await prepare(w)
  responder = () => { throw new TypeError('response lost') }
  await w.get('[data-test=start-form]').trigger('submit'); await flushPromises()
  expect(calls.filter((c) => c.url.endsWith('/start'))).toHaveLength(1)
  expect(w.find('[data-test=retry]').exists()).toBe(false); expect(w.find('[data-test=task]').exists()).toBe(false)
  expect(w.get('[data-test=uncertain]').text()).toContain('未确认')
  responder = () => ({ status_code: 8005, status_msg: '学习测试已过期' })
  await w.get('[data-test=current]').trigger('click'); await flushPromises()
  expect(w.text()).toContain('学习测试已过期'); expect(w.find('[data-test=task]').exists()).toBe(false)
  w.unmount()
})

it('suppresses duplicate sends and ignores cancelled/switched/unmounted late responses', async () => {
  const page = useV2Learning(); page.selectSession(sid)
  let finish
  responder = () => new Promise((resolve) => { finish = resolve })
  const sending = page.perform('start', { topic: 'Redis' })
  await page.perform('start', { topic: 'Redis' }); expect(calls).toHaveLength(1)
  page.abort(); expect(calls[0].init.signal.aborted).toBe(true)
  page.selectSession('9007199254740993125')
  finish({ status_code: 1000, learning: task }); await sending
  expect(page.task.value).toBe(null); expect(page.sessionId.value).toBe('9007199254740993125')
  const next = page.perform('start', { topic: 'next' }); page.dispose()
  finish({ status_code: 1000, learning: task }); await next
  expect(page.task.value).toBe(null); expect(page.busy.value).toBe(false)
})

it('keeps uncertain Start after progress lookup and discards a retry from a different recovered task', async () => {
  const page = useV2Learning(); page.selectSession(sid)
  responder = () => { throw new TypeError('lost') }
  await page.perform('start', { topic: 'Redis' })
  expect(page.uncertain.value).toBe(true); expect(page.pending.value).toBe(null)
  responder = () => ({ status_code: 1000, progress: { mastery_score: 25 } })
  await page.perform('progress', { topic: 'Redis' }); expect(page.uncertain.value).toBe(true)
  responder = () => ({ status_code: 1000, learning: task })
  await page.perform('current'); expect(page.uncertain.value).toBe(false)
  responder = () => ({ status_code: 8006, status_msg: '判分失败' })
  await page.perform('answer', { answer: 'old' }); expect(page.pending.value).not.toBe(null)
  responder = () => ({ status_code: 1000, learning: { ...task, id: 'new-task' } })
  await page.perform('current'); expect(page.pending.value).toBe(null)
  expect(page.task.value.id).toBe('new-task')
  page.dispose()
})

it('mounts protected standalone navigation while retaining the old learning route', async () => {
  const authStore = { isAuthenticated: () => false, signOut: () => {} }
  const router = createAppRouter({ history: createMemoryHistory(), authStore })
  await router.push('/learning/v2'); await router.isReady()
  expect(router.currentRoute.value.name).toBe('login')
  expect(router.currentRoute.value.query.redirect).toBe('/learning/v2')
  authStore.isAuthenticated = () => true
  await router.push('/learning/v2'); expect(router.currentRoute.value.name).toBe('v2-learning')
  await router.push('/learning'); expect(router.currentRoute.value.name).toBe('learning')
})
