import fs from 'node:fs'
import path from 'node:path'
import crypto from 'node:crypto'
import { pathToFileURL } from 'node:url'
import { assertOutsideWorktree } from '../l2l3/lib/pathguard.mjs'
import { attemptIds, checkScenario, resolveAttempt, scenarioIdOf } from './scenarios.mjs'
import { scoreAttempt } from './score.mjs'
import { pairedReport, singleReport } from './statistics.mjs'
import { attemptKey, categoryReport, judgeCategories, judgeInput, truncationReport } from './truncation.mjs'
import { compareModes, renderMarkdown } from './layered_metrics.mjs'
const hash = data => crypto.createHash('sha256').update(data).digest('hex')
const lines = file => fs.readFileSync(file, 'utf8').split(/\r?\n/).filter(Boolean).map(line => JSON.parse(line))
export function main(args) {
  const [command, ...rest] = args, opts = {}
  for (let i = 0; i < rest.length; i += 2) {
    if (!rest[i].startsWith('--') || !rest[i + 1] || rest[i + 1].startsWith('--') || Object.hasOwn(opts, rest[i].slice(2))) throw new Error('invalid or duplicate option')
    opts[rest[i].slice(2)] = rest[i + 1]
  }
  const allowed = command === 'score' ? ['dataset', 'records', 'out', 'split']
    : command === 'report' ? ['run', 'out', 'left', 'right', 'split']
      : command === 'truncation-report' ? ['dataset', 'prepare', 'out', 'arm']
        : command === 'judge-input' ? ['dataset', 'records', 'out', 'categories', 'outcomes']
          : command === 'layered-metrics' ? ['runs', 'records', 'outcomes', 'dataset', 'out']
            : command === 'category-report' ? ['run', 'dataset', 'out', 'left', 'right', 'judgments', 'split'] : []
  if (!allowed.length || Object.keys(opts).some(k => !allowed.includes(k))) throw new Error('unknown command or option')
  const split = opts.split ?? 'dev'
  if (!['dev', 'sealed'].includes(split)) throw new Error('invalid split')
  for (const k of command === 'score' ? ['dataset', 'records', 'out'] : command === 'judge-input' ? ['dataset', 'records', 'out', 'outcomes'] : command === 'truncation-report' ? ['dataset', 'prepare', 'out'] : command === 'layered-metrics' ? ['out'] : command === 'category-report' ? ['dataset', 'out'] : ['run', 'out']) assertOutsideWorktree(k, opts[k])
  if (fs.existsSync(opts.out)) throw new Error('output exists')
  if (command === 'layered-metrics') {
    const runs = JSON.parse(fs.readFileSync(opts.runs, 'utf8'))
    if (!Array.isArray(runs)) throw new Error('runs file must hold an array of runs')
    const scenarios = new Map((opts.dataset ? lines(opts.dataset) : []).map(s => [s.id, s]))
    const records = [...(opts.records ? lines(opts.records) : []), ...(opts.outcomes ? lines(opts.outcomes) : [])]
    const report = compareModes(runs, records, scenarios)
    fs.writeFileSync(opts.out, JSON.stringify(report, null, 2), { flag: 'wx' })
    process.stdout.write(renderMarkdown(report))
  } else if (command === 'truncation-report') {
    const scenarios = new Map(lines(opts.dataset).map(s => [s.id, s]))
    const warmups = new Map()
    for (const entry of fs.readdirSync(opts.prepare, { withFileTypes: true })) {
      if (!entry.isDirectory()) continue
      const file = path.join(opts.prepare, entry.name, 'warmup.json')
      if (!fs.existsSync(file)) continue
      const id = entry.name.slice(0, entry.name.lastIndexOf('.'))
      warmups.set(id, JSON.parse(fs.readFileSync(file, 'utf8')))
    }
    fs.writeFileSync(opts.out, JSON.stringify(truncationReport(scenarios, warmups, { arm: opts.arm ?? 'unknown' }), null, 2), { flag: 'wx' })
  } else if (command === 'judge-input') {
    const scenarios = new Map(lines(opts.dataset).map(s => [s.id, s]))
    const selected = opts.categories ? opts.categories.split(',').map(c => c.trim()).filter(Boolean) : judgeCategories()
    let pending = null
    if (opts.outcomes) {
      pending = new Set()
      for (const row of lines(opts.outcomes)) {
        if (row?.verdict !== 'needs_judge') continue
        pending.add(attemptKey(row.id, row.arm, row.condition, row.rep))
      }
    }
    const rows = judgeInput(scenarios, lines(opts.records), { selected, pending })
    fs.writeFileSync(opts.out, rows.map(row => JSON.stringify(row)).join('\n') + (rows.length ? '\n' : ''), { flag: 'wx' })
    const broken = rows.filter(row => row.error)
    if (broken.length) {
      console.error(`${broken.length} attempt(s) could not be prepared for judging:`)
      for (const row of broken) console.error(`  ${row.item_id} ${row.arm ?? '-'} ${row.condition} rep ${row.rep ?? '-'}: ${row.error}`)
      console.error('The rows carry no input. Nothing was guessed; resolve them before judging.')
      process.exitCode = 1
    }
  } else if (command === 'category-report') {
    const scenarios = new Map(lines(opts.dataset).map(s => [s.id, s]))
    const manifest = JSON.parse(fs.readFileSync(path.join(opts.run, 'manifest.json'), 'utf8'))
    const data = fs.readFileSync(path.join(opts.run, 'outcomes.jsonl'))
    if (manifest.schema !== 'l4.score.v1' || manifest.split !== split || hash(data) !== manifest.outcomes_sha256) throw new Error('run manifest mismatch')
    const selector = text => { const parts = String(text).split(':'); if (parts.length !== 2 || !/^[A-Za-z0-9._-]+$/.test(parts[0]) || !['main', 'control', 'distractor'].includes(parts[1])) throw new Error('selector must be arm:condition'); return { arm: parts[0], condition: parts[1] } }
    const judgments = opts.judgments ? lines(opts.judgments) : []
    fs.writeFileSync(opts.out, JSON.stringify(categoryReport(lines(path.join(opts.run, 'outcomes.jsonl')), scenarios, { left: selector(opts.left), right: selector(opts.right), judgments }), null, 2), { flag: 'wx' })
  } else if (command === 'score') {
    const scenarios = lines(opts.dataset), byId = new Map()
    for (const s of scenarios) {
      if (checkScenario(s).length || s.split !== split || byId.has(s.id)) throw new Error('invalid, duplicate or wrong-split scenario')
      byId.set(s.id, s)
    }
    if (!scenarios.length) throw new Error('empty dataset')
    const records = lines(opts.records), identities = new Set()
    const outcomes = records.map(r => {
      if (!r || typeof r !== 'object' || typeof r.arm !== 'string' || !/^[A-Za-z0-9._-]+$/.test(r.arm)) throw new Error('invalid record identity')
      // A record of a scenario with several questions names one of its
      // attempts, and the attempt carries the scenario's id with the
      // question's number on it. The record has to name a question that
      // exists, or it is not a record of this dataset.
      const s = byId.get(scenarioIdOf(r.qid))
      if (!s || !resolveAttempt(s, r)) throw new Error('record is not in selected dataset')
      const condition = r.condition ?? 'main', identity = JSON.stringify([r.arm, r.qid, condition, r.rep])
      if (identities.has(identity)) throw new Error('duplicate record'); identities.add(identity)
      return scoreAttempt(r, s, condition)
    })
    const data = outcomes.map(r => JSON.stringify(r) + '\n').join('')
    fs.mkdirSync(opts.out)
    fs.writeFileSync(path.join(opts.out, 'outcomes.jsonl'), data, { flag: 'wx' })
    // The inventory holds one entry per attempt, not per scenario: a scenario
    // with several questions is scored as several attempts, and the report
    // groups the outcomes by the entries of this inventory.
    const inventory = []
    for (const s of scenarios) {
      const conditions = s.category === 'irrelevant' ? ['control', 'distractor'] : ['main']
      for (const attempt of attemptIds(s)) inventory.push({ id: attempt, conditions })
    }
    fs.writeFileSync(path.join(opts.out, 'manifest.json'), JSON.stringify({ schema: 'l4.score.v1', split, dataset_sha256: hash(fs.readFileSync(opts.dataset)), records_sha256: hash(fs.readFileSync(opts.records)), outcomes_sha256: hash(data), count: outcomes.length, scenarios: inventory, notice: 'Offline scoring; fake results are not quality evidence.' }, null, 2), { flag: 'wx' })
  } else {
    const manifest = JSON.parse(fs.readFileSync(path.join(opts.run, 'manifest.json'), 'utf8'))
    const data = fs.readFileSync(path.join(opts.run, 'outcomes.jsonl'))
    if (manifest.schema !== 'l4.score.v1' || manifest.split !== split || hash(data) !== manifest.outcomes_sha256) throw new Error('run manifest mismatch')
    const selector = text => { const parts = String(text).split(':'); if (parts.length !== 2 || !/^[A-Za-z0-9._-]+$/.test(parts[0]) || !['main', 'control', 'distractor'].includes(parts[1])) throw new Error('selector must be arm:condition'); return { arm: parts[0], condition: parts[1] } }
    const rows = lines(path.join(opts.run, 'outcomes.jsonl')), left = selector(opts.left), right = opts.right ? selector(opts.right) : null
    if (!Array.isArray(manifest.scenarios)) throw new Error('missing expected scenario inventory')
    const inventory = new Map()
    for (const s of manifest.scenarios) {
      if (!s || typeof s.id !== 'string' || inventory.has(s.id) || !Array.isArray(s.conditions) || !(s.conditions.length === 1 && s.conditions[0] === 'main' || s.conditions.length === 2 && new Set(s.conditions).size === 2 && s.conditions.every(c => ['control', 'distractor'].includes(c)))) throw new Error('invalid expected scenario inventory')
      inventory.set(s.id, s.conditions)
    }
    if (manifest.count !== rows.length || rows.some(r => !r || r.schema !== 'l4.outcome.v1' || !inventory.get(r.id)?.includes(r.condition))) throw new Error('outcomes differ from manifest inventory')
    const expectedIds = manifest.scenarios.filter(s => s.conditions.includes(left.condition) && (!right || s.conditions.includes(right.condition))).map(s => s.id)
    if (!expectedIds.length) throw new Error('selectors match no expected scenarios')
    const report = right ? pairedReport(rows, left, right, { expectedIds }) : { schema: 'l4.single.v1' }
    const single = [singleReport(rows, left, expectedIds)]
    if (right) single.push(singleReport(rows, right, expectedIds))
    fs.writeFileSync(opts.out, JSON.stringify({ ...report, single, split, notice: manifest.notice }, null, 2), { flag: 'wx' })
  }
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try { main(process.argv.slice(2)) } catch (error) { console.error(error.message); process.exitCode = 1 }
}
