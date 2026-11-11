// The second round's scenarios.
//
// The six new categories are checked by the same validator as the first
// round's, and every rule of the first round still runs on them. The tests
// below are table driven: every category is built in the shape that makes it
// valid, and then broken in one way at a time, and the error the validator
// reports has to be the one that names the broken thing.
import test from 'node:test'
import assert from 'node:assert/strict'
import { checkScenario, estimateTokens, splitForId, tailTokens } from '../scenarios.mjs'

const TURNS = 70
const KEY_TURN = 30
const NAME = '测试物'

// buildTurns builds a history of the right length.
//
// Every turn carries enough text for the tail of the history to be worth
// more than the nine hundred tokens the key turn has to be away from the
// question, and only the key turn carries the name.
function buildTurns(name = NAME, keyTurn = KEY_TURN) {
  const turns = []
  for (let index = 1; index <= TURNS; index += 1) {
    const userAt = new Date(Date.UTC(2026, 3, 1, 9, 0, 0) + index * 5 * 60_000)
    const assistantAt = new Date(userAt.getTime() + 60_000)
    const filler = '这是一段用于把历史撑到足够长的填充文字，它本身不承载任何答案。'
    const user = index === keyTurn ? `我们说说${name}这件事。` : filler
    const assistant = index === keyTurn ? `我整理了${name}的清单，第一项是甲，第二项是乙。` : filler
    turns.push({ user, assistant, user_at: userAt.toISOString(), assistant_at: assistantAt.toISOString() })
  }
  return turns
}

// withTurns replaces the messages of the turns a scenario cares about.
//
// The name only ever appears at the key turn, so a scenario whose key turn is
// another one has to take the name off the default turn: a name outside the
// key turns is a second object in the history, and the validator refuses it.
function withTurns(patches) {
  const turns = buildTurns()
  for (const [index, patch] of Object.entries(patches)) {
    const at = Number(index)
    turns[at - 1] = { ...turns[at - 1], ...patch }
  }
  return turns
}

// scenario builds a scenario of one category.
function scenario(category, edit = {}) {
  const id = edit.id ?? 'l4r2-2026aa-a3-01'
  const name = edit.entity_name ?? NAME
  const base = {
    schema_version: 'l4.scenario.draft.v4',
    id,
    category,
    split: splitForId(id),
    entity_name: name,
    key_turns: { artifact_turn: KEY_TURN },
    evidence_turns: [KEY_TURN],
    distance_to_question: TURNS + 1 - KEY_TURN,
    turns: buildTurns(name),
    question: `「${name}」现在是什么情况？`,
    answer: { mode: 'fact', accepted: ['甲'], forbidden: ['乙'], aliases: [] },
    ...edit,
  }
  return base
}

