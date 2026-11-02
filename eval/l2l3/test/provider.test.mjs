import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import GopherLearnL23Provider from '../provider/gl_provider.mjs'
import { configProblem, resolveConfig, TrialRunner } from '../provider/trial.mjs'
import { startFakeBackend } from '../fake/server.mjs'
import { insideWorktree } from '../lib/pathguard.mjs'

const tmp = () => fs.mkdtempSync(path.join(os.tmpdir(), 'l23-'))
const skipIfTmpInRepo = (t) => {
  if (insideWorktree(os.tmpdir()) !== null) t.skip('the temp directory is inside a git working tree')
  return insideWorktree(os.tmpdir()) !== null
}

const good = {
  answer: 'tides',
  answerBasis: 'personal_knowledge',
  knowledgeStatus: 'grounded',
  knowledgeReason: 'none',
  sources: [{ file_name: 'a.md', doc_id: 'd', start_line: 1, end_line: 2, snippet: '', score: 1 }],
  toolCalls: 1,
  modelCalls: 2,
  contextBudget: { projectedInput: 1234 },
}
const toolStep = { sequence: 2, step_type: 'tool', tool_name: 'knowledge_search', duration_millis: 12, status: 'completed' }
const scenario = (question, ...reps) => ({ question, reps: reps.length ? reps : [{ body: good, steps: [toolStep] }] })

async function harness(t, scenarios, overrides = {}) {
  const dir = tmp()
  const fake = await startFakeBackend({ scenarios })
  t.after(async () => {
    await fake.close()
    fs.rmSync(dir, { recursive: true, force: true })
  })
  const config = {
    env: 'fake',
    baseUrl: fake.url,
    email: 'u@example.invalid',
    password: 'x',
    maxRuns: 100,
    ledgerPath: path.join(dir, 'ledger.jsonl'),
    rawDir: path.join(dir, 'raw'),
    timeoutMs: 3000,
    ...overrides,
  }
  const runner = new TrialRunner(resolveConfig(config, {}))
  const ledger = () => (fs.existsSync(config.ledgerPath) ? fs.readFileSync(config.ledgerPath, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l)) : [])
  return { fake, runner, config, dir, ledger }
}

test('a trial returns a record with the answer, the states, the citations and the steps', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const h = await harness(t, [scenario('q?')])
  const record = await h.runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'q?' })
  assert.equal(record.schema, 'record.v1')
  assert.equal(record.error, undefined)
  assert.deepEqual([record.arm, record.qid, record.rep, record.env, record.http, record.statusCode], ['a', 'q1', 0, 'fake', 200, 1000])
  assert.equal(record.answerBasis, 'personal_knowledge')
  assert.equal(record.sources.length, 1)
  assert.deepEqual(record.steps, [toolStep])
  assert.equal(record.projectedInput, 1234)
  assert.equal(record.toolCalls, 1)
  // The record is on disk where the scorer will look.
  const file = path.join(h.config.rawDir, 'a', 'q1.r0.json')
  assert.deepEqual(JSON.parse(fs.readFileSync(file, 'utf8')), record)
})

test('every trial logs in once, opens its own session and uses a fresh requestId', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const h = await harness(t, [scenario('q?')])
  for (let rep = 0; rep < 4; rep += 1) await h.runner.run({ arm: 'a', qid: 'q1', rep, question: 'q?' })
  assert.equal(h.fake.stats.logins, 1)
  assert.equal(h.fake.stats.sessions, 4)
  assert.equal(h.fake.stats.runs, 4)
  assert.equal(new Set(h.fake.stats.requestIds).size, 4, 'a repeated requestId would be taken for a replay and skip the model')
})

test('the ledger records the run before the request is answered, and every phase after', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const h = await harness(t, [scenario('slow?', { body: good, steps: [toolStep], delayMs: 400 })])
  const pending = h.runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'slow?' })
  await new Promise((resolve) => setTimeout(resolve, 150))
  assert.deepEqual(h.ledger().map((e) => e.phase), ['run'], 'the attempt must be on the ledger while the request is still in flight')
  await pending
  assert.deepEqual(h.ledger().map((e) => e.phase), ['run', 'result', 'steps'])
})

