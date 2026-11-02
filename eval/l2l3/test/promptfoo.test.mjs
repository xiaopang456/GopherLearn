import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

import { buildConfig, buildTests, loadQuestions } from '../promptfoo/build-config.mjs'
import { insideWorktree } from '../lib/pathguard.mjs'
import { QUESTIONS, goodScript } from '../fake/scenarios.mjs'
import { startFakeBackend } from '../fake/server.mjs'
import { resolveConfig, TrialRunner } from '../provider/trial.mjs'
import { record, withTools } from './helpers.mjs'

const here = path.dirname(fileURLToPath(import.meta.url))
const testdata = path.join(here, '..', 'testdata')
const l1data = path.join(here, '..', '..', 'testdata')
const require = createRequire(import.meta.url)

/** The synthetic dev questions only: the fixture file also holds a sealed row, which the loader must refuse. */
function devOnlyDataset(dir) {
  const rows = fs.readFileSync(path.join(l1data, 'dataset.jsonl'), 'utf8').split(/\r?\n/).filter(Boolean).filter((l) => JSON.parse(l).split === 'dev')
  const file = path.join(dir, 'dev.jsonl')
  fs.writeFileSync(file, `${rows.join('\n')}\n`)
  return file
}

const spec = (dir, over = {}) => ({
  env: 'fake',
  arms: [{ label: 'a', baseUrl: 'http://127.0.0.1:9' }, { label: 'b', baseUrl: 'http://127.0.0.1:10', knowledgePolicy: 'knowledge_only' }],
  repeat: 3,
  maxRuns: 50,
  ledgerPath: path.join(dir, 'ledger.jsonl'),
  rawDir: path.join(dir, 'raw'),
  datasetPath: devOnlyDataset(dir),
  labelsPath: path.join(testdata, 'labels.jsonl'),
  l3OnlyPath: path.join(testdata, 'l3only.jsonl'),
  corpusManifest: path.join(l1data, 'corpus.json'),
  ...over,
})

const skip = () => insideWorktree(os.tmpdir()) !== null

test('the generated configuration fixes what is easy to get wrong', { skip: skip() }, (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'l23-cfg-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const s = spec(dir)
  const config = buildConfig(s, loadQuestions(s))

  assert.deepEqual(config.evaluateOptions, { maxConcurrency: 1, cache: false, repeat: 3 })
  assert.equal(config.sharing, false)
  assert.deepEqual(config.providers.map((p) => p.label), ['a', 'b'])
  assert.equal(config.providers[1].config.knowledgePolicy, 'knowledge_only')
  assert.equal(config.providers[0].config.knowledgePolicy, 'knowledge_preferred')
  assert.equal(config.providers[0].config.env, 'fake')
  assert.match(config.providers[0].id, /^file:\/\/.*provider\/gl_provider\.mjs$/)
  assert.match(config.defaultTest.assert[0].value, /assertions\.cjs:judgeAssertion$/)

  // Four dev questions with gold evidence (one is unanswerable) plus three L3-only ones;
  // the sealed question never appears.
  assert.equal(config.tests.length, 8)
  assert.equal(JSON.stringify(config).includes('q-graft-when'), false)
  for (const test of config.tests) {
    // Every variable is a string: an array would be expanded into a matrix of test cases.
    for (const [name, value] of Object.entries(test.vars)) assert.equal(typeof value, 'string', `${test.description}: ${name}`)
    assert.equal(typeof JSON.parse(test.vars.q).layers, 'object')
  }
  const neap = config.tests.find((x) => x.vars.id === 'q-neap')
  assert.equal(neap.description, 'q-neap (L2+L3)')
  assert.equal(config.tests.find((x) => x.vars.id === 'l3-chat-01').description, 'l3-chat-01 (L3)')
  assert.equal(config.tests.find((x) => x.vars.id === 'q-tungsten').description, 'q-tungsten (L2)')
})

