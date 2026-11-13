import test from 'node:test'
import assert from 'node:assert/strict'
import { judgeFact } from '../score.mjs'

// The value in force now, and the value that was in force before.
//
// A question that asks for the value in force names it in the answer key, and
// the value that was in force before is the forbidden one. An answer may well
// name the old value - to say what it was and what it was changed to.
//
// There is no automatic pass here. A marker that names a change (`改成`,
// `取代` and the like) does not prove that the change is the one in force:
// the same words introduce a plan, a change that was undone and a change
// that happened. An answer that names both values goes to the semantic
// judging, which reads the whole answer. What the scorer refuses to do is
// decide such an answer by matching strings.
//
// The scope matters as well: a number in another frame than the one the
// question asks in does not answer the question. "18 questions a week" is not
// an answer to "how many questions at a time".
//
// The entities and the numbers here are made up: the tests do not name a
// scenario of the dataset.
const key = ({ accepted, forbidden, question = '', category = 'A2' }) => ({
  category,
  question,
  answer: { mode: 'fact', accepted, forbidden, aliases: [] },
})

test('the value in force, on its own, passes', () => {
  const s = key({ accepted: ['7道题'], forbidden: ['13道题'] })
  assert.equal(judgeFact('现在一轮是 7 道题。', s).verdict, 'pass')
})

test('the old value given as the value in force still fails', () => {
  const s = key({ accepted: ['7道题'], forbidden: ['13道题'] })
  assert.equal(judgeFact('现在一轮是 13 道题。', s).verdict, 'fail')
  assert.equal(judgeFact('每轮 13 道题，一直是这样。', s).verdict, 'fail')
  // The answer gives the value in force and denies the old one: that passes.
  assert.equal(judgeFact('现在一轮是 7 道题，不是 13 道题。', s).verdict, 'pass')
})

test('an answer that names both values goes to the semantic judging', () => {
  const s = key({ accepted: ['7道题'], forbidden: ['13道题'] })
  for (const text of [
    '现在一轮是 7 道题。原来定的是 13 道题，后来已经改成 7 道题了。',
    '现在一轮是 7 道题。原来是 13 道题，后来改成 7 道题了。',
    '现在每轮是 7 道题；13 道题已经被 7 道题取代。',
  ]) {
    const judged = judgeFact(text, s)
    assert.equal(judged.verdict, 'needs_judge', text)
    assert.notEqual(judged.verdict, 'pass', text)
  }
})

test('a change that is only planned is not a value in force', () => {
  const s = key({ accepted: ['7道题'], forbidden: ['13道题'] })
  const judged = judgeFact('现在一轮是 13 道题。后面准备改成 7 道题。', s)
  assert.notEqual(judged.verdict, 'pass')
})

test('a change that was undone is not a value in force', () => {
  const s = key({ accepted: ['7道题'], forbidden: ['13道题'] })
  const judged = judgeFact('曾经改成 7 道题，后来又撤销了，现在还是 13 道题。', s)
  assert.notEqual(judged.verdict, 'pass')
})

test('a change that was taken back is not a value in force', () => {
  const s = key({ accepted: ['7道题'], forbidden: ['13道题'] })
  const judged = judgeFact('原来一次是 13 道题，后来又改回 7 道题了。', s)
  assert.notEqual(judged.verdict, 'pass')
})

test('a change that is denied is not a value in force', () => {
  const s = key({ accepted: ['7道题'], forbidden: ['13道题'] })
  const judged = judgeFact('没有改成 7 道题，还是 13 道题。', s)
  assert.notEqual(judged.verdict, 'pass')
})

test('a change that is only considered is not a value in force', () => {
  const s = key({ accepted: ['7道题'], forbidden: ['13道题'] })
  const judged = judgeFact('现在一轮还是 13 道题。我在想是不是应该改成 7 道题。', s)
  assert.notEqual(judged.verdict, 'pass')
})

