import test from 'node:test'
import assert from 'node:assert/strict'
import {judgeFact} from '../score.mjs'

test('fact expressions normalize numbers, units and times within prose', () => {
  const rows = [
    ['38分钟','21分钟','三十八分钟','二十一分钟'],
    ['7次','3次','七次','三次'],
    ['41页','26页','四十一页','二十六页'],
    ['5天','3天','五天','三天'],
    ['06:30','22:10','早上六点半','晚上十点十分'],
    ['20:05','21:30','晚上八点零五分','晚上九点半'],
    ['15:00','06:30','下午三点','早上六点半'],
    ['38分钟','21分钟','38 min','21 min'],
  ]
  for (const [expected, old, spoken, oldSpoken] of rows) {
    const scenario = {answer: {accepted:[expected], forbidden:[old], aliases:[]}}
    for (const [answer, verdict] of [[spoken,'pass'],[oldSpoken,'fail'],[`不是${oldSpoken}，而是${spoken}`,'pass'],[`${oldSpoken}已取消，现在是${spoken}`,'pass'],[`不是${spoken}`,'needs_judge'],[`${spoken}和${oldSpoken}`,'needs_judge']]) {
      assert.equal(judgeFact(answer, scenario).verdict, verdict, answer)
    }
  }
})

test('the no-evidence fallback banner is stripped before judging', () => {
  const scenario = {category:'R1', answer:{accepted:['岱岑'], forbidden:[], aliases:[]}}
  const banner = '你的个人知识库中没有找到足以回答该问题的笔记证据。\n以下内容来自模型的通用知识，不是你上传笔记中的结论，可能与你的笔记表述不一致。'
  const value = '是“岱岑”——你之前定的那个固定词，为的是脚本能按标题定位。'
  // The banner is retrieval status, not a statement about the value. The
  // value behind it is affirmed, even though the sentence attributes it to an
  // earlier turn ("你之前定的").
  assert.equal(judgeFact(`${banner}\n\n${value}`, scenario).verdict, 'pass')
  assert.equal(judgeFact(value, scenario).verdict, 'pass')
  // The retrieval-unavailable banner is stripped the same way.
  const unavailable = '当前个人知识库检索暂时不可用，本次没有使用你上传的笔记。\n以下内容来自模型的通用知识，请在知识库恢复后再核对。'
  assert.equal(judgeFact(`${unavailable}\n\n${value}`, scenario).verdict, 'pass')
  // A banner must not mask a genuinely denied value.
  assert.equal(judgeFact(`${banner}\n\n不是岱岑。`, scenario).verdict, 'needs_judge')
})

test('attributional "你之前定的" is not a past marker, but real past values stay pending', () => {
  const name = {answer:{accepted:['澜岐'], forbidden:[], aliases:[]}}
  assert.equal(judgeFact('你之前定的名字是澜岐。', name).verdict, 'pass')
  assert.equal(judgeFact('之前叫澜岐，现在改叫别的了。', name).verdict, 'needs_judge')
  const schedule = {answer:{accepted:['7次'], forbidden:['3次'], aliases:[]}}
  assert.equal(judgeFact('你之前定的是每周 7 次。', schedule).verdict, 'pass')
  assert.equal(judgeFact('之前是 3 次。', schedule).verdict, 'needs_judge')
  assert.equal(judgeFact('现在是每周 3 次。', schedule).verdict, 'fail')
})

test('R3 routes answers naming both historical and current values to the semantic checkpoint', () => {
  const scenario = {category:'R3', answer:{accepted:['24分钟'], forbidden:['76分钟'], aliases:[]}}
  for (const answer of ['24分钟已取消，现在是76分钟', '最初是24分钟，后来改成76分钟', '不是76分钟，最初是24分钟']) {
    assert.equal(judgeFact(answer, scenario).verdict, 'needs_judge', answer)
  }
  assert.equal(judgeFact('76分钟', scenario).verdict, 'fail')
  assert.equal(judgeFact('二十四分钟', scenario).verdict, 'pass')
})

test('ambiguous ordinary expressions never establish a fact', () => {
  const rows = [
    ['01:00', '这一点很重要'],
    ['03:00', '有三点建议'],
    ['3次', '三次元内容'],
    ['3次', 'v3次格式'],
    ['3天', '三天线设备'],
    ['3天', '三天气温变化'],
    ['3h', '3hourslater'],
    ['06:30', '六点半'],
  ]
  for (const [key, answer] of rows) {
    assert.equal(judgeFact(answer, {answer:{accepted:[key], aliases:[]}}).verdict, 'needs_judge', answer)
  }
  const r3 = {category:'R3', answer:{accepted:['01:00'],forbidden:['22:10'],aliases:[]}}
  assert.equal(judgeFact('这一点很重要，后来是晚上十点十分', r3).verdict, 'needs_judge')
})

test('punctuated clock keys normalize before R3 historical-value routing', () => {
  for (const field of ['accepted', 'aliases']) {
    const answer = {accepted: field === 'accepted' ? ['06:30。'] : ['06:30'], aliases: field === 'aliases' ? ['06:30。'] : [], forbidden:['22:10！']}
    const scenario = {category:'R3', answer}
    assert.equal(judgeFact('早上六点半', scenario).verdict, 'pass')
    assert.equal(judgeFact('最初六点半，后来晚上十点十分', scenario).verdict, 'needs_judge')
    assert.equal(judgeFact('晚上十点十分', scenario).verdict, 'fail')
  }
})
