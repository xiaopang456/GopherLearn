import { categories, materialize } from './scenarios.mjs'

// The statistics of one comparison between two rolling-summary policies.
//
// The comparison has two questions that the ordinary report cannot answer.
//
// The first is what the truncation rule cost: a summary that outgrew its
// budget is trimmed, and what is trimmed is gone. The second is whether
// what was gone was the evidence a question is asked about. A dropped item
// cites the messages it rested on, so the dropped message ids are the loss
// in the units the questions are written in, and the evidence turns of the
// frozen scenarios are known by their position in the history.
//
// The third is the per-category success rate. A fact answer is decided by
// the structure, but a constraint answer is not: whether an answer obeyed the
// constraint the learner stated needs a judgement. The script therefore
// prepares that judgement and takes its result back; it never decides
// semantics itself.

// A turn of one prepared conversation, with the message ids the summary
// cited. The user message of a turn is the only message a summary may cite,
// so the turn of a message id is found through the user messages.
function turnIndexByMessageID(warmup) {
  const index = new Map()
  const turns = Array.isArray(warmup?.turns) ? warmup.turns : []
  for (let i = 0; i < turns.length; i += 1) {
    const id = turns[i]?.UserID
    if (Number.isInteger(id) && id > 0 && !index.has(id)) index.set(id, i + 1)
  }
  return index
}

// truncationEvents are the cuts one turn of one conversation reported.
function truncationEvents(warmup) {
  const events = []
  const turns = Array.isArray(warmup?.turns) ? warmup.turns : []
  for (let i = 0; i < turns.length; i += 1) {
    const truncations = turns[i]?.Summary?.Truncations
    if (!Array.isArray(truncations)) continue
    for (const truncation of truncations) {
      if (!truncation || typeof truncation !== 'object') continue
      events.push({
        turn: i + 1,
        covered_message_id: turns[i]?.Summary?.CoveredMessageID ?? null,
        kind: truncation.Kind ?? null,
        group: truncation.Group ?? null,
        kept: truncation.Kept ?? null,
        dropped: truncation.Dropped ?? null,
        dropped_message_ids: Array.isArray(truncation.DroppedMessageIDs) ? truncation.DroppedMessageIDs : [],
      })
    }
  }
  return events
}

// truncationReport says what the budget cut out of each conversation and
// whether what it cut was the evidence a question is about.
//
// scenarios is a Map of scenario id to scenario, warmups a Map of scenario id
// to the warmup report of the preparation of that scenario.
export function truncationReport(scenarios, warmups, { arm = 'unknown' } = {}) {
  if (!(scenarios instanceof Map) || !(warmups instanceof Map)) throw new Error('invalid truncation report input')
  const rows = []
  const totals = { scenarios: 0, truncation_events: 0, dropped_items: 0, dropped_message_ids: 0, scenarios_with_truncation: 0, scenarios_losing_evidence_turn: 0, lost_evidence_turns: 0, degraded_turns: 0 }
  for (const [id, scenario] of [...scenarios].sort(([a], [b]) => a.localeCompare(b))) {
    const warmup = warmups.get(id)
    if (!warmup) continue
    const index = turnIndexByMessageID(warmup)
    const events = truncationEvents(warmup)
    const evidence = new Set((scenario.evidence_turns ?? []).map(n => Number(n)))
    const lostEvidenceTurns = new Set()
    const lostMessageIds = new Set()
    const unknownMessageIds = new Set()
    for (const event of events) {
      for (const id of event.dropped_message_ids) {
        lostMessageIds.add(id)
        const turn = index.get(id)
        if (turn === undefined) { unknownMessageIds.add(id); continue }
        if (evidence.has(turn)) lostEvidenceTurns.add(turn)
      }
    }
    const lost = [...lostEvidenceTurns].sort((a, b) => a - b)
    totals.scenarios += 1
    totals.truncation_events += events.length
    totals.dropped_items += events.reduce((total, event) => total + (event.dropped ?? 0), 0)
    totals.dropped_message_ids += lostMessageIds.size
    totals.degraded_turns += warmup.degraded_turns ?? 0
    if (events.length) totals.scenarios_with_truncation += 1
    if (lost.length) { totals.scenarios_losing_evidence_turn += 1; totals.lost_evidence_turns += lost.length }
    rows.push({
      id, category: scenario.category ?? null, evidence_turns: [...evidence].sort((a, b) => a - b),
      truncation_events: events.length,
      dropped_items: events.reduce((total, event) => total + (event.dropped ?? 0), 0),
      dropped_message_ids: [...lostMessageIds].sort((a, b) => a - b),
      lost_evidence_turns: lost,
      unknown_message_ids: [...unknownMessageIds].sort((a, b) => a - b),
      degraded_turns: warmup.degraded_turns ?? 0,
      summary_failure_reasons: warmup.summary_failure_reasons ?? {},
      events,
    })
  }
  return {
    schema: 'l4.truncation.v1', arm, scenarios: rows, totals,
    note: 'A lost evidence turn is a turn of the frozen history whose message was cited by an item the compression budget dropped. It is the loss in the units the questions are written in; the questions themselves are scored separately.',
  }
}