test('the value in force written in another way is never failed on the old value', () => {
  const s = key({ accepted: ['7道题'], forbidden: ['13道题'] })
  // The key says "7道题", the answer says "7 道": the number is the value, the
  // counter noun around it is not.
  const judged = judgeFact('现在一轮是 7 道。最初定的是 13 道题。', s)
  assert.equal(judged.verdict, 'needs_judge')
  assert.notEqual(judged.verdict, 'fail')
})

test('a value named only while another question is described is never failed', () => {
  const s = key({ accepted: ['合并跳转'], forbidden: ['并发搬运', '批量分片'] })
  const judged = judgeFact('「甲」第二题练的是跳转，第一题练的是并发搬运，第三题是批量分片。', s)
  assert.equal(judged.verdict, 'needs_judge')
  assert.notEqual(judged.verdict, 'fail')
})

test('a forbidden value the answer puts in the past is never failed', () => {
  const s = key({ accepted: ['9页'], forbidden: ['19页'] })
  const judged = judgeFact('以前每次看 19 页，现在每次看多少我这边没有记录。', s)
  assert.equal(judged.verdict, 'needs_judge')
  assert.notEqual(judged.verdict, 'fail')
})

test('a denied value in force is no value in force', () => {
  const s = key({ accepted: ['7道题'], forbidden: ['13道题'] })
  const judged = judgeFact('不是 7 道题。', s)
  assert.equal(judged.verdict, 'needs_judge')
  assert.equal(judged.reason, 'no_fact_value')
})

test('the number in the frame the question asks in answers the question', () => {
  const s = key({ accepted: ['11题'], forbidden: ['29题'], question: '「甲」现在一次要做多少题？' })
  assert.equal(judgeFact('一次 11 题。', s).verdict, 'pass')
})

test('a direct answer without a frame is left alone', () => {
  const s = key({ accepted: ['11题'], forbidden: ['29题'], question: '「甲」现在一次要做多少题？' })
  // No frame word at all: the answer is not required to name one.
  assert.equal(judgeFact('现在是 11 题。', s).verdict, 'pass')
})

test('the same number in another frame does not answer the question', () => {
  const s = key({ accepted: ['11题'], forbidden: ['29题'], question: '「甲」现在一次要做多少题？' })
  for (const text of [
    '按你现在的表，定的是**一周 11 题**，没有定「一次多少题」这个数。',
    '这一周是 11 题，一次做多少没说。',
  ]) {
    const judged = judgeFact(text, s)
    assert.equal(judged.verdict, 'needs_judge', text)
    assert.notEqual(judged.verdict, 'pass', text)
  }
})

test('a weekly number does not answer a question about each time', () => {
  const s = key({ accepted: ['9页'], forbidden: ['19页'], question: '「乙」现在每次看多少页？' })
  const judged = judgeFact('现在每周看 9 页。', s)
  assert.equal(judged.verdict, 'needs_judge')
  assert.notEqual(judged.verdict, 'pass')
})

test('an answer that only quotes the number or asks it back does not state it', () => {
  const s = key({ accepted: ['11题'], forbidden: ['29题'], question: '「甲」现在一次要做多少题？' })
  for (const text of [
    '之前的记录里没有固定题量，之前出差那一周改成过“一次 11 题”，但那是临时压缩的安排。要不要以 11 题为基础？',
    '你现在没有确定的数量——唯一出现过的数字是 **11 题**，但那是出差那周设的量：「甲」临时要出差一周，整块时间没了，改成一次 11 题吧。',
    '按记录，你现在这套「甲」是一周 11 题。如果你要的是“一次坐下来做多少”，你的记录里没定过，需要你补一句。',
  ]) {
    const judged = judgeFact(text, s)
    assert.equal(judged.verdict, 'needs_judge', text)
    assert.notEqual(judged.verdict, 'pass', text)
  }
})

test('a scenario about history still goes to the semantic checkpoint', () => {
  const s = key({ accepted: ['7道题'], forbidden: ['13道题'], category: 'R3' })
  assert.equal(judgeFact('现在一轮是 7 道题。原来是 13 道题。', s).verdict, 'needs_judge')
})

