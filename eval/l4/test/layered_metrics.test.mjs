import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { main } from '../cli.mjs'
import { aggregateRuns, compareModes, layeredMetrics, productMetrics, renderMarkdown, sumCounters } from '../layered_metrics.mjs'

// run builds a layered run the harness would have written.
function run(mode, overrides = {}) {
  return {
    scenario: 'synthetic', condition: 'main', mode, turns: 4, messages: 8,
    segments: [{ seq: 1, start_message_id: 1, end_message_id: 8, topic: 't', content: 'c', points: 2, changes: [{ key: 'k', old_value: 'old', new_value: 'new' }], diagnostics: {} }],
    whiteboard: '{}',
    coverage: { covered_message_id: 8, total_messages: 8, ratio: 1, series: [8], monotonic: true, stalled: false },
    failures: { segments: 0, old_summary: 0, total: 0 },
    board: { applied: 1, changed: 1, fabricated: 0, suspicious_delete: 0, missed_update: 0, truncated: 0, dropped: 0, dropped_done: 0, superseded_marks: 1 },
    retrieval: { question: 'q', selected: [1], evidence: [1], hit: true },
    model_calls: 2, prompt_tokens: 100, completion_tokens: 20,
    counters: { 'summary_segment_fallback_total{mode=on,reason=not_needed}': 1 },
    ...overrides
  }
}

test('a run reads the seven numbers the plan asks for', () => {
  const metrics = layeredMetrics(run('on'))
  assert.equal(metrics.M1_coverage.ratio, 1)
  assert.equal(metrics.M1_coverage.monotonic, true)
  assert.equal(metrics.M2_failures.total, 0)
  assert.equal(metrics.M3_sync_fallback.total, 1)
  assert.equal(metrics.M6_cost.prompt_tokens, 100)
  assert.equal(metrics.M7_board.fabricated, 0)
  assert.equal(metrics.board_marks.superseded_marks, 1)
  assert.equal(metrics.segment_retrieval.hit, true)
})

test('a run without a board reports no rate instead of a clean one', () => {
  const metrics = layeredMetrics({ mode: 'off', coverage: { covered_message_id: 4, total_messages: 8, ratio: 0.5 }, failures: {}, board: {}, segments: [], retrieval: {}, counters: {} })
  assert.equal(metrics.M7_board.missed_update_rate, null)
  assert.equal(metrics.segment_retrieval.hit_rate, undefined)
  assert.equal(metrics.M2_failures.total, 0)
})

test('the counters of one family are added up whatever their labels are', () => {
  const counters = {
    'summary_segment_fallback_total{mode=on,reason=not_needed}': 2,
    'summary_segment_fallback_total{mode=on,reason=generation_failed}': 3,
    'summary_segment_failed_total{mode=on,reason=invalid_output}': 1
  }
  assert.equal(sumCounters(counters, 'summary_segment_fallback_total'), 5)
  assert.equal(sumCounters(counters, 'summary_segment_failed_total'), 1)
  assert.equal(sumCounters(counters, 'whiteboard_missed_update_total'), 0)
})

test('the runs of one mode are aggregated and the arms are compared', () => {
  const off = run('off', { coverage: { covered_message_id: 4, total_messages: 8, ratio: 0.5, monotonic: true }, board: { missed_update: 1 }, model_calls: 3, prompt_tokens: 300 })
  const on = run('on', { coverage: { covered_message_id: 8, total_messages: 8, ratio: 1, monotonic: true }, board: { missed_update: 0 }, model_calls: 2, prompt_tokens: 100 })
  const report = compareModes([off, on])
  assert.equal(report.modes.off.runs, 1)
  assert.equal(report.modes.on.M1_coverage.average_ratio, 1)
  assert.equal(report.delta.off_to_on.M1_coverage_ratio, 0.5)
  assert.equal(report.delta.off_to_on.M2_failures, 0)
  assert.equal(report.delta.off_to_on.M6_model_calls, -1)
  assert.equal(report.delta.off_to_on.M6_prompt_tokens, -200)
  assert.equal(report.delta.off_to_on.M7_missed_update, -1)
})

test('the coverage of a run is averaged and not summed', () => {
  const first = run('on', { coverage: { covered_message_id: 4, total_messages: 8, ratio: 0.5, monotonic: true } })
  const second = run('on', { coverage: { covered_message_id: 8, total_messages: 8, ratio: 1, monotonic: true } })
  const aggregate = aggregateRuns([first, second])
  assert.equal(aggregate.runs, 2)
  assert.equal(aggregate.M1_coverage.average_ratio, 0.75)
  assert.equal(aggregate.M1_coverage.covered_total, 12)
})

test('the product numbers come from the product records', () => {
  const product = productMetrics([
    { question_ms: 100, score: true, category: 'stale' },
    { question_ms: 300, score: false, category: 'stale' },
    { question_ms: 200, score: true, category: 'constraint' }
  ])
  assert.equal(product.M4_accuracy.rate, 2 / 3)
  assert.equal(product.M4_accuracy.by_category.stale.rate, 0.5)
  assert.equal(product.M5_wait.average_ms, 200)
  assert.equal(product.M5_wait.median_ms, 200)
  assert.equal(productMetrics([]), null)
})

test('a scored outcome is read and a row that needs a judge is not a score', () => {
  const scenarios = new Map([['s1', { id: 's1', category: 'stale' }]])
  const product = productMetrics([
    { id: 's1', verdict: 'pass', question_ms: 10 },
    { id: 's1', verdict: 'fail', question_ms: 20 },
    { id: 's1', verdict: 'needs_judge', question_ms: 30 },
    { id: 's1', verdict: 'error', question_ms: 40 }
  ], scenarios)
  assert.equal(product.attempts, 4)
  assert.equal(product.M4_accuracy.scored, 2)
  assert.equal(product.M4_accuracy.rate, 0.5)
  assert.equal(product.M4_accuracy.by_category.stale.rate, 0.5)
  assert.equal(product.M5_wait.average_ms, 25)
})

test('the table writes a dash for a number that is not there', () => {
  const report = compareModes([run('off'), run('on')])
  const table = renderMarkdown(report)
  assert.match(table, /M1 覆盖率/)
  assert.match(table, /旧值标记次数/)
  assert.match(table, /M4 成功率/)
  assert.match(table, /—/)
})

test('the CLI writes the layered report and refuses to overwrite it', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'layered-metrics-')); test.after?.(() => fs.rmSync(dir, { recursive: true, force: true }))
  const runs = path.join(dir, 'runs.json'), records = path.join(dir, 'records.jsonl'), out = path.join(dir, 'report.json')
  fs.writeFileSync(runs, JSON.stringify([run('off'), run('on')]))
  fs.writeFileSync(records, JSON.stringify({ qid: 'synthetic', arm: 'on', question_ms: 150, score: true, category: 'stale' }) + '\n')
  main(['layered-metrics', '--runs', runs, '--records', records, '--out', out])
  const report = JSON.parse(fs.readFileSync(out, 'utf8'))
  assert.equal(report.modes.on.runs, 1)
  assert.equal(report.product.M5_wait.average_ms, 150)
  assert.throws(() => main(['layered-metrics', '--runs', runs, '--records', records, '--out', out]), /output exists/)
})