test('the budget stops the run before the request, and says so in a record', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const h = await harness(t, [scenario('q?')], { maxRuns: 2 })
  const records = []
  for (let rep = 0; rep < 4; rep += 1) records.push(await h.runner.run({ arm: 'a', qid: 'q1', rep, question: 'q?' }))
  assert.equal(h.fake.stats.runs, 2)
  assert.equal(records[2].error.startsWith('budget guard'), true)
  assert.equal(records[3].error.startsWith('budget guard'), true)
  assert.equal(h.ledger().filter((e) => e.phase === 'run').length, 2)
  assert.equal(h.ledger().filter((e) => e.phase === 'refused').length, 2)

  const none = await harness(t, [scenario('q?')], { maxRuns: 0 })
  const refused = await none.runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'q?' })
  assert.equal(none.fake.stats.runs, 0)
  assert.match(refused.error, /budget guard/)
})

test('the budget carries over: a second runner on the same ledger sees what the first spent', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const h = await harness(t, [scenario('q?')], { maxRuns: 2 })
  await h.runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'q?' })
  await h.runner.run({ arm: 'a', qid: 'q1', rep: 1, question: 'q?' })
  const again = new TrialRunner(resolveConfig(h.config, {}))
  const record = await again.run({ arm: 'a', qid: 'q1', rep: 2, question: 'q?' })
  assert.match(record.error, /budget guard: 2 runs already recorded/)
  assert.equal(h.fake.stats.runs, 2)
})

test('nothing is retried: a failed call is one request and one record', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const h = await harness(t, [
    scenario('boom?', { http: 500, body: { status_code: 4001 } }),
    scenario('busy?', { body: { status_code: 4001, status_msg: 'busy' } }),
    scenario('junk?', { rawText: 'not json at all' }),
  ])
  const boom = await h.runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'boom?' })
  const busy = await h.runner.run({ arm: 'a', qid: 'q2', rep: 0, question: 'busy?' })
  const junk = await h.runner.run({ arm: 'a', qid: 'q3', rep: 0, question: 'junk?' })
  assert.equal(h.fake.stats.runs, 3)
  // /agent/run was called once per trial and never again. Only a run the service answered
  // with a run id gets its view read (once, no model); a 500 or a non-JSON body does not.
  assert.equal(h.fake.stats.stepReads, 1)
  assert.match(boom.error, /agent\/run failed: http 500/)
  assert.match(busy.error, /status_code 4001/)
  assert.match(junk.error, /agent\/run failed/)
  assert.equal(boom.steps, undefined)
  assert.equal(junk.steps, undefined)
  assert.equal(busy.statusMsg, 'busy')
})

test('a failed run keeps the service message and how the run ended, so unknown_tool is told from the other failures', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const failedView = (reason) => ({ status_code: 1000, run: { run_id: 'x', status: 'failed', termination_reason: reason, steps: [toolStep] } })
  const shared = 'Agent 达到安全执行边界，本次任务未完成'
  const h = await harness(t, [
    scenario('tool?', { body: { status_code: 5003, status_msg: shared }, stepsBody: failedView('unknown_tool') }),
    scenario('limit?', { body: { status_code: 5003, status_msg: shared }, stepsBody: failedView('model_call_limit') }),
    scenario('bare?', { body: { status_code: 5003 }, stepsBody: { status_code: 1000, run: { run_id: 'x', status: 'failed', steps: [] } } }),
  ])
  const unknownTool = await h.runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'tool?' })
  const limit = await h.runner.run({ arm: 'a', qid: 'q2', rep: 0, question: 'limit?' })
  const bare = await h.runner.run({ arm: 'a', qid: 'q3', rep: 0, question: 'bare?' })

  // Both look the same from the status code and message; the termination reason separates them.
  assert.equal(unknownTool.statusCode, 5003)
  assert.equal(unknownTool.statusMsg, shared)
  assert.equal(limit.statusMsg, shared)
  assert.equal(unknownTool.terminationReason, 'unknown_tool')
  assert.equal(limit.terminationReason, 'model_call_limit')
  assert.equal(unknownTool.runStatus, 'failed')
  assert.equal(bare.terminationReason, undefined)
  for (const record of [unknownTool, limit, bare]) assert.match(record.error, /status_code 5003/)
  // One /agent/run per trial, one read of the run view, no retry.
  assert.equal(h.fake.stats.runs, 3)
  assert.equal(h.fake.stats.stepReads, 3)
  assert.equal(h.ledger().filter((e) => e.phase === 'run').length, 3)
})

