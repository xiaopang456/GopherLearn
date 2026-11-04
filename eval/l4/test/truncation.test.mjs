import test from 'node:test'
import assert from 'node:assert/strict'
import { categoryRate, categoryReport, judgeInput, mergedOutcomes, truncationReport } from '../truncation.mjs'

// A conversation of three turns. The user message of a turn is the only
// message a summary may cite, so the turn of a message id is found through
// the user messages.
const warmup = overrides => ({
  degraded_turns: 0,
  turns: [
    { UserID: 11, Summary: { CoveredMessageID: 4, Truncations: [] } },
    { UserID: 12, Summary: { CoveredMessageID: 4, Truncations: [] } },
    { UserID: 13, Summary: { CoveredMessageID: 6, Truncations: [{ Kind: 'items', Group: 'open_loops', Kept: 20, Dropped: 2, DroppedMessageIDs: [11, 12] }] } },
  ],
  ...overrides,
})
const scenario = { id: 'l4-constraint-01', category: 'constraint', evidence_turns: [1, 3] }

test('a dropped message is matched to the turn it belongs to', () => {
  const report = truncationReport(new Map([[scenario.id, scenario]]), new Map([[scenario.id, warmup()]]), { arm: 'truncation' })
  const row = report.scenarios[0]
  assert.equal(row.truncation_events, 1)
  assert.equal(row.dropped_items, 2)
  assert.deepEqual(row.dropped_message_ids, [11, 12])
  // Turn one is an evidence turn of this scenario and its message was
  // dropped; turn three is an evidence turn and its message was not.
  assert.deepEqual(row.lost_evidence_turns, [1])
  assert.equal(report.totals.scenarios_losing_evidence_turn, 1)
  assert.equal(report.totals.lost_evidence_turns, 1)
})

test('a message that belongs to no turn is reported instead of ignored', () => {
  const conversation = warmup({
    turns: [{ UserID: 11, Summary: { CoveredMessageID: 4, Truncations: [{ Kind: 'items', Group: 'open_loops', Kept: 20, Dropped: 1, DroppedMessageIDs: [99] }] } }],
  })
  const report = truncationReport(new Map([[scenario.id, scenario]]), new Map([[scenario.id, conversation]]))
  assert.deepEqual(report.scenarios[0].unknown_message_ids, [99])
  assert.deepEqual(report.scenarios[0].lost_evidence_turns, [])
})

test('a conversation that was never trimmed reports nothing', () => {
  const report = truncationReport(new Map([[scenario.id, scenario]]), new Map([[scenario.id, warmup()]]))
  assert.equal(report.totals.scenarios_with_truncation, 1)
  const quiet = truncationReport(new Map([[scenario.id, scenario]]), new Map([[scenario.id, { turns: [{ UserID: 11, Summary: { CoveredMessageID: 4 } }], degraded_turns: 2 }]]))
  assert.equal(quiet.totals.scenarios_with_truncation, 0)
  assert.equal(quiet.scenarios[0].truncation_events, 0)
  assert.equal(quiet.scenarios[0].degraded_turns, 2)
})

test('the judge input carries the constraint and the answer, and never judges it', () => {
  const scenarios = new Map([[scenario.id, { ...scenario, question: '出一道题', answer: { mode: 'needs_judge', required: ['只用标准库'] } }]])
  const records = [{ qid: scenario.id, arm: 'truncation', condition: 'main', rep: 0, env: 'real', answer: '这里只用标准库' }]
  const rows = judgeInput(scenarios, records)
  assert.equal(rows.length, 1)
  assert.deepEqual(rows[0].required, ['只用标准库'])
  assert.equal(rows[0].answer, '这里只用标准库')
  assert.equal(rows[0].id, scenario.id)
})

test('a judgement settles a pending outcome and never a decided one', () => {
  const outcomes = [
    { id: 'one', arm: 'a', condition: 'main', rep: 0, verdict: 'needs_judge' },
    { id: 'one', arm: 'a', condition: 'main', rep: 1, verdict: 'fail' },
  ]
  const merged = mergedOutcomes(outcomes, [{ id: 'one', arm: 'a', condition: 'main', rep: 0, verdict: 'pass', reason: 'judged' }])
  assert.equal(merged[0].verdict, 'pass')
  assert.equal(merged[1].verdict, 'fail')
  assert.throws(() => mergedOutcomes(outcomes, [{ id: 'one', arm: 'a', condition: 'main', rep: 0, verdict: 'maybe' }]), /verdict/)
})

test('a category rate keeps the three repeats of a scenario together', () => {
  const rows = [
    { id: 'one', category: 'constraint', arm: 'a', condition: 'main', rep: 0, verdict: 'pass' },
    { id: 'one', category: 'constraint', arm: 'a', condition: 'main', rep: 1, verdict: 'pass' },
    { id: 'one', category: 'constraint', arm: 'a', condition: 'main', rep: 2, verdict: 'fail' },
    { id: 'two', category: 'constraint', arm: 'a', condition: 'main', rep: 0, verdict: 'pass' },
  ]
  const rate = categoryRate(rows, 'constraint')
  assert.equal(rate.scenarios, 2)
  assert.equal(rate.attempts, 4)
  assert.equal(rate.success_rate_conservative, .75)
  assert.equal(rate.scenario_all_pass_rate, 0)
  assert.equal(rate.complete_scenarios, 1)
})

test('the category report compares the categories apart and leaves the pending ones pending', () => {
  const scenarios = new Map([
    ['l4-constraint-01', { id: 'l4-constraint-01', category: 'constraint' }],
    ['l4-early_fact-01', { id: 'l4-early_fact-01', category: 'early_fact' }],
  ])
  const outcomes = []
  for (const arm of ['baseline', 'truncation']) {
    for (const rep of [0, 1, 2]) {
      outcomes.push({ id: 'l4-constraint-01', arm, condition: 'main', rep, verdict: 'needs_judge' })
      outcomes.push({ id: 'l4-early_fact-01', arm, condition: 'main', rep, verdict: arm === 'baseline' ? 'pass' : 'fail' })
    }
  }
  const report = categoryReport(outcomes, scenarios, { left: { arm: 'baseline', condition: 'main' }, right: { arm: 'truncation', condition: 'main' } })
  const constraint = report.categories.find(row => row.category === 'constraint')
  const fact = report.categories.find(row => row.category === 'early_fact')
  // Without a judgement the constraint answers stay pending: they are not
  // failures and they are not successes either.
  assert.equal(constraint.left.needs_judge, 3)
  assert.equal(constraint.left.success_rate_conservative, 0)
  assert.equal(constraint.difference_conservative, 0)
  assert.equal(fact.left.success_rate_conservative, 1)
  assert.equal(fact.right.success_rate_conservative, 0)
  assert.equal(fact.difference_conservative, 1)
  // With a judgement the constraint category becomes readable.
  const judged = categoryReport(outcomes, scenarios, {
    left: { arm: 'baseline', condition: 'main' }, right: { arm: 'truncation', condition: 'main' },
    judgments: outcomes.filter(row => row.id === 'l4-constraint-01' && row.arm === 'baseline').map(row => ({ ...row, verdict: 'pass', reason: 'judged' })),
  })
  assert.equal(judged.categories.find(row => row.category === 'constraint').left.success_rate_conservative, 1)
  assert.equal(judged.judged_attempts, 3)
})
