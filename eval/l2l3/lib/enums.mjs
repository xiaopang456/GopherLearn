// The structural states a run reports, and the verdicts an assertion returns.
//
// The state values mirror v2/common/rag/answer_policy.go; a drift test reads that
// file and fails when the two disagree.

export const AnswerBasis = Object.freeze({
  Conversation: 'conversation',
  PersonalKnowledge: 'personal_knowledge',
  GeneralKnowledge: 'general_knowledge',
  Refused: 'refused',
  LegacyUnspecified: 'legacy_unspecified',
})

export const KnowledgeStatus = Object.freeze({
  NotRequired: 'not_required',
  Grounded: 'grounded',
  NoEvidence: 'no_evidence',
  Unavailable: 'unavailable',
  InvalidGrounding: 'invalid_grounding',
  LegacyUnspecified: 'legacy_unspecified',
})

export const KnowledgeReason = Object.freeze({
  None: 'none',
  SearchRequired: 'search_required',
  NoActiveDocument: 'no_active_document',
  NoCandidate: 'no_candidate',
  ModelAbstained: 'model_abstained',
  EmbeddingUnavailable: 'embedding_unavailable',
  VectorUnavailable: 'vector_unavailable',
  LexicalUnavailable: 'lexical_unavailable',
  IndexUnavailable: 'index_unavailable',
  DocumentMetadataUnavailable: 'document_metadata_unavailable',
  InvalidCitation: 'invalid_citation',
  MissingCitation: 'missing_citation',
  FallbackCitation: 'fallback_citation',
  // The turn answered without retrieving from the learner's material at all.
  // It is a different fact from `no_candidate` (a search ran and found
  // nothing), and the Go side names it the same way
  // (`answer_policy.go`: KnowledgeReasonSearchNotPerformed).
  SearchNotPerformed: 'search_not_performed',
})

export const KnowledgePolicy = Object.freeze({
  Preferred: 'knowledge_preferred',
  Only: 'knowledge_only',
})

/** `/agent/run` success code (v2/common/code: CodeSuccess). */
export const SUCCESS_CODE = 1000

/**
 * The four things an assertion can say.
 *
 * `needs_judge` is not a failure: the structural state cannot decide, and the
 * judging model will. `error` is not a failure either: the service was
 * unavailable or the record is incomplete, which says nothing about the answer. A
 * trial that wrongly counted an unparseable verdict as "not passed" produced five
 * false failures; keeping the two apart is the lesson.
 */
export const Verdict = Object.freeze({
  Pass: 'pass',
  Fail: 'fail',
  NeedsJudge: 'needs_judge',
  Error: 'error',
})

export function isValid(enumObject, value) {
  return Object.values(enumObject).includes(value)
}
