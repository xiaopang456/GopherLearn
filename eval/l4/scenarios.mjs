import crypto from 'node:crypto'
import { normalizeValue } from './normalize.mjs'

export const categories = new Set(['early_fact', 'constraint', 'stale', 'no_fabrication', 'irrelevant'])
export function splitForId(id) {
  const n = crypto.createHash('sha256').update(id, 'utf8').digest().readBigUInt64BE()
  return n * 5n < 3n * (1n << 64n) ? 'dev' : 'sealed'
}
// factValueKey is how the answer key compares its own values: the same normalization
// the scorer applies before it looks for a mention. So "9道" and "九道" are one value
// here as well, and a key that accepts one of them cannot forbid the other.
function factValueKey(value) {
  return normalizeValue(value)
}
export function checkScenario(s) {
  const errors = []
  if (!s || typeof s !== 'object') return ['scenario is not an object']
  if (typeof s.id !== 'string' || !/^[A-Za-z0-9_-]+$/.test(s.id)) errors.push('invalid id')
  if (!categories.has(s.category) && !(s.schema_version === 'l4.scenario.draft.v3' && /^R[1-6]$/.test(s.category))) errors.push('unknown category')
  if (s.split !== splitForId(String(s.id))) errors.push('split differs from id hash')
  if (typeof s.question !== 'string' || !s.question.trim()) errors.push('missing question')
  if (!Array.isArray(s.turns) || s.turns.length < 1) errors.push('missing history')
  let last = -Infinity
  for (const t of Array.isArray(s.turns) ? s.turns : []) {
    if (!t || typeof t !== 'object') { errors.push('invalid turn'); continue }
    const u = Date.parse(t.user_at), a = Date.parse(t.assistant_at)
    if (typeof t.user !== 'string' || !t.user.trim() || typeof t.assistant !== 'string' || !t.assistant.trim()) errors.push('empty message')
    if (!Number.isFinite(u) || !Number.isFinite(a) || u <= last || a <= u) errors.push('invalid timestamps')
    last = a
  }
  if (!Array.isArray(s.evidence_turns) || s.evidence_turns.some(n => !Number.isInteger(n) || n < 1 || n > (s.turns?.length ?? 0))) errors.push('invalid evidence turns')
  if (!['fact', 'needs_judge'].includes(s.answer?.mode)) errors.push('unknown scoring mode')
  if (s.answer?.mode === 'fact' && (!Array.isArray(s.answer.accepted) || !s.answer.accepted.length || s.answer.accepted.some(v => typeof v !== 'string' || !v.trim()))) errors.push('missing accepted answers')
  for (const key of ['aliases', 'forbidden']) {
    const values = s.answer?.[key]
    if (values !== undefined && (!Array.isArray(values) || values.some(v => typeof v !== 'string' || !v.trim()))) errors.push(`invalid ${key}`)
  }
  // An answer key that names a value both right and wrong cannot be scored: the
  // scorer would find the same mention acceptable and forbidden, and only the
  // order in which it inspects them would decide the attempt. Full-width digits
  // and spacing are the same value to the scorer, so they are the same value
  // here.
  const keys = list => (Array.isArray(list) ? list : []).map(factValueKey)
  const acceptedKeys = keys(s.answer?.accepted), forbiddenKeys = keys(s.answer?.forbidden), aliasKeys = keys(s.answer?.aliases)
  if (acceptedKeys.some(value => forbiddenKeys.includes(value))) errors.push('accepted value is also forbidden')
  if (aliasKeys.some(value => forbiddenKeys.includes(value))) errors.push('alias is also forbidden')
  if (s.category === 'irrelevant') {
    for (const name of ['control', 'distractor']) {
      const c = s.conditions?.[name]
      if (!c || !Number.isInteger(c.replace_turn) || c.replace_turn < 1 || c.replace_turn > (s.turns?.length ?? 0) || typeof c.user !== 'string' || !c.user.trim() || typeof c.assistant !== 'string' || !c.assistant.trim()) errors.push(`invalid ${name} condition`)
    }
    if (s.conditions?.control?.replace_turn !== s.conditions?.distractor?.replace_turn) errors.push('conditions replace different turns')
  }
  if (s.schema_version === 'l4.scenario.draft.v3') {
    const N = s.turns?.length ?? 0
    if (N < 60 || N > 110) errors.push('invalid v3 history length')
    const valid = n => Number.isInteger(n) && n >= 1 && n <= N
    if (s.category === 'R2' || s.category === 'R3') {
      if (!valid(s.original_turn) || !valid(s.superseded_turn) || s.original_turn <= 25 || s.original_turn > 40 || s.superseded_turn < 45 || s.superseded_turn > 70 || s.original_turn >= s.superseded_turn || N + 1 - s.original_turn < 20 || N + 1 - s.superseded_turn < 20) errors.push('invalid update turns')
      if (typeof s.superseded_value !== 'string' || !s.superseded_value.trim()) errors.push('invalid old value')
      if (s.distance_to_question !== N + 1 - s.superseded_turn) errors.push('invalid distance')
      if (!Array.isArray(s.evidence_turns) || !s.evidence_turns.includes(s.original_turn) || !s.evidence_turns.includes(s.superseded_turn)) errors.push('missing update evidence')
      const values = s.category === 'R2' ? s.answer?.forbidden : s.answer?.accepted
      if (!Array.isArray(values) || !values.includes(s.superseded_value)) errors.push('old value absent from answer key')
    } else {
      if (!valid(s.fact_turn) || s.distance_to_question !== N + 1 - s.fact_turn || (!Array.isArray(s.evidence_turns) || !s.evidence_turns.includes(s.fact_turn))) errors.push('invalid fact turn or distance')
      if (['R1', 'R5'].includes(s.category) && (s.fact_turn < 30 || s.fact_turn > 50 || s.distance_to_question < 20)) errors.push('invalid middle turn')
      if (s.category === 'R4' && ![2, 3].includes(s.distance_to_question)) errors.push('invalid tail turn')
      if (s.category === 'R6' && (s.fact_turn < 3 || s.fact_turn > 10)) errors.push('invalid early turn')
    }
    if (s.category === 'R5' ? s.answer?.mode !== 'needs_judge' : s.answer?.mode !== 'fact') errors.push('invalid category answer mode')
  }
  return [...new Set(errors)]
}
export function materialize(s, condition = 'main') {
  const errors = checkScenario(s)
  if (errors.length) throw new Error(errors.join('; '))
  const turns = structuredClone(s.turns)
  if (s.category === 'irrelevant') {
    if (!['control', 'distractor'].includes(condition)) throw new Error('paired scenario requires a condition')
    const c = s.conditions[condition]
    turns[c.replace_turn - 1].user = c.user
    turns[c.replace_turn - 1].assistant = c.assistant
  } else if (condition !== 'main') throw new Error('unexpected condition')
  return turns
}
