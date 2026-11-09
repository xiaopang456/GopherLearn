import test from 'node:test'
import assert from 'node:assert/strict'
import { judgeFact } from '../score.mjs'
import { checkScenario } from '../scenarios.mjs'
import { MEASURE_WORDS, chineseNumber, normalizeValue } from '../normalize.mjs'

// The quantity normalization: one measure-word table, one function, and the answer
// key's own comparison uses it too.

test('every measure word the frozen answer keys use is in the table', () => {
  // The census over the four frozen sets found these and no others as count units.
  for (const word of ['分', '次', '页', '天', '道']) assert.ok(MEASURE_WORDS.includes(word), word)
  // "个" was measured in needs_judge requirements only and is deliberately not in the table.
  assert.equal(MEASURE_WORDS.includes('个'), false)
})

test('a Chinese numeral and its digits are one value', () => {
  const rows = [
    ['9道', '九道'],
    ['9次', '九次'],
    ['15道', '十五道'],
    ['15次', '十五次'],
    ['38分钟', '三十八分钟'],
    ['41页', '四十一页'],
    ['5天', '五天'],
  ]
  for (const [digits, spoken] of rows) {
    assert.equal(normalizeValue(digits), normalizeValue(spoken), `${digits} / ${spoken}`)
  }
})

test('a count and a duration are different values', () => {
  assert.notEqual(normalizeValue('9次'), normalizeValue('9道'))
  assert.notEqual(normalizeValue('9次'), normalizeValue('9秒'))
  assert.notEqual(normalizeValue('9分钟'), normalizeValue('9次'))
})

test('chineseNumber reads the thousands and the ten-thousands', () => {
  assert.equal(chineseNumber('十'), 10)
  assert.equal(chineseNumber('二十'), 20)
  assert.equal(chineseNumber('二十五'), 25)
  assert.equal(chineseNumber('一百零八'), 108)
  assert.equal(chineseNumber('一千'), 1000)
  assert.equal(chineseNumber('三千五百'), 3500)
  assert.equal(chineseNumber('一万'), 10000)
  assert.equal(chineseNumber('两万五千'), 25000)
  assert.equal(chineseNumber('一万二千三百四十五'), 12345)
  assert.equal(chineseNumber('十万'), 100000)
  // What it cannot read, it does not guess.
  assert.ok(Number.isNaN(chineseNumber('廿')))
  assert.ok(Number.isNaN(chineseNumber('两亿')))
})

// The boundary: only a complete, legal writing is read. Everything else keeps its
// text and is matched literally - so a key can never be satisfied by a different
// value that happens to be read as zero.
test('a malformed Chinese numeral is not read at all', () => {
  const malformed = [
    '万',        // a lone ten-thousand
    '万万',      // two ten-thousands
    '一百百',    // a unit that repeats
    '十十',      // a unit that repeats
    '一二',      // two digits in a row
    '千十',      // the units grow instead of shrinking
    '万五千',    // the ten-thousands part is not written out
    '百',        // a hundred without the digit in front of it
    '千',        // a thousand without the digit in front of it
    '一百零',    // a zero in the middle that never gets its digit
    '一百二零三', // a zero on top of a digit that is still waiting for its unit
    '亿',        // a unit this parser does not know
    '',
  ]
  for (const text of malformed) {
    assert.ok(Number.isNaN(chineseNumber(text)), `${text} must not be read as a number`)
  }
})

test('a key of "万次" is not satisfied by an answer of "0次"', () => {
  // The bug this closes: "万" used to be read as zero, so the key "万次" and the
  // answer "0次" were the same value, and a wrong answer passed.
  const key = { answer: { accepted: ['万次'], forbidden: [], aliases: [] } }
  assert.equal(normalizeValue('万次'), 'text:万次')
  assert.notEqual(normalizeValue('万次'), normalizeValue('0次'))
  const wrong = judgeFact('答案是0次。', key)
  assert.notEqual(wrong.verdict, 'pass', 'the wrong answer must not pass')
  // A key that cannot be read is not a value the scorer can rule on: the item goes
  // to a judge, the same as any other value that is not read (see "两亿次" above).
  assert.equal(wrong.verdict, 'needs_judge')
  assert.equal(judgeFact('答案是万次。', key).verdict, 'pass')
})

test('a malformed numeral in the key keeps its text and is matched literally', () => {
  const rows = [
    ['万次', '答案是万次。', 'pass'],
    ['万次', '答案是0次。', 'needs_judge'],
    ['万万次', '答案是万万次。', 'pass'],
    ['万万次', '答案是0次。', 'needs_judge'],
    ['一百百次', '答案是一百百次。', 'pass'],
    ['一百百次', '答案是200次。', 'needs_judge'],
    // The digit before the zero must not be dropped: "一百二零三" is not a hundred
    // and three.
    ['一百二零三次', '答案是一百二零三次。', 'pass'],
    ['一百二零三次', '答案是103次。', 'needs_judge'],
  ]
  for (const [keyValue, answer, expected] of rows) {
    const scenario = { answer: { accepted: [keyValue], forbidden: [], aliases: [] } }
    assert.equal(judgeFact(answer, scenario).verdict, expected, `${keyValue} / ${answer}`)
  }
})

