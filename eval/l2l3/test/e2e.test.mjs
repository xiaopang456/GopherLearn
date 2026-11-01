// The whole offline chain: fake backend -> provider -> raw records -> offline scoring.
//
// Two scripted arms play against the synthetic corpus. What every trial must come out as
// was worked out by hand (see fake/scenarios.mjs for what each arm does wrong), and the
// result is also pinned as a golden file the Go side reads, so a change to the outcome
// schema on either side fails a test.

import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { fakeInspector, startFakeBackend } from '../fake/server.mjs'
import { flawedScript, goodScript, QUESTIONS } from '../fake/scenarios.mjs'
import { insideWorktree } from '../lib/pathguard.mjs'
import { resolveConfig, TrialRunner } from '../provider/trial.mjs'
import { readRecords, scoreRun, writeOutcomes, writePin } from '../score.mjs'

const here = path.dirname(fileURLToPath(import.meta.url))
const testdata = path.join(here, '..', 'testdata')
const l1data = path.join(here, '..', '..', 'testdata')
const GOLDEN = path.join(testdata, 'outcomes.fixture.jsonl')
const GOLDEN_META = path.join(testdata, 'outcomes.fixture.meta.json')

const paths = {
  datasetPath: path.join(l1data, 'dataset.jsonl'),
  labelsPath: path.join(testdata, 'labels.jsonl'),
  l3OnlyPath: path.join(testdata, 'l3only.jsonl'),
  corpusManifest: path.join(l1data, 'corpus.json'),
}

const TRIALS = [
  ['q-neap', QUESTIONS.neap], ['q-bar', QUESTIONS.bar], ['q-wick-glass', QUESTIONS.wick], ['q-fuel', QUESTIONS.fuel],
  ['q-tungsten', QUESTIONS.tungsten], ['l3-chat-01', QUESTIONS.chat], ['l3-mem-01', QUESTIONS.mem], ['l3-hist-01', QUESTIONS.hist],
]
const REPEATS = 3

async function runArm(arm, script, base) {
  const fake = await startFakeBackend(script)
  const runner = new TrialRunner(resolveConfig({
    env: 'fake', baseUrl: fake.url, email: 'u', password: 'p', maxRuns: 1000,
    ledgerPath: path.join(base, `${arm}.ledger.jsonl`), rawDir: path.join(base, 'raw'), timeoutMs: 3000,
  }, {}))
  for (const [qid, question] of TRIALS) {
    for (let rep = 0; rep < REPEATS; rep += 1) await runner.run({ arm, qid, rep, question })
  }
  await fake.close()
}

/** Play both arms into a fresh directory and return it. */
async function playBoth(t) {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), 'l23-e2e-'))
  t.after(() => fs.rmSync(base, { recursive: true, force: true }))
  await runArm('good', goodScript(), base)
  await runArm('flawed', flawedScript(), base)
  writePin(path.join(base, 'raw'), paths)
  return base
}

const skip = () => insideWorktree(os.tmpdir()) !== null

function tally(outcomes, arm, layer) {
  const counts = {}
  for (const o of outcomes.filter((x) => x.arm === arm && x.layers[layer])) {
    const v = o.layers[layer].verdict
    counts[v] = (counts[v] ?? 0) + 1
  }
  return counts
}

function byQuestion(outcomes, arm, layer) {
  const out = {}
  for (const o of outcomes.filter((x) => x.arm === arm && x.layers[layer])) {
    ;(out[o.qid] ??= []).push(`${o.layers[layer].verdict}${o.layers[layer].class ? `:${o.layers[layer].class}` : ''}`)
  }
  return out
}

test('the good arm passes every trial on every layer', { skip: skip() }, async (t) => {
  const base = await playBoth(t)
  const { outcomes } = await scoreRun({ rawDir: path.join(base, 'raw'), ...paths, defaultMaxToolCalls: 3 })
  assert.deepEqual(tally(outcomes, 'good', 'l2'), { pass: 15 }) // 5 L2 questions x 3 repeats
  assert.deepEqual(tally(outcomes, 'good', 'l3'), { pass: 21 }) // 7 L3 questions x 3 repeats
})