test('a stop file ends the run: nothing is sent while it exists, and the trial leaves no run in the ledger', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const stopFile = path.join(tmp(), 'STOP.json')
  t.after(() => fs.rmSync(path.dirname(stopFile), { recursive: true, force: true }))
  const h = await harness(t, [scenario('q?')], { stopFile })
  const first = await h.runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'q?' })
  assert.equal(first.error, undefined)
  assert.equal(h.fake.stats.runs, 1)

  fs.writeFileSync(stopFile, '{}')
  const sessionsBefore = h.fake.stats.sessions
  const stopped = await h.runner.run({ arm: 'a', qid: 'q2', rep: 0, question: 'q?' })
  assert.match(stopped.error, /refused: the stop file .* exists/)
  assert.equal(h.fake.stats.runs, 1, '/agent/run was not called')
  assert.equal(h.fake.stats.sessions, sessionsBefore, 'not even a session was opened')
  assert.equal(h.ledger().filter((e) => e.phase === 'run').length, 1)
  assert.equal(fs.existsSync(path.join(h.config.rawDir, 'a', 'q2.r0.json')), false, 'a refused trial writes no record')

  // Removing the file (a person decided) lets the run go on.
  fs.rmSync(stopFile)
  const resumed = await h.runner.run({ arm: 'a', qid: 'q3', rep: 0, question: 'q?' })
  assert.equal(resumed.error, undefined)
  assert.equal(h.fake.stats.runs, 2)
})

test('enableThinking is sent as given on every request, and left out when it is not given', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const on = await harness(t, [scenario('q?')], { enableThinking: true })
  const rec = await on.runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'q?' })
  await on.runner.run({ arm: 'a', qid: 'q1', rep: 1, question: 'q?' })
  assert.deepEqual(on.fake.stats.runBodies.map((b) => b.enableThinking), [true, true])
  assert.equal(rec.enableThinking, true)

  const off = await harness(t, [scenario('q?')], { enableThinking: false })
  const recOff = await off.runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'q?' })
  assert.equal(off.fake.stats.runBodies[0].enableThinking, false)
  assert.equal(recOff.enableThinking, false)

  const unset = await harness(t, [scenario('q?')])
  const recUnset = await unset.runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'q?' })
  assert.equal('enableThinking' in unset.fake.stats.runBodies[0], false, 'not sent at all')
  assert.equal(recUnset.enableThinking, null)

  // The environment variable is read as a boolean; anything else is refused before a request.
  assert.equal(resolveConfig({}, { GL_L23_ENABLE_THINKING: 'true' }).enableThinking, true)
  assert.equal(resolveConfig({}, { GL_L23_ENABLE_THINKING: 'false' }).enableThinking, false)
  assert.equal(resolveConfig({}, {}).enableThinking, undefined)
  const bad = await harness(t, [scenario('q?')], { enableThinking: 'maybe' })
  const refused = await bad.runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'q?' })
  assert.match(refused.error, /enableThinking must be true or false/)
  assert.equal(bad.fake.stats.runs, 0)
})

test('a timeout is kept as a record, once', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const h = await harness(t, [scenario('slow?', { body: good, delayMs: 800 })], { timeoutMs: 150 })
  const record = await h.runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'slow?' })
  assert.match(record.error, /provider exception/)
  assert.equal(h.fake.stats.runs, 1)
  assert.equal(h.ledger().filter((e) => e.phase === 'exception').length, 1)
  assert.ok(fs.existsSync(path.join(h.config.rawDir, 'a', 'q1.r0.json')))
})

test('an unreadable step list is kept beside the answer, not turned into a failed trial', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const h = await harness(t, [
    scenario('a?', { body: good, stepsHttp: 404 }),
    scenario('b?', { body: good, stepsBody: { status_code: 2009 } }),
  ])
  const notFound = await h.runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'a?' })
  const missing = await h.runner.run({ arm: 'a', qid: 'q2', rep: 0, question: 'b?' })
  assert.equal(notFound.error, undefined)
  assert.equal(notFound.answer, 'tides')
  assert.match(notFound.stepsError, /http 404/)
  assert.match(missing.stepsError, /status_code 2009/)
  assert.equal(notFound.steps, undefined)
})

test('a connection failure is a record and costs no run', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const h = await harness(t, [scenario('q?')])
  const dead = new TrialRunner(resolveConfig({ ...h.config, baseUrl: 'http://127.0.0.1:1' }, {}))
  const record = await dead.run({ arm: 'a', qid: 'q1', rep: 0, question: 'q?' })
  assert.match(record.error, /provider exception/)
  assert.equal(h.ledger().filter((e) => e.phase === 'run').length, 0)
})

