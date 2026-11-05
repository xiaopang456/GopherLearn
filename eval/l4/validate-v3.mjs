import fs from 'node:fs'
import path from 'node:path'
import {checkScenario} from './scenarios.mjs'

// Dataset checks complement the per-scenario structural checks; human review
// remains necessary for semantics, transitions, paraphrases and relative dates.
// Every check below either fails the dataset or lands on the review list with
// the turn it came from; nothing is quietly dropped.

const SECONDS_PER = {分钟: 60n, 分鐘: 60n, 分: 60n, min: 60n, mins: 60n, minute: 60n, minutes: 60n, 小时: 3600n, 小時: 3600n, 时: 3600n, 時: 3600n, h: 3600n, hr: 3600n, hrs: 3600n, hour: 3600n, hours: 3600n, 秒钟: 1n, 秒鐘: 1n, 秒: 1n, sec: 1n, secs: 1n, second: 1n, seconds: 1n, s: 1n}
const QUANTITY = /^(\d+(?:\.\d+)?)(分钟|分鐘|分|mins?|minutes?|小时|小時|时|時|h|hrs?|hours?|秒钟|秒鐘|秒|secs?|seconds?)$/
// A bare number is compared against the answer's own number, so the unit the
// filler used never hides a near miss.
const NUMBER = /(\d+(?:[.,]\d+)?)\s*(分钟|分鐘|小时|小時|秒钟|秒鐘|天|周|轮|次|题|页|条|个|段|分钟|ms|s|h|分|秒)?/g
const NEAR_TOLERANCE = 3
const DERIVED_MULTIPLIERS = [2n, 3n, 4n]
const GRAM_WIDTH = 12
const RELATIVE_TIME = [/今天/, /明天/, /后天/, /昨天/, /前天/, /本周/, /这周/, /上周/, /下周/, /下个月/, /上个月/, /本月/, /月初/, /月末/, /周末/, /今年/, /明年/, /几年后/, /[一两二三四五六七八九十\d]+\s*(?:天|小时|分钟|周|个月)后/, /再过/, /稍后/, /待会/, /刚才/, /刚刚/, /最近/, /近期/, /\b(?:today|tomorrow|yesterday|next week|last week|in \d+ days)\b/i]
const DATE_WITH_YEAR_AND_WEEKDAY = /(20\d{2})[-年](\d{1,2})[-月](\d{1,2})日?\s*(?:星期|周|礼拜)([一二三四五六日天])/g
const MONTH_DAY_WEEKDAY = /(?<![\d年月])(\d{1,2})月(\d{1,2})日\s*(?:星期|周|礼拜)([一二三四五六日天])/g
const DATE_WITH_YEAR = /(20\d{2})[-年](\d{1,2})[-月](\d{1,2})日?/g
const DATE_WITHOUT_YEAR = /(?<![\d年月])(\d{1,2})月(\d{1,2})日/g
const WEEKDAYS = {'日': 0, '天': 0, '一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6}

const fold = text => String(text ?? '').normalize('NFKC').replace(/\s+/gu, '').toLowerCase()
const compact = text => String(text ?? '').normalize('NFKC').replace(/\s+/gu, '')

// quantitySeconds turns "29分钟" into 1740 so a different spelling of the same
// length is caught; a value with no unit returns null and is only compared as a
// bare number.
function quantitySeconds(value) {
 const match = QUANTITY.exec(fold(value))
 if (!match) return null
 const [whole, digits, unit] = match
 const [integer, fraction = ''] = digits.split('.')
 return (BigInt(integer + fraction) * SECONDS_PER[unit]) / (10n ** BigInt(fraction.length))
}

function numbers(text) {
 const found = []
 for (const match of compact(text).matchAll(NUMBER)) {
  const digits = match[1].replace(',', '.')
  const value = Number(digits)
  if (!Number.isFinite(value) || digits === '') continue
  found.push({value, unit: match[2] ?? '', seconds: quantitySeconds(match[1] + (match[2] ?? '')), at: match.index})
 }
 return found
}

// answerValues are the quantities and names the key already declares.
function answerValues(scenario) {
 const answer = scenario.answer ?? {}
 const lists = [answer.accepted ?? [], answer.forbidden ?? [], answer.aliases ?? [], scenario.superseded_value ?? '']
 const out = []
 for (const list of lists) for (const value of [].concat(list)) {
  const text = String(value ?? '').trim()
  if (!text) continue
  out.push({text, folded: fold(text), seconds: quantitySeconds(text), numbers: [...new Set(numbers(text).map(item => item.value))]})
 }
 return out
}

function turnText(turn) {
 return `${turn.user}\u0000${turn.assistant}`
}

function scenarioRange(scenario) {
 const turns = scenario.turns ?? []
 const first = Date.parse(turns[0]?.user_at ?? ''), last = Date.parse(turns[turns.length - 1]?.assistant_at ?? '')
 return {first, last}
}

// gramsOf returns the fixed-width character windows of one history, which is how
// two histories written from the same sentences are spotted without trusting a
// single exact-match comparison.
function gramsOf(scenario, width = GRAM_WIDTH) {
 const grams = new Set()
 for (const turn of scenario.turns ?? []) {
  const text = compact(turnText(turn))
  for (let at = 0; at + width <= text.length; at += 1) grams.add(text.slice(at, at + width))
 }
 return grams
}

export function validateDataset(scenarios, priorTexts = []) {
 const errors = [], review = [], names = new Map(), ids = new Set(), turnsSeen = new Map(), grams = new Map(), owned = []
 const prior = priorTexts.map(fold)
 const add = (s, message) => errors.push({id: s.id, message})
 const note = (s, kind, detail, turn) => review.push({id: s.id, kind, detail, ...(turn === undefined ? {} : {turn})})
 for (const s of scenarios) {
  for (const e of checkScenario(s)) add(s, e)
  if (ids.has(s.id)) add(s, 'duplicate id')
  ids.add(s.id)
  if (typeof s.entity_name !== 'string' || !s.entity_name.trim()) { add(s, 'missing entity_name'); continue }
  const name = fold(s.entity_name)
  if (names.has(name)) add(s, `duplicate name: ${names.get(name)}`)
  names.set(name, s.id)
  if (prior.some(text => text.includes(name))) add(s, 'name found in earlier dataset')
  const evidence = new Set(Array.isArray(s.evidence_turns) ? s.evidence_turns : [])
  const values = answerValues(s)
  owned.push({owner: s.id, kind: 'name', value: name, text: s.entity_name})
  for (const value of values) if (value.folded.length >= 2) owned.push({owner: s.id, kind: 'answer', value: value.folded, text: value.text})
  const quantities = values.filter(value => value.seconds !== null)
  const bare = [...new Set(values.flatMap(value => value.numbers).filter(Number.isFinite))]
  const middle = s.category === 'R2' || s.category === 'R3'
  const keyTurn = middle ? s.superseded_turn : s.fact_turn
  // A total, a per-part length and a difference between the two key values are
  // all readings a model could answer with; they are listed, never assumed safe.
  const pairs = []
  for (let i = 0; i < bare.length; i += 1) for (let j = i + 1; j < bare.length; j += 1) pairs.push({left: bare[i], right: bare[j], sum: bare[i] + bare[j], difference: Math.abs(bare[i] - bare[j])})

  // The declared key turn has to actually carry the answer, otherwise every
  // distance check passes while the evidence points at nothing.
  const at = index => {
   const turn = (s.turns ?? [])[index - 1]
   return turn ? fold(turnText(turn)) : ''
  }
  if (keyTurn >= 1 && keyTurn <= (s.turns?.length ?? 0)) {
   const text = at(keyTurn)
   if (s.answer?.mode === 'fact') {
    if (middle) {
     // The turn that changed the value has to state the value that replaced the
     // old one. Which key holds that value depends on what the question asks
     // for: the current value is accepted when the question wants today's answer
     // and forbidden when the question wants the original one.
     const current = (s.category === 'R2' ? s.answer?.accepted : s.answer?.forbidden) ?? []
     if (!current.some(value => fold(value) && text.includes(fold(value)))) add(s, `current value absent from superseded turn ${keyTurn}`)
    } else if (!(s.answer?.accepted ?? []).some(value => fold(value) && text.includes(fold(value)))) {
     add(s, `accepted value absent from key turn ${keyTurn}`)
    }
   } else {
    note(s, 'key turn needs a reader', `turn ${keyTurn} is where the constraint is stated; whether it reads as lasting is not decidable here`, keyTurn)
   }
   if (!text.includes(name)) add(s, `name absent from key turn ${keyTurn}`)
   if (middle) {
    const original = at(s.original_turn)
    if (!original.includes(fold(s.superseded_value))) add(s, `old value absent from original turn ${s.original_turn}`)
    if (!original.includes(name)) add(s, `plan name absent from original turn ${s.original_turn}`)
   }
  }
  const seen = new Set()
  // A topic map is what lets a reader check the transitions in order, so it has
  // to describe the history it is attached to instead of drifting behind it.
  const historyLength = s.turns?.length ?? 0
  if (Array.isArray(s.segments) && s.segments.length) {
   let next = 1
   for (const [index, segment] of s.segments.entries()) {
    if (!segment || typeof segment.topic !== 'string' || !segment.topic.trim()) { add(s, `segment ${index + 1} has no topic`); continue }
    if (segment.start_turn !== next) { add(s, `segment ${index + 1} starts at ${segment.start_turn}, expected ${next}`); continue }
    if (!Number.isInteger(segment.end_turn) || segment.end_turn < segment.start_turn) { add(s, `segment ${index + 1} has an invalid range`); continue }
    next = segment.end_turn + 1
   }
   if (next !== historyLength + 1) add(s, `segments reach turn ${next - 1}, but the history has ${historyLength} turns`)
  } else if (historyLength) {
   note(s, 'no topic map', 'the scenario carries no segment list, so a reader cannot walk the topic order in one pass')
  }
  for (const [index, turn] of (s.turns ?? []).entries()) {
   const number = index + 1
   const raw = turnText(turn)
   const text = compact(raw)
   if (seen.has(text)) add(s, `duplicate turn ${number}`)
   seen.add(text)
   const elsewhere = turnsSeen.get(text)
   if (elsewhere && !elsewhere.has(s.id)) add(s, `turn ${number} reuses history text from ${[...elsewhere].join(', ')}`)
   if (!elsewhere) turnsSeen.set(text, new Set([s.id]))
   else elsewhere.add(s.id)
   if (evidence.has(number)) {
    if (!middle && number !== keyTurn) note(s, 'extra evidence turn', `turn ${number} is evidence but not the declared key turn`, number)
    continue
   }
   const lower = fold(raw)
   if (lower.includes(name)) add(s, `name occurs outside evidence at turn ${number}`)
   for (const value of values) if (value.folded && lower.includes(value.folded)) add(s, `answer occurs outside evidence at turn ${number}: ${value.text}`)
   // A different spelling of the same quantity is still the answer.
   for (const item of numbers(text)) {
    if (item.seconds !== null && quantities.some(q => q.seconds === item.seconds)) add(s, `answer value occurs outside evidence at turn ${number} in another unit: ${item.value}${item.unit}`)
    const close = bare.find(own => Math.abs(own - item.value) <= NEAR_TOLERANCE && own !== item.value)
    if (close !== undefined) note(s, 'near value', `turn ${number} says ${item.value}${item.unit || ''}, within ${NEAR_TOLERANCE} of the key value ${close}`, number)
    for (const own of bare) for (const factor of DERIVED_MULTIPLIERS) {
     const width = Number(factor)
     if (item.value === own * width || (own % width === 0 && item.value === own / width)) note(s, 'derived value', `turn ${number} says ${item.value}${item.unit || ''}, a multiple or a division of the key value ${own}`, number)
    }
    for (const pair of pairs) {
     if (item.value === pair.sum || item.value === pair.difference) note(s, 'derived value', `turn ${number} says ${item.value}${item.unit || ''}, the sum or difference of the key values ${pair.left} and ${pair.right}`, number)
    }
   }
   for (const pattern of RELATIVE_TIME) if (pattern.test(raw)) note(s, 'relative time', `turn ${number} matches ${pattern.source}`, number)
   const {first, last} = scenarioRange(s)
   const conversationYear = Number.isFinite(first) ? new Date(first).getUTCFullYear() : Number.NaN
   const dated = []
   for (const match of raw.matchAll(DATE_WITH_YEAR_AND_WEEKDAY)) dated.push({year: Number(match[1]), month: Number(match[2]), day: Number(match[3]), weekday: match[4], raw: match[0]})
   for (const match of raw.matchAll(MONTH_DAY_WEEKDAY)) dated.push({year: conversationYear, month: Number(match[1]), day: Number(match[2]), weekday: match[3], raw: match[0], noYear: true})
   for (const match of raw.matchAll(DATE_WITH_YEAR)) dated.push({year: Number(match[1]), month: Number(match[2]), day: Number(match[3]), weekday: '', raw: match[0]})
   for (const match of raw.matchAll(DATE_WITHOUT_YEAR)) dated.push({year: conversationYear, month: Number(match[1]), day: Number(match[2]), weekday: '', raw: match[0], noYear: true})
   const reported = new Set()
   for (const date of dated) {
    const key = `${date.year}-${date.month}-${date.day}-${date.weekday}`
    if (reported.has(key)) continue
    reported.add(key)
    const day = new Date(Date.UTC(date.year, date.month - 1, date.day))
    if (day.getUTCFullYear() !== date.year || day.getUTCMonth() !== date.month - 1 || day.getUTCDate() !== date.day) { add(s, `impossible date at turn ${number}: ${date.raw}`); continue }
    if (date.weekday && day.getUTCDay() !== WEEKDAYS[date.weekday]) add(s, `weekday/date mismatch at turn ${number}: ${date.raw}`)
    if (date.noYear) note(s, 'date without a year', `turn ${number} says ${date.raw}; it is read as ${date.year}`, number)
    else if (Number.isFinite(first) && (day.getTime() < first - 86400000 || day.getTime() > last + 180 * 86400000)) note(s, 'date outside the conversation window', `turn ${number} says ${date.raw}`, number)
   }
  }
  const question = fold(s.question)
  for (const value of values) if (value.folded && question.includes(value.folded)) add(s, 'question contains an answer')
  if (middle && !question.includes(name)) add(s, 'question missing plan name')
  if (middle && /\d/.test(compact(s.question))) add(s, 'question carries a number, which hints at the answer')
  for (const required of s.answer?.required ?? []) {
   if (question.includes(fold(required))) add(s, `question states the constraint it is testing: ${required}`)
   else if (compact(required).length < 6) note(s, 'short check point', `required "${required}" is short enough that a paraphrase can pass it`, undefined)
  }
  grams.set(s.id, gramsOf(s))
 }
 // One scenario's name or answer must never be readable inside another: the
 // histories are separate sessions, and a shared word would let a question be
 // answered from the wrong history.
 for (const s of scenarios) {
  const others = owned.filter(entry => entry.owner !== s.id && entry.value.length >= 2)
  if (!others.length) continue
  for (const [index, turn] of (s.turns ?? []).entries()) {
   const lower = fold(turnText(turn))
   for (const entry of others) if (lower.includes(entry.value)) add(s, `turn ${index + 1} carries the ${entry.kind} "${entry.text}" of ${entry.owner}`)
  }
  const asked = fold(s.question)
  for (const entry of others) if (asked.includes(entry.value)) add(s, `the question carries the ${entry.kind} "${entry.text}" of ${entry.owner}`)
 }
 for (const [left, leftGrams] of grams) for (const [right, rightGrams] of grams) {
  if (left >= right) continue
  let shared = 0
  for (const gram of leftGrams) if (rightGrams.has(gram)) shared += 1
  if (!shared) continue
  review.push({id: left, kind: 'shared history', detail: `${shared} shared ${GRAM_WIDTH}-character windows with ${right}`, pair: [left, right]})
 }
 const byCategory = {}
 for (const s of scenarios) {
  const row = byCategory[s.category] ??= {dev: 0, sealed: 0}
  row[s.split] = (row[s.split] ?? 0) + 1
 }
 for (const category of ['R1', 'R2', 'R3']) if ((byCategory[category]?.dev ?? 0) < 6) review.push({id: '*', kind: 'dev count below target', detail: `${category} has ${byCategory[category]?.dev ?? 0} development scenarios, the plan requires at least 6 before freezing`})
 const nameList = [...names.keys()]
 for (const [index, left] of nameList.entries()) for (const right of nameList.slice(index + 1)) {
  const shared = [...left].filter(character => right.includes(character))
  if (shared.length) review.push({id: '*', kind: 'similar names', detail: `${left} and ${right} share ${shared.join('')}`})
 }
 return {count: scenarios.length, errors, review, by_category: byCategory, limitations: [
  'natural transitions, wording variety and whether a filler sentence really reads as unrelated are not decidable here',
  'whether the retrieval step actually selects the segment that holds the old value is a property of a run, not of the dataset',
  'whether a tail turn is still un-summarised when the question is asked is only visible in the run log',
  'semantic paraphrases of an answer value escape a string comparison; only a reader can rule them out',
  'relative dates are listed for a person to resolve, never decided here',
 ]}
}

if (process.argv[1] && import.meta.url === new URL(`file:///${process.argv[1].replaceAll('\\', '/')}`).href) {
 const args = process.argv.slice(2)
 const outAt = args.indexOf('--out')
 const outFile = outAt >= 0 ? args.splice(outAt, 2)[1] : ''
 const dir = args[0]
 if (!dir) throw Error('supply scenario directory')
 const files = fs.readdirSync(dir).filter(x => x.endsWith('.json')).sort()
 const data = files.map(f => JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8').replace(/^\uFEFF/, '')))
 const priorDir = args[1]
 const readFiles = directory => fs.readdirSync(directory, {withFileTypes: true}).flatMap(e => e.isDirectory() ? readFiles(path.join(directory, e.name)) : /\.(json|jsonl|txt)$/i.test(e.name) ? [fs.readFileSync(path.join(directory, e.name), 'utf8')] : [])
 const report = validateDataset(data, priorDir ? readFiles(priorDir) : [])
 if (!priorDir) report.limitations.push('earlier dataset was not supplied for name checks')
 const summary = `scenarios=${report.count} errors=${report.errors.length} review=${report.review.length}`
 if (outFile) fs.writeFileSync(outFile, `${JSON.stringify(report, null, 2)}\n`, 'utf8')
 else process.stdout.write(`${JSON.stringify(report, null, 2)}\n`)
 process.stdout.write(`${summary}${outFile ? ` report=${outFile}` : ''}${priorDir ? '' : ' prior-dataset=absent'}\n`)
 if (report.errors.length) process.exitCode = 1
}
