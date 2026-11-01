// The Promptfoo face of the assertions.
//
// Promptfoo shows each trial as passed or failed, so this wrapper maps the four verdicts
// onto that: only `pass` passes, and the reason names the real verdict (`needs_judge`,
// `error`) so a reader of the Promptfoo table is not told something is wrong with an
// answer when the service was down. The authoritative scoring is the offline one
// (score.mjs): it reads the same raw record and calls the same functions, so the two
// cannot disagree about a trial.
//
// This file is CommonJS because Promptfoo loads assertion files as scripts; the real work
// is in ES modules and is imported on first use.

'use strict'

const path = require('node:path')
const { pathToFileURL } = require('node:url')

let loaded
async function load() {
  if (!loaded) {
    const lib = (name) => import(pathToFileURL(path.join(__dirname, '..', 'lib', name)).href)
    const [judge, corpus] = await Promise.all([lib('judge.mjs'), lib('corpus.mjs')])
    let upload = {}
    if (process.env.GL_L23_UPLOAD_NAMES) upload = JSON.parse(require('node:fs').readFileSync(process.env.GL_L23_UPLOAD_NAMES, 'utf8'))
    if (!process.env.GL_L23_CORPUS) throw new Error('GL_L23_CORPUS (the corpus manifest) is not set')
    loaded = { judgeTrial: judge.judgeTrial, corpus: corpus.loadCorpusManifest(process.env.GL_L23_CORPUS, upload).corpus }
  }
  return loaded
}

/**
 * Promptfoo `javascript` assertion. The question travels in `vars.q` as a JSON string
 * (Promptfoo expands array-valued variables into a matrix of test cases, so nothing is
 * passed as an array).
 */
async function judgeAssertion(output, context) {
  const record = context?.providerResponse?.metadata?.record
  if (!record) return { pass: false, score: 0, reason: '[error] the provider returned no record' }
  const { judgeTrial, corpus } = await load()
  const question = JSON.parse(context.vars.q)
  const outcome = await judgeTrial(record, question, { corpus, defaultMaxToolCalls: Number(process.env.GL_L23_DEFAULT_MAX_TOOL_CALLS ?? 3) })
  const parts = []
  let pass = true
  for (const layer of ['l2', 'l3']) {
    const result = outcome.layers[layer]
    if (!result) continue
    if (result.verdict !== 'pass') pass = false
    parts.push(`${layer}=${result.verdict}${result.class ? `:${result.class}` : ''}`)
  }
  return { pass, score: pass ? 1 : 0, reason: parts.join(' ') }
}

module.exports = { judgeAssertion }