// attemptKey names one attempt of one scenario.
//
// The identity is kept beside the payload the model reads and never inside
// it: the judge is asked about an answer, not about which run produced it.
export function attemptKey(item_id, arm, condition, rep) {
  return JSON.stringify([item_id ?? null, arm ?? null, condition ?? 'main', rep ?? null])
}

// judgeInput prepares the rows a judge has to read.
//
// The row is one check point of one attempt. The model reads the `input`
// object and nothing else: the question, the answer key, the evidence of the
// history, the answer, and the one check point to judge. The identity of the
// attempt — which scenario, which arm, which condition, which repeat, and
// which check point — sits beside that payload and is not part of what the
// model reads.
//
// The evidence follows the fixed rule of the judging window: the scenarios
// that name their evidence turns are read at those turns, and the ones that
// do not are read at every turn that mentions the name the question quotes.
// A scenario where the rule finds nothing stops here rather than being
// guessed at: the row is written with an error and no input, so the gap is
// visible instead of silently missing.
export function judgeInput(scenarios, records, { selected = judgeCategories(), pending = null } = {}) {
  if (!(scenarios instanceof Map) || !Array.isArray(records)) throw new Error('invalid judge input')
  const wanted = new Set(selected)
  for (const category of wanted) if (!categories.has(category)) throw new Error('unknown category')
  const rows = []
  for (const record of records) {
    const scenario = scenarios.get(record?.qid)
    if (!scenario) continue
    if (!wanted.has(scenario.category)) continue
    const condition = record.condition ?? 'main'
    if (pending && !pending.has(attemptKey(scenario.id, record.arm ?? null, condition, record.rep ?? null))) continue
    const identity = { item_id: scenario.id, arm: record.arm ?? null, condition, rep: record.rep ?? null }
    if (typeof record.answer !== 'string' || !record.answer.trim()) {
      rows.push({ ...identity, check_point_index: null, error: 'the answer of this attempt is not available', input: null })
      continue
    }
    const evidence = evidenceFor(scenario, condition)
    const checkPoints = checkPointsFor(scenario)
    if (evidence.error || checkPoints.error) {
      rows.push({ ...identity, check_point_index: null, error: evidence.error ?? checkPoints.error, input: null })
      continue
    }
    for (let index = 0; index < checkPoints.points.length; index += 1) {
      rows.push({
        ...identity,
        check_point_index: index,
        input: {
          question: scenario.question,
          answer_key: scenario.answer,
          evidence: evidence.evidence,
          response: record.answer,
          check_point: checkPoints.points[index],
        },
      })
    }
  }
  return rows.sort((a, b) => [a.item_id, a.arm ?? '', a.condition, String(a.rep), String(a.check_point_index)].join('|').localeCompare([b.item_id, b.arm ?? '', b.condition, String(b.rep), String(b.check_point_index)].join('|')))
}

// judgeCategories are the categories whose answers are not decided by the
// structure and are therefore judged.
export function judgeCategories() {
  return ['constraint', 'irrelevant', 'no_fabrication', 'stale']
}

// quotedName is the name the question quotes, when it quotes exactly one.
//
// The rule is a plain substring match on the raw text: case sensitive, no
// normalisation, no shortening. A question that quotes no name or more than
// one has no single name to look for, and the extraction stops there.
export function quotedName(question) {
  const found = [...String(question ?? '').matchAll(/“([^”]*)”/gu)].map(match => match[1])
  if (found.length !== 1 || !found[0].trim()) return null
  return found[0]
}

