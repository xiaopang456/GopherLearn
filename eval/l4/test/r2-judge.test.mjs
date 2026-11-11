// The second round's categories at the judge and at the scorer.
//
// Two things are checked here, and both are about the identity of an attempt.
//
// A3 is judged, and it is judged by a criterion of its own: its right answer
// is not a value but the statement that nothing was decided, so a judge that
// was asked whether the answer gave the right value could not decide the
// attempt at all.
//
// A scenario with several questions has one attempt per question, and each
// attempt is read against the key of the question it answers. The three
// questions of one scenario are three attempts, not one attempt counted
// thrice.
import test from 'node:test'
import assert from 'node:assert/strict'
import { judgeCategories, judgeInput } from '../truncation.mjs'
import { scoreAttempt } from '../score.mjs'
import { splitForId } from '../scenarios.mjs'

const TURNS = 70
const FILLER = '这是一段用于把历史撑到足够长的填充文字，它本身不承载任何答案。'

let counter = 0

// secondRoundTurns builds a history the second round's rules accept.
//
// The filler carries no value and no acceptance, so a scenario that puts its
// own content into two turns says what it means and nothing else.
function secondRoundTurns(patch) {
  return Array.from({ length: TURNS }, (_, index) => {
    const at = index + 1
    const turn = {
      user: FILLER,
      assistant: FILLER,
      user_at: new Date(1767225600000 + index * 300000).toISOString(),
      assistant_at: new Date(1767225600000 + index * 300000 + 60000).toISOString(),
    }
    return { ...turn, ...(patch[at] ?? {}) }
  })
}

// a3Scenario is a valid second-round A3 scenario: the assistant proposed
// something, the learner did not accept it, and the question asks whether it
// was ever decided.
function a3Scenario() {
  counter += 1
  const id = `l4r2-a3-${counter}`
  return {
    id, category: 'A3', split: splitForId(id), schema_version: 'l4.scenario.draft.v4',
    entity_name: '测试物',
    key_turns: { proposal_turn: 28, refuse_turn: 29 },
    evidence_turns: [28, 29],
    distance_to_question: TURNS + 1 - 28,
    turns: secondRoundTurns({
      28: { assistant: '我提议用甲来做「测试物」这一步。' },
      29: { user: '我想想，暂时不这么定。' },
      30: { user: '这一轮先按原来的安排走。', assistant: '明白了。' },
    }),
    question: '「测试物」这件事最后定下来了吗？',
    answer: { mode: 'needs_judge', required: ['明确说明这个安排并没有定下来'], rubric: '把提议说成已定的安排不通过。' },
  }
}

// f1Scenario is a valid second-round F1 scenario: one history, two questions
// asked in a row, each with its own key.
function f1Scenario() {
  counter += 1
  const id = `l4r2-f1-${counter}`
  return {
    id, category: 'F1', split: splitForId(id), schema_version: 'l4.scenario.draft.v4',
    entity_name: '测试物',
    key_turns: { artifact_turn: 30 },
    evidence_turns: [30],
    distance_to_question: TURNS + 1 - 30,
    turns: secondRoundTurns({
      30: { user: '我们说说测试物这件事。', assistant: '我整理了测试物的清单，第一项是甲，第二项是乙。' },
    }),
    question: undefined,
    answer: undefined,
    questions: [
      { question: '「测试物」这份清单的第一项练什么？', answer: { mode: 'fact', accepted: ['甲'], forbidden: ['乙'], aliases: [] } },
      { question: '那第二项呢？', answer: { mode: 'fact', accepted: ['乙'], forbidden: ['甲'], aliases: [] } },
    ],
  }
}

const attempt = (qid, overrides = {}) => ({ qid, arm: 'truncation', condition: 'main', rep: 0, env: 'real', answer: '这是答案。', ...overrides })

test('A3 is among the categories the judge is asked about', () => {
  assert.ok(judgeCategories().includes('A3'))
})

test('an A3 attempt is put to the judge with the criterion of its category', () => {
  const s = a3Scenario()
  const rows = judgeInput(new Map([[s.id, s]]), [attempt(s.id, { answer: '这件事当时只是提议，还没有定下来。' })])
  assert.equal(rows.length, 1)
  assert.equal(rows[0].error, undefined)
  assert.equal(rows[0].input.check_point, '明确说明这个安排并没有定下来')
  // The criterion is the category's own. It says what the right answer looks
  // like, and it says that naming the proposal is not the mistake.
  assert.ok(rows[0].input.criterion, 'the A3 row carries no criterion')
  assert.match(rows[0].input.criterion, /没有定下来/)
  assert.match(rows[0].input.criterion, /只是当时的提议/)
  assert.deepEqual(Object.keys(rows[0].input), ['question', 'answer_key', 'evidence', 'response', 'check_point', 'criterion'])
})

test('a category with no criterion of its own carries none', () => {
  const s = a3Scenario()
  s.category = 'constraint'
  const rows = judgeInput(new Map([[s.id, s]]), [attempt(s.id, { answer: '这是答案。' })])
  assert.equal(rows.length, 1)
  assert.equal(rows[0].input.criterion, undefined)
})

test('a record of a multi-question scenario is read against the question it answers', () => {
  const s = f1Scenario()
  const records = [
    attempt(`${s.id}#q1`, { question_index: 0, answer: '甲' }),
    attempt(`${s.id}#q2`, { question_index: 1, answer: '乙' }),
  ]
  // F1 is decided by the values themselves, so it is not asked about by
  // default. The judge is called here on purpose, to show that a record is
  // resolved to its own question and not to the scenario's first one.
  const rows = judgeInput(new Map([[s.id, s]]), records, { selected: ['F1'] })
  assert.equal(rows.length, 2)
  assert.equal(rows[0].item_id, `${s.id}#q1`)
  assert.equal(rows[1].item_id, `${s.id}#q2`)
  assert.deepEqual(rows[0].input.answer_key.accepted, ['甲'])
  assert.deepEqual(rows[1].input.answer_key.accepted, ['乙'])
  assert.equal(rows[1].input.question, '那第二项呢？')
})

test('a record that names a question the scenario does not have is not resolved', () => {
  const s = f1Scenario()
  const rows = judgeInput(new Map([[s.id, s]]), [attempt(`${s.id}#q3`, { question_index: 2 })], { selected: ['F1'] })
  assert.deepEqual(rows, [])
})

test('the questions of one scenario are scored one at a time, each against its own key', () => {
  const s = f1Scenario()
  const first = scoreAttempt({ qid: `${s.id}#q1`, question_index: 0, rep: 0, arm: 'current', env: 'fake', answer: '甲' }, s)
  const second = scoreAttempt({ qid: `${s.id}#q2`, question_index: 1, rep: 0, arm: 'current', env: 'fake', answer: '甲' }, s)
  assert.equal(first.verdict, 'pass')
  assert.equal(first.id, `${s.id}#q1`)
  assert.equal(first.question_index, 0)
  // The same answer is wrong for the second question: its key forbids the
  // first question's value. An attempt that was scored against the scenario's
  // first key would call this a pass.
  assert.equal(second.verdict, 'fail')
  assert.equal(second.id, `${s.id}#q2`)
  assert.equal(second.question_index, 1)
})
