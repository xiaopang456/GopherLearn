import { test } from 'node:test'
import assert from 'node:assert/strict'
import { checkScenario, splitForId } from '../scenarios.mjs'

function example(category) {
 const turns = Array.from({length: 95}, (_, i) => ({user:'question', assistant:'answer', user_at:new Date(1700000000000 + i*120000).toISOString(), assistant_at:new Date(1700000000000 + i*120000+60000).toISOString()}))
 const s = {id:'v3-test', split:splitForId('v3-test'), schema_version:'l4.scenario.draft.v3', category, question:'question?', turns, evidence_turns:[35], fact_turn:35, distance_to_question:61, answer:{mode:'fact', accepted:['a']}}
 if (category === 'R2' || category === 'R3') { delete s.fact_turn; s.original_turn=35; s.superseded_turn=60; s.superseded_value='a'; s.distance_to_question=36; s.evidence_turns=[35,60]; s.answer=category === 'R2'?{mode:'fact',accepted:['b'],forbidden:['a']}:{mode:'fact',accepted:['a'],forbidden:['b']} }
 if (category === 'R4') {s.fact_turn=94;s.distance_to_question=2;s.evidence_turns=[94]}
 if (category === 'R5') s.answer={mode:'needs_judge'}
 if (category === 'R6') {s.fact_turn=6;s.distance_to_question=90;s.evidence_turns=[6]}
 return s
}
test('v3 validates each category and recomputes distances', () => {
 for (const c of ['R1','R2','R3','R4','R5','R6']) { const s=example(c); assert.deepEqual(checkScenario(s),[]); s.distance_to_question++; assert.ok(checkScenario(s).length) }
})
test('updates need both evidence turns and old value in correct key',()=>{
 const s=example('R2'); s.evidence_turns=[60]; s.answer.forbidden=['x']; assert.ok(checkScenario(s).length >= 2)
})
test('an answer key cannot name one value as both right and wrong',()=>{
 // example('R2') answers 'b' and forbids 'a'; the mutations below push one value
 // into both lists, in three spellings the scorer would read as the same value.
 for (const [name, mutate, want] of [
  ['forbidden repeats the accepted value', s => { s.forbidden=['b'] }, 'accepted value is also forbidden'],
  ['forbidden repeats it with spacing', s => { s.forbidden=[' b '] }, 'accepted value is also forbidden'],
  ['forbidden repeats it in full width', s => { s.forbidden=['Ｂ'] }, 'accepted value is also forbidden'],
  ['alias equals a forbidden value', s => { s.aliases=['a'] }, 'alias is also forbidden'],
  ['alias equals a forbidden value in full width', s => { s.aliases=['Ａ'] }, 'alias is also forbidden'],
 ]) {
  const s=example('R2'); s.answer.aliases=[]; mutate(s.answer)
  const errors=checkScenario(s)
  assert.ok(errors.includes(want), `${name}: ${JSON.stringify(errors)}`)
 }
})
test('a distinct answer key stays valid',()=>{
 const s=example('R2'); s.answer.aliases=[' ｂ ']
 assert.deepEqual(checkScenario(s),[])
})
test('the overlap rule also guards the earlier categories',()=>{
 const stale={id:'v1-test',split:splitForId('v1-test'),category:'stale',question:'现在“示例”每次多长？',turns:example('R1').turns,evidence_turns:[11,23],answer:{mode:'fact',accepted:['48分钟'],forbidden:['16分钟','31分钟','93分钟'],aliases:['四十八分钟']}}
 assert.deepEqual(checkScenario(stale),[])
 stale.answer.aliases=['31 分钟']
 assert.deepEqual(checkScenario(stale),['alias is also forbidden'])
 stale.answer.aliases=[]
 stale.answer.forbidden=['4８分钟']
 assert.deepEqual(checkScenario(stale),['accepted value is also forbidden'])
})
