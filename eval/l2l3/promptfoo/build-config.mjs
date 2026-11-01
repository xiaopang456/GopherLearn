// Generate the Promptfoo configuration and the run's pin from a small run spec.
//
// Promptfoo is only the running shell, so the configuration is generated rather than
// written by hand: the same spec always yields the same file, the question set is the
// development split only, and the details that are easy to get wrong — an array-valued
// variable being expanded into extra test cases, a parallel run, a cache serving old
// answers — are fixed here once.
//
// A run spec (JSON):
//   { "env": "fake|real", "arms": [{ "label", "baseUrl", "knowledgePolicy"? }],
//     "repeat": 3, "maxRuns": 0, "ledgerPath", "rawDir",
//     "datasetPath", "labelsPath", "l3OnlyPath", "corpusManifest", "uploadNamesPath"? }

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

/** Load the questions of a run: the dev split only, so the sealed set cannot be asked by mistake. */
export function loadQuestions(spec) {
  const rows = spec.datasetPath ? readJsonl(spec.datasetPath) : []
  const sealed = rows.filter((r) => r.split !== 'dev')
  if (sealed.length > 0) throw new Error(`${spec.datasetPath} has ${sealed.length} question(s) that are not in the dev split (first: ${sealed[0].id}); this tool only runs the development set`)
  const l3Only = spec.l3OnlyPath ? readJsonl(spec.l3OnlyPath) : []
  const notDev = l3Only.filter((r) => r.split !== 'dev')
  if (notDev.length > 0) throw new Error(`${spec.l3OnlyPath} has question(s) outside the dev split (first: ${notDev[0].id})`)
  return buildQuestions({ datasetRows: rows, labels: spec.labelsPath ? readJsonl(spec.labelsPath) : [], l3OnlyRows: l3Only })
}

async function main(argv) {
  const [specPath, outDir] = argv
  if (!specPath || !outDir) throw new Error('usage: node build-config.mjs <run-spec.json> <output-dir>')
  const spec = JSON.parse(fs.readFileSync(specPath, 'utf8'))
  for (const name of ['ledgerPath', 'rawDir']) assertOutsideWorktree(name, spec[name])
  assertOutsideWorktree('output directory', outDir)
  const questions = loadQuestions(spec)
  const config = buildConfig(spec, questions)
  fs.mkdirSync(outDir, { recursive: true })
  fs.writeFileSync(path.join(outDir, 'promptfooconfig.json'), `${JSON.stringify(config, null, 2)}\n`)
  writePin(spec.rawDir, spec)
  console.log(`wrote ${config.tests.length} test(s) x ${config.providers.length} arm(s) x ${config.evaluateOptions.repeat} repeat(s) -> ${outDir}`)
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(`build-config: ${error.message}`)
    process.exit(1)
  })
}