// evidenceFor reads the evidence of one item out of the history it was asked
// about.
//
// The scenarios that name their evidence turns are read at those turns of the
// condition's own history; the others are read at every turn that mentions
// the name the question quotes. Nothing is added to either, and nothing is
// cut: the judge reads the turns as they were.
export function evidenceFor(scenario, condition = 'main') {
  let history
  try {
    history = materialize(scenario, condition)
  } catch (error) {
    return { error: `the history of this condition is not available: ${error.message}` }
  }
  if (scenario.category === 'no_fabrication') {
    const name = quotedName(scenario.question)
    if (!name) return { error: 'the question quotes no single name to look for' }
    const evidence = []
    for (let index = 0; index < history.length; index += 1) {
      const turn = history[index]
      if (turn.user.includes(name) || turn.assistant.includes(name)) {
        evidence.push({ turn: index + 1, user: turn.user, assistant: turn.assistant })
      }
    }
    if (!evidence.length) return { error: 'the quoted name matches no turn of the history' }
    return { evidence }
  }
  const turns = Array.isArray(scenario.evidence_turns) ? scenario.evidence_turns : []
  if (!turns.length) return { error: 'the scenario names no evidence turn' }
  const evidence = []
  for (const number of turns) {
    const turn = history[number - 1]
    if (!turn) return { error: `evidence turn ${number} is outside the history` }
    evidence.push({ turn: number, user: turn.user, assistant: turn.assistant })
  }
  return { evidence }
}

// checkPointsFor are the checkpoints of one scenario: one per required.
//
// A scenario that asks for a current answer and names no required gets one
// synthetic checkpoint, because there is still one question to answer: is the
// current answer right and is no old value passed off as the current one.
export function checkPointsFor(scenario) {
  const required = scenario.answer?.required
  if (Array.isArray(required) && required.length) return { points: required }
  if (scenario.category === 'stale') return { points: ['当前答案正确且无旧值冒充当前值'] }
  return { error: 'the scenario names no check point' }
}

// mergedOutcomes replaces the pending outcomes with the judgements of the
// judge.
//
// A judgement settles one check point of one attempt, and the attempt is
// decided by its check points: one "no" fails it, otherwise one "error"
// fails it as a run, otherwise one "unsure" leaves it undecided, and only
// "yes" everywhere passes it.
//
// An outcome the structure decided is never overwritten, and an attempt whose
// check points are not all there stays pending: half of the checkpoints of
// an attempt is not a judgement of it. The number of check points an attempt
// has is not known here, so it is given: checkPointsOf answers how many
// check points a scenario has, and a scenario it cannot answer for keeps its
// pending outcomes pending rather than being judged on an unknown count.
export function mergedOutcomes(outcomes, judgments, { checkPointsOf } = {}) {
  if (!Array.isArray(outcomes) || !Array.isArray(judgments)) throw new Error('invalid outcome merge')
  if (typeof checkPointsOf !== 'function') throw new Error('the number of check points of a scenario is not known')
  const groups = new Map()
  for (const judgment of judgments) {
    if (!judgment || typeof judgment !== 'object') throw new Error('invalid judgement')
    if (!['yes', 'no', 'unsure', 'error'].includes(judgment.verdict)) throw new Error('invalid judgement verdict')
    const item = judgment.item_id ?? judgment.id
    if (typeof item !== 'string' || typeof judgment.arm !== 'string' || !Number.isInteger(judgment.rep) || !Number.isInteger(judgment.check_point_index) || judgment.check_point_index < 0) throw new Error('invalid judgement identity')
    const identity = attemptKey(item, judgment.arm, judgment.condition ?? 'main', judgment.rep)
    if (!groups.has(identity)) groups.set(identity, new Map())
    const points = groups.get(identity)
    if (points.has(judgment.check_point_index)) throw new Error('duplicate judgement')
    points.set(judgment.check_point_index, judgment.verdict)
  }
  return outcomes.map(row => {
    if (row?.verdict !== 'needs_judge') return row
    const group = groups.get(attemptKey(row.id, row.arm, row.condition, row.rep))
    if (!group) return row
    const expected = checkPointsOf(row.id)
    if (!Number.isInteger(expected) || expected < 1) return row
    if (group.size !== expected) return row
    const verdicts = [...group.keys()].sort((a, b) => a - b).map(index => group.get(index))
    return { ...row, verdict: judgeVerdict(verdicts), reason: `judged:${group.size}` }
  })
}