// The old value is read in the same context as the value in force. It can
// only fail the attempt when the answer itself asserts it, in the frame the
// question asks about.
test('the old value in another frame does not fail the attempt', () => {
  const s = key({ accepted: ['23道题'], forbidden: ['46道题'], question: '「甲」现在一次要做多少题？' })
  // The number is counted in another frame than the question asks in: the
  // answer does not say what the question asks, so it is not failed on it.
  const scoped = judgeFact('现在每周46道题。', s)
  assert.equal(scoped.verdict, 'needs_judge')
  assert.equal(scoped.reason, 'scope_mismatch')
  // The same number in the frame the question asks in is the answer's own
  // claim, and that still fails.
  const asserted = judgeFact('现在每次46道题。', s)
  assert.equal(asserted.verdict, 'fail')
  assert.equal(asserted.reason, 'forbidden_value_present')
})

test('the old value the answer only quotes does not fail the attempt', () => {
  const s = key({ accepted: ['23道题'], forbidden: ['46道题'], question: '「甲」现在一次要做多少题？' })
  const quoted = judgeFact('引用原文：「每次46道题」。', s)
  assert.equal(quoted.verdict, 'needs_judge')
  assert.equal(quoted.reason, 'value_not_asserted')
  // The answer states the value in force and quotes the old one: that is a
  // reading for the semantic judging, not a failure.
  const both = judgeFact('现在一次是23道题。记录里写着「每次46道题」。', s)
  assert.equal(both.verdict, 'needs_judge')
  assert.notEqual(both.verdict, 'fail')
})

test('a question without a frame leaves the old value where it stands', () => {
  const s = key({ accepted: ['23道题'], forbidden: ['46道题'] })
  // No frame is asked for, so the frame of the answer decides nothing: the
  // old value the answer states is still the old value given as the value in
  // force.
  assert.equal(judgeFact('现在每次46道题。', s).verdict, 'fail')
  assert.equal(judgeFact('现在每周46道题。', s).verdict, 'fail')
})

test('a place has no frame of its own', () => {
  const s = key({ accepted: ['青芦岸'], forbidden: ['白石滩'], question: '「丙」现在的活动场地定在哪里？' })
  // The old place the answer gives as the place in force still fails.
  assert.equal(judgeFact('现在定在白石滩。', s).verdict, 'fail')
  // The old place the answer puts in the past is not the answer either.
  const past = judgeFact('原来定在白石滩，现在定在青芦岸。', s)
  assert.equal(past.verdict, 'needs_judge')
  assert.notEqual(past.verdict, 'fail')
})

// The known false passes of the reference set, in anonymous form: each of them
// passed the mechanical scorer because the accepted value appears as a
// substring of the answer, and none of them gives the value the question asks
// for. The answer that only asks the number back is one of them.
test('the known false passes do not pass', () => {
  const s = key({ accepted: ['23题'], forbidden: ['46题'], question: '「甲」现在一次要做多少题？' })
  for (const text of [
    '现在的记录里，甲没有固定题量，仍旧是「一次做很多」。之前出差那一周，你把它改成过“一次 23 题”，但那是临时压缩的安排，不是常态。所以要给甲定一个固定数字，还需要你自己决定一个。你要不要以 23 题为基础？',
    '现在定的是**一周 23 题**，没有定「一次做多少题」这个数。',
    '这一周是 23 题，一次做多少没说。',
  ]) {
    const judged = judgeFact(text, s)
    assert.equal(judged.verdict, 'needs_judge', text)
    assert.notEqual(judged.verdict, 'pass', text)
  }
})

test('an answer that only asks the number back does not state it', () => {
  const s = key({ accepted: ['23题'], forbidden: ['46题'], question: '「甲」现在一次要做多少题？' })
  const judged = judgeFact('要不要以 23 题为基础？', s)
  assert.equal(judged.verdict, 'needs_judge')
  assert.equal(judged.reason, 'value_not_asserted')
  // The old value the answer only asks about is not its own claim either.
  const old = key({ accepted: ['23道题'], forbidden: ['46道题'], question: '「甲」现在一次要做多少题？' })
  const asked = judgeFact('之前是 46 道题吧？', old)
  assert.equal(asked.verdict, 'needs_judge')
  assert.notEqual(asked.verdict, 'fail')
})