test('an arm that names enableThinking passes it to its provider; one that does not leaves it out', { skip: skip() }, (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'l23-cfg-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const s = spec(dir, { arms: [{ label: 'on', baseUrl: 'http://127.0.0.1:9', enableThinking: true }, { label: 'off', baseUrl: 'http://127.0.0.1:10', enableThinking: false }, { label: 'unset', baseUrl: 'http://127.0.0.1:11' }] })
  const config = buildConfig(s, loadQuestions(s))
  assert.equal(config.providers[0].config.enableThinking, true)
  assert.equal(config.providers[1].config.enableThinking, false)
  assert.equal('enableThinking' in config.providers[2].config, false)
})

test('the configuration refuses what it should not generate', { skip: skip() }, (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'l23-cfg-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const s = spec(dir)
  const questions = loadQuestions(s)
  assert.throws(() => buildConfig({ ...s, env: undefined }, questions), /env: "fake" or "real"/)
  assert.throws(() => buildConfig({ ...s, arms: [] }, questions), /at least one arm/)
  assert.throws(() => buildConfig({ ...s, arms: [{ label: 'x', baseUrl: 'u' }, { label: 'x', baseUrl: 'v' }] }, questions), /distinct/)
  assert.throws(() => buildConfig({ ...s, repeat: 0 }, questions), /positive integer/)
  assert.throws(() => buildConfig({ ...s, arms: [{ label: 'x', baseUrl: 'u', enableThinking: 'yes' }] }, questions), /enableThinking of an arm must be true or false/)
  // The sealed set is never loaded: a question file with a non-dev row is refused whole.
  assert.throws(() => loadQuestions({ ...s, datasetPath: path.join(l1data, 'dataset.jsonl') }), /not in the dev split \(first: q-graft-when\)/)
})

/** The synthetic sealed question only. */
function sealedOnlyDataset(dir) {
  const rows = fs.readFileSync(path.join(l1data, 'dataset.jsonl'), 'utf8').split(/\r?\n/).filter(Boolean).filter((l) => JSON.parse(l).split === 'sealed')
  const file = path.join(dir, 'sealed.jsonl')
  fs.writeFileSync(file, `${rows.join('\n')}\n`)
  return file
}

const sealedSpec = (dir, over = {}) => spec(dir, { split: 'sealed', datasetPath: sealedOnlyDataset(dir), labelsPath: undefined, l3OnlyPath: undefined, ...over })

test('the sealed split is refused unless it is asked for, and then it is the only split loaded', { skip: skip() }, (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'l23-sealed-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))

  // By default a sealed question file is refused, and the message says how to ask for it.
  assert.throws(() => loadQuestions(spec(dir, { datasetPath: sealedOnlyDataset(dir) })), /not in the dev split \(first: q-graft-when\).*--split sealed/)
  assert.throws(() => loadQuestions(spec(dir, { split: 'dev', datasetPath: sealedOnlyDataset(dir) })), /not in the dev split/)

  // Asked for explicitly, only sealed questions load, and only the answer layer is built.
  const s = sealedSpec(dir)
  const questions = loadQuestions(s)
  assert.deepEqual([...questions.keys()], ['q-graft-when'])
  const config = buildConfig(s, questions)
  assert.deepEqual(config.tests.map((x) => x.description), ['q-graft-when (L2)'])
})

