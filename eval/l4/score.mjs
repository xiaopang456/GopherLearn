import { checkScenario, isMultiQuestion, resolveAttempt } from './scenarios.mjs'
import { MENTION_EXPRESSION, canonicalExpression, chineseNumber, compoundMention, normalizeValue } from './normalize.mjs'

// Normalize presentation differences centrally, keeping answer keys semantic.
// Only a complete quantity is converted; numbers inside narrative answers still
// require judgment, including negations and comparisons of old/new values.
//
// The measure words and the normalization itself live in normalize.mjs, which the
// answer key's own comparison calls too: one table, one function, so "9道" and "九道"
// are the same value everywhere.
export { normalizeFact } from './normalize.mjs'

// Normalize numeric expressions without touching surrounding prose. Match spans
// in the original string so negation handling retains its original offsets.
// The expression scanner, the canonical form and the measure-word table all come
// from normalize.mjs.
const FACT_EXPR = MENTION_EXPRESSION

// A fact answer is judged by what the answer says, not by whether it says it
// in one word. The answer mentions a value or it does not; a value that the
// answer denies or withdraws does not count as mentioned; and a mention whose
// denial is not clear enough to call stays pending instead of being decided.
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
// The product prepends a fixed banner when its knowledge retrieval found
// nothing (or was unavailable) and the answer fell back to general knowledge.
// The banner is retrieval status, not a statement about any answer value, but
// its "不是你上传笔记中的结论" reads as a hard negation to the clause
// analysis, so it is removed before the answer is judged.
const STANDARD_FALLBACK_BANNERS = [
  '你的个人知识库中没有找到足以回答该问题的笔记证据。\n以下内容来自模型的通用知识，不是你上传笔记中的结论，可能与你的笔记表述不一致。',
  '当前个人知识库检索暂时不可用，本次没有使用你上传的笔记。\n以下内容来自模型的通用知识，请在知识库恢复后再核对。',
]
function stripStandardFallbackBanners(text) {
  let out = text
  for (const banner of STANDARD_FALLBACK_BANNERS) {
    out = out.split(banner.normalize('NFKC')).join('\n')
  }
  return out
}
// A soft marker says the value belongs to the past. It does not deny the
// value, and it is not confident enough to call either: such an answer stays
// pending instead of being decided.
//
// A marker can also sit inside an attribution: "你之前定的名字是北砚",
// "这是你在之前定下时说的". Such a marker says where the value came from
// instead of putting the value in the past, but only when the phrase the
// attribution opens does not cover the value. When the phrase does cover it
// ("这是你之前叫的北砚"), the marker modifies the value itself, so the
// mention stays historical and the answer stays pending.
const SOFT_ATTRIBUTION = /[你您](?:在|于)?(?:之前|原来|原先|此前|以前|先前)/u
// A copula closes the phrase that an attribution opens.
const PHRASE_CLOSE = /(?:是|为|叫做|叫作|即)/u
// phraseEnd returns where the phrase an attribution opens ends: at the next
// copula or at the end of the sentence.
function phraseEnd(text, from) {
  const rest = text.slice(from)
  const close = PHRASE_CLOSE.exec(rest)
  const stop = rest.search(STRONG_BREAK)
  if (close && (stop < 0 || close.index < stop)) return from + close.index
  if (stop >= 0) return from + stop
  return text.length
}
// softPastScopes reports whether a soft marker in the given clause puts the
// mention in the past. A plain marker always does. A marker inside an
// attribution only does when the phrase that the attribution opens covers the
// mention: "这是你之前叫的北砚" puts the mention in the past, while
// "你之前定的名字是北砚" does not.
function softPastScopes(text, clauseStart, clauseEnd, span) {
  const clause = text.slice(clauseStart, clauseEnd)
  if (!hasMarker(clause, SOFT_MARKER)) return false
  const attribution = SOFT_ATTRIBUTION.exec(clause)
  if (!attribution) return true
  const start = clauseStart + attribution.index
  const end = phraseEnd(text, clauseStart + attribution.index + attribution[0].length)
  return span.start >= start && span.end <= end
}
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

