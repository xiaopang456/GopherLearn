// The layered summary's own metrics.
//
// The plan asks for seven numbers and the comparison is only readable if
// every arm reports the same seven. This module reads the runs the harness
// wrote and turns them into those numbers, so the numbers are computed in
// one place and two reports of the same run cannot disagree.
//
// The numbers that belong to the product's own answer path (the success rate
// of the three question kinds and the wait before the answer) are not in a
// layered run: they are in the product records, and they are read from there
// when those records are given.

import { scenarioIdOf } from './scenarios.mjs'

const COUNTER = /^([a-z_]+)(?:\{([^}]*)\})?$/

// counterParts splits a counter name into its name and its labels.
function counterParts(key) {
  const match = COUNTER.exec(String(key ?? ''))
  if (!match) return { name: String(key ?? ''), labels: {} }
  const labels = {}
  for (const part of (match[2] ?? '').split(',')) {
    const index = part.indexOf('=')
    if (index > 0) labels[part.slice(0, index).trim()] = part.slice(index + 1).trim()
  }
  return { name: match[1], labels }
}

// sumCounters adds up the counters of one family.
//
// The families are counted by name, so a counter that gains a new label - a
// new failure reason, a new mode - still lands in the family it belongs to.
export function sumCounters(counters, prefix) {
  let total = 0
  for (const [key, value] of Object.entries(counters ?? {})) {
    if (counterParts(key).name !== prefix) continue
    total += Number(value) || 0
  }
  return total
}

// counterValues lists the counters of one family with their labels.
export function counterValues(counters, prefix) {
  const out = []
  for (const [key, value] of Object.entries(counters ?? {})) {
    const parts = counterParts(key)
    if (parts.name !== prefix) continue
    out.push({ labels: parts.labels, value: Number(value) || 0 })
  }
  return out
}

// layeredMetrics reads the numbers of one run.
//
// A number that is not there is not zero: a run that never produced a board
// has no board numbers, and saying "0 missed updates" for it would read as a
// clean board rather than as no board at all.
export function layeredMetrics(run) {
  const coverage = run?.coverage ?? {}
  const failures = run?.failures ?? {}
  const board = run?.board ?? {}
  const retrieval = run?.retrieval ?? {}
  const segments = Array.isArray(run?.segments) ? run.segments : []
  const counters = run?.counters ?? {}
  const fallback = sumCounters(counters, 'summary_segment_fallback_total')
  const segmentFailures = sumCounters(counters, 'summary_segment_failed_total')
  const points = segments.reduce((sum, segment) => sum + (Number(segment?.points) || 0), 0)
  const changes = segments.reduce((sum, segment) => sum + (Array.isArray(segment?.changes) ? segment.changes.length : 0), 0)
  const superseded = segments.reduce((sum, segment) => {
    for (const change of segment?.changes ?? []) {
      if (change?.old_value && change?.new_value) sum += 1
    }
    return sum
  }, 0)
  return {
    mode: run?.mode ?? 'unknown',
    scenario: run?.scenario ?? 'unknown',
    M1_coverage: {
      covered_message_id: Number(coverage.covered_message_id) || 0,
      total_messages: Number(coverage.total_messages) || 0,
      ratio: Number(coverage.ratio) || 0,
      monotonic: coverage.monotonic === true,
      stalled: coverage.stalled === true,
      explanation: coverage.explanation ?? ''
    },
    M2_failures: {
      total: Number(failures.total) || 0,
      segments: Number(failures.segments) || 0,
      old_summary: Number(failures.old_summary) || 0,
      counted_by_the_product: segmentFailures
    },
    M3_sync_fallback: {
      total: fallback,
      reasons: counterValues(counters, 'summary_segment_fallback_total').map(entry => ({ reason: entry.labels.reason ?? 'none', count: entry.value }))
    },
    M6_cost: {
      model_calls: Number(run?.model_calls) || 0,
      prompt_tokens: Number(run?.prompt_tokens) || 0,
      completion_tokens: Number(run?.completion_tokens) || 0
    },
    M7_board: {
      applied: Number(board.applied) || 0,
      changed: Number(board.changed) || 0,
      fabricated: Number(board.fabricated) || 0,
      suspicious_delete: Number(board.suspicious_delete) || 0,
      missed_update: Number(board.missed_update) || 0,
      truncated: Number(board.truncated) || 0,
      dropped: Number(board.dropped) || 0,
      dropped_done: Number(board.dropped_done) || 0,
      // The rate is per written segment: a board that was never written has
      // no rate, and a rate of zero over zero segments would look perfect.
      missed_update_rate: rate(Number(board.missed_update) || 0, segments.length),
      suspicious_delete_rate: rate(Number(board.suspicious_delete) || 0, segments.length),
      fabricated_rate: rate(Number(board.fabricated) || 0, segments.length)
    },
    board_marks: {
      superseded_marks: superseded,
      changes: changes,
      segment_points: points,
      segments: segments.length
    },
    segment_retrieval: {
      question: retrieval.question ?? '',
      selected: Array.isArray(retrieval.selected) ? retrieval.selected : [],
      evidence: Array.isArray(retrieval.evidence) ? retrieval.evidence : [],
      hit: retrieval.hit === true
    }
  }
}