// The six valid shapes, one per new category.
//
// Every one of them is checked for "no errors at all", so a rule that is
// missing from the validator shows up here rather than in a run.
const VALID = {
  A1: () => scenario('A1'),
  A2: () => scenario('A2', {
    key_turns: { proposal_turn: 28, change_turn: KEY_TURN },
    evidence_turns: [28, KEY_TURN],
    distance_to_question: TURNS + 1 - 28,
    turns: withTurns({
      28: { assistant: `我提议用甲来做「${NAME}」这一步。` },
      [KEY_TURN]: { user: '好的，那就改用丙来跑这一轮。', assistant: '嗯，那就用丙。' },
    }),
    answer: { mode: 'fact', accepted: ['丙'], forbidden: ['甲'], aliases: [] },
  }),
  A3: () => scenario('A3', {
    key_turns: { proposal_turn: 28, refuse_turn: 29 },
    evidence_turns: [28, 29],
    distance_to_question: TURNS + 1 - 28,
    turns: withTurns({
      28: { assistant: `我提议用甲来做「${NAME}」这一步。` },
      29: { user: '我想想，暂时不这么定。' },
      [KEY_TURN]: { user: '这一轮先按原来的安排走。', assistant: '明白了。' },
    }),
    answer: { mode: 'needs_judge', required: ['明确说明这个安排并没有定下来'], rubric: '把提议说成已定的安排不通过。' },
  }),
  A4: () => scenario('A4', {
    key_turns: { said_turn: 27, echo_turn: 28, change_turn: KEY_TURN },
    evidence_turns: [27, 28, KEY_TURN],
    distance_to_question: TURNS + 1 - 27,
    turns: withTurns({
      27: { user: `这一轮「${NAME}」按甲来。` },
      28: { assistant: '好的，这一轮按甲来。' },
      [KEY_TURN]: { user: '改成按乙来。', assistant: '明白了。' },
    }),
    answer: { mode: 'fact', accepted: ['乙'], forbidden: ['甲'], aliases: [] },
  }),
  P1: () => scenario('P1', {
    key_turns: { session_turn: KEY_TURN },
    evidence_turns: [KEY_TURN],
    distance_to_question: TURNS + 1 - KEY_TURN,
    turns: withTurns({
      [KEY_TURN]: { user: `「${NAME}」我最近换成用丙了。` },
    }),
    question: `「${NAME}」我现在用哪个编辑器？`,
    answer: { mode: 'fact', accepted: ['丙'], forbidden: ['旧值'], aliases: [] },
    seed_memories: [{ scope: 'global', subject: '常用编辑器', content: '常用编辑器: 旧值', tags: [], confirmed: true, saved_at: '2026-01-14' }],
  }),
  F1: () => scenario('F1', {
    key_turns: { artifact_turn: KEY_TURN },
    evidence_turns: [KEY_TURN, 40, 42],
    distance_to_question: TURNS + 1 - KEY_TURN,
    turns: buildTurns(),
    question: undefined,
    answer: undefined,
    questions: [
      { turn: 40, question: `「${NAME}」这份清单的第一项练什么？`, answer: { mode: 'fact', accepted: ['甲'], forbidden: ['乙'], aliases: ['甲项'] } },
      { turn: 42, question: '那第二项呢？', answer: { mode: 'fact', accepted: ['乙'], forbidden: ['甲'], aliases: [] } },
    ],
  }),
}

test('the token estimator is the product\'s own estimator', () => {
  // The product counts CJK characters one by one and ASCII words four
  // characters to a token. A space is a token of its own.
  assert.equal(estimateTokens('中文'), 2)
  assert.equal(estimateTokens('abcd'), 1)
  assert.equal(estimateTokens('abcde'), 2)
  assert.equal(estimateTokens('a b'), 3)
  assert.equal(estimateTokens('\n'), 1)
})

test('the tail of the history is measured from the key turn', () => {
  const turns = buildTurns()
  assert.equal(tailTokens(turns, TURNS), estimateTokens(turns[TURNS - 1].user) + estimateTokens(turns[TURNS - 1].assistant))
  assert.ok(tailTokens(turns, KEY_TURN) >= 900)
})

test('every second-round category is accepted in its own shape', () => {
  for (const [category, build] of Object.entries(VALID)) {
    const errors = checkScenario(build())
    assert.deepEqual(errors, [], `${category}: ${errors.join('; ')}`)
  }
})

