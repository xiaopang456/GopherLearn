// Generate the Promptfoo configuration and the run's pin from a small run spec.
//
// Promptfoo is only the running shell, so the configuration is generated rather than
// written by hand: the same spec always yields the same file, the question set is the
// development split unless the sealed split is asked for explicitly, and the details
// that are easy to get wrong — an array-valued variable being expanded into extra test
// cases, a parallel run, a cache serving old answers — are fixed here once.
//
// The sealed set is for milestones only. It is refused by default; a run over it needs
// `--split sealed` on the command line (or "split": "sealed" in the spec). A sealed run
// takes only a sealed question file — no L3 labels and no L3-only questions, so it
// reports the answer layer alone — and its pin (run.json) records that it was sealed,
// so a raw directory is never shared between a development run and a sealed one.
//
// A run spec (JSON):
//   { "env": "fake|real", "arms": [{ "label", "baseUrl", "knowledgePolicy"? }],
//     "repeat": 3, "maxRuns": 0, "ledgerPath", "rawDir",
//     "datasetPath", "labelsPath", "l3OnlyPath", "corpusManifest", "uploadNamesPath"?,
//     "split"?: "dev" (default) | "sealed" }

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import { buildQuestions, readJsonl } from '../lib/question.mjs'
import { assertOutsideWorktree } from '../lib/pathguard.mjs'
import { writePin } from '../score.mjs'

const here = path.dirname(fileURLToPath(import.meta.url))
const toFileUrl = (p) => `file://${p.replace(/\\/g, '/')}`

/** The tests: the dev split of the L1 questions that have a layer, plus the L3-only questions. */
export function buildTests(questions) {
  const tests = []
  for (const question of questions.values()) {
    if (!question.layers.l2 && !question.layers.l3) continue
    const layers = [question.layers.l2 ? 'L2' : null, question.layers.l3 ? 'L3' : null].filter(Boolean).join('+')
    tests.push({
      description: `${question.id} (${layers})`,
      vars: {
        id: question.id,
        question: question.text,
        // The whole question travels as one JSON string: an array-valued variable would be
        // expanded by Promptfoo into one test case per element.
        q: JSON.stringify(question),
      },
    })
  }
  return tests
}

export function buildConfig(spec, questions) {
  if (spec.env !== 'fake' && spec.env !== 'real') throw new Error('the run spec must name env: "fake" or "real"')
  if (!Array.isArray(spec.arms) || spec.arms.length === 0) throw new Error('the run spec needs at least one arm')
  const labels = spec.arms.map((a) => a.label)
  if (new Set(labels).size !== labels.length || labels.some((l) => !l)) throw new Error('arm labels must be present and distinct')
  if (spec.arms.some((a) => a.enableThinking !== undefined && typeof a.enableThinking !== 'boolean')) throw new Error('enableThinking of an arm must be true or false when given')
  const repeat = spec.repeat ?? 3
  if (!Number.isInteger(repeat) || repeat < 1) throw new Error('repeat must be a positive integer')

  return {
    description: `GopherLearn L2/L3 (${spec.env})`,
    sharing: false,
    prompts: ['{{question}}'],
    providers: spec.arms.map((arm) => ({
      id: toFileUrl(path.join(here, '..', 'provider', 'gl_provider.mjs')),
      label: arm.label,
      config: {
        label: arm.label,
        env: spec.env,
        baseUrl: arm.baseUrl,
        knowledgePolicy: arm.knowledgePolicy ?? 'knowledge_preferred',
        ...(arm.enableThinking === undefined ? {} : { enableThinking: arm.enableThinking }),
        maxRuns: spec.maxRuns ?? 0,
        ledgerPath: spec.ledgerPath,
        rawDir: spec.rawDir,
      },
    })),
    // One call at a time, no cache: a cached answer would be scored as a new trial, and
    // concurrent trials would share the budget guard's count.
    evaluateOptions: { maxConcurrency: 1, cache: false, repeat },
    defaultTest: {
      assert: [{ type: 'javascript', value: `${toFileUrl(path.join(here, 'assertions.cjs'))}:judgeAssertion`, metric: 'trial' }],
    },
    tests: buildTests(questions),
  }
}

export const SPLITS = ['dev', 'sealed']