// rate is a count over a number of batches, or null when there are none.
function rate(count, batches) {
  if (!batches) return null
  return count / batches
}

// aggregate sums the numbers of the runs of one mode.
//
// The coverage is averaged rather than summed: every run of a mode covers
// its own conversation, and the question a comparison asks is how far a
// conversation was covered, not how much was covered in total.
export function aggregateRuns(runs) {
  const list = Array.isArray(runs) ? runs : []
  if (!list.length) return null
  const metrics = list.map(layeredMetrics)
  const coverageRatios = metrics.map(entry => entry.M1_coverage.ratio)
  const retrievalHits = metrics.filter(entry => entry.segment_retrieval.hit).length
  const retrievalAnswered = metrics.filter(entry => entry.segment_retrieval.evidence.length > 0).length
  return {
    runs: list.length,
    scenarios: metrics.map(entry => entry.scenario),
    M1_coverage: {
      average_ratio: average(coverageRatios),
      covered_total: metrics.reduce((total, entry) => total + entry.M1_coverage.covered_message_id, 0),
      messages_total: metrics.reduce((total, entry) => total + entry.M1_coverage.total_messages, 0),
      monotonic_runs: metrics.filter(entry => entry.M1_coverage.monotonic).length,
      stalled_runs: metrics.filter(entry => entry.M1_coverage.stalled).length
    },
    M2_failures: {
      total: metrics.reduce((total, entry) => total + entry.M2_failures.total, 0),
      segments: metrics.reduce((total, entry) => total + entry.M2_failures.segments, 0),
      old_summary: metrics.reduce((total, entry) => total + entry.M2_failures.old_summary, 0)
    },
    M3_sync_fallback: {
      total: metrics.reduce((total, entry) => total + entry.M3_sync_fallback.total, 0)
    },
    M6_cost: {
      model_calls: metrics.reduce((total, entry) => total + entry.M6_cost.model_calls, 0),
      prompt_tokens: metrics.reduce((total, entry) => total + entry.M6_cost.prompt_tokens, 0),
      completion_tokens: metrics.reduce((total, entry) => total + entry.M6_cost.completion_tokens, 0)
    },
    M7_board: {
      fabricated: metrics.reduce((total, entry) => total + entry.M7_board.fabricated, 0),
      suspicious_delete: metrics.reduce((total, entry) => total + entry.M7_board.suspicious_delete, 0),
      missed_update: metrics.reduce((total, entry) => total + entry.M7_board.missed_update, 0),
      changed: metrics.reduce((total, entry) => total + entry.M7_board.changed, 0)
    },
    board_marks: {
      superseded_marks: metrics.reduce((total, entry) => total + entry.board_marks.superseded_marks, 0),
      segment_points: metrics.reduce((total, entry) => total + entry.board_marks.segment_points, 0),
      segments: metrics.reduce((total, entry) => total + entry.board_marks.segments, 0)
    },
    segment_retrieval: {
      hits: retrievalHits,
      answerable: retrievalAnswered,
      hit_rate: retrievalAnswered ? retrievalHits / retrievalAnswered : null
    }
  }
}