// ---- what it refuses to start ------------------------------------------------

test('the environment must be named, and a fake run may only reach the loopback address', () => {
  const base = { baseUrl: 'http://127.0.0.1:9', maxRuns: 5, ledgerPath: path.join(os.tmpdir(), 'x', 'l.jsonl'), rawDir: path.join(os.tmpdir(), 'x', 'raw') }
  assert.match(configProblem(resolveConfig({ ...base }, {})), /GL_L23_ENV must be set/)
  assert.match(configProblem(resolveConfig({ ...base, env: 'prod' }, {})), /GL_L23_ENV must be set/)
  assert.match(configProblem(resolveConfig({ ...base, env: 'fake', baseUrl: 'https://api.example.com' }, {})), /loopback/)
  assert.match(configProblem(resolveConfig({ ...base, env: 'fake', baseUrl: 'http://10.0.0.5:80' }, {})), /loopback/)
  assert.match(configProblem(resolveConfig({ ...base, env: 'fake', baseUrl: 'nonsense' }, {})), /not a URL/)
  assert.match(configProblem(resolveConfig({ ...base, env: 'real', maxRuns: 0 }, {})), /above zero/)
  assert.match(configProblem(resolveConfig({ ...base, env: 'real', maxRuns: undefined }, {})), /above zero/) // the default budget is 0
})

test('a refused configuration sends nothing and writes nothing', async (t) => {
  const fake = await startFakeBackend({ scenarios: [scenario('q?')] })
  t.after(() => fake.close())
  const dir = tmp()
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const runner = new TrialRunner(resolveConfig({ baseUrl: fake.url, ledgerPath: path.join(dir, 'l.jsonl'), rawDir: path.join(dir, 'raw') }, {}))
  const record = await runner.run({ arm: 'a', qid: 'q1', rep: 0, question: 'q?' })
  assert.match(record.error, /^refused: /)
  assert.deepEqual(fake.stats.requests, [])
  assert.equal(fs.existsSync(path.join(dir, 'raw')), false)
})

test('the ledger and the raw directory may not sit inside a git working tree', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const repo = tmp()
  t.after(() => fs.rmSync(repo, { recursive: true, force: true }))
  fs.mkdirSync(path.join(repo, '.git'))
  const cfg = resolveConfig({ env: 'fake', baseUrl: 'http://127.0.0.1:9', maxRuns: 1, ledgerPath: path.join(repo, 'ledger.jsonl'), rawDir: path.join(tmp(), 'raw') }, {})
  assert.match(configProblem(cfg), /ledgerPath .* inside a git working tree/)
  const other = resolveConfig({ ...cfg, ledgerPath: path.join(tmp(), 'l.jsonl'), rawDir: path.join(repo, 'a', 'b', 'raw') }, {})
  assert.match(configProblem(other), /rawDir .* inside a git working tree/)
})

test('the Promptfoo provider wraps a trial and reports a failure as an error', async (t) => {
  if (skipIfTmpInRepo(t)) return
  const dir = tmp()
  const fake = await startFakeBackend({ scenarios: [scenario('q?'), scenario('bad?', { http: 500, body: {} })] })
  t.after(async () => {
    await fake.close()
    fs.rmSync(dir, { recursive: true, force: true })
  })
  const provider = new GopherLearnL23Provider({
    label: 'arm-x',
    config: { env: 'fake', baseUrl: fake.url, email: 'u', password: 'p', maxRuns: 9, ledgerPath: path.join(dir, 'l.jsonl'), rawDir: path.join(dir, 'raw') },
  })
  assert.equal(provider.id(), 'gopherlearn-l23:arm-x')
  const ok = await provider.callApi('ignored', { vars: { id: 'q1', question: 'q?' }, repeatIndex: 2 })
  assert.equal(ok.output, 'tides')
  assert.equal(ok.error, undefined)
  assert.deepEqual([ok.metadata.record.arm, ok.metadata.record.qid, ok.metadata.record.rep], ['arm-x', 'q1', 2])
  const bad = await provider.callApi('ignored', { vars: { id: 'q2', question: 'bad?' } })
  assert.match(bad.error, /agent\/run failed/)
  assert.equal(bad.metadata.record.rep, 0)
})
