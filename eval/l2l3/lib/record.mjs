// The raw record: what one call of the system under test left behind.
//
// The record is the source of truth. Assertions are pure functions over it, so a run
// can be scored again offline, any number of times, and a Promptfoo run and an
// offline scoring pass cannot disagree about a trial.

import { SUCCESS_CODE } from './enums.mjs'

export const RECORD_SCHEMA = 'record.v1'

/** What is wrong with the shape of a record, as plain sentences. Empty when it is fine. */
export function recordProblems(record) {
  const problems = []
  if (!record || typeof record !== 'object' || Array.isArray(record)) return ['the record is not an object']
  if (record.schema !== RECORD_SCHEMA) problems.push(`schema is ${JSON.stringify(record.schema)}, want ${RECORD_SCHEMA}`)
  if (typeof record.arm !== 'string' || record.arm === '') problems.push('arm is missing')
  if (typeof record.qid !== 'string' || record.qid === '') problems.push('qid is missing')
  if (!Number.isInteger(record.rep) || record.rep < 0) problems.push('rep is not a non-negative integer')
  if (record.env !== 'fake' && record.env !== 'real') problems.push('env must be fake or real')
  if (runFailure(record) === '') {
    // A call that succeeded must carry what the assertions read.
    if (!Array.isArray(record.sources)) problems.push('sources is not a list')
    if (typeof record.answerBasis !== 'string') problems.push('answerBasis is missing')
    if (typeof record.knowledgeStatus !== 'string') problems.push('knowledgeStatus is missing')
    if (typeof record.knowledgeReason !== 'string') problems.push('knowledgeReason is missing')
    if (!Number.isInteger(record.toolCalls)) problems.push('toolCalls is missing')
  }
  return problems
}

/**
 * Why the call produced no answer, or '' when it did. A call that did not complete
 * says nothing about the answer, so every layer reports it as an error rather than
 * as a failure.
 */
export function runFailure(record) {
  if (record.error) return `provider error: ${String(record.error).slice(0, 160)}`
  if (record.http !== 200) return `http ${record.http}`
  if (record.statusCode !== SUCCESS_CODE) return `status_code ${record.statusCode}`
  if (record.replayed === true) return 'the run was a replay of an earlier request, not a new call'
  return ''
}

/** The steps that executed a tool, in order. A plan step is not a tool call. */
export function toolSteps(record) {
  return (Array.isArray(record.steps) ? record.steps : []).filter((step) => step && step.step_type === 'tool')
}
