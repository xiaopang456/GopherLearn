import { checkScenario, isMultiQuestion, resolveAttempt } from './scenarios.mjs'
import { MENTION_EXPRESSION, canonicalExpression, chineseNumber, compoundMention, foldCharacters, normalizeValue } from './normalize.mjs'

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
  // The haystack is the answer, folded by the folding of normalize.mjs - the same one
  // the values of the answer key go through. Every character keeps the index it had
  // in the folded text, so a mention can still be pointed at.
  const haystack = foldCharacters(text)
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

// ---------------------------------------------------------------------------
// The value in force now, and the value that was in force before.
//
// The answer key of a scenario that asks for a value names the value it expects
// and the value it forbids. When the question asks for the value in force now,
// the forbidden value is the one that was in force before - and an answer may
// well name it, to say what the value was and what it was changed to. Reading
// "the forbidden value is written down" as "the forbidden value is the answer"
// is the mistake this part of the scorer must not make.
//
// The three cases are read apart:
//
//   - the answer gives the value in force and says the other one was replaced
//     -> pass. The old value is named, but not as the answer.
//   - the answer gives the other value as the value in force
//     -> fail. That is the answer the key forbids.
//   - the answer names both without saying which one is in force, or names the
//     value in force in a writing the key does not use
//     -> pending. The relation cannot be read reliably from the text, so the
//     item goes to the semantic judging instead of being decided by matching
//     strings.

// A value the answer puts in the past. The soft markers of mentionState read
// "原来", "之前" and the rest; these are the ones it does not know. They are
// read here only to keep an old value from being failed.
const PAST_MARKER = [...SOFT_MARKER, '最初', '最早', '起初', '一开始', '当初', '以前', '过去', '当年', '原本']
// A replacement: the marker that names the change between the value that was
// in force and the value that took its place. The markers of a withdrawal are
// not here - a withdrawn value is already read as a withdrawn one.
const REPLACEMENT_MARKER = [
  '改成', '改为', '调整成', '调整为', '调到', '调成', '换成', '变成', '更新成', '更新为',
  '取代', '替代', '改成了', '换成了', '已取代', '被取代',
  /\b(?:changed to|changed into|replaced by|updated to|switched to)\b/iu,
]
// The runs of characters that mean nothing on their own. A run like "方式" is
// no sign that the answer named the value the key names.
const GENERIC_RUN = ['方式', '方法', '数量', '内容', '问题', '时间', '名字', '地方', '情况', '结果', '部分', '东西', '一次', '一个', '几个', '这个', '那个']

// sentenceRange returns the range of the sentence a position is in.
function sentenceRange(text, at) {
  let start = 0
  for (let i = at - 1; i >= 0; i -= 1) {
    if (STRONG_BREAK.includes(text[i])) { start = i + 1; break }
  }
  let end = text.length
  for (let i = at; i < text.length; i += 1) {
    if (STRONG_BREAK.includes(text[i])) { end = i; break }
  }
  return [start, end]
}

// readsAsOld reports whether a mention of the old value is read as the value
// that was in force before: a past marker or a replacement marker stands
// where it does.
function readsAsOld(text, span, spans) {
  const [sentenceStart, sentenceEnd] = sentenceRange(text, span.start)
  const backward = clauseRanges(text, sentenceStart, span.start)
  const before = backward.length ? backward[backward.length - 1] : null
  if (before && !holdsAnotherValue(before, span, spans)) {
    const clause = text.slice(before[0], before[1])
    if (hasMarker(clause, PAST_MARKER) || hasMarker(clause, REPLACEMENT_MARKER)) return true
  }
  for (const range of clauseRanges(text, span.end, sentenceEnd)) {
    const clause = text.slice(range[0], range[1])
    // a replacement marker names the change between the two values, so it is
    // read even in a clause that holds the value in force
    if (hasMarker(clause, REPLACEMENT_MARKER)) return true
    if (holdsAnotherValue(range, span, spans)) continue
    if (hasMarker(clause, PAST_MARKER)) return true
  }
  return false
}

// superseded reports whether the answer says the old value was replaced: the
// value in force is given, the old value is read as the old one, and a marker
// that names the change stands between them.
function superseded(text, oldSpans, currentSpans, spans) {
  for (const old of oldSpans) {
    if (!readsAsOld(text, old, spans)) continue
    for (const current of currentSpans) {
      // the value in force has to stand after the old one: "it was that, and
      // it was changed to this"
      if (current.start <= old.end) continue
      // the window reaches to the end of the sentence the value in force is
      // in, so that "14 道题已经被 9 道题取代" is read as a replacement too
      const [, sentenceEnd] = sentenceRange(text, current.start)
      if (!hasMarker(text.slice(old.end, sentenceEnd), REPLACEMENT_MARKER)) continue
      return true
    }
  }
  return false
}

// valueNumber is the number a value carries, and '' when it carries none.
function valueNumber(canonical) {
  if (canonical.startsWith('quantity:')) return canonical.split(':')[1]
  if (canonical.startsWith('seconds:')) return canonical.slice('seconds:'.length)
  const text = canonical.startsWith('text:') ? canonical.slice('text:'.length) : canonical
  const found = /^([0-9]+(?:\.[0-9]+)?|[零〇一二两兩三四五六七八九十百千万萬]+)/u.exec(text)
  if (!found) return ''
  const number = chineseNumber(found[1])
  return Number.isFinite(number) ? String(number) : ''
}

// valueCore is the characters a value is written with, the digits left out.
function valueCore(canonical) {
  const text = canonical.startsWith('text:') ? canonical.slice('text:'.length) : canonical
  return [...text].filter(character => /[\u3400-\u9fff]/u.test(character)).join('')
}

