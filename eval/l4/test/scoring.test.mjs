import test from 'node:test'
import assert from 'node:assert/strict'
import { checkScenario, materialize, splitForId } from '../scenarios.mjs'
import { scoreAttempt, normalizeFact, judgeFact } from '../score.mjs'
const scenario = () => ({ id: 'synthetic-case', category: 'early_fact', split: splitForId('synthetic-case'), question: 'What name did I choose?', turns: [{ user: 'I chose Lantern.', assistant: 'Understood.', user_at: '2026-01-01T00:00:00Z', assistant_at: '2026-01-01T00:00:01Z' }], evidence_turns: [1], answer: { mode: 'fact', accepted: ['Lantern'], forbidden: ['Harbor'] } })
const record = answer => ({ qid: 'synthetic-case', rep: 0, arm: 'current', env: 'fake', answer })
test('facts use exact answers, while negation and mixed values remain pending', () => {
  const s = scenario()
  assert.equal(scoreAttempt(record('Lantern.'), s).verdict, 'pass')
  assert.equal(scoreAttempt(record('Harbor'), s).verdict, 'fail')
  for (const text of ['Not Lantern', 'Harbor was old; Lantern is current', 'I cannot recall']) assert.equal(scoreAttempt(record(text), s).verdict, 'needs_judge', text)
  assert.equal(scoreAttempt(record(''), s).verdict, 'error')
})
test('real attempts require validated background evidence and correct identities', () => {
  const s = scenario()
  assert.equal(scoreAttempt({ ...record('Lantern'), env: 'real' }, s).reason, 'background_unverified')
  assert.equal(scoreAttempt({ ...record('Lantern'), env: 'real', backgroundAudit: { valid: true } }, s).verdict, 'pass')
  assert.equal(scoreAttempt({ ...record('Lantern'), rep: 3 }, s).verdict, 'error')
  assert.equal(scoreAttempt({ ...record('Lantern'), qid: 'other' }, s).verdict, 'error')
  assert.equal(scoreAttempt({ ...record('Lantern'), arm: undefined }, s).verdict, 'error')
})
test('semantic cases remain pending even when an expected word occurs', () => {
  const s = scenario(); s.answer.mode = 'needs_judge'
  assert.equal(scoreAttempt(record('Lantern'), s).verdict, 'needs_judge')
})
test('hash split does not depend on question content or dataset ordering', () => {
  const s = scenario(); s.question = 'Another wording'
  assert.equal(checkScenario(s).length, 0)
  s.split = s.split === 'dev' ? 'sealed' : 'dev'
  assert.ok(checkScenario(s).includes('split differs from id hash'))
})
test('paired histories only replace the selected turn and preserve timestamps', () => {
  const s = scenario(); s.category = 'irrelevant'
  s.conditions = { control: { replace_turn: 1, user: 'Neutral', assistant: 'Okay' }, distractor: { replace_turn: 1, user: 'Other plan', assistant: 'Separate plan' } }
  const control = materialize(s, 'control'), distractor = materialize(s, 'distractor')
  assert.equal(control[0].user_at, distractor[0].user_at)
  assert.equal(s.turns[0].user, 'I chose Lantern.')
  assert.throws(() => materialize(s), /requires a condition/)
})
test('malformed scenarios fail validation without throwing', () => {
  for (const turns of [null, 1, {}, [null]]) { const s = scenario(); s.turns = turns; assert.ok(checkScenario(s).length) }
})
test('quantity spacing, width and unit spellings have identical verdicts', () => {
  const s = scenario(); s.answer = { mode: 'fact', accepted: ['66分钟'], forbidden: ['33分钟'] }
  for (const value of ['66 分钟', '66分钟', '６６分钟', '66分鐘', '66 min', '66 minutes', '1.1小时', '3960秒']) assert.equal(scoreAttempt(record(value), s).verdict, 'pass', value)
  for (const value of ['33 分钟', '３３分钟', '33 mins']) assert.equal(scoreAttempt(record(value), s).verdict, 'fail', value)
  for (const value of ['不是66分钟', '旧33分钟，新66分钟', '66']) assert.equal(scoreAttempt(record(value), s).verdict, 'needs_judge', value)
  assert.equal(normalizeFact('0 分钟'), normalizeFact('0小时'))
})
test('explicit aliases pass without broad substring matching', () => {
  const s = scenario(); s.answer.aliases = ['Lamp']
  assert.equal(scoreAttempt(record('Lamp'), s).verdict, 'pass')
  assert.equal(scoreAttempt(record('Not Lamp'), s).verdict, 'needs_judge')
  s.answer.aliases = [null]
  assert.ok(checkScenario(s).includes('invalid aliases'))
})

