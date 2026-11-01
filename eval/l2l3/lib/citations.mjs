// Citations against the corpus and against the gold evidence.
//
// Everything is counted in lines — (file, line) pairs — exactly as the retrieval
// evaluation counts them, so a citation and a retrieved chunk are judged by the same
// measure: lines of different files never mix, and an overlap between two citations
// counts once.

/**
 * The corpus as the citations see it.
 *
 * `files` maps a manifest file name to its line count. `uploadNames` maps the name the
 * product knows a document by (what it cites) back to the manifest name; without it the
 * two are the same.
 */
export function makeCorpus({ files, uploadNames = {} }) {
  const lines = new Map(Object.entries(files))
  return {
    lineCount: (file) => lines.get(file),
    resolve(citedName) {
      const mapped = Object.hasOwn(uploadNames, citedName) ? uploadNames[citedName] : citedName
      return lines.has(mapped) ? mapped : undefined
    },
  }
}

const isLine = (value) => Number.isInteger(value)

/** Each source with the manifest file it names and what is wrong with it. */
export function inspectSources(sources, corpus) {
  return (Array.isArray(sources) ? sources : []).map((source) => {
    const problems = []
    const cited = typeof source?.file_name === 'string' ? source.file_name : ''
    const file = corpus.resolve(cited)
    if (file === undefined) problems.push('citation_unknown_doc')
    const start = source?.start_line
    const end = source?.end_line
    if (!isLine(start) || !isLine(end) || start < 1 || end < start) {
      problems.push('citation_out_of_range')
    } else if (file !== undefined && end > corpus.lineCount(file)) {
      problems.push('citation_out_of_range')
    }
    return { cited, file, start, end, problems }
  })
}

/** Spans the gold comparison may use: a resolved file and whole-number lines. */
export function usableSpans(inspected) {
  return inspected
    .filter((s) => s.file !== undefined && isLine(s.start) && isLine(s.end) && s.start >= 1 && s.end >= s.start)
    .map((s) => ({ file: s.file, start: s.start, end: s.end }))
}

export function linesOf(spans) {
  const lines = new Set()
  for (const span of spans) {
    for (let n = span.start; n <= span.end; n += 1) lines.add(`${span.file}\u0000${n}`)
  }
  return lines
}

/**
 * The best evidence set the spans cover and by how much: the share of that set's
 * lines the spans contain. A set needs all of its spans, so covering one of two spans
 * is half. A tie goes to the set listed first.
 */
export function bestCoverage(goldSets, spans) {
  const retrieved = linesOf(spans)
  let best = { coverage: 0, index: -1 }
  goldSets.forEach((set, index) => {
    const gold = linesOf(set)
    if (gold.size === 0) return
    let covered = 0
    for (const line of gold) if (retrieved.has(line)) covered += 1
    const coverage = covered / gold.size
    if (best.index < 0 || coverage > best.coverage) best = { coverage, index }
  })
  return best
}

/** True when any span shares at least one line with any evidence set. */
export function sharesALine(goldSets, spans) {
  const retrieved = linesOf(spans)
  for (const set of goldSets) {
    for (const line of linesOf(set)) if (retrieved.has(line)) return true
  }
  return false
}