/** The split a spec asks for; the development split unless it says otherwise. */
export function splitOf(spec) {
  const split = spec.split ?? 'dev'
  if (!SPLITS.includes(split)) throw new Error(`split must be "dev" or "sealed", got ${JSON.stringify(split)}`)
  return split
}

/**
 * Load the questions of a run. A development run takes only dev rows, so the sealed set
 * cannot be asked by mistake. A sealed run takes only sealed rows, and no L3 files.
 */
export function loadQuestions(spec) {
  const split = splitOf(spec)
  const rows = spec.datasetPath ? readJsonl(spec.datasetPath) : []
  if (split === 'sealed') {
    if (spec.labelsPath || spec.l3OnlyPath) throw new Error('a sealed run is for the answer layer only: remove labelsPath and l3OnlyPath from the spec')
    if (rows.length === 0) throw new Error('a sealed run needs a datasetPath with sealed questions')
    const notSealed = rows.filter((r) => r.split !== 'sealed')
    if (notSealed.length > 0) throw new Error(`${spec.datasetPath} has ${notSealed.length} question(s) that are not in the sealed split (first: ${notSealed[0].id}); a sealed run takes sealed questions only`)
    return buildQuestions({ datasetRows: rows, labels: [], l3OnlyRows: [] })
  }
  const sealed = rows.filter((r) => r.split !== 'dev')
  if (sealed.length > 0) throw new Error(`${spec.datasetPath} has ${sealed.length} question(s) that are not in the dev split (first: ${sealed[0].id}); a development run takes development questions only (the sealed set needs --split sealed)`)
  const l3Only = spec.l3OnlyPath ? readJsonl(spec.l3OnlyPath) : []
  const notDev = l3Only.filter((r) => r.split !== 'dev')
  if (notDev.length > 0) throw new Error(`${spec.l3OnlyPath} has question(s) outside the dev split (first: ${notDev[0].id})`)
  return buildQuestions({ datasetRows: rows, labels: spec.labelsPath ? readJsonl(spec.labelsPath) : [], l3OnlyRows: l3Only })
}

/** `[--split dev|sealed] <run-spec.json> <output-dir>` */
export function parseArgs(argv) {
  const positional = []
  let split
  for (let i = 0; i < argv.length; i += 1) {
    if (argv[i] === '--split') {
      if (split !== undefined) throw new Error('--split was given twice')
      split = argv[++i]
      if (split === undefined) throw new Error('--split needs a value: dev or sealed')
    } else if (argv[i].startsWith('--')) {
      throw new Error(`unknown option ${argv[i]}`)
    } else {
      positional.push(argv[i])
    }
  }
  return { specPath: positional[0], outDir: positional[1], split, extra: positional.slice(2) }
}

async function main(argv) {
  const { specPath, outDir, split, extra } = parseArgs(argv)
  if (!specPath || !outDir || extra.length > 0) throw new Error('usage: node build-config.mjs [--split dev|sealed] <run-spec.json> <output-dir>')
  const spec = JSON.parse(fs.readFileSync(specPath, 'utf8'))
  if (split !== undefined) {
    if (spec.split !== undefined && spec.split !== split) throw new Error(`--split ${split} disagrees with "split": ${JSON.stringify(spec.split)} in the spec`)
    spec.split = split
  }
  const runSplit = splitOf(spec)
  for (const name of ['ledgerPath', 'rawDir']) assertOutsideWorktree(name, spec[name])
  assertOutsideWorktree('output directory', outDir)
  const questions = loadQuestions(spec)
  const config = buildConfig(spec, questions)
  fs.mkdirSync(outDir, { recursive: true })
  fs.writeFileSync(path.join(outDir, 'promptfooconfig.json'), `${JSON.stringify(config, null, 2)}\n`)
  writePin(spec.rawDir, spec)
  if (runSplit === 'sealed') console.log('SEALED RUN: the sealed question set is used; run it at a milestone only and do not tune on its results')
  console.log(`wrote ${config.tests.length} ${runSplit} test(s) x ${config.providers.length} arm(s) x ${config.evaluateOptions.repeat} repeat(s) -> ${outDir}`)
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(`build-config: ${error.message}`)
    process.exit(1)
  })
}
