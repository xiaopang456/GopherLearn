// L2 assertion for a question the corpus cannot answer — decided from the structural
// state, never from the wording of the answer.
//
// A trial wrote "an unanswerable question must come back `refused`" as a hard rule and
// misjudged five of six answers: a grounded "the documents say there is no such thing"
// is an honest answer with a citation, and a general-knowledge supplement after a stated
// "this is not from your notes" is disclosed, not invented. The structural state cannot
// tell those from a fabrication, so it does not try: it answers only what it can see and
// hands the rest to the judging model as `needs_judge`.

import { AnswerBasis, KnowledgeStatus, Verdict } from './enums.mjs'
import { stateValid } from './answer.mjs'

const result = (name, verdict, reason, extra = {}) => ({ name, verdict, reason, ...extra })

/**
 * @returns the assertion result, with `tier` for the CRAG-style scoring:
 *   `abstained` (0) the system said it has no evidence — a structural, honest "I don't know";
 *   `pending` the state cannot tell honest from invented, so the judge decides.
 */
export function unanswerableAssertion(record) {
  const state = stateValid(record)
  if (state.verdict !== Verdict.Pass) return state

  const { answerBasis: basis, knowledgeStatus: status } = record
  const cited = Array.isArray(record.sources) ? record.sources.length : 0
  const name = 'unanswerable_handled'

  if (status === KnowledgeStatus.Unavailable || status === KnowledgeStatus.InvalidGrounding) {
    return result(name, Verdict.Error, `the service could not answer (${status}, ${record.knowledgeReason})`, { label: status })
  }
  if (basis === AnswerBasis.Refused) {
    return result(name, Verdict.Pass, 'the system stated it has no evidence', { tier: 'abstained', label: 'refused', cited })
  }
  if (basis === AnswerBasis.GeneralKnowledge && status === KnowledgeStatus.NoEvidence) {
    return result(name, Verdict.NeedsJudge, 'answered from general knowledge after stating there is no evidence; what follows the statement is unchecked', {
      tier: 'pending',
      label: 'disclosed_general_knowledge',
      cited,
    })
  }
  if (basis === AnswerBasis.PersonalKnowledge && status === KnowledgeStatus.Grounded) {
    return result(name, Verdict.NeedsJudge, 'grounded answer with citations; the state cannot tell an honest "there is none" from an irrelevant passage taken as the answer', {
      tier: 'pending',
      label: 'grounded_answer',
      cited,
    })
  }
  return result(name, Verdict.Fail, `answerBasis=${basis} knowledgeStatus=${status} is not an outcome a knowledge question should have`, {
    label: 'unexpected_basis',
    cited,
  })
}
