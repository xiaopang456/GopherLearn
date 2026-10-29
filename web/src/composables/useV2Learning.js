import { ref } from 'vue'
import { listSessions, readList, normalizeSession } from '../api/sessions.js'
import * as api from '../api/v2Learning.js'
import { localId } from '../lib/id.js'

// Presentation of server state only. No learning transitions or scoring here.
export function useV2Learning() {
  const sessions = ref([]), sessionId = ref(''), task = ref(null), attempt = ref(null), progress = ref(null)
  const busy = ref(false), error = ref(null), uncertain = ref(false), pending = ref(null)
  let generation = 0, controller, listGeneration = 0
  const clone = (value) => JSON.parse(JSON.stringify(value))
  const operations = {
    start: (body, signal) => api.startV2Learning(body, signal),
    current: (body, signal) => api.currentV2Learning(body.sessionId, signal),
    answer: (body, signal) => api.answerV2Learning(body, signal),
    action: (body, signal) => api.actionV2Learning(body, signal),
    cancel: (body, signal) => api.cancelV2Learning(body, signal),
    correct: (body, signal) => api.correctV2Learning(body, signal),
    progress: (body, signal) => api.progressV2Learning(body.topic, signal),
  }
  async function loadSessions() {
    const current = ++listGeneration
    try {
      const result = await listSessions()
      if (current === listGeneration) sessions.value = readList(result, 'sessions').map(normalizeSession).filter((s) => s.mode === 'learning_agent')
    } catch (cause) { if (current === listGeneration) error.value = { message: cause.message, code: cause.code } }
  }
  function abort() {
    const wasBusy = busy.value
    ++generation; controller?.abort(); busy.value = false
    if (wasBusy) { uncertain.value = true; error.value = { message: '请求已取消；服务端状态未确认，请查询 current。', code: 'AbortError' } }
  }
  function selectSession(id) {
    abort(); sessionId.value = String(id); task.value = null; attempt.value = null; progress.value = null
    pending.value = null; error.value = null; uncertain.value = false
  }
  async function send(op) {
    if (busy.value) return null
    const execute = operations[op.kind]
    if (!execute) throw new Error('未知学习操作')
    const current = ++generation
    controller = new AbortController(); busy.value = true; error.value = null
    // Only submission-based operations have an explicit retry button.
    if (['answer', 'action'].includes(op.kind)) pending.value = clone(op)
    else if (!['current', 'progress'].includes(op.kind)) pending.value = null
    if (op.kind === 'current') { task.value = null; attempt.value = null }
    try {
      const response = await execute(op.body, controller.signal)
      if (current !== generation) return null
      const result = response.result || response
      if (result.learning) {
        task.value = result.learning
        if (pending.value && pending.value.body.learningSessionId !== result.learning.id) pending.value = null
      }
      if (result.attempt) attempt.value = result.attempt
      if (Object.hasOwn(result, 'progress')) progress.value = result.progress
      if (op.kind === 'start') { attempt.value = null; progress.value = null }
      if (op.kind !== 'progress') uncertain.value = false
      return response
    } catch (cause) {
      if (current === generation) {
        error.value = { message: cause.name === 'AbortError' ? '请求已取消；服务端状态未确认，请查询 current。' : cause.message, code: cause.code || cause.name }
        if (!['current', 'progress'].includes(op.kind)) uncertain.value = true
        if (cause.nativeCode === 8002 || cause.nativeCode === 8005) {
          task.value = null; attempt.value = null; pending.value = null; uncertain.value = false
        }
      }
      return null
    } finally { if (current === generation) busy.value = false }
  }
  function perform(kind, fields = {}) {
    if (busy.value) return Promise.resolve(null)
    const body = clone(fields)
    if (['start', 'current', 'answer', 'action', 'cancel'].includes(kind)) body.sessionId = sessionId.value
    if (['answer', 'action', 'cancel'].includes(kind)) body.learningSessionId = task.value?.id
    if (['answer', 'action'].includes(kind)) {
      body.questionId = task.value?.question?.id
      body.submissionId = localId('v2-learning-submission')
    }
    return send({ kind, body })
  }
  function retry() { return pending.value ? send(clone(pending.value)) : Promise.resolve(null) }
  function dispose() { ++listGeneration; abort(); pending.value = null }
  return { sessions, sessionId, task, attempt, progress, busy, error, uncertain, pending, loadSessions, selectSession, perform, retry, abort, dispose }
}
