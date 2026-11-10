// Offline scoring: raw records + questions -> outcomes.jsonl.
//
// It reads files and writes files. It calls no service and no model, so it can be run
// again and again, and the same inputs give the same bytes. A run is pinned to the
// question files and the corpus it was made against (run.json, written when the run's
// configuration is generated); scoring against anything else is refused, because the
// result would mix two question sets.

import fs from 'node:fs'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

import { loadCorpusManifest } from './lib/corpus.mjs'
import { judgeTrial } from './lib/judge.mjs'
import { buildQuestions, labelProblems, readJsonl, sha256File } from './lib/question.mjs'
import { RECORD_SCHEMA } from './lib/record.mjs'

export const OUTCOMES_META_SCHEMA = 'outcomes-meta.v1'

/** The hashes a run is pinned to: the question files and the corpus it is made against. */
export function computeHashes({ datasetPath, labelsPath, l3OnlyPath, corpusManifest }) {
  return {
    dataset_sha256: datasetPath ? sha256File(datasetPath) : null,
    labels_sha256: labelsPath ? sha256File(labelsPath) : null,
    l3_only_sha256: l3OnlyPath ? sha256File(l3OnlyPath) : null,
    corpus_hash: loadCorpusManifest(corpusManifest).hash,
  }
}

/**
 * Write run.json beside the raw records, when the run's configuration is generated. It
 * says which split the run is over, and a directory that already holds a run of the
 * other split is refused: the records of a development run and a sealed run never mix.
 */
export function writePin(rawDir, paths) {
  const split = paths.split ?? 'dev'
  const pinPath = path.join(rawDir, 'run.json')
  if (fs.existsSync(pinPath)) {
    const earlier = JSON.parse(fs.readFileSync(pinPath, 'utf8')).split ?? 'dev'
    if (earlier !== split) throw new Error(`${rawDir} already holds a ${earlier} run; a ${split} run needs its own directory`)
  }
  fs.mkdirSync(rawDir, { recursive: true })
  fs.writeFileSync(pinPath, `${JSON.stringify({ split, ...computeHashes(paths) }, null, 2)}\n`)
}

/** All raw records under `rawDir`, in a fixed order (arm, qid, rep). */
export function readRecords(rawDir) {
  const records = []
  for (const arm of fs.readdirSync(rawDir, { withFileTypes: true }).filter((d) => d.isDirectory()).map((d) => d.name).sort()) {
    for (const file of fs.readdirSync(path.join(rawDir, arm)).filter((f) => f.endsWith('.json')).sort()) {
      const record = JSON.parse(fs.readFileSync(path.join(rawDir, arm, file), 'utf8'))
      if (record.schema !== RECORD_SCHEMA) throw new Error(`${path.join(arm, file)}: schema is ${JSON.stringify(record.schema)}, want ${RECORD_SCHEMA}`)
      records.push(record)
    }
  }
  records.sort((a, b) => (a.arm < b.arm ? -1 : a.arm > b.arm ? 1 : a.qid < b.qid ? -1 : a.qid > b.qid ? 1 : a.rep - b.rep))
  return records
}

/**
 * @param opts.rawDir            directory of raw records (arm/qid.rN.json), plus run.json
 * @param opts.datasetPath       the L1 question file, or null for an L3-only run
 * @param opts.labelsPath        the L3 labels, or null
 * @param opts.l3OnlyPath        the L3-only questions, or null
 * @param opts.corpusManifest    the corpus manifest
 * @param opts.uploadNamesPath   optional JSON object: name the product cites -> manifest name
 * @param opts.defaultMaxToolCalls
 * @param opts.inspector         optional, for doc_constraint
 * @param opts.allowUnpinned     score a run that has no run.json (tests, ad-hoc use)
 */
