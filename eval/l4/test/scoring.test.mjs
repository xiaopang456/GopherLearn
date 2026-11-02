import test from 'node:test'
import assert from 'node:assert/strict'
import { checkScenario, materialize, splitForId } from '../scenarios.mjs'
import { scoreAttempt } from '../score.mjs'
const scenario = () => ({ id: 'synthetic-case', category: 'early_fact', split: splitForId('synthetic-case'), question: 'What name did I choose?', turns: [{ user: 'I chose Lantern.', assistant: 'Understood.', user_at: '2026-01-01T00:00:00Z', assistant_at: '2026-01-01T00:00:01Z' }], evidence_turns: [1], answer: { mode: 'fact', accepted: ['Lantern'], forbidden: ['Harbor'] } })
const record = answer => ({ qid: 'synthetic-case', rep: 0, arm: 'current', env: 'fake', answer })
test('facts use exact answers, while negation and mixed values remain pending', () => {
  const s = scenario()
  assert.equal(scoreAttempt(record('Lantern.'), s).verdict, 'pass')
  assert.equal(scoreAttempt(record('Harbor'), s).verdict, 'fail')
  for (const text of ['Not Lantern', 'Harbor was old; Lantern is current', 'I cannot recall']) assert.equal(scoreAttempt(record(text), s).verdict, 'needs_judge')
  assert.equal(scoreAttempt(record(''), s).verdict, 'error')
})
test('real attempts require validated background evidence and correct identities', () => {
  const s = scenario()
  assert.equal(scoreAttempt({ ...record('Lantern'), env: 'real' }, s).reason, 'background_unverified')
  assert.equal(scoreAttempt({ ...record('Lantern'), env: 'real', backgroundAudit: { valid: true } }, s).verdict, 'pass')
  assert.equal(scoreAttempt({ ...record('Lantern'), rep: 3 }, s).verdict, 'error')
  assert.equal(scoreAttempt({ ...record('Lantern'), qid: 'other' }, s).verdict, 'error')
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