// compactFolded is the haystack the mentions are looked for in: the same folding the
// answer key's values get, so that a value and the answer that states it are written
// the same way. Every character keeps the index it had in the original text, so a
// mention can still be pointed at in the original.
function compactFolded(text) {
  let out = ''
  const map = []
  for (let i = 0; i < text.length; i += 1) {
    if (/\s/u.test(text[i])) continue
    for (const character of text[i].toLowerCase()) {
      out += character
      map.push(i)
    }
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
  // The value and the answer go through the same normalization: the value becomes its
  // canonical form, and every mention the answer contains is put into the same form
  // before the two are compared.
  const canonical = normalizeValue(value)
  const haystack = compactFolded(text)
  if (!canonical.startsWith('text:')) {
    FACT_EXPR.lastIndex = 0
    for (let match = FACT_EXPR.exec(haystack.text); match; match = FACT_EXPR.exec(haystack.text)) {
      if (canonicalExpression(match[0]) !== canonical) continue
      const start = match.index, end = start + match[0].length
      const expression = match[0].toLowerCase()
      const before = haystack.text.slice(Math.max(0, start - 2), start)
      const after = haystack.text.slice(end)
      // A bare 点 may mean a point, not a clock time; even a precise
      // unqualified twelve-hour expression may mean AM or PM. Compound
      // words such as 次元 and 天气 are not count/duration mentions.
      const spokenClock = expression.includes('点')
      const hasPeriod = /^(?:凌晨|早上|上午|中午|下午|晚上|傍晚)/u.test(expression)
      const hour = spokenClock ? chineseNumber(expression.replace(/^(?:凌晨|早上|上午|中午|下午|晚上|傍晚)/u, '').split('点')[0]) : 0
      const ambiguousClock = spokenClock && ((!hasPeriod && hour <= 12) || /[这那哪每]/u.test(before.slice(-1)) || /^(?:建议|意见|原因|问题|内容|要求|理由|好处|区别|方面|注意|重要)/u.test(after))
      const ambiguousCompound = compoundMention(expression, after) || /[a-z]$/iu.test(expression) && /^[a-z]/iu.test(after)
      const embedded = /[a-z0-9零〇一二两兩三四五六七八九十百千万萬.]/iu.test(text[haystack.map[start] - 1] ?? '')
      found.push({ start: haystack.map[start], end: haystack.map[end - 1] + 1, value, ambiguous: ambiguousClock || ambiguousCompound || embedded })
    }
    return found
  }
  // A value that is not an expression is matched literally, in the same folded form
  // the answer key's own comparison uses.
  const needle = canonical.slice('text:'.length)
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
    if (!holdsAnotherValue(before, span, spans) && softPastScopes(text, before[0], before[1], span)) return 'uncertain'
  }
  for (const range of forward) {
    if (holdsAnotherValue(range, span, spans)) continue
    if (softPastScopes(text, range[0], range[1], span)) return 'uncertain'
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
  const text = stripStandardFallbackBanners(String(answer).normalize('NFKC'))
  const accepted = [...scenario.answer.accepted, ...(scenario.answer.aliases ?? [])]
  const forbidden = [...(scenario.answer.forbidden ?? [])]
  const spans = []
  for (const value of accepted) spans.push(...mentionsOf(text, value))
  for (const value of forbidden) spans.push(...mentionsOf(text, value))
  for (const span of spans) span.state = span.ambiguous ? 'uncertain' : mentionState(text, span, spans)
  // R3 asks about history: saying the original value was cancelled does not
  // make the historical claim false. Send any answer naming both values to
  // the R3 semantic checkpoint, including explicit withdrawals or denials.
  if (scenario.category === 'R3' && spans.some(span => accepted.includes(span.value)) && spans.some(span => forbidden.includes(span.value))) {
    return { verdict: 'needs_judge', reason: 'mixed_fact_values' }
  }
  const reading = factReading(spans, accepted)
  const wrong = factReading(spans, forbidden)
  if (reading.uncertain || wrong.uncertain) return { verdict: 'needs_judge', reason: 'negation_uncertain' }
  if (reading.present && !wrong.present) return { verdict: 'pass', reason: 'fact_value_present' }
  if (wrong.present && !reading.present) return { verdict: 'fail', reason: 'forbidden_value_present' }
  if (reading.present && wrong.present) return { verdict: 'needs_judge', reason: 'mixed_fact_values' }
  return { verdict: 'needs_judge', reason: 'no_fact_value' }
}

// scoreAttempt scores one attempt.
//
// A scenario of the first round has one question, so the record names the
// scenario and the scenario's own answer key decides the attempt. A scenario
// of the second round's F1 shape has several questions, each with its own
// key, so the record names the question it answers and the attempt is
// scored against that question's key. The attempt id carries the question
// number, so the three questions of one scenario are three attempts and not
// one attempt counted thrice.
export function scoreAttempt(record, scenario, condition = 'main') {
  const multi = isMultiQuestion(scenario)
  const attempt = resolveAttempt(scenario, record)
  const base = { schema: 'l4.outcome.v1', id: multi ? (attempt?.attempt ?? record?.qid ?? scenario?.id) : scenario?.id, condition, rep: record?.rep, arm: record?.arm, env: record?.env }
  const result = (verdict, reason) => ({ ...base, verdict, reason })
  if (checkScenario(scenario).length) return result('error', 'invalid_scenario')
  if (!attempt || typeof record?.arm !== 'string' || !/^[A-Za-z0-9._-]+$/.test(record.arm) || !Number.isInteger(record?.rep) || record.rep < 0 || record.rep > 2) return result('error', 'identity_mismatch')
  if (!(scenario.category === 'irrelevant' ? ['control', 'distractor'].includes(condition) : condition === 'main')) return result('error', 'condition_mismatch')
  if (record.error || typeof record.answer !== 'string' || !record.answer.trim()) return result('error', 'answer_unavailable')
  if (!['fake', 'real'].includes(record.env)) return result('error', 'unknown_environment')
  if (record.env === 'real' && record.backgroundAudit?.valid !== true) return result('error', 'background_unverified')
  const key = attempt.answer ?? scenario.answer
  if (key?.mode === 'needs_judge') {
    const out = result('needs_judge', 'semantic_judgment_required')
    if (multi) out.question_index = attempt.index
    return out
  }
  // The fact reading wants the scenario shape, because it reads the
  // category: a scenario that asks about history is judged differently from
  // one that asks about the present. Only the answer key changes, and only
  // for a scenario with more than one question.
  const judged = judgeFact(record.answer, multi ? { ...scenario, answer: key } : scenario)
  const out = result(judged.verdict, judged.reason)
  if (multi) out.question_index = attempt.index
  return out
}