test('a sealed run takes sealed questions only, and no L3 files', { skip: skip() }, (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'l23-sealed-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))

  // A file that mixes in development questions is refused whole.
  assert.throws(() => loadQuestions(sealedSpec(dir, { datasetPath: devOnlyDataset(dir) })), /not in the sealed split \(first: q-neap\)/)
  assert.throws(() => loadQuestions(sealedSpec(dir, { datasetPath: path.join(l1data, 'dataset.jsonl') })), /not in the sealed split/)
  // The sealed set has no L3 labels; passing them would quietly build L3 tests for dev ids.
  assert.throws(() => loadQuestions(sealedSpec(dir, { labelsPath: path.join(testdata, 'labels.jsonl') })), /answer layer only/)
  assert.throws(() => loadQuestions(sealedSpec(dir, { l3OnlyPath: path.join(testdata, 'l3only.jsonl') })), /answer layer only/)
  // No question file at all is not a sealed run.
  assert.throws(() => loadQuestions(sealedSpec(dir, { datasetPath: undefined })), /needs a datasetPath/)
  // Only the two names exist.
  assert.throws(() => loadQuestions(spec(dir, { split: 'Sealed' })), /split must be "dev" or "sealed"/)
  assert.throws(() => loadQuestions(spec(dir, { split: 'test' })), /split must be "dev" or "sealed"/)
})

test('the command line: --split is explicit, and run.json records the split', { skip: skip() }, (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'l23-sealed-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const script = path.join(here, '..', 'promptfoo', 'build-config.mjs')
  const write = (name, s) => {
    const file = path.join(dir, name)
    fs.writeFileSync(file, JSON.stringify(s))
    return file
  }
  const run = (...args) => spawnSync(process.execPath, [script, ...args], { encoding: 'utf8' })
  const pin = (raw) => JSON.parse(fs.readFileSync(path.join(raw, 'run.json'), 'utf8'))

  // Without the flag a sealed spec's file is refused, and nothing is written.
  const noFlag = sealedSpec(dir, { split: undefined, rawDir: path.join(dir, 'raw-a') })
  let result = run(write('a.json', noFlag), path.join(dir, 'out-a'))
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /not in the dev split/)
  assert.equal(fs.existsSync(path.join(dir, 'raw-a')), false)

  // With the flag it runs, says so loudly, and pins the split.
  result = run('--split', 'sealed', write('b.json', noFlag), path.join(dir, 'out-b'))
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /SEALED RUN/)
  assert.match(result.stdout, /1 sealed test\(s\)/)
  assert.equal(pin(path.join(dir, 'raw-a')).split, 'sealed')

  // The spec may name the split itself; the flag and the spec must agree.
  result = run(write('c.json', sealedSpec(dir, { rawDir: path.join(dir, 'raw-c') })), path.join(dir, 'out-c'))
  assert.equal(result.status, 0, result.stderr)
  assert.equal(pin(path.join(dir, 'raw-c')).split, 'sealed')
  result = run('--split', 'dev', write('d.json', sealedSpec(dir, { rawDir: path.join(dir, 'raw-d') })), path.join(dir, 'out-d'))
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /disagrees/)

  // A development run pins "dev", and its raw directory cannot be reused for a sealed run (or the reverse).
  const devSpec = spec(dir, { rawDir: path.join(dir, 'raw-dev') })
  result = run(write('e.json', devSpec), path.join(dir, 'out-e'))
  assert.equal(result.status, 0, result.stderr)
  assert.doesNotMatch(result.stdout, /SEALED/)
  assert.equal(pin(path.join(dir, 'raw-dev')).split, 'dev')
  result = run('--split', 'sealed', write('f.json', sealedSpec(dir, { rawDir: path.join(dir, 'raw-dev') })), path.join(dir, 'out-f'))
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /already holds a dev run/)
  result = run(write('g.json', spec(dir, { rawDir: path.join(dir, 'raw-c') })), path.join(dir, 'out-g'))
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /already holds a sealed run/)

  // Bad command lines.
  assert.match(run('--split').stderr, /needs a value/)
  assert.match(run('--split', 'all', write('h.json', devSpec), path.join(dir, 'out-h')).stderr, /split must be/)
  assert.match(run('--split', 'dev', '--split', 'dev', write('i.json', devSpec), path.join(dir, 'out-i')).stderr, /twice/)
  assert.match(run('--seal', write('j.json', devSpec), path.join(dir, 'out-j')).stderr, /unknown option/)
  assert.match(run(write('k.json', devSpec)).stderr, /usage/)
})