// productMetrics reads the numbers that belong to the product's answer path.
//
// Two kinds of rows are read. A product record carries the answer and the
// wait before it; a scored outcome carries the verdict the scorer gave. A
// row that only says "there was an attempt" is counted as an attempt but not
// as a score: a missing score is not a failing score.
//
// The scenarios are optional. Without them the categories are unknown, and a
// category that is not known is reported as unknown rather than merged into
// one bucket.
export function productMetrics(records, scenarios = new Map()) {
  const rows = Array.isArray(records) ? records : []
  if (!rows.length) return null
  const waits = rows.map(row => Number(row?.question_ms)).filter(value => Number.isFinite(value))
  const scored = []
  const byCategory = {}
  for (const row of rows) {
    const verdict = verdictOf(row)
    if (!verdict) continue
    scored.push({ row, correct: verdict === 'pass' })
    const category = row?.category ?? scenarios.get(scenarioIdOf(row?.id))?.category ?? 'unknown'
    byCategory[category] ??= { attempts: 0, correct: 0 }
    byCategory[category].attempts += 1
    if (verdict === 'pass') byCategory[category].correct += 1
  }
  for (const entry of Object.values(byCategory)) entry.rate = entry.attempts ? entry.correct / entry.attempts : null
  const correct = scored.filter(entry => entry.correct).length
  return {
    attempts: rows.length,
    M4_accuracy: {
      scored: scored.length,
      correct,
      rate: scored.length ? correct / scored.length : null,
      by_category: byCategory
    },
    M5_wait: {
      average_ms: average(waits),
      median_ms: median(waits),
      samples: waits.length
    }
  }
}

// verdictOf reads the score of one row, or null when it carries none.
//
// A judged row that says "needs a judge" is not a score: counting it as a
// failure would turn "not measured" into "measured and wrong".
function verdictOf(row) {
  if (typeof row?.verdict === 'string') {
    if (row.verdict === 'pass') return 'pass'
    if (row.verdict === 'fail') return 'fail'
    return null
  }
  if (row?.score === true || row?.score === 'correct' || row?.score?.correct === true) return 'pass'
  if (row?.score === false || row?.score === 'incorrect' || row?.score?.correct === false) return 'fail'
  return null
}

// compareModes groups the runs by mode and reads the difference.
//
// The difference is only written for the pairs the comparison is about: the
// arm that exists today against the arm that is new.
export function compareModes(runs, records = [], scenarios = new Map()) {
  const list = Array.isArray(runs) ? runs : []
  const byMode = {}
  for (const run of list) {
    const mode = run?.mode ?? 'unknown'
    byMode[mode] ??= []
    byMode[mode].push(run)
  }
  const modes = {}
  for (const [mode, group] of Object.entries(byMode)) {
    modes[mode] = aggregateRuns(group)
  }
  const product = productMetrics(records, scenarios)
  const delta = {}
  if (modes.off && modes.on) {
    delta.off_to_on = {
      M1_coverage_ratio: difference(modes.on.M1_coverage.average_ratio, modes.off.M1_coverage.average_ratio),
      M2_failures: difference(modes.on.M2_failures.total, modes.off.M2_failures.total),
      M3_sync_fallback: difference(modes.on.M3_sync_fallback.total, modes.off.M3_sync_fallback.total),
      M6_model_calls: difference(modes.on.M6_cost.model_calls, modes.off.M6_cost.model_calls),
      M6_prompt_tokens: difference(modes.on.M6_cost.prompt_tokens, modes.off.M6_cost.prompt_tokens),
      M6_completion_tokens: difference(modes.on.M6_cost.completion_tokens, modes.off.M6_cost.completion_tokens),
      M7_missed_update: difference(modes.on.M7_board.missed_update, modes.off.M7_board.missed_update),
      M7_suspicious_delete: difference(modes.on.M7_board.suspicious_delete, modes.off.M7_board.suspicious_delete),
      M7_fabricated: difference(modes.on.M7_board.fabricated, modes.off.M7_board.fabricated)
    }
  }
  return { modes, delta, product, notice: 'Offline measurement of a replay; it is not a quality conclusion about the product.' }
}

