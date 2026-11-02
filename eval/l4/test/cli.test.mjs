import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { main } from '../cli.mjs'
import { splitForId } from '../scenarios.mjs'
test('offline CLI scores deterministically, reports pairs and detects tampering', t => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'conversation-score-')); t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const id = 'synthetic-case', split = splitForId(id)
  const s = { id, category: 'early_fact', split, question: 'Chosen name?', turns: [{ user: 'Lantern', assistant: 'Okay', user_at: '2026-01-01T00:00:00Z', assistant_at: '2026-01-01T00:00:01Z' }], evidence_turns: [1], answer: { mode: 'fact', accepted: ['Lantern'], forbidden: ['Harbor'] } }
  const dataset = path.join(dir, 'dataset.jsonl'), records = path.join(dir, 'records.jsonl'), output = path.join(dir, 'run')
  fs.writeFileSync(dataset, JSON.stringify(s) + '\n')
  fs.writeFileSync(records, ['left', 'right'].flatMap(arm => [0, 1, 2].map(rep => JSON.stringify({ qid: id, arm, rep, env: 'fake', answer: arm === 'left' ? 'Lantern' : 'Harbor' }))).join('\n'))
  main(['score', '--dataset', dataset, '--records', records, '--out', output, '--split', split])
  const report = path.join(dir, 'report.json')
  main(['report', '--run', output, '--out', report, '--left', 'left:main', '--right', 'right:main', '--split', split])
  assert.equal(JSON.parse(fs.readFileSync(report)).mean_difference, 1)
  assert.throws(() => main(['score', '--dataset', dataset, '--records', records, '--out', output, '--split', split]), /output exists/)
  fs.appendFileSync(path.join(output, 'outcomes.jsonl'), '\n')
  assert.throws(() => main(['report', '--run', output, '--out', path.join(dir, 'bad.json'), '--left', 'left:main', '--right', 'right:main', '--split', split]), /manifest mismatch/)
})
