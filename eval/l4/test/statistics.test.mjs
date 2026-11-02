import test from 'node:test'
import assert from 'node:assert/strict'
import { pairedReport, singleReport } from '../statistics.mjs'
const left = { arm: 'current', condition: 'control' }, right = { arm: 'current', condition: 'distractor' }
const group = (id, condition, verdict) => [0, 1, 2].map(rep => ({ id, arm: 'current', condition, rep, verdict }))
test('paired report clusters repeats by scenario and is reproducible', () => {
  const rows = [...group('one', 'control', 'pass'), ...group('one', 'distractor', 'fail'), ...group('two', 'control', 'pass'), ...group('two', 'distractor', 'pass')]
  const r = pairedReport(rows, left, right)
  assert.equal(r.complete_scenarios, 2); assert.equal(r.mean_difference, .5)
  assert.equal(r.only_left_all_pass, 1); assert.equal(r.mcnemar_exact_p, 1)
  assert.deepEqual(pairedReport(rows, left, right), r)
  assert.deepEqual(r.ci95, [0, 1])
})
test('missing repeats and pending judgments do not become successes', () => {
  const rows = [...group('complete', 'control', 'pass'), ...group('complete', 'distractor', 'needs_judge'), ...group('partial', 'control', 'pass'), ...group('partial', 'distractor', 'pass').slice(0, 2)]
  const r = pairedReport(rows, left, right)
  assert.equal(r.complete_scenarios, 1); assert.deepEqual(r.missing_scenarios, ['partial']); assert.equal(r.pending_attempts, 3); assert.equal(r.ci95, null)
})
test('duplicate repeated attempts are rejected', () => {
  const rows = group('one', 'control', 'pass'); rows.push(rows[0])
  assert.throws(() => pairedReport(rows, left, right), /duplicate/)
})
test('single configuration includes unattempted scenarios in conservative denominators', () => {
  const rows = group('present', 'control', 'pass')
  const r = singleReport(rows, left, ['present', 'absent'])
  assert.equal(r.success_rate_conservative, .5)
  assert.equal(r.success_rate_adjudicated, 1)
  assert.equal(r.pass3_conservative, .5)
  assert.equal(r.pass2_complete_only, 1)
  assert.deepEqual(r.missing_scenarios, ['absent'])
  const comparison = pairedReport(rows, left, right, { expectedIds: ['present', 'absent'] })
  assert.deepEqual(comparison.missing_scenarios, ['absent', 'present'])
})
