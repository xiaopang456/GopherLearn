import { checkScenario } from './scenarios.mjs'
// Normalize presentation differences centrally, keeping answer keys semantic.
// Only a complete quantity is converted; numbers inside narrative answers still
// require judgment, including negations and comparisons of old/new values.
export function normalizeFact(s) {
  const text = s.normalize('NFKC').replace(/\s+/gu, '').replace(/[。.!！]$/u, '').toLowerCase()
  const quantity = /^(\d+(?:\.\d+)?)(分钟|分鐘|分|mins?|minutes?|小时|小時|时|時|h|hrs?|hours?|秒钟|秒鐘|秒|s|secs?|seconds?)$/u.exec(text)
  if (!quantity) return text
  const [, value, unit] = quantity
  const factor = /^(分钟|分鐘|分|mins?|minutes?)$/u.test(unit) ? 60n : /^(小时|小時|时|時|h|hrs?|hours?)$/u.test(unit) ? 3600n : 1n
  const [integer, fraction = ''] = value.split('.')
  let numerator = BigInt(integer + fraction) * factor, denominator = 10n ** BigInt(fraction.length)
  let a = numerator, b = denominator
  while (b) [a, b] = [b, a % b]
  numerator /= a; denominator /= a
  return `seconds:${numerator}/${denominator}`
}

