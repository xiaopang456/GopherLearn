import test from 'node:test'
import assert from 'node:assert/strict'
import { pairedReport, singleReport, discordantMDE } from '../statistics.mjs'
const left = { arm: 'current', condition: 'control' }, right = { arm: 'current', condition: 'distractor' }
const group = (id, condition, verdict) => [0, 1, 2].map(rep => ({ id, arm: 'current', condition, rep, verdict, env: 'fake' }))
test('paired report clusters repeats by scenario and is reproducible', () => {
  const rows = [...group('one', 'control', 'pass'), ...group('one', 'distractor', 'fail'), ...group('two', 'control', 'pass'), ...group('two', 'distractor', 'pass')]
  const r = pairedReport(rows, left, right)
  assert.equal(r.complete_scenarios, 2); assert.equal(r.mean_difference, .5)
  assert.equal(r.only_left_all_pass, 1); assert.equal(r.mcnemar_exact_p, 1)
  assert.deepEqual(pairedReport(rows, left, right), r)
  assert.deepEqual(r.ci95, [0, 1])
})
test('missing and unadjudicated outcomes remain visible in sensitivity bounds', () => {
  const r = pairedReport(group('one', 'control', 'pass'), left, right, { expectedIds: ['one', 'absent'] })
  assert.equal(r.mean_difference, null)
  assert.deepEqual(r.sensitivity.missing_attempts, { left: 3, right: 6 })
  assert.deepEqual(r.sensitivity.unresolved_outcome_bounds, [-.5, 1])
  assert.equal(r.sensitivity.all_unresolved_as_non_success_difference, .5)
  assert.deepEqual(r.pairs[1].right, [null, null, null])
})
test('conditional exact power does not claim sensitivity from zero discordance', () => {
  assert.equal(discordantMDE(16, 0).difference, null)
  assert.equal(discordantMDE(16, 5).difference, null)
  const r = discordantMDE(16, 8), q = r.conditional_favor_left_probability
  // For eight discordants only the two extremes reject at two-sided .05.
  assert.ok(Math.abs(q ** 8 + (1 - q) ** 8 - .8) < 1e-10)
  assert.ok(Math.abs(r.difference - .5 * (2 * q - 1)) < 1e-10)
  assert.throws(() => discordantMDE(2, 3), /invalid/)
})
test('variance approximation uses independent scenarios and labels infeasible effects', () => {
  const rows = [...group('one', 'control', 'pass'), ...group('one', 'distractor', 'fail'), ...group('two', 'control', 'fail'), ...group('two', 'distractor', 'pass')]
  const estimate = pairedReport(rows, left, right).mde.clustered_rate_normal_approx
  assert.ok(Math.abs(estimate.difference - 2.801585218112968) < 1e-10)
  assert.equal(estimate.within_feasible_effect_range, false)
})
test('malformed and mixed-environment outcomes fail closed', () => {
  assert.throws(() => singleReport([null], left), /invalid outcome/)
  const rows = group('one', 'control', 'pass'); rows[1].env = 'real'
  assert.throws(() => singleReport(rows, left), /mixed/)
  const mixed = [...group('one', 'control', 'pass'), ...group('one', 'distractor', 'pass').map(r => ({ ...r, env: 'real' }))]
  assert.throws(() => pairedReport(mixed, left, right), /mixed/)
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
