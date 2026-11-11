import test from 'node:test'
import assert from 'node:assert/strict'
import { categoryRate, categoryReport, checkPointsFor, judgeCategories, judgeInput, judgeVerdict, mergedOutcomes, truncationReport } from '../truncation.mjs'
import { splitForId } from '../scenarios.mjs'

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

let counter = 0
// A scenario that passes the validator. The judge input is built out of the
// history, so a scenario that cannot be materialized cannot be judged.
function scenarioOf(category, overrides = {}) {
  counter += 1
  const id = `l4-${category}-${counter}`
  const base = {
    id, category, split: splitForId(id),
    question: '出一道题',
    turns: [
      { user: '第一轮用户消息', assistant: '第一轮助手消息', user_at: '2026-01-01T00:00:00Z', assistant_at: '2026-01-01T00:00:01Z' },
      { user: '第二轮用户消息', assistant: '第二轮助手消息', user_at: '2026-01-01T00:01:00Z', assistant_at: '2026-01-01T00:01:01Z' },
      { user: '第三轮用户消息', assistant: '第三轮助手消息', user_at: '2026-01-01T00:02:00Z', assistant_at: '2026-01-01T00:02:01Z' },
    ],
    evidence_turns: [1],
    answer: { mode: 'needs_judge', required: ['只用标准库'] },
  }
  if (category === 'irrelevant') {
    base.conditions = {
      control: { replace_turn: 2, user: '控制轮用户消息', assistant: '控制轮助手消息' },
      distractor: { replace_turn: 2, user: '干扰轮用户消息', assistant: '干扰轮助手消息' },
    }
  }
  return { ...base, ...overrides }
}
const attempt = (qid, overrides = {}) => ({ qid, arm: 'truncation', condition: 'main', rep: 0, env: 'real', answer: '这里只用标准库', ...overrides })

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

test('the judge input gives the model the five fields and nothing else', () => {
  const s = scenarioOf('constraint', { answer: { mode: 'needs_judge', required: ['只用标准库', '不要引入第三方依赖'] } })
  const rows = judgeInput(new Map([[s.id, s]]), [attempt(s.id)])
  assert.equal(rows.length, 2)
  assert.deepEqual(Object.keys(rows[0].input), ['question', 'answer_key', 'evidence', 'response', 'check_point'])
  assert.equal(rows[0].item_id, s.id)
  assert.equal(rows[0].arm, 'truncation')
  assert.equal(rows[0].condition, 'main')
  assert.equal(rows[0].rep, 0)
  assert.equal(rows[0].check_point_index, 0)
  assert.equal(rows[0].input.check_point, '只用标准库')
  assert.equal(rows[1].check_point_index, 1)
  assert.equal(rows[1].input.check_point, '不要引入第三方依赖')
  assert.equal(rows[0].input.response, '这里只用标准库')
  assert.equal(rows[0].input.question, s.question)
  assert.deepEqual(rows[0].input.answer_key, s.answer)
  assert.deepEqual(rows[0].input.evidence, [{ turn: 1, user: '第一轮用户消息', assistant: '第一轮助手消息' }])
})

test('the model reads neither the arm nor the condition nor the repeat', () => {
  const s = scenarioOf('constraint')
  const rows = judgeInput(new Map([[s.id, s]]), [attempt(s.id, { arm: 'truncation', condition: 'main', rep: 2 })])
  assert.equal(rows.length, 1)
  assert.equal(rows[0].rep, 2)
  assert.equal(Object.keys(rows[0].input).includes('arm'), false)
  assert.equal(Object.keys(rows[0].input).includes('condition'), false)
  assert.equal(Object.keys(rows[0].input).includes('rep'), false)
})

test('a fabrication scenario is read at every turn that names the thing it quotes', () => {
  const s = scenarioOf('no_fabrication', {
    question: '前面提到的“雾桥”模拟面试我最终答对了几道题？',
    answer: { mode: 'needs_judge', required: ['说明本段历史没有这场模拟面试答对题数的记录'] },
    turns: [
      { user: '我约了“雾桥”模拟面试', assistant: '先把提纲准备好', user_at: '2026-01-01T00:00:00Z', assistant_at: '2026-01-01T00:00:01Z' },
      { user: '这一轮只说别的', assistant: '这一轮也不提名字', user_at: '2026-01-01T00:01:00Z', assistant_at: '2026-01-01T00:01:01Z' },
      { user: '又提到“雾桥”了', assistant: '还是“雾桥”这件事', user_at: '2026-01-01T00:02:00Z', assistant_at: '2026-01-01T00:02:01Z' },
    ],
  })
  const rows = judgeInput(new Map([[s.id, s]]), [attempt(s.id)])
  assert.equal(rows.length, 1)
  assert.deepEqual(rows[0].input.evidence.map(entry => entry.turn), [1, 3])
  assert.equal(rows[0].input.evidence[0].user, '我约了“雾桥”模拟面试')
})