// A fact answer is judged by what the answer says, not by whether it says it
// in one word. The answer mentions a value or it does not; a value that the
// answer denies or withdraws does not count as mentioned; and a mention whose
// denial is not clear enough to call stays pending instead of being decided.
const DURATION_MENTION = /(\d+(?:\.\d+)?)(分钟|分鐘|小时|小時|时|時|秒钟|秒鐘|秒|分|minutes|minute|mins|min|hours|hour|hrs|hr|seconds|second|secs|sec|h|s)/giu
// A denial negates the value that follows it.
const HARD_NEGATION = [
  '不是', '并非', '并不是', '并不', '而非', '没有', '不再', '不等于', '不算', '有误', '错了',
  /\b(?:not|isn't|is not|no longer|never|rather than|instead of|wrong|incorrect|obsolete)\b/iu,
]
// A withdrawal cancels the value that precedes it.
const HARD_CANCEL = [
  '不再执行', '不再采用', '不再适用', '不再有效', '不再生效', '已取消', '已经取消', '作废', '已作废', '不算了',
  /\b(?:no longer applies|is cancelled|is canceled|is void|was cancelled|was canceled|is dropped)\b/iu,
]
// A soft marker says the value belongs to the past. It does not deny the
// value, and it is not confident enough to call either: such an answer stays
// pending instead of being decided.
const SOFT_MARKER = [
  '原来', '之前', '此前', '原先', '以前的', '旧的', '曾经', '早先',
  /\b(?:old|older|previous|previously|formerly|used to be|existed|earlier)\b/iu,
]
// "而是" introduces the affirmed term, "而不是" denies the one that follows.
const AFFIRM_INTRODUCER = '而是'
// A weak break separates clauses inside one sentence; a strong break ends the
// sentence or the thought. A denial or a withdrawal binds only inside the
// strong segment it stands in.
const CLAUSE_BREAK = '，,、:：'
const STRONG_BREAK = '；;。！？!?\n'

function compact(text) {
  let out = ''
  const map = []
  for (let i = 0; i < text.length; i += 1) {
    if (/\s/u.test(text[i])) continue
    out += text[i]
    map.push(i)
  }
  return { text: out, map }
}

function hasMarker(window, markers) {
  return markers.some(marker => (typeof marker === 'string' ? window.includes(marker) : marker.test(window)))
}

function lastMarkerIndex(window, markers) {
  let at = -1
  for (const marker of markers) {
    if (typeof marker === 'string') {
      const index = window.lastIndexOf(marker)
      if (index > at) at = index
      continue
    }
    const found = [...window.matchAll(new RegExp(marker.source, marker.flags.includes('g') ? marker.flags : marker.flags + 'g'))]
    if (found.length) {
      const index = found[found.length - 1].index
      if (index > at) at = index
    }
  }
  return at
}

// clauseRanges splits an absolute range into the parts a denial can bind to.
// A part that holds another value belongs to that value, not to this one.
function clauseRanges(text, from, to) {
  const ranges = []
  let start = from
  for (let i = from; i < to; i += 1) {
    if (CLAUSE_BREAK.includes(text[i])) {
      if (i > start) ranges.push([start, i])
      start = i + 1
    }
  }
  if (to > start) ranges.push([start, to])
  return ranges
}

// mentionsOf returns every place the answer states the value.
function mentionsOf(text, value) {
  const found = []
  const normalized = normalizeFact(value)
  const isDuration = normalized.startsWith('seconds:')
  const haystack = compact(text)
  if (isDuration) {
    DURATION_MENTION.lastIndex = 0
    for (let match = DURATION_MENTION.exec(haystack.text); match; match = DURATION_MENTION.exec(haystack.text)) {
      if (normalizeFact(match[0]) !== normalized) continue
      found.push({ start: haystack.map[match.index], end: haystack.map[match.index + match[0].length - 1] + 1, value })
    }
    return found
  }
  const needle = compact(value).text
  if (!needle) return found
  let at = haystack.text.indexOf(needle)
  while (at >= 0) {
    found.push({ start: haystack.map[at], end: haystack.map[at + needle.length - 1] + 1, value })
    at = haystack.text.indexOf(needle, at + needle.length)
  }
  return found
}

// holdsAnotherValue reports whether a clause already states a different value.
function holdsAnotherValue(range, span, spans) {
  return spans.some(other => other !== span && other.start < range[1] && other.end > range[0])
}

// mentionState decides how one mention of a value reads.
function mentionState(text, span, spans) {
  const windowStart = Math.max(0, span.start - 24)
  let backwardFrom = windowStart
  for (let i = span.start - 1; i >= windowStart; i -= 1) {
    if (STRONG_BREAK.includes(text[i])) { backwardFrom = i + 1; break }
  }
  const backward = clauseRanges(text, backwardFrom, span.start)
  const before = backward.length ? backward[backward.length - 1] : null
  const forwardTo = Math.min(text.length, span.end + 24)
  let forwardEnd = forwardTo
  for (let i = span.end; i < forwardTo; i += 1) {
    if (STRONG_BREAK.includes(text[i])) { forwardEnd = i; break }
  }
  const forward = clauseRanges(text, span.end, forwardEnd)
  if (before && !holdsAnotherValue(before, span, spans)) {
    const clause = text.slice(before[0], before[1])
    if (/而不是/u.test(clause.slice(-3))) return 'negated'
    const affirmAt = clause.lastIndexOf(AFFIRM_INTRODUCER)
    const negationAt = lastMarkerIndex(clause, HARD_NEGATION)
    if (affirmAt >= 0 && affirmAt > negationAt) return 'present'
    if (negationAt >= 0) return 'negated'
  }
  for (const range of forward) {
    if (holdsAnotherValue(range, span, spans)) continue
    const clause = text.slice(range[0], range[1])
    if (hasMarker(clause, HARD_CANCEL)) return 'negated'
    if (hasMarker(clause, HARD_NEGATION)) return 'negated'
  }
  if (before) {
    const clause = text.slice(before[0], before[1])
    if (!holdsAnotherValue(before, span, spans) && hasMarker(clause, SOFT_MARKER)) return 'uncertain'
  }
  for (const range of forward) {
    if (holdsAnotherValue(range, span, spans)) continue
    if (hasMarker(text.slice(range[0], range[1]), SOFT_MARKER)) return 'uncertain'
  }
  return 'present'
}

// factReading is what the answer says about one list of values.
function factReading(spans, values) {
  const states = spans.filter(span => values.includes(span.value)).map(span => span.state)
  return {
    present: states.some(state => state === 'present'),
    uncertain: states.some(state => state === 'uncertain'),
  }
}

// judgeFact decides a scenario whose answer key names the values it expects
// and the values it forbids.
export function judgeFact(answer, scenario) {
  const text = answer.normalize('NFKC')
  const accepted = [...scenario.answer.accepted, ...(scenario.answer.aliases ?? [])]
  const forbidden = [...(scenario.answer.forbidden ?? [])]
  const spans = []
  for (const value of accepted) spans.push(...mentionsOf(text, value))
  for (const value of forbidden) spans.push(...mentionsOf(text, value))
  for (const span of spans) span.state = mentionState(text, span, spans)
  const reading = factReading(spans, accepted)
  const wrong = factReading(spans, forbidden)
  if (reading.uncertain || wrong.uncertain) return { verdict: 'needs_judge', reason: 'negation_uncertain' }
  if (reading.present && !wrong.present) return { verdict: 'pass', reason: 'fact_value_present' }
  if (wrong.present && !reading.present) return { verdict: 'fail', reason: 'forbidden_value_present' }
  if (reading.present && wrong.present) return { verdict: 'needs_judge', reason: 'mixed_fact_values' }
  return { verdict: 'needs_judge', reason: 'no_fact_value' }
}

export function scoreAttempt(record, scenario, condition = 'main') {
  const base = { schema: 'l4.outcome.v1', id: scenario?.id, condition, rep: record?.rep, arm: record?.arm, env: record?.env }
  const result = (verdict, reason) => ({ ...base, verdict, reason })
  if (checkScenario(scenario).length) return result('error', 'invalid_scenario')
  if (record?.qid !== scenario.id || typeof record?.arm !== 'string' || !/^[A-Za-z0-9._-]+$/.test(record.arm) || !Number.isInteger(record?.rep) || record.rep < 0 || record.rep > 2) return result('error', 'identity_mismatch')
  if (!(scenario.category === 'irrelevant' ? ['control', 'distractor'].includes(condition) : condition === 'main')) return result('error', 'condition_mismatch')
  if (record.error || typeof record.answer !== 'string' || !record.answer.trim()) return result('error', 'answer_unavailable')
  if (!['fake', 'real'].includes(record.env)) return result('error', 'unknown_environment')
  if (record.env === 'real' && record.backgroundAudit?.valid !== true) return result('error', 'background_unverified')
  if (scenario.answer.mode === 'needs_judge') return result('needs_judge', 'semantic_judgment_required')
  const judged = judgeFact(record.answer, scenario)
  return result(judged.verdict, judged.reason)
}