test('each second-round category is broken in one way at a time', () => {
  const cases = [
    ['A1 answer value is not in the assistant message', () => scenario('A1', {
      answer: { mode: 'fact', accepted: ['不存在'], forbidden: ['乙'], aliases: [] },
    }), /A1 accepted value is not in the assistant message/],
    ['A1 answer value is in a user message', () => scenario('A1', {
      turns: withTurns({ 10: { user: '甲也在别处出现过。' } }),
    }), /A1 answer value is in a user message/],
    ['A2 new value is not said by the user', () => scenario('A2', {
      key_turns: { proposal_turn: 28, change_turn: KEY_TURN },
      evidence_turns: [28, KEY_TURN],
      distance_to_question: TURNS + 1 - 28,
      turns: withTurns({
        28: { assistant: `我提议用甲来做「${NAME}」这一步。` },
        [KEY_TURN]: { user: '那就改一下。', assistant: '嗯。' },
      }),
      answer: { mode: 'fact', accepted: ['丙'], forbidden: ['甲'], aliases: [] },
    }), /A2 accepted value is not said by the user/],
    ['A2 has no acceptance', () => scenario('A2', {
      key_turns: { proposal_turn: 28, change_turn: KEY_TURN },
      evidence_turns: [28, KEY_TURN],
      distance_to_question: TURNS + 1 - 28,
      turns: withTurns({
        28: { assistant: `我提议用甲来做「${NAME}」这一步。` },
        [KEY_TURN]: { user: '那就改用丙来跑这一轮。', assistant: '嗯。' },
      }),
      answer: { mode: 'fact', accepted: ['丙'], forbidden: ['甲'], aliases: [] },
    }), /A2 has no acceptance/],
    ['A3 carries an acceptance', () => scenario('A3', {
      key_turns: { proposal_turn: 28, refuse_turn: 29 },
      evidence_turns: [28, 29],
      distance_to_question: TURNS + 1 - 28,
      turns: withTurns({
        28: { assistant: `我提议用甲来做「${NAME}」这一步。` },
        29: { user: '好的，就按这个来。' },
        [KEY_TURN]: { assistant: '明白了。' },
      }),
      answer: { mode: 'needs_judge', required: ['明确说明这个安排并没有定下来'], rubric: '' },
    }), /A3 has an acceptance marker/],
    ['A3 answer key is a value', () => scenario('A3', {
      key_turns: { proposal_turn: 28, refuse_turn: 29 },
      evidence_turns: [28, 29],
      distance_to_question: TURNS + 1 - 28,
      turns: withTurns({
        28: { assistant: `我提议用甲来做「${NAME}」这一步。` },
        29: { user: '我想想，暂时不这么定。' },
        [KEY_TURN]: { assistant: '明白了。' },
      }),
      answer: { mode: 'fact', accepted: ['甲'], forbidden: [], aliases: [] },
    }), /A3 must be judged, not matched against a value/],
    ['A4 turns are out of order', () => scenario('A4', {
      key_turns: { said_turn: 28, echo_turn: 27, change_turn: KEY_TURN },
      evidence_turns: [27, 28, KEY_TURN],
      distance_to_question: TURNS + 1 - 27,
      turns: withTurns({
        27: { user: `这一轮「${NAME}」按甲来。` },
        28: { assistant: '好的，这一轮按甲来。' },
        [KEY_TURN]: { user: '改成按乙来。', assistant: '明白了。' },
      }),
      answer: { mode: 'fact', accepted: ['乙'], forbidden: ['甲'], aliases: [] },
    }), /A4 turns must be in the order said, echo, change/],
    ['P1 seeded memory is not global', () => scenario('P1', {
      key_turns: { session_turn: KEY_TURN },
      evidence_turns: [KEY_TURN],
      distance_to_question: TURNS + 1 - KEY_TURN,
      turns: withTurns({ [KEY_TURN]: { user: `「${NAME}」我最近换成用丙了。` } }),
      question: `「${NAME}」我现在用哪个编辑器？`,
      answer: { mode: 'fact', accepted: ['丙'], forbidden: ['旧值'], aliases: [] },
      seed_memories: [{ scope: 'topic', subject: '常用编辑器', content: '常用编辑器: 旧值', tags: [], confirmed: true, saved_at: '2026-01-14' }],
    }), /a seeded profile memory must have the global scope/],
    ['P1 seeded value is in the history', () => scenario('P1', {
      key_turns: { session_turn: KEY_TURN },
      evidence_turns: [KEY_TURN],
      distance_to_question: TURNS + 1 - KEY_TURN,
      turns: withTurns({
        12: { user: '以前用的是旧值。' },
        [KEY_TURN]: { user: `「${NAME}」我最近换成用丙了。` },
      }),
      question: `「${NAME}」我现在用哪个编辑器？`,
      answer: { mode: 'fact', accepted: ['丙'], forbidden: ['旧值'], aliases: [] },
      seed_memories: [{ scope: 'global', subject: '常用编辑器', content: '常用编辑器: 旧值', tags: [], confirmed: true, saved_at: '2026-01-14' }],
    }), /P1 seeded value appears in the history/],
    ['F1 has one question', () => scenario('F1', {
      key_turns: { artifact_turn: KEY_TURN },
      evidence_turns: [KEY_TURN, 40],
      distance_to_question: TURNS + 1 - KEY_TURN,
      turns: buildTurns(),
      question: undefined,
      answer: undefined,
      questions: [{ turn: 40, question: `「${NAME}」这份清单的第一项练什么？`, answer: { mode: 'fact', accepted: ['甲'], forbidden: [], aliases: [] } }],
    }), /F1 needs two or three questions/],
    ['F1 two questions have the same answer', () => scenario('F1', {
      key_turns: { artifact_turn: KEY_TURN },
      evidence_turns: [KEY_TURN, 40, 42],
      distance_to_question: TURNS + 1 - KEY_TURN,
      turns: buildTurns(),
      question: undefined,
      answer: undefined,
      questions: [
        { turn: 40, question: `「${NAME}」这份清单的第一项练什么？`, answer: { mode: 'fact', accepted: ['甲'], forbidden: [], aliases: [] } },
        { turn: 42, question: '那第二项呢？', answer: { mode: 'fact', accepted: ['甲'], forbidden: [], aliases: [] } },
      ],
    }), /question 2 has the same answer as the one before it/],
    ['F1 a question names no turn', () => scenario('F1', {
      key_turns: { artifact_turn: KEY_TURN },
      evidence_turns: [KEY_TURN, 42],
      distance_to_question: TURNS + 1 - KEY_TURN,
      turns: buildTurns(),
      question: undefined,
      answer: undefined,
      questions: [
        { turn: 40, question: `「${NAME}」这份清单的第一项练什么？`, answer: { mode: 'fact', accepted: ['甲'], forbidden: ['乙'], aliases: [] } },
        { question: '那第二项呢？', answer: { mode: 'fact', accepted: ['乙'], forbidden: ['甲'], aliases: [] } },
      ],
    }), /question 2 does not name the turn it was asked at/],
    ['F1 a question names a turn that is not in the history', () => scenario('F1', {
      key_turns: { artifact_turn: KEY_TURN },
      evidence_turns: [KEY_TURN, 40, TURNS + 5],
      distance_to_question: TURNS + 1 - KEY_TURN,
      turns: buildTurns(),
      question: undefined,
      answer: undefined,
      questions: [
        { turn: 40, question: `「${NAME}」这份清单的第一项练什么？`, answer: { mode: 'fact', accepted: ['甲'], forbidden: ['乙'], aliases: [] } },
        { turn: TURNS + 5, question: '那第二项呢？', answer: { mode: 'fact', accepted: ['乙'], forbidden: ['甲'], aliases: [] } },
      ],
    }), /question 2 names a turn that is not in the history/],
    ['F1 a question is asked at a turn that is not evidence', () => scenario('F1', {
      key_turns: { artifact_turn: KEY_TURN },
      evidence_turns: [KEY_TURN, 40],
      distance_to_question: TURNS + 1 - KEY_TURN,
      turns: buildTurns(),
      question: undefined,
      answer: undefined,
      questions: [
        { turn: 40, question: `「${NAME}」这份清单的第一项练什么？`, answer: { mode: 'fact', accepted: ['甲'], forbidden: ['乙'], aliases: [] } },
        { turn: 42, question: '那第二项呢？', answer: { mode: 'fact', accepted: ['乙'], forbidden: ['甲'], aliases: [] } },
      ],
    }), /question 2 is asked at turn 42, which is not an evidence turn/],
    ['F1 a question is asked before the one before it', () => scenario('F1', {
      key_turns: { artifact_turn: KEY_TURN },
      evidence_turns: [KEY_TURN, 40, 42],
      distance_to_question: TURNS + 1 - KEY_TURN,
      turns: buildTurns(),
      question: undefined,
      answer: undefined,
      questions: [
        { turn: 42, question: `「${NAME}」这份清单的第一项练什么？`, answer: { mode: 'fact', accepted: ['甲'], forbidden: ['乙'], aliases: [] } },
        { turn: 40, question: '那第二项呢？', answer: { mode: 'fact', accepted: ['乙'], forbidden: ['甲'], aliases: [] } },
      ],
    }), /question 2 is asked at turn 40, which is not after the turn of the question before it/],
    ['the question carries a number', () => scenario('A1', { question: `「${NAME}」现在第 3 项是什么情况？` }), /question carries a number/],
    ['the entity name is outside the key turns', () => scenario('A1', {
      turns: withTurns({ 10: { user: `顺便说说${NAME}。` } }),
    }), /entity name occurs outside the key turns/],
    ['the key turn is not evidence', () => scenario('A1', { evidence_turns: [KEY_TURN + 1] }), /key turn artifact_turn is not evidence/],
    ['the key turn is too close to the question', () => scenario('A1', { key_turns: { artifact_turn: TURNS }, evidence_turns: [TURNS], distance_to_question: 1 }), /tail tokens, needs 900/],
  ]
  for (const [name, build, pattern] of cases) {
    const errors = checkScenario(build())
    assert.ok(errors.some(error => pattern.test(error)), `${name}: ${JSON.stringify(errors)}`)
  }
})