test('a zero may only fill a place that is empty', () => {
  // "一百零三" is a hundred and three, and the zero fills the tens place.
  assert.equal(chineseNumber('一百零三'), 103)
  assert.equal(chineseNumber('二百零三'), 203)
  // "一百二十三" has no zero at all and is a hundred and twenty-three.
  assert.equal(chineseNumber('一百二十三'), 123)
  // "一百二零三" writes a two that has no place of its own, so it is not a number.
  assert.ok(Number.isNaN(chineseNumber('一百二零三')))
  assert.notEqual(normalizeValue('一百二零三次'), normalizeValue('103次'))
})

test('a quantity with a large numeral is the same value however it is written', () => {
  assert.equal(normalizeValue('1500次'), normalizeValue('一千五百次'))
  assert.equal(normalizeValue('3页'), normalizeValue('三页'))
  assert.equal(normalizeValue('2000次'), normalizeValue('两千次'))
  assert.equal(normalizeValue('20000000次'), normalizeValue('两千万次'))
})

test('a number that cannot be read keeps its text and is matched literally', () => {
  // "两亿" cannot be read by the parser, so the value stays as it was written and
  // the answer has to state it the same way.
  const unreadable = normalizeValue('两亿次')
  assert.equal(unreadable, 'text:两亿次')
  const scenario = { answer: { accepted: ['两亿次'], forbidden: [], aliases: [] } }
  assert.equal(judgeFact('答案是两亿次。', scenario).verdict, 'pass')
  assert.equal(judgeFact('答案是两亿次', scenario).verdict, 'pass')
  // A different value is not the answer.
  assert.equal(judgeFact('答案是两亿零次', scenario).verdict, 'needs_judge')
})

test('an answer states the value in the other spelling and still passes', () => {
  const scenario = { answer: { accepted: ['9次'], forbidden: ['15次', '十五次', '十五道', '15道'], aliases: ['9道', '九次', '九道'] } }
  assert.equal(judgeFact('一共九次。', scenario).verdict, 'pass')
  assert.equal(judgeFact('一共9次。', scenario).verdict, 'pass')
  assert.equal(judgeFact('一共九道。', scenario).verdict, 'pass')
  assert.equal(judgeFact('一共15次。', scenario).verdict, 'fail')
  assert.equal(judgeFact('一共十五道。', scenario).verdict, 'fail')
  // Denied, withdrawn and mixed values keep their own reading.
  assert.equal(judgeFact('不是九次。', scenario).verdict, 'needs_judge')
  // The cancelled value is the wrong one and the current one is right: the answer
  // names the current value, so it passes.
  assert.equal(judgeFact('十五次已取消，现在是九次。', scenario).verdict, 'pass')
  // The other way round: the current value is the forbidden one, so it fails.
  assert.equal(judgeFact('九次已取消，现在是十五次。', scenario).verdict, 'fail')
})

test('a count word inside an ordinary word does not establish a fact', () => {
  const rows = [
    ['9道', '9道路'],
    ['9道', '9道理'],
    ['9道', '9味道'],
    ['9次', '9次元'],
    ['9天', '9天线'],
  ]
  for (const [key, answer] of rows) {
    assert.equal(judgeFact(answer, { answer: { accepted: [key], aliases: [] } }).verdict, 'needs_judge', answer)
  }
})

test('the answer key compares its own values after the normalization', () => {
  // "9次" and "九次" are one value now, so a key that accepts one and forbids the
  // other contradicts itself and is refused.
  const scenario = {
    id: 'synthetic-quantity', category: 'early_fact', split: 'dev', question: 'q',
    turns: [{ user: 'u', assistant: 'a', user_at: '2026-01-01T00:00:00Z', assistant_at: '2026-01-01T00:00:01Z' }],
    evidence_turns: [1],
    answer: { mode: 'fact', accepted: ['9次'], forbidden: ['九次'], aliases: [] },
  }
  const errors = checkScenario(scenario)
  assert.ok(errors.includes('accepted value is also forbidden'), errors.join('; '))
  // The same key with two spellings of the same value on the same side is fine.
  assert.deepEqual(
    checkScenario({ ...scenario, answer: { mode: 'fact', accepted: ['9次'], forbidden: ['15次'], aliases: ['九次', '九道'] } }),
    [],
  )
})