export async function scoreRun(opts) {
  const uploadNames = opts.uploadNamesPath ? JSON.parse(fs.readFileSync(opts.uploadNamesPath, 'utf8')) : {}
  const corpus = loadCorpusManifest(opts.corpusManifest, uploadNames)

  const labels = opts.labelsPath ? readJsonl(opts.labelsPath) : []
  const problems = labels.flatMap((label) => labelProblems(label).map((p) => `${label.id ?? '?'}: ${p}`))
  if (problems.length > 0) throw new Error(`the L3 labels are not well formed:\n  ${problems.join('\n  ')}`)

  const hashes = computeHashes(opts)
  const pinPath = path.join(opts.rawDir, 'run.json')
  if (fs.existsSync(pinPath)) {
    const pin = JSON.parse(fs.readFileSync(pinPath, 'utf8'))
    for (const key of Object.keys(hashes)) {
      if ((pin[key] ?? null) !== hashes[key]) {
        throw new Error(`${key} changed since this run was made (run used ${String(pin[key]).slice(0, 12)}, the file is ${String(hashes[key]).slice(0, 12)}); scoring it would mix two question sets`)
      }
    }
  } else if (!opts.allowUnpinned) {
    throw new Error(`${pinPath} is missing, so the run is not pinned to a question set; pass allowUnpinned to score it anyway`)
  }

  const questions = buildQuestions({
    datasetRows: opts.datasetPath ? readJsonl(opts.datasetPath) : [],
    labels,
    l3OnlyRows: opts.l3OnlyPath ? readJsonl(opts.l3OnlyPath) : [],
  })

  const records = readRecords(opts.rawDir)
  const unknown = [...new Set(records.filter((r) => !questions.has(r.qid)).map((r) => r.qid))]
  if (unknown.length > 0) throw new Error(`records for question(s) no question file has: ${unknown.join(', ')}`)

  const outcomes = []
  for (const record of records) {
    const question = questions.get(record.qid)
    if (!question.layers.l2 && !question.layers.l3) continue // a question with neither a gold answer nor a label has nothing to judge
    outcomes.push(await judgeTrial(record, question, { corpus: corpus.corpus, defaultMaxToolCalls: opts.defaultMaxToolCalls, inspector: opts.inspector }))
  }

  const meta = {
    schema: OUTCOMES_META_SCHEMA,
    ...hashes,
    records: records.length,
    outcomes: outcomes.length,
    envs: [...new Set(records.map((r) => r.env))].sort(),
    arms: [...new Set(records.map((r) => r.arm))].sort(),
  }
  return { outcomes, meta }
}

/** Write outcomes.jsonl and outcomes.meta.json into `outDir`. The same run writes the same bytes. */
export function writeOutcomes(outDir, { outcomes, meta }) {
  fs.mkdirSync(outDir, { recursive: true })
  fs.writeFileSync(path.join(outDir, 'outcomes.jsonl'), outcomes.map((o) => JSON.stringify(o)).join('\n') + (outcomes.length ? '\n' : ''))
  fs.writeFileSync(path.join(outDir, 'outcomes.meta.json'), `${JSON.stringify(meta, null, 2)}\n`)
}

// ---- command line: node score.mjs --raw DIR --corpus FILE [--dataset F] [--labels F] [--l3-only F] [--upload-names F] --out DIR
async function main(argv) {
  const args = {}
  for (let i = 0; i < argv.length; i += 1) {
    if (!argv[i].startsWith('--')) throw new Error(`unexpected argument ${argv[i]}`)
    const key = argv[i].slice(2)
    args[key] = key === 'allow-unpinned' ? true : argv[++i]
  }
  for (const required of ['raw', 'corpus', 'out']) if (!args[required]) throw new Error(`--${required} is required`)
  const result = await scoreRun({
    rawDir: args.raw,
    corpusManifest: args.corpus,
    datasetPath: args.dataset,
    labelsPath: args.labels,
    l3OnlyPath: args['l3-only'],
    uploadNamesPath: args['upload-names'],
    defaultMaxToolCalls: args['default-max-tool-calls'] ? Number(args['default-max-tool-calls']) : 4,
    allowUnpinned: args['allow-unpinned'] === true,
  })
  writeOutcomes(args.out, result)
  console.log(`scored ${result.meta.records} record(s) into ${result.outcomes.length} outcome(s) -> ${args.out}`)
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(`score: ${error.message}`)
    process.exit(1)
  })
}
