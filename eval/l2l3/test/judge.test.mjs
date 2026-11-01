import test from 'node:test'
import assert from 'node:assert/strict'

import { combine, judgeTrial } from '../lib/judge.mjs'
import { answerable, corpus, label, record, withTools } from './helpers.mjs'

const ctx = { corpus, defaultMaxToolCalls: 3 }
const both = (overrides = {}) => answerable({ label: label({ expect: ['knowledge_search'], forbid: ['memory_draft'] }), layers: { l2: true, l3: true }, ...overrides })

test('combine: an error outranks a failure, a failure outranks a pending judgement, informational results never count', () => {
  const a = (verdict, extra = {}) => ({ name: 'x', verdict, ...extra })
  assert.equal(combine([a('pass'), a('pass')]), 'pass')
  assert.equal(combine([a('pass'), a('needs_judge')]), 'needs_judge')
  assert.equal(combine([a('needs_judge'), a('fail')]), 'fail')
  assert.equal(combine([a('fail'), a('error')]), 'error')
  assert.equal(combine([a('pass'), a('fail', { informational: true })]), 'pass')
  assert.equal(combine([]), 'pass')
})

test('a good trial passes both layers and carries its cost', async () => {
  const outcome = await judgeTrial(withTools('knowledge_search'), both(), ctx)
  assert.equal(outcome.schema, 'outcome.v1')
  assert.deepEqual([outcome.arm, outcome.qid, outcome.rep, outcome.env], ['arm-a', 'q1', 0, 'fake'])
  assert.equal(outcome.layers.l2.verdict, 'pass')
  assert.equal(outcome.layers.l3.verdict, 'pass')
  assert.equal(outcome.layers.l2.coverage, 1)
  assert.deepEqual(outcome.cost, { modelCalls: 2, toolCalls: 1, durationMs: 100, projectedInput: null })
})

test('the two layers keep their own verdicts: well answered, but searched when it must not have', async () => {
  const question = both({ label: label({ forbid: ['knowledge_search'] }) })
  const outcome = await judgeTrial(withTools('knowledge_search'), question, ctx)
  assert.equal(outcome.layers.l2.verdict, 'pass')
  assert.equal(outcome.layers.l3.verdict, 'fail')
  assert.equal(outcome.layers.l3.class, 'forbidden_tool_used')
})

test('a layer a question does not have is null, not a pass', async () => {
  const l2only = await judgeTrial(withTools('knowledge_search'), answerable(), ctx)
  assert.equal(l2only.layers.l3, null)
  const l3only = await judgeTrial(withTools(), { ...answerable(), answerable: null, evidenceSets: [], label: label(), layers: { l2: false, l3: true } }, ctx)
  assert.equal(l3only.layers.l2, null)
  assert.equal(l3only.layers.l3.verdict, 'pass')
})

test('an unanswerable question goes through the structural rule', async () => {
  const question = answerable({ answerable: false, evidenceSets: [] })
  const refused = await judgeTrial(record({ answerBasis: 'refused', knowledgeStatus: 'no_evidence', sources: [] }), question, ctx)
  assert.equal(refused.layers.l2.verdict, 'pass')
  assert.equal(refused.layers.l2.tier, 'abstained')
  const grounded = await judgeTrial(record(), question, ctx)
  assert.equal(grounded.layers.l2.verdict, 'needs_judge')
})

test('a call that did not produce an answer is an error on every layer, never a failure', async () => {
  const cases = [
    [{ http: 500 }, 'run_failed'],
    [{ statusCode: 4001 }, 'run_failed'],
    [{ error: 'provider exception: socket hang up' }, 'run_failed'],
    [{ replayed: true }, 'run_failed'],
    [{ schema: 'record.v0' }, 'bad_record'],
    [{ sources: 'not a list' }, 'bad_record'],
    [{ toolCalls: undefined }, 'bad_record'],
    [{ rep: -1 }, 'bad_record'],
  ]
  for (const [overrides, cls] of cases) {
    const outcome = await judgeTrial(record(overrides), both(), ctx)
    for (const layer of ['l2', 'l3']) {
      assert.equal(outcome.layers[layer].verdict, 'error', `${JSON.stringify(overrides)} ${layer}`)
      assert.equal(outcome.layers[layer].class, cls, JSON.stringify(overrides))
    }
  }
})

test('a missing field in a failed call is fine: the failure is what is reported', async () => {
  const outcome = await judgeTrial({ schema: 'record.v1', arm: 'a', qid: 'q1', rep: 0, env: 'fake', http: 500, error: 'boom' }, both(), ctx)
  assert.equal(outcome.layers.l2.class, 'run_failed')
  assert.equal(outcome.cost.toolCalls, null)
})

test('the inspector reaches the doc constraint through the judge', async () => {
  const question = both({ label: label({ doc_constraint: true }) })
  const inspector = { inspect: async () => ({ steps: [{ sequence: 1, capability: 'document_read', docIds: ['x'] }] }) }
  const outcome = await judgeTrial(withTools('document_read'), question, { ...ctx, inspector })
  assert.equal(outcome.layers.l3.verdict, 'fail')
  assert.equal(outcome.layers.l3.class, 'document_not_retrieved')
})