test('a fabrication scenario that matches no turn stops instead of guessing', () => {
  const s = scenarioOf('no_fabrication', { question: '前面提到的“雾桥”模拟面试我最终答对了几道题？' })
  const rows = judgeInput(new Map([[s.id, s]]), [attempt(s.id)])
  assert.equal(rows.length, 1)
  assert.equal(rows[0].input, null)
  assert.match(rows[0].error, /quoted name/)
})

test('the judge asks about the v2 categories that need asking', () => {
  // A3 is in the list because its right answer is not a value but the
  // statement that nothing was decided: the value reading cannot decide it,
  // and without the judge the attempts of this category would stay pending
  // forever and no report would show them as anything at all.
  assert.deepEqual(judgeCategories().slice().sort(), ['A3', 'R2', 'R3', 'R5', 'constraint', 'irrelevant', 'no_fabrication', 'stale'].sort())
})

test('each update category is asked about the value its question wants', () => {
  assert.deepEqual(checkPointsFor({ category: 'R2' }).points, ['当前值正确，且没有拿被取代的旧值冒充当前值'])
  assert.deepEqual(checkPointsFor({ category: 'R3' }).points, ['最初值正确，且没有拿后来的新值冒充最初值'])
  assert.deepEqual(checkPointsFor({ category: 'R5', answer: { mode: 'needs_judge', required: ['只用标准库'] } }).points, ['只用标准库'])
  // Without required and without an update direction there is nothing to ask, and
  // saying so is better than inventing a check point.
  assert.ok(checkPointsFor({ category: 'R5', answer: { mode: 'needs_judge' } }).error)
  assert.ok(checkPointsFor({ category: 'R1' }).error)
})

// v3Update builds a draft-schema update scenario the scenario validator accepts:
// building the judge input reads the history through materialize, so a fixture the
// validator would refuse cannot be put to the judge at all.
function v3Update(category) {
  counter += 1
  const id = `l4-${category}-${counter}`
  const turns = Array.from({ length: 64 }, (_, index) => ({
    user: `第${index + 1}轮用户消息`, assistant: `第${index + 1}轮助手消息`,
    user_at: new Date(1767225600000 + index * 300000).toISOString(), assistant_at: new Date(1767225600000 + index * 300000 + 60000).toISOString(),
  }))
  const answer = category === 'R2'
    ? { mode: 'fact', accepted: ['26分钟'], forbidden: ['41分钟'] }
    : { mode: 'fact', accepted: ['41分钟'], forbidden: ['26分钟'] }
  return {
    id, category, split: splitForId(id), schema_version: 'l4.scenario.draft.v3',
    question: '这个计划每次多长？', turns,
    original_turn: 26, superseded_turn: 45, superseded_value: '41分钟',
    distance_to_question: 20, evidence_turns: [26, 45], answer,
  }
}

test('an undecided update attempt is put to the judge with one check point', () => {
  for (const [category, want] of [['R2', '当前值正确，且没有拿被取代的旧值冒充当前值'], ['R3', '最初值正确，且没有拿后来的新值冒充最初值']]) {
    const s = v3Update(category)
    const rows = judgeInput(new Map([[s.id, s]]), [attempt(s.id, { answer: '要么是 26 分钟，要么是 41 分钟' })])
    assert.equal(rows.length, 1, `${category}: ${JSON.stringify(rows)}`)
    assert.equal(rows[0].error, undefined, `${category}: ${rows[0].error}`)
    assert.equal(rows[0].input.check_point, want)
    assert.deepEqual(Object.keys(rows[0].input), ['question', 'answer_key', 'evidence', 'response', 'check_point'])
    assert.deepEqual(rows[0].input.evidence.map(item => item.turn), [26, 45])
  }
})

test('a middle-fact attempt is not put to the judge', () => {
  const s = v3Update('R2')
  s.category = 'R1'
  s.fact_turn = 26
  delete s.original_turn
  delete s.superseded_turn
  delete s.superseded_value
  s.distance_to_question = 64 + 1 - 26
  s.evidence_turns = [26]
  s.answer = { mode: 'fact', accepted: ['标签名'] }
  assert.deepEqual(judgeInput(new Map([[s.id, s]]), [attempt(s.id, { answer: '想不起来了' })]), [])
})