// A fact answer is judged by what it says, so a sentence that states the
// expected value is a pass even when the sentence is not the value itself.
const minutes = (accepted, forbidden) => ({ mode: 'fact', accepted, forbidden })
test('a sentence that states the expected value passes', () => {
  const s = scenario(); s.answer = minutes(['51分钟'], ['93分钟'])
  assert.equal(scoreAttempt(record('现在“杉渡”学习安排每次总共是 51 分钟。'), s).verdict, 'pass')
  assert.equal(scoreAttempt(record('现在“杉渡”学习安排每次总共是 51 分钟；三段等长，因此每段是 17 分钟。'), s).verdict, 'pass')
  assert.equal(scoreAttempt(record('每次总共 51 分钟。'), s).verdict, 'pass')
})
test('a sentence that states only a forbidden value fails', () => {
  const s = scenario(); s.answer = minutes(['51分钟'], ['93分钟'])
  assert.equal(scoreAttempt(record('现在每次总共是 93 分钟。'), s).verdict, 'fail')
  assert.equal(scoreAttempt(record('每次总共 93 分钟。'), s).verdict, 'fail')
})
test('an answer that states both values stays pending', () => {
  const s = scenario(); s.answer = minutes(['51分钟'], ['93分钟'])
  const judged = scoreAttempt(record('每次总共 51 分钟，每段 93 分钟。'), s)
  assert.equal(judged.verdict, 'needs_judge')
  assert.equal(judged.reason, 'mixed_fact_values')
})
test('a denied value does not count as stated', () => {
  const s = scenario(); s.answer = minutes(['51分钟'], ['93分钟'])
  assert.equal(scoreAttempt(record('现在每次总共是 51 分钟，不是 93 分钟。'), s).verdict, 'pass')
  assert.equal(scoreAttempt(record('现在每次总共是 51 分钟；93 分钟不再执行。'), s).verdict, 'pass')
  assert.equal(scoreAttempt(record('每次总共 51 分钟，不是 93 分钟，而是 48 分钟。'), s).verdict, 'pass')
})
test('a denied expected value does not count as stated either', () => {
  const s = scenario(); s.answer = minutes(['51分钟'], ['93分钟'])
  const judged = scoreAttempt(record('不是 51 分钟。'), s)
  assert.equal(judged.verdict, 'needs_judge')
  assert.equal(judged.reason, 'no_fact_value')
})
test('a value the answer only mentions as the old one stays pending', () => {
  const s = scenario(); s.answer = minutes(['51分钟'], ['93分钟'])
  const judged = scoreAttempt(record('现在每次总共是 51 分钟，93 分钟是旧的。'), s)
  assert.equal(judged.verdict, 'needs_judge')
  assert.equal(judged.reason, 'negation_uncertain')
})
test('a withdrawn old value is not a forbidden value', () => {
  const s = scenario(); s.answer = minutes(['26分钟'], ['41分钟', '78分钟', '123分钟'])
  assert.equal(scoreAttempt(record('“柳坞”每次总时长改为 78 分钟，仍等分三段，所以每个小段是 26 分钟。原来的“每次 123 分钟、每段 41 分钟”不再执行。'), s).verdict, 'needs_judge')
  // The withdrawn values are read as old, which is not confident enough to call:
  // the answer stays pending instead of being counted as a forbidden value.
  assert.equal(scoreAttempt(record('每次总时长 78 分钟，每个小段是 26 分钟。原来的“每次 123 分钟、每段 41 分钟”不再执行。'), s).reason, 'negation_uncertain')
})
test('an english denial is read as a denial', () => {
  const s = scenario(); s.answer = minutes(['51分钟'], ['93分钟'])
  assert.equal(scoreAttempt(record('each session is 51 minutes, not 93 minutes'), s).verdict, 'pass')
  assert.equal(scoreAttempt(record('each session is 93 minutes'), s).verdict, 'fail')
})
test('the judge reports what it read', () => {
  const s = scenario(); s.answer = minutes(['51分钟'], ['93分钟'])
  assert.deepEqual(judgeFact('每次总共 51 分钟。', s), { verdict: 'pass', reason: 'fact_value_present' })
  assert.deepEqual(judgeFact('每次总共 93 分钟。', s), { verdict: 'fail', reason: 'forbidden_value_present' })
  assert.deepEqual(judgeFact('我记得是 51 分钟吧。', s), { verdict: 'pass', reason: 'fact_value_present' })
})
