import crypto from 'node:crypto'

export const categories = new Set(['early_fact', 'constraint', 'stale', 'no_fabrication', 'irrelevant'])
export function splitForId(id) {
  const n = crypto.createHash('sha256').update(id, 'utf8').digest().readBigUInt64BE()
  return n * 5n < 3n * (1n << 64n) ? 'dev' : 'sealed'
}
export function checkScenario(s) {
  const errors = []
  if (!s || typeof s !== 'object') return ['scenario is not an object']
  if (typeof s.id !== 'string' || !/^[A-Za-z0-9_-]+$/.test(s.id)) errors.push('invalid id')
  if (!categories.has(s.category)) errors.push('unknown category')
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
  if (s.category === 'irrelevant') {
    for (const name of ['control', 'distractor']) {
      const c = s.conditions?.[name]
      if (!c || !Number.isInteger(c.replace_turn) || c.replace_turn < 1 || c.replace_turn > (s.turns?.length ?? 0) || typeof c.user !== 'string' || !c.user.trim() || typeof c.assistant !== 'string' || !c.assistant.trim()) errors.push(`invalid ${name} condition`)
    }
    if (s.conditions?.control?.replace_turn !== s.conditions?.distractor?.replace_turn) errors.push('conditions replace different turns')
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
