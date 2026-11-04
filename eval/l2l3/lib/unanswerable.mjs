// L2 assertion for a question the corpus cannot answer — decided from the structural
// state, never from the wording of the answer.
//
// A trial wrote "an unanswerable question must come back `refused`" as a hard rule and
// misjudged five of six answers: a grounded "the documents say there is no such thing"
// is an honest answer with a citation, and a general-knowledge supplement after a stated
// "this is not from your notes" is disclosed, not invented. The structural state cannot
// tell those from a fabrication, so it does not try: it answers only what it can see and
// hands the rest to the judging model as `needs_judge`.
//
// 「没查过」和「查了没有」是两种不同的事实，必须分开说：`no_evidence` 是查过了、
// 资料里没有支撑内容；`search_not_performed` 是这一轮根本没有检索过用户的资料，
// 答案用的是模型自己的通用知识。用户能据此做不同的事 —— 后者可以重新提问、
// 重新上传资料，前者只能接受「你的资料里确实没有」。所以这一轮没查过资料，
// 和查了资料没查到，必须落到不同的判定上，不能合并成一句「没有依据」。

import { AnswerBasis, KnowledgeReason, KnowledgeStatus, Verdict } from './enums.mjs'
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
  // A turn that answered from general knowledge without retrieving from the
  // material at all. The reason is what says the material was never consulted:
  // `not_required` on its own only says the store was not needed, and any
  // other reason means something else (a search that ran and found nothing,
  // a service that was down). So this branch requires all three of the
  // general basis, the not-required status and the `search_not_performed`
  // reason - without the reason the turn is not this case and must not be
  // counted as it.
  if (
    basis === AnswerBasis.GeneralKnowledge &&
    status === KnowledgeStatus.NotRequired &&
    record.knowledgeReason === KnowledgeReason.SearchNotPerformed
  ) {
    return result(name, Verdict.NeedsJudge, 'answered from general knowledge without searching the material at all; the state cannot tell whether skipping the search was right', {
      tier: 'pending',
      label: 'general_knowledge_without_search',
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