// quantitiesIn returns the quantities the answer names, each with the state it
// is read in: a quantity the answer denies or withdraws is not a value it
// states.
function quantitiesIn(text, spans) {
  const haystack = foldCharacters(text)
  const found = []
  FACT_EXPR.lastIndex = 0
  for (let match = FACT_EXPR.exec(haystack.text); match; match = FACT_EXPR.exec(haystack.text)) {
    const canonical = canonicalExpression(match[0])
    if (!canonical || canonical.startsWith('clock:')) continue
    const start = haystack.map[match.index]
    const end = haystack.map[match.index + match[0].length - 1] + 1
    const span = { start, end, value: canonical }
    span.state = mentionState(text, span, [...spans, ...found])
    found.push(span)
  }
  return found
}

// longestRun returns the longest run of characters the two texts share.
function longestRun(one, other) {
  let best = ''
  for (let i = 0; i < one.length; i += 1) {
    for (let j = one.length; j > i; j -= 1) {
      const run = one.slice(i, j)
      if (!other.includes(run)) continue
      if (run.length > best.length) best = run
      break
    }
  }
  return best
}

// deniedRun reports whether the run of characters the answer shares with the
// key is denied where it stands: "不是跳转" names no value.
function deniedRun(folded, run) {
  let at = folded.indexOf(run)
  while (at >= 0) {
    const before = folded.slice(Math.max(0, at - 12), at)
    if (hasMarker(before, HARD_NEGATION)) return true
    at = folded.indexOf(run, at + 1)
  }
  return false
}

// currentValueEcho reports whether the answer names the value in force in
// another writing than the key writes it: the key says "9道题" and the answer
// says "9 道", or the key says "重定向跳转" and the answer says "跳转". The
// number is the value; the measure word and the counter noun around it are
// not. Such an answer names the value in force, so it is never failed on the
// forbidden value alone.
function currentValueEcho(text, spans, values) {
  const folded = foldCharacters(text).text
  const quantities = quantitiesIn(text, spans)
  for (const value of values) {
    const canonical = normalizeValue(value)
    const number = valueNumber(canonical)
    if (number) {
      const stated = quantities.some(quantity => quantity.state === 'present' && valueNumber(quantity.value) === number)
      if (stated) return { value, kind: 'number' }
      continue
    }
    const core = valueCore(canonical)
    if (core.length < 2) continue
    const run = longestRun(core, folded)
    if (run.length < 2) continue
    // A run of the edge of the key ("跳转" of "重定向跳转") is a sign of the
    // value; a run inside the key ("取消传" of "上下文取消传递") is a sign of it
    // too, but only when it is long enough to be more than a word.
    const atTheEdge = core.startsWith(run) || core.endsWith(run)
    if (!atTheEdge && run.length < 3) continue
    if (GENERIC_RUN.includes(run)) continue
    if (deniedRun(folded, run)) continue
    return { value, kind: 'text' }
  }
  return null
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
  const currentSpans = spans.filter(span => accepted.includes(span.value) && span.state === 'present')
  const oldPresent = spans.filter(span => forbidden.includes(span.value) && span.state === 'present')
  const oldSpans = spans.filter(span => forbidden.includes(span.value) && span.state !== 'negated')
  // The answer gives the value in force and says the old one was replaced.
  // This is read before the uncertainty of a mention, because an old value
  // standing in a past clause is what a replacement statement looks like.
  if (currentSpans.length && oldSpans.length && superseded(text, oldSpans, currentSpans, spans)) {
    return { verdict: 'pass', reason: 'fact_value_present_old_value_superseded' }
  }
  if (reading.uncertain || wrong.uncertain) return { verdict: 'needs_judge', reason: 'negation_uncertain' }
  if (reading.present && !wrong.present) return { verdict: 'pass', reason: 'fact_value_present' }
  if (reading.present && wrong.present) return { verdict: 'needs_judge', reason: 'mixed_fact_values' }
  // The value in force is not affirmed. An answer that names it in another
  // writing names it all the same, so it is not read as an answer that names
  // no value at all - and it is never failed on the forbidden value alone.
  const echo = currentValueEcho(text, spans, accepted)
  if (wrong.present) {
    if (echo) return { verdict: 'needs_judge', reason: 'current_value_echo' }
    // A forbidden value the answer puts in the past is not the answer either:
    // the answer gives no value in force, which is not the same as giving the
    // wrong one.
    if (oldPresent.length && oldPresent.every(span => readsAsOld(text, span, spans))) {
      return { verdict: 'needs_judge', reason: 'old_value_only' }
    }
    return { verdict: 'fail', reason: 'forbidden_value_present' }
  }
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
  // The question index of the outcome is the round of the conversation the
  // question was asked at, when the scenario names that round. The turn says
  // where in the conversation the answer has to come from; the position in the
  // list says only where in the file the question was written down.
  const questionIndex = attempt?.turn ?? attempt?.index
  if (key?.mode === 'needs_judge') {
    const out = result('needs_judge', 'semantic_judgment_required')
    if (multi) out.question_index = questionIndex
    return out
  }
  // The fact reading wants the scenario shape, because it reads the
  // category: a scenario that asks about history is judged differently from
  // one that asks about the present. Only the answer key changes, and only
  // for a scenario with more than one question.
  const judged = judgeFact(record.answer, multi ? { ...scenario, answer: key } : scenario)
  const out = result(judged.verdict, judged.reason)
  if (multi) out.question_index = questionIndex
  return out
}