// difference is the change from the old arm to the new one.
function difference(next, previous) {
  if (next === null || next === undefined || previous === null || previous === undefined) return null
  return next - previous
}

// average is the mean of the numbers, or null when there are none.
function average(values) {
  const list = (values ?? []).filter(value => Number.isFinite(value))
  if (!list.length) return null
  return list.reduce((total, value) => total + value, 0) / list.length
}

// median is the middle number, or null when there are none.
function median(values) {
  const list = (values ?? []).filter(value => Number.isFinite(value)).sort((left, right) => left - right)
  if (!list.length) return null
  const middle = Math.floor(list.length / 2)
  return list.length % 2 ? list[middle] : (list[middle - 1] + list[middle]) / 2
}

// renderMarkdown writes the comparison as a table.
//
// The table is the report a person reads, so a number that is not there is
// written as a dash instead of as a zero.
export function renderMarkdown(report) {
  const modes = Object.keys(report?.modes ?? {})
  if (!modes.length) return 'No runs.\n'
  const rows = [
    ['指标', ...modes, 'on − off'],
    ['---', ...modes.map(() => '---'), '---'],
    ['M1 覆盖率', ...modes.map(mode => fmt(report.modes[mode].M1_coverage.average_ratio)), fmt(report.delta?.off_to_on?.M1_coverage_ratio)],
    ['M1 停更的运行数', ...modes.map(mode => String(report.modes[mode].M1_coverage.stalled_runs ?? 0)), null],
    ['M2 失败次数', ...modes.map(mode => String(report.modes[mode].M2_failures.total)), fmt(report.delta?.off_to_on?.M2_failures)],
    ['M3 退回同步次数', ...modes.map(mode => String(report.modes[mode].M3_sync_fallback.total)), fmt(report.delta?.off_to_on?.M3_sync_fallback)],
    ['M4 成功率', ...modes.map(mode => fmt(report.product?.M4_accuracy?.rate ?? null)), null],
    ['M5 平均等待(ms)', ...modes.map(mode => fmt(report.product?.M5_wait?.average_ms ?? null)), null],
    ['M6 模型调用', ...modes.map(mode => String(report.modes[mode].M6_cost.model_calls)), fmt(report.delta?.off_to_on?.M6_model_calls)],
    ['M6 输入 token', ...modes.map(mode => String(report.modes[mode].M6_cost.prompt_tokens)), fmt(report.delta?.off_to_on?.M6_prompt_tokens)],
    ['M6 输出 token', ...modes.map(mode => String(report.modes[mode].M6_cost.completion_tokens)), fmt(report.delta?.off_to_on?.M6_completion_tokens)],
    ['M7 漏改', ...modes.map(mode => String(report.modes[mode].M7_board.missed_update)), fmt(report.delta?.off_to_on?.M7_missed_update)],
    ['M7 可疑删除', ...modes.map(mode => String(report.modes[mode].M7_board.suspicious_delete)), fmt(report.delta?.off_to_on?.M7_suspicious_delete)],
    ['M7 凭空捏造', ...modes.map(mode => String(report.modes[mode].M7_board.fabricated)), fmt(report.delta?.off_to_on?.M7_fabricated)],
    ['旧值标记次数', ...modes.map(mode => String(report.modes[mode].board_marks.superseded_marks)), null],
    ['段的检索命中', ...modes.map(mode => fmt(report.modes[mode].segment_retrieval.hit_rate)), null]
  ]
  return rows.map(row => `| ${row.map(cell => cell === null || cell === undefined ? '—' : String(cell)).join(' | ')} |`).join('\n') + '\n'
}

// fmt writes a number, and a dash for a number that is not there.
function fmt(value) {
  if (value === null || value === undefined || !Number.isFinite(Number(value))) return '—'
  const number = Number(value)
  return Number.isInteger(number) ? String(number) : number.toFixed(3)
}
