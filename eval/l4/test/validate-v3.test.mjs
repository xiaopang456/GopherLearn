import { test } from 'node:test'
import assert from 'node:assert/strict'
import {validateDataset} from '../validate-v3.mjs'
import {splitForId} from '../scenarios.mjs'
function scenario(){
 const id='example'; const turns=Array.from({length:60},(_,i)=>({user:`user ${i}`,assistant:`answer ${i}`,user_at:new Date(1700000000000+i*120000).toISOString(),assistant_at:new Date(1700000060000+i*120000).toISOString()})); turns[34].user='name is Lantern'
 return {id,split:splitForId(id),schema_version:'l4.scenario.draft.v3',category:'R1',entity_name:'Lantern',turns,question:'What was the name?',fact_turn:35,distance_to_question:26,evidence_turns:[35],answer:{mode:'fact',accepted:['Lantern']}}
}
test('flags answer leakage, reused names, and earlier names',()=>{
 const s=scenario();assert.equal(validateDataset([s]).errors.length,0)
 s.turns[38].user='Lantern';assert.ok(validateDataset([s]).errors.some(x=>x.message.includes('outside evidence')))
 assert.ok(validateDataset([scenario()],['the lantern project']).errors.some(x=>x.message.includes('earlier')))
})
test('detects contradictory weekday',()=>{ const s=scenario();s.turns[0].user='2026-01-05 星期二';assert.ok(validateDataset([s]).errors.some(x=>x.message.includes('weekday'))) })
