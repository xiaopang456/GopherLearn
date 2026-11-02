import fs from 'node:fs'
import path from 'node:path'
import crypto from 'node:crypto'
import { pathToFileURL } from 'node:url'
import { assertOutsideWorktree } from '../l2l3/lib/pathguard.mjs'
import { checkScenario } from './scenarios.mjs'
import { scoreAttempt } from './score.mjs'
import { pairedReport } from './statistics.mjs'
const hash = data => crypto.createHash('sha256').update(data).digest('hex')
const lines = file => fs.readFileSync(file, 'utf8').split(/\r?\n/).filter(Boolean).map(line => JSON.parse(line))
export function main(args) {
  const [command, ...rest] = args, opts = {}
  for (let i = 0; i < rest.length; i += 2) {
    if (!rest[i].startsWith('--') || !rest[i + 1] || rest[i + 1].startsWith('--') || Object.hasOwn(opts, rest[i].slice(2))) throw new Error('invalid or duplicate option')
    opts[rest[i].slice(2)] = rest[i + 1]
  }
  const allowed = command === 'score' ? ['dataset', 'records', 'out', 'split'] : command === 'report' ? ['run', 'out', 'left', 'right', 'split'] : []
  if (!allowed.length || Object.keys(opts).some(k => !allowed.includes(k))) throw new Error('unknown command or option')
  const split = opts.split ?? 'dev'
  if (!['dev', 'sealed'].includes(split)) throw new Error('invalid split')
  for (const k of command === 'score' ? ['dataset', 'records', 'out'] : ['run', 'out']) assertOutsideWorktree(k, opts[k])
  if (fs.existsSync(opts.out)) throw new Error('output exists')
  if (command === 'score') {
    const scenarios = lines(opts.dataset), byId = new Map()
    for (const s of scenarios) {
      if (checkScenario(s).length || s.split !== split || byId.has(s.id)) throw new Error('invalid, duplicate or wrong-split scenario')
      byId.set(s.id, s)
    }
    if (!scenarios.length) throw new Error('empty dataset')
    const records = lines(opts.records), identities = new Set()
    const outcomes = records.map(r => {
      const s = byId.get(r.qid)
      if (!s) throw new Error('record is not in selected dataset')
      const condition = r.condition ?? 'main', identity = JSON.stringify([r.arm, r.qid, condition, r.rep])
      if (identities.has(identity)) throw new Error('duplicate record'); identities.add(identity)
      return scoreAttempt(r, s, condition)
    })
    const data = outcomes.map(r => JSON.stringify(r) + '\n').join('')
    fs.mkdirSync(opts.out)
    fs.writeFileSync(path.join(opts.out, 'outcomes.jsonl'), data, { flag: 'wx' })
    fs.writeFileSync(path.join(opts.out, 'manifest.json'), JSON.stringify({ schema: 'l4.score.v1', split, dataset_sha256: hash(fs.readFileSync(opts.dataset)), records_sha256: hash(fs.readFileSync(opts.records)), outcomes_sha256: hash(data), count: outcomes.length, notice: 'Offline scoring; fake results are not quality evidence.' }, null, 2), { flag: 'wx' })
  } else {
    const manifest = JSON.parse(fs.readFileSync(path.join(opts.run, 'manifest.json'), 'utf8'))
    const data = fs.readFileSync(path.join(opts.run, 'outcomes.jsonl'))
    if (manifest.schema !== 'l4.score.v1' || manifest.split !== split || hash(data) !== manifest.outcomes_sha256) throw new Error('run manifest mismatch')
    const selector = text => { const parts = String(text).split(':'); if (parts.length !== 2 || parts.some(p => !p)) throw new Error('selector must be arm:condition'); return { arm: parts[0], condition: parts[1] } }
    const report = pairedReport(lines(path.join(opts.run, 'outcomes.jsonl')), selector(opts.left), selector(opts.right))
    fs.writeFileSync(opts.out, JSON.stringify({ ...report, split, notice: manifest.notice }, null, 2), { flag: 'wx' })
  }
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try { main(process.argv.slice(2)) } catch (error) { console.error(error.message); process.exitCode = 1 }
}
