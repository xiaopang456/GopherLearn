import fs from 'node:fs'
import path from 'node:path'
import {checkScenario} from './scenarios.mjs'

// Dataset checks complement the per-scenario structural checks; human review
// remains necessary for semantics, transitions, paraphrases and relative dates.
export function validateDataset(scenarios) {
 const errors = [], names = new Map(), ids = new Set()
 const add = (s, message) => errors.push({id:s.id, message})
 for (const s of scenarios) {
  for (const e of checkScenario(s)) add(s,e)
  if (ids.has(s.id)) add(s,'duplicate id'); ids.add(s.id)
  if (typeof s.entity_name !== 'string' || !s.entity_name.trim()) { add(s,'missing entity_name'); continue }
  const name=s.entity_name.normalize('NFKC').trim().toLowerCase()
  if (names.has(name)) add(s,`duplicate name: ${names.get(name)}`); names.set(name,s.id)
  const evidence = new Set(Array.isArray(s.evidence_turns) ? s.evidence_turns : [])
  const seen = new Set()
  for (const [i,t] of (s.turns || []).entries()) {
   const content = `${t.user || ''}\u0000${t.assistant || ''}`.normalize('NFKC').replace(/\s+/g,'')
   if (seen.has(content)) add(s,`duplicate turn ${i+1}`)
   seen.add(content)
   for (const text of [t.user || '', t.assistant || '']) {
    for (const m of text.matchAll(/(20\d{2})[-年](\d{1,2})[-月](\d{1,2})日?\s*(?:星期|周)([一二三四五六日天])/g)) {
     const date = new Date(Date.UTC(Number(m[1]), Number(m[2])-1, Number(m[3])))
     const day = {'日':0,'天':0,'一':1,'二':2,'三':3,'四':4,'五':5,'六':6}[m[4]]
     if (date.getUTCFullYear() !== Number(m[1]) || date.getUTCMonth() !== Number(m[2])-1 || date.getUTCDate() !== Number(m[3]) || date.getUTCDay() !== day) add(s,`weekday/date mismatch at turn ${i+1}`)
    }
   }
  }
  const needles = new Set([...(s.answer?.accepted || []), ...(s.answer?.aliases || []), ...(s.answer?.forbidden || []), s.superseded_value].filter(x=>typeof x==='string' && x.trim()).map(x=>x.normalize('NFKC').replace(/\s+/g,'').toLowerCase()))
  for (const [i,t] of (s.turns || []).entries()) {
   if (evidence.has(i+1)) continue
   const text = `${t.user} ${t.assistant}`.normalize('NFKC').replace(/\s+/g,'').toLowerCase()
   if (text.includes(name)) add(s,`name occurs outside evidence at turn ${i+1}`)
   for (const x of needles) if (text.includes(x)) add(s,`answer occurs outside evidence at turn ${i+1}: ${x}`)
  }
  const q=(s.question || '').normalize('NFKC').replace(/\s+/g,'').toLowerCase()
  for (const x of needles) if (q.includes(x)) add(s,'question contains an answer')
  if ((s.category === 'R2' || s.category === 'R3') && !q.includes(name)) add(s,'question missing plan name')
 }
 return {count:scenarios.length, errors, limitations:['semantic similarity, approximate quantities, relative dates, natural transitions and uniqueness against earlier datasets require separate review']}
}
if (process.argv[1] && import.meta.url === new URL(`file:///${process.argv[1].replaceAll('\\','/')}`).href) {
 const dir=process.argv[2]; if (!dir) throw Error('supply scenario directory')
 const files=fs.readdirSync(dir).filter(x=>x.endsWith('.json')).sort()
 const data=files.map(f=>JSON.parse(fs.readFileSync(path.join(dir,f),'utf8').replace(/^\uFEFF/,'')))
 const report=validateDataset(data); console.log(JSON.stringify(report,null,2)); if (report.errors.length) process.exitCode=1
}