test('the category guard still refuses a name that is in neither list', () => {
  const s = scenarioOf('constraint')
  assert.throws(() => judgeInput(new Map([[s.id, s]]), [attempt(s.id)], { selected: ['R7'] }), /unknown category/)
  assert.throws(() => judgeInput(new Map([[s.id, s]]), [attempt(s.id)], { selected: ['nope'] }), /unknown category/)
})

test('an undecided update attempt leaves the category rate pending, not failed', () => {
  const s = v3Update('R2')
  const outcomes = []
  for (const rep of [0, 1, 2]) outcomes.push({ id: s.id, arm: 'truncation', condition: 'main', rep, verdict: 'needs_judge' })
  const report = categoryReport(outcomes, new Map([[s.id, s]]), { left: { arm: 'truncation', condition: 'main' }, right: { arm: 'truncation', condition: 'main' } })
  const row = report.categories.find(item => item.category === 'R2')
  assert.ok(row, JSON.stringify(report.categories.map(item => item.category)))
  assert.equal(row.left.needs_judge, 3)
  assert.equal(row.left.pass, 0)
  assert.equal(row.left.fail, 0)
})

test('a stale scenario without a required gets the synthetic check point', () => {
  const s = scenarioOf('stale', { answer: { mode: 'fact', accepted: ['48分钟'], forbidden: ['93分钟'], derived: false } })
  const rows = judgeInput(new Map([[s.id, s]]), [attempt(s.id)])
  assert.equal(rows.length, 1)
  assert.equal(rows[0].input.check_point, '当前答案正确且无旧值冒充当前值')
})

test('only the attempts that are still pending are asked about', () => {
  const s = scenarioOf('constraint')
  const records = [attempt(s.id, { rep: 0 }), attempt(s.id, { rep: 1 })]
  const pending = new Set([JSON.stringify([s.id, 'truncation', 'main', 1])])
  const rows = judgeInput(new Map([[s.id, s]]), records, { pending })
  assert.equal(rows.length, 1)
  assert.equal(rows[0].rep, 1)
})

test('an attempt is judged only when every one of its check points is there', () => {
  const outcomes = [{ id: 'one', arm: 'a', condition: 'main', rep: 0, verdict: 'needs_judge' }]
  const checkPointsOf = () => 2
  const half = mergedOutcomes(outcomes, [{ item_id: 'one', arm: 'a', condition: 'main', rep: 0, check_point_index: 0, verdict: 'yes' }], { checkPointsOf })
  assert.equal(half[0].verdict, 'needs_judge')
  const whole = mergedOutcomes(outcomes, [
    { item_id: 'one', arm: 'a', condition: 'main', rep: 0, check_point_index: 0, verdict: 'yes' },
    { item_id: 'one', arm: 'a', condition: 'main', rep: 0, check_point_index: 1, verdict: 'yes' },
  ], { checkPointsOf })
  assert.equal(whole[0].verdict, 'pass')
  assert.equal(whole[0].reason, 'judged:2')
  assert.throws(() => mergedOutcomes(outcomes, [], {}), /check points/)
})

test('a judgement never overwrites a decided outcome', () => {
  const outcomes = [
    { id: 'one', arm: 'a', condition: 'main', rep: 0, verdict: 'needs_judge' },
    { id: 'one', arm: 'a', condition: 'main', rep: 1, verdict: 'fail' },
  ]
  const merged = mergedOutcomes(outcomes, [{ item_id: 'one', arm: 'a', condition: 'main', rep: 0, check_point_index: 0, verdict: 'no' }], { checkPointsOf: () => 1 })
  assert.equal(merged[0].verdict, 'fail')
  assert.equal(merged[1].verdict, 'fail')
  assert.throws(() => mergedOutcomes(outcomes, [{ item_id: 'one', arm: 'a', condition: 'main', rep: 0, check_point_index: 0, verdict: 'maybe' }], { checkPointsOf: () => 1 }), /verdict/)
  assert.throws(() => mergedOutcomes(outcomes, [
    { item_id: 'one', arm: 'a', condition: 'main', rep: 0, check_point_index: 0, verdict: 'yes' },
    { item_id: 'one', arm: 'a', condition: 'main', rep: 0, check_point_index: 0, verdict: 'no' },
  ], { checkPointsOf: () => 1 }), /duplicate/)
})