test('buildTests skips a question that has neither a gold answer nor a label', () => {
  const questions = new Map([
    ['a', { id: 'a', text: 'A', layers: { l2: false, l3: false } }],
    ['b', { id: 'b', text: 'B', layers: { l2: true, l3: false } }],
  ])
  assert.deepEqual(buildTests(questions).map((x) => x.vars.id), ['b'])
})

test('the Promptfoo assertion maps verdicts to pass or fail and names the real verdict', { skip: skip() }, async (t) => {
  process.env.GL_L23_CORPUS = path.join(l1data, 'corpus.json')
  t.after(() => { delete process.env.GL_L23_CORPUS })
  const { judgeAssertion } = require('../promptfoo/assertions.cjs')
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'l23-asr-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const s = spec(dir)
  const questions = loadQuestions(s)
  const varsFor = (id) => ({ id, question: questions.get(id).text, q: JSON.stringify(questions.get(id)) })
  const ctx = (rec, id) => ({ vars: varsFor(id), providerResponse: { metadata: { record: rec } } })

  const goodRecord = {
    ...withTools('knowledge_search'),
    qid: 'q-neap',
    sources: [{ file_name: 'tide-tables.md', doc_id: 'd', start_line: 14, end_line: 16, snippet: '', score: 1 }],
  }
  const pass = await judgeAssertion('', ctx(goodRecord, 'q-neap'))
  assert.equal(pass.pass, true)
  assert.equal(pass.reason, 'l2=pass l3=pass')

  const wrongDoc = { ...goodRecord, sources: [{ file_name: 'lantern-maintenance.md', doc_id: 'd', start_line: 1, end_line: 2, snippet: '', score: 1 }] }
  const fail = await judgeAssertion('', ctx(wrongDoc, 'q-neap'))
  assert.equal(fail.pass, false)
  assert.equal(fail.reason, 'l2=fail:citation_miss l3=pass')

  const down = await judgeAssertion('', ctx({ ...goodRecord, http: 500 }, 'q-neap'))
  assert.equal(down.pass, false)
  assert.match(down.reason, /l2=error:run_failed l3=error:run_failed/) // a failed call is an error, not a poor answer

  const pending = await judgeAssertion('', ctx(record({ qid: 'q-tungsten', env: 'fake' }), 'q-tungsten'))
  assert.equal(pending.pass, false)
  assert.match(pending.reason, /l2=needs_judge:grounded_answer/)

  const none = await judgeAssertion('', { vars: varsFor('q-neap'), providerResponse: {} })
  assert.equal(none.pass, false)
  assert.match(none.reason, /^\[error\]/)
})

test('a configuration generated for a real run keeps the budget at zero unless told otherwise', { skip: skip() }, (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'l23-cfg-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const s = spec(dir, { env: 'real', maxRuns: undefined })
  const config = buildConfig(s, loadQuestions(s))
  assert.equal(config.providers[0].config.maxRuns, 0)
})

test('the questions Promptfoo asks are the questions the fake backend has scripts for', { skip: skip() }, async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'l23-cfg-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const s = spec(dir)
  const questions = [...loadQuestions(s).values()].map((q) => q.text).sort()
  assert.deepEqual(questions, Object.values(QUESTIONS).sort())
  const fake = await startFakeBackend(goodScript())
  t.after(() => fake.close())
  const runner = new TrialRunner(resolveConfig({ env: 'fake', baseUrl: fake.url, email: 'u', password: 'p', maxRuns: 20, ledgerPath: path.join(dir, 'l.jsonl'), rawDir: path.join(dir, 'raw') }, {}))
  for (const text of questions) {
    const r = await runner.run({ arm: 'a', qid: 'x', rep: 0, question: text })
    assert.equal(r.error, undefined, text)
  }
})
