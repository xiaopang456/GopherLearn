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


test('R3 routes answers naming both historical and current values to the semantic checkpoint', () => {
  const scenario = {category:'R3', answer:{accepted:['24分钟'], forbidden:['76分钟'], aliases:[]}}
  for (const answer of ['24分钟已取消，现在是76分钟', '最初是24分钟，后来改成76分钟', '不是76分钟，最初是24分钟']) {
    assert.equal(judgeFact(answer, scenario).verdict, 'needs_judge', answer)
  }
  assert.equal(judgeFact('76分钟', scenario).verdict, 'fail')
  assert.equal(judgeFact('二十四分钟', scenario).verdict, 'pass')
})
