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
