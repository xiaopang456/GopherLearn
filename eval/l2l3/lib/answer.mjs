// L2 assertions for a question that has an answer in the corpus.
//
// Every function is pure: (record, question, corpus) -> assertion result. A result is
// { name, verdict, label?, reason, ...numbers }. `label` names the failure class so a
// report can count refusals, general-knowledge answers and bad citations apart instead
// of lumping them into one "failed".

import {
  AnswerBasis,
  KnowledgeReason,
  KnowledgeStatus,
  Verdict,
  isValid,
} from './enums.mjs'
import { bestCoverage, inspectSources, sharesALine, usableSpans } from './citations.mjs'

const result = (name, verdict, reason, extra = {}) => ({ name, verdict, reason, ...extra })

/** The three states must be values the product can report at all. */
export function stateValid(record) {
  const bad = []
  if (!isValid(AnswerBasis, record.answerBasis)) bad.push(`answerBasis=${record.answerBasis}`)
  if (!isValid(KnowledgeStatus, record.knowledgeStatus)) bad.push(`knowledgeStatus=${record.knowledgeStatus}`)
  if (!isValid(KnowledgeReason, record.knowledgeReason)) bad.push(`knowledgeReason=${record.knowledgeReason}`)
  if (bad.length === 0) return result('state_valid', Verdict.Pass, 'all three states are known values')
  // An unknown state is a problem with the record or the build, not with the answer.
  return result('state_valid', Verdict.Error, `unknown state value(s): ${bad.join(', ')}`, { label: 'invalid_state' })
}

/**
 * The answer came from the learner's own knowledge: `personal_knowledge` and
 * `grounded`. Everything else is a failure of a named kind, except a service that
 * could not answer, which is an error.
 */
export function answeredFromKnowledge(record) {
  const { answerBasis: basis, knowledgeStatus: status } = record
  if (basis === AnswerBasis.PersonalKnowledge && status === KnowledgeStatus.Grounded) {
    return result('answered_from_knowledge', Verdict.Pass, 'personal_knowledge, grounded')
  }
  if (status === KnowledgeStatus.Unavailable) {
    return result('answered_from_knowledge', Verdict.Error, `knowledge was unavailable (${record.knowledgeReason})`, { label: 'unavailable' })
  }
  if (status === KnowledgeStatus.InvalidGrounding) {
    return result('answered_from_knowledge', Verdict.Error, `the grounding was invalid (${record.knowledgeReason})`, { label: 'invalid_grounding' })
  }
  let label = 'unexpected_basis'
  if (basis === AnswerBasis.Refused) label = 'refused'
  else if (basis === AnswerBasis.GeneralKnowledge) label = 'general_knowledge'
  else if (status === KnowledgeStatus.NoEvidence) label = 'no_evidence'
  return result('answered_from_knowledge', Verdict.Fail, `answerBasis=${basis} knowledgeStatus=${status}`, { label })
}

export function citationPresent(record) {
  const n = Array.isArray(record.sources) ? record.sources.length : 0
  return n >= 1
    ? result('citation_present', Verdict.Pass, `${n} citation(s)`)
    : result('citation_present', Verdict.Fail, 'the answer cites nothing', { label: 'no_citation' })
}

/** Every citation names a corpus document and a line range inside it. */
export function citationLegal(record, corpus) {
  const inspected = inspectSources(record.sources, corpus)
  const bad = inspected.filter((s) => s.problems.length > 0)
  if (inspected.length === 0) return result('citation_legal', Verdict.Fail, 'there is no citation to check', { label: 'no_citation' })
  if (bad.length === 0) return result('citation_legal', Verdict.Pass, `${inspected.length} citation(s) are inside the corpus`)
  const label = bad.some((s) => s.problems.includes('citation_unknown_doc')) ? 'citation_unknown_doc' : 'citation_out_of_range'
  const where = bad.map((s) => `${s.cited}:${s.start}-${s.end} (${s.problems.join(',')})`).join('; ')
  return result('citation_legal', Verdict.Fail, `illegal citation(s): ${where}`, { label })
}

/** A citation shares at least one line with any alternative evidence set. */
export function citationHitsGold(record, question, corpus) {
  const spans = usableSpans(inspectSources(record.sources, corpus))
  return sharesALine(question.evidenceSets, spans)
    ? result('citation_hits_gold', Verdict.Pass, 'a citation overlaps an evidence set')
    : result('citation_hits_gold', Verdict.Fail, 'no citation overlaps any evidence set', { label: 'citation_miss' })
}

/** How much of the best evidence set the citations contain. A number, not a verdict. */
export function citationCoverage(record, question, corpus) {
  const spans = usableSpans(inspectSources(record.sources, corpus))
  const { coverage } = bestCoverage(question.evidenceSets, spans)
  return result('citation_coverage', Verdict.Pass, `best evidence set covered to ${coverage.toFixed(3)}`, { value: coverage, informational: true })
}

/** `fallback_citation` is flagged, never failed: the citation was chosen by the server. */
export function fallbackCitation(record) {
  return record.knowledgeReason === KnowledgeReason.FallbackCitation
    ? result('fallback_citation', Verdict.Pass, 'the server chose the citation as a fallback', { flag: true, informational: true })
    : null
}

/**
 * Overall L2 verdict for an answerable question: it succeeds when it was answered
 * from knowledge and its citations are present, legal and on the gold evidence.
 */
export function answerableAssertions(record, question, corpus) {
  const state = stateValid(record)
  if (state.verdict !== Verdict.Pass) return [state]
  const out = [state, answeredFromKnowledge(record)]
  out.push(citationPresent(record))
  out.push(citationLegal(record, corpus))
  out.push(citationHitsGold(record, question, corpus))
  out.push(citationCoverage(record, question, corpus))
  const fallback = fallbackCitation(record)
  if (fallback) out.push(fallback)
  return out
}