test('the flawed arm fails each question in the way it was scripted to, repeat by repeat', { skip: skip() }, async (t) => {
  const base = await playBoth(t)
  const { outcomes } = await scoreRun({ rawDir: path.join(base, 'raw'), ...paths, defaultMaxToolCalls: 3 })

  assert.deepEqual(byQuestion(outcomes, 'flawed', 'l2'), {
    'q-neap': ['fail:citation_miss', 'fail:citation_miss', 'fail:citation_miss'], // the wrong document
    'q-bar': ['fail:refused', 'fail:refused', 'fail:refused'], // refused though the answer is there
    'q-wick-glass': ['pass', 'pass', 'error:run_failed'], // the service failed once: an error, not a failure
    'q-fuel': ['pass', 'pass', 'fail:citation_out_of_range'], // flaky third repeat, a citation past the end
    'q-tungsten': ['needs_judge:grounded_answer', 'needs_judge:grounded_answer', 'needs_judge:grounded_answer'],
  })
  assert.deepEqual(tally(outcomes, 'flawed', 'l2'), { pass: 4, fail: 7, needs_judge: 3, error: 1 })

  assert.deepEqual(byQuestion(outcomes, 'flawed', 'l3'), {
    'q-neap': ['pass', 'pass', 'pass'], // wrong citation, right tool use
    'q-bar': ['pass', 'pass', 'pass'],
    'q-wick-glass': ['pass', 'pass', 'error:run_failed'],
    'q-fuel': ['pass', 'pass', 'pass'],
    'l3-chat-01': ['fail:forbidden_tool_used', 'fail:forbidden_tool_used', 'fail:forbidden_tool_used'], // searched while chatting
    'l3-mem-01': ['fail:missing_required_tool', 'fail:missing_required_tool', 'fail:missing_required_tool'], // saved nothing
    'l3-hist-01': ['pass', 'pass', 'pass'], // search_past_sessions is the same capability as memory_search
  })
  assert.deepEqual(tally(outcomes, 'flawed', 'l3'), { pass: 14, fail: 6, error: 1 })
})

test('unanswerable and L3-only questions get only the layers they have', { skip: skip() }, async (t) => {
  const base = await playBoth(t)
  const { outcomes } = await scoreRun({ rawDir: path.join(base, 'raw'), ...paths, defaultMaxToolCalls: 3 })
  const of = (arm, qid) => outcomes.find((o) => o.arm === arm && o.qid === qid && o.rep === 0)
  assert.equal(of('good', 'q-tungsten').layers.l3, null) // no tool label for it
  assert.equal(of('good', 'q-tungsten').layers.l2.tier, 'abstained')
  assert.equal(of('good', 'l3-chat-01').layers.l2, null) // no gold answer, nothing to judge on L2
  assert.equal(of('good', 'q-wick-glass').layers.l2.coverage, 1)
  assert.equal(of('good', 'q-fuel').layers.l2.coverage, 1) // one citation covering both alternative sets
  assert.equal(of('good', 'q-neap').cluster, 'tide-tables.md')
  assert.equal(of('good', 'l3-chat-01').cluster, 'chitchat')
  assert.equal(outcomes.some((o) => o.qid === 'q-graft-when'), false) // the sealed question was never asked
})

test('scoring twice writes the same bytes, and the golden file pins the outcome schema', { skip: skip() }, async (t) => {
  const base = await playBoth(t)
  const options = { rawDir: path.join(base, 'raw'), ...paths, defaultMaxToolCalls: 3 }
  const first = await scoreRun(options)
  const second = await scoreRun(options)
  writeOutcomes(path.join(base, 'a'), first)
  writeOutcomes(path.join(base, 'b'), second)
  const a = fs.readFileSync(path.join(base, 'a', 'outcomes.jsonl'), 'utf8')
  assert.equal(a, fs.readFileSync(path.join(base, 'b', 'outcomes.jsonl'), 'utf8'))
  assert.equal(first.outcomes.length, 2 * TRIALS.length * REPEATS) // every trial has at least one layer

  // Durations differ from run to run; everything else is a function of the scripts.
  const scrub = (text) => text.split('\n').filter(Boolean).map((line) => {
    const o = JSON.parse(line)
    o.cost.durationMs = 0
    return JSON.stringify(o)
  }).join('\n') + '\n'
  const meta = `${JSON.stringify(first.meta, null, 2)}\n`
  if (process.env.UPDATE_GOLDEN === '1') {
    fs.writeFileSync(GOLDEN, scrub(a))
    fs.writeFileSync(GOLDEN_META, meta)
  }
  assert.equal(scrub(a), fs.readFileSync(GOLDEN, 'utf8').replace(/\r\n/g, '\n'), 'outcomes differ from testdata/outcomes.fixture.jsonl (UPDATE_GOLDEN=1 rewrites it)')
  assert.equal(meta, fs.readFileSync(GOLDEN_META, 'utf8').replace(/\r\n/g, '\n'))
})