test('one failing check point fails the attempt and one unsure leaves it pending', () => {
  assert.equal(judgeVerdict(['yes', 'yes']), 'pass')
  assert.equal(judgeVerdict(['yes', 'no']), 'fail')
  assert.equal(judgeVerdict(['yes', 'error']), 'error')
  assert.equal(judgeVerdict(['yes', 'unsure']), 'needs_judge')
  assert.equal(judgeVerdict([]), 'needs_judge')
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

test('a category is compared over the conditions its own attempts run under', () => {
  const constraint = scenarioOf('constraint')
  const irrelevant = scenarioOf('irrelevant')
  const scenarios = new Map([[constraint.id, constraint], [irrelevant.id, irrelevant]])
  const outcomes = []
  for (const arm of ['baseline', 'truncation']) {
    for (const condition of ['control', 'distractor']) {
      for (const rep of [0, 1, 2]) {
        outcomes.push({ id: irrelevant.id, arm, condition, rep, verdict: arm === 'baseline' ? 'pass' : 'fail' })
      }
    }
    for (const rep of [0, 1, 2]) {
      outcomes.push({ id: constraint.id, arm, condition: 'main', rep, verdict: 'needs_judge' })
    }
  }
  const report = categoryReport(outcomes, scenarios, { left: { arm: 'baseline', condition: 'main' }, right: { arm: 'truncation', condition: 'main' } })
  const constraintRow = report.categories.find(row => row.category === 'constraint')
  const irrelevantRow = report.categories.find(row => row.category === 'irrelevant')
  assert.deepEqual(constraintRow.conditions, ['main'])
  // The paired scenarios run under two conditions, so a selector naming one
  // condition would leave the category empty and read as "nothing to
  // compare".
  assert.deepEqual(irrelevantRow.conditions, ['control', 'distractor'])
  assert.equal(irrelevantRow.left.attempts, 6)
  assert.equal(irrelevantRow.right.attempts, 6)
  assert.equal(irrelevantRow.difference_conservative, 1)
})

test('the category report compares the categories apart and leaves the pending ones pending', () => {
  const constraint = scenarioOf('constraint')
  const fact = scenarioOf('early_fact', { answer: { mode: 'fact', accepted: ['Lantern'] } })
  const scenarios = new Map([[constraint.id, constraint], [fact.id, fact]])
  const outcomes = []
  for (const arm of ['baseline', 'truncation']) {
    for (const rep of [0, 1, 2]) {
      outcomes.push({ id: constraint.id, arm, condition: 'main', rep, verdict: 'needs_judge' })
      outcomes.push({ id: fact.id, arm, condition: 'main', rep, verdict: arm === 'baseline' ? 'pass' : 'fail' })
    }
  }
  const report = categoryReport(outcomes, scenarios, { left: { arm: 'baseline', condition: 'main' }, right: { arm: 'truncation', condition: 'main' } })
  const constraintRow = report.categories.find(row => row.category === 'constraint')
  const factRow = report.categories.find(row => row.category === 'early_fact')
  // Without a judgement the constraint answers stay pending: they are not
  // failures and they are not successes either.
  assert.equal(constraintRow.left.needs_judge, 3)
  assert.equal(constraintRow.left.success_rate_conservative, 0)
  assert.equal(constraintRow.difference_conservative, 0)
  assert.equal(factRow.left.success_rate_conservative, 1)
  assert.equal(factRow.right.success_rate_conservative, 0)
  assert.equal(factRow.difference_conservative, 1)
  // With a judgement of every check point the constraint category becomes
  // readable, and a judgement of one check point of two does not.
  const judgments = outcomes.filter(row => row.id === constraint.id && row.arm === 'baseline').map(row => ({ item_id: row.id, arm: row.arm, condition: row.condition, rep: row.rep, check_point_index: 0, verdict: 'yes' }))
  const judged = categoryReport(outcomes, scenarios, {
    left: { arm: 'baseline', condition: 'main' }, right: { arm: 'truncation', condition: 'main' },
    judgments,
  })
  assert.equal(judged.categories.find(row => row.category === 'constraint').left.success_rate_conservative, 1)
  assert.equal(judged.judged_check_points, 3)
  assert.equal(judged.judged_attempts, 3)
  const partial = categoryReport(outcomes, scenarios, {
    left: { arm: 'baseline', condition: 'main' }, right: { arm: 'truncation', condition: 'main' },
    judgments: judgments.slice(0, 1),
  })
  // Only the attempt whose check point was judged becomes decided; the two
  // attempts that were not judged stay pending.
  assert.equal(partial.categories.find(row => row.category === 'constraint').left.needs_judge, 2)
  assert.equal(partial.categories.find(row => row.category === 'constraint').left.pass, 1)
})