// judgeVerdict is what the check points of one attempt say about it.
export function judgeVerdict(verdicts) {
  if (!verdicts.length) return 'needs_judge'
  if (verdicts.includes('no')) return 'fail'
  if (verdicts.includes('error')) return 'error'
  if (verdicts.includes('unsure')) return 'needs_judge'
  if (verdicts.every(verdict => verdict === 'yes')) return 'pass'
  return 'needs_judge'
}

// categoryRate is the success rate of one category inside one arm.
//
// The three repeats of a scenario stay together: a scenario that passed once
// is not the same evidence as a scenario that passed three times.
export function categoryRate(rows, category) {
  const groups = new Map()
  for (const row of rows) {
    if (row?.category !== category) continue
    if (!groups.has(row.id)) groups.set(row.id, [])
    groups.get(row.id).push(row)
  }
  let pass = 0, fail = 0, pending = 0, errors = 0, attempts = 0, complete = 0, all = 0
  for (const values of groups.values()) {
    attempts += values.length
    const passed = values.filter(row => row.verdict === 'pass').length
    pass += passed
    fail += values.filter(row => row.verdict === 'fail').length
    pending += values.filter(row => row.verdict === 'needs_judge').length
    errors += values.filter(row => row.verdict === 'error').length
    if (values.length === 3) { complete += 1; all += Number(passed === 3) }
  }
  const scenarios = groups.size
  return {
    category, scenarios, attempts, pass, fail, needs_judge: pending, errors,
    success_rate_conservative: attempts ? pass / attempts : null,
    success_rate_adjudicated: pass + fail ? pass / (pass + fail) : null,
    scenario_all_pass_rate: scenarios ? all / scenarios : null,
    complete_scenarios: complete,
  }
}

// categoryReport is the per-category comparison of two arms.
//
// The categories are kept apart on purpose: the question is not whether the
// answers got worse, it is whether the answers that depend on what the
// summary kept got worse.
//
// A category is compared over the conditions its own attempts run under. The
// paired scenarios run under two conditions and the rest under one, so a
// selector naming a condition the category does not use would leave the
// category empty and read as "nothing to compare". The conditions a category
// was compared over are named in the row.
export function categoryReport(outcomes, scenarios, { left, right, judgments = [] } = {}) {
  if (!Array.isArray(outcomes) || !(scenarios instanceof Map)) throw new Error('invalid category report')
  if (!left?.arm || !right?.arm) throw new Error('invalid comparison selectors')
  const checkPointsOf = id => {
    const scenario = scenarios.get(id)
    if (!scenario) return null
    const points = checkPointsFor(scenario)
    return points.error ? null : points.points.length
  }
  const merged = mergedOutcomes(outcomes, judgments, { checkPointsOf })
  const withCategory = merged.map(row => ({ ...row, category: scenarios.get(row.id)?.category ?? null }))
  const conditionsOf = category => {
    const present = [...new Set(withCategory.filter(row => row.category === category).map(row => row.condition))].sort()
    if (present.includes(left.condition)) return [left.condition]
    if (present.includes(right.condition)) return [right.condition]
    return present
  }
  const rows = []
  for (const category of [...categories].sort()) {
    const conditions = conditionsOf(category)
    const a = categoryRate(withCategory.filter(row => row.arm === left.arm && conditions.includes(row.condition)), category)
    const b = categoryRate(withCategory.filter(row => row.arm === right.arm && conditions.includes(row.condition)), category)
    const leftPass = a.pass, rightPass = b.pass, leftAttempts = a.attempts, rightAttempts = b.attempts
    rows.push({
      category, conditions, left: a, right: b,
      difference_conservative: leftAttempts && rightAttempts ? leftPass / leftAttempts - rightPass / rightAttempts : null,
      difference_adjudicated: a.pass + a.fail && b.pass + b.fail ? a.pass / (a.pass + a.fail) - b.pass / (b.pass + b.fail) : null,
    })
  }
  return {
    schema: 'l4.category.v1', left, right, categories: rows,
    judged_check_points: judgments.length,
    judged_attempts: new Set(judgments.map(judgment => attemptKey(judgment.item_id ?? judgment.id, judgment.arm, judgment.condition ?? 'main', judgment.rep))).size,
    note: 'A category whose answers are decided by a judge needs the judgements to be read as decided; without them its rate is pending and not a failure. An attempt is decided only when every one of its check points is judged. Each category is compared over the conditions its own attempts run under.',
  }
}