test('a run is refused when the question files changed after it was made', { skip: skip() }, async (t) => {
  const base = await playBoth(t)
  const changed = path.join(base, 'dataset-changed.jsonl')
  fs.writeFileSync(changed, `${fs.readFileSync(paths.datasetPath, 'utf8')}\n`)
  await assert.rejects(scoreRun({ rawDir: path.join(base, 'raw'), ...paths, datasetPath: changed }), /dataset_sha256 changed since this run was made/)
  const labels = path.join(base, 'labels-changed.jsonl')
  fs.writeFileSync(labels, fs.readFileSync(paths.labelsPath, 'utf8').replace('"max_tool_calls":3', '"max_tool_calls":2'))
  await assert.rejects(scoreRun({ rawDir: path.join(base, 'raw'), ...paths, labelsPath: labels }), /labels_sha256 changed/)
})

test('an unpinned run, a record for an unknown question and a malformed label are all refused', { skip: skip() }, async (t) => {
  const base = await playBoth(t)
  const raw = path.join(base, 'raw')

  fs.renameSync(path.join(raw, 'run.json'), path.join(base, 'run.json.bak'))
  await assert.rejects(scoreRun({ rawDir: raw, ...paths }), /not pinned to a question set/)
  const allowed = await scoreRun({ rawDir: raw, ...paths, allowUnpinned: true })
  assert.ok(allowed.outcomes.length > 0)
  fs.renameSync(path.join(base, 'run.json.bak'), path.join(raw, 'run.json'))

  const stray = JSON.parse(fs.readFileSync(path.join(raw, 'good', 'q-neap.r0.json'), 'utf8'))
  stray.qid = 'q-ghost'
  fs.writeFileSync(path.join(raw, 'good', 'q-ghost.r0.json'), JSON.stringify(stray))
  await assert.rejects(scoreRun({ rawDir: raw, ...paths }), /no question file has: q-ghost/)
  fs.rmSync(path.join(raw, 'good', 'q-ghost.r0.json'))

  const bad = path.join(base, 'labels-bad.jsonl')
  fs.writeFileSync(bad, '{"id":"q-neap","expect":["web_search"],"forbid":["knowledge_search"],"max_tool_calls":-1}\n')
  await assert.rejects(scoreRun({ rawDir: raw, ...paths, labelsPath: bad, allowUnpinned: true }), /unknown capability "web_search"/)
})

test('the records are read in a fixed order and none is skipped', { skip: skip() }, async (t) => {
  const base = await playBoth(t)
  const records = readRecords(path.join(base, 'raw'))
  assert.equal(records.length, 2 * TRIALS.length * REPEATS)
  const keys = records.map((r) => `${r.arm}/${r.qid}/${r.rep}`)
  assert.equal(keys[0], 'flawed/l3-chat-01/0') // arms in name order, then question id, then repeat
  assert.equal(keys.at(-1), 'good/q-wick-glass/2')
  assert.equal(new Set(keys).size, keys.length)
})

test('the fake inspector feeds the doc constraint through the whole chain', { skip: skip() }, async (t) => {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), 'l23-doc-'))
  t.after(() => fs.rmSync(base, { recursive: true, force: true }))
  const script = goodScript()
  // This question's run searched doc-a and then read doc-b, which it never retrieved.
  script.scenarios[0].reps[0].inspect = [
    { sequence: 1, capability: 'knowledge_search', docIds: ['doc-a'] },
    { sequence: 2, capability: 'document_read', docIds: ['doc-b'] },
  ]
  const fake = await startFakeBackend(script)
  t.after(() => fake.close())
  const runner = new TrialRunner(resolveConfig({
    env: 'fake', baseUrl: fake.url, email: 'u', password: 'p', maxRuns: 10,
    ledgerPath: path.join(base, 'l.jsonl'), rawDir: path.join(base, 'raw'),
  }, {}))
  await runner.run({ arm: 'good', qid: 'q-neap', rep: 0, question: QUESTIONS.neap })

  const labels = path.join(base, 'labels.jsonl')
  fs.writeFileSync(labels, '{"id":"q-neap","expect":["knowledge_search"],"forbid":[],"max_tool_calls":3,"doc_constraint":true}\n')
  const { outcomes } = await scoreRun({
    rawDir: path.join(base, 'raw'), ...paths, labelsPath: labels, allowUnpinned: true, inspector: fakeInspector(fake.url),
  })
  assert.equal(outcomes[0].layers.l3.verdict, 'fail')
  assert.equal(outcomes[0].layers.l3.class, 'document_not_retrieved')
})
