import test from 'node:test'
import assert from 'node:assert/strict'
import { judgeFact } from '../score.mjs'

// The value in force now, and the value that was in force before.
//
// A question that asks for the value in force names it in the answer key, and
// the value that was in force before is the forbidden one. An answer may well
// name the old value - to say what it was and what it was changed to. Reading
// "the forbidden value is written down" as "the forbidden value is the answer"
// is the mistake these tests pin down.
//
// The three cases:
//
//   - the value in force is given and the old one is said to have been
//     replaced -> pass;
//   - the old value is given as the value in force -> fail;
//   - the relation between the two cannot be read -> pending, so that the
//     item goes to the semantic judging.
const key = (accepted, forbidden, category = 'A2') => ({
  category,
  answer: { mode: 'fact', accepted, forbidden, aliases: [] },
})

test('the value in force, with the old value said to have been replaced, passes', () => {
  const s = key(['9道题'], ['14道题'])
  assert.equal(judgeFact('现在一轮是 9 道题。原来定的是 14 道题，后来已经改成 9 道题了。', s).verdict, 'pass')
  assert.equal(judgeFact('现在一轮是 9 道题。原来是 14 道题，后来改成 9 道题了。', s).verdict, 'pass')
  assert.equal(judgeFact('现在每轮是 9 道题；14 道题已经被 9 道题取代。', s).verdict, 'pass')
})

test('the old value given as the value in force still fails', () => {
  const s = key(['9道题'], ['14道题'])
  assert.equal(judgeFact('现在一轮是 14 道题。', s).verdict, 'fail')
  assert.equal(judgeFact('每轮 14 道题，一直是这样。', s).verdict, 'fail')
  // The answer gives the value in force and denies the old one: that passes.
  assert.equal(judgeFact('现在一轮是 9 道题，不是 14 道题。', s).verdict, 'pass')
})

test('a relation that cannot be read stays pending', () => {
  const s = key(['9道题'], ['14道题'])
  for (const text of [
    '可能是 9 道题，也可能是 14 道题。',
    '现在一轮是 9 道题，14 道题也在计划里。',
  ]) assert.equal(judgeFact(text, s).verdict, 'needs_judge', text)
})

test('the value in force written in another way is never failed on the old value', () => {
  const s = key(['9道题'], ['14道题'])
  // The key says "9道题", the answer says "9 道": the number is the value, the
  // counter noun around it is not.
  const judged = judgeFact('现在一轮是 9 道。最初定的是 14 道题。', s)
  assert.equal(judged.verdict, 'needs_judge')
  assert.notEqual(judged.verdict, 'fail')
})

test('a value named only while another question is described is never failed', () => {
  const s = key(['重定向跳转'], ['连接复用', '分块传输'])
  const judged = judgeFact('「浔漳」第二题练的是跳转，第一题练的是连接复用，第三题是分块传输。', s)
  assert.equal(judged.verdict, 'needs_judge')
  assert.notEqual(judged.verdict, 'fail')
})

test('a forbidden value the answer puts in the past is never failed', () => {
  const s = key(['23页'], ['12页'])
  const judged = judgeFact('以前每次看 12 页，现在每次看多少我这边没有记录。', s)
  assert.equal(judged.verdict, 'needs_judge')
  assert.notEqual(judged.verdict, 'fail')
})

test('a denied value in force is no value in force', () => {
  const s = key(['9道题'], ['14道题'])
  const judged = judgeFact('不是 9 道题。', s)
  assert.equal(judged.verdict, 'needs_judge')
  assert.equal(judged.reason, 'no_fact_value')
})

test('the old value given as the value in force is still failed', () => {
  const s = key(['23页'], ['12页'])
  assert.equal(judgeFact('现在每次看 12 页。', s).verdict, 'fail')
  assert.equal(judgeFact('「菽茗」现在默认每次看 12 页讲义。', s).verdict, 'fail')
})

test('a scenario about history still goes to the semantic checkpoint', () => {
  const s = key(['9道题'], ['14道题'], 'R3')
  assert.equal(judgeFact('现在一轮是 9 道题。原来是 14 道题。', s).verdict, 'needs_judge')
})
