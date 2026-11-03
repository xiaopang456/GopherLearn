/**
 * How an agent answer's basis, and what the knowledge store had to say about it,
 * are worded for a learner.
 *
 * The server answers with three words of its own: the basis of the answer, the
 * state of the knowledge behind it, and the reason behind that state. Those words
 * are the server's vocabulary (`v2/common/rag/answer_policy.go`) and they are
 * correct there — but they are not words a learner can read. `grounded` tells a
 * learner nothing about the answer they are reading, and `invalid_citation`
 * tells them less than nothing: it names an internal distinction and reads like
 * a fault report. So the words are translated here, once, and the card asks for
 * the Chinese wording.
 *
 * This is the one table, and it lives in this file on purpose. The card and the
 * rule that decides whether the card is shown at all both read their wording from
 * here, so a basis cannot be called one thing in the card and another thing in
 * the rule that hides the card.
 *
 * A value this table does not know is never shown as it arrived. An unknown value
 * is a fact the server recorded and this client cannot name, and the honest
 * wording for that is a neutral one — not the raw word. A learner reading
 * `model` may well take it for a feature name, and a learner reading
 * `invalid_grounding` learns nothing at all.
 *
 * Three values mean "there is nothing to report" rather than "there is something
 * I cannot report": `not_required` (this question did not call for the knowledge
 * store), `none` (there is no reason behind the state) and `legacy_unspecified`
 * (an answer stored before the server kept these fields). Those produce no line
 * at all, because a line saying "there was nothing here" is noise on a card
 * that already says what the answer was based on.
 *
 * `search_not_performed` 这个取值必须显示出来，不能算进上面「无事可报」的那一组：
 * 它不是「没有可报告的东西」，而是「这一轮根本没有查过用户的资料，答案是模型
 * 自己的通用知识」——这是一条要如实告诉用户的事实。用户读到「资料里没有找到支撑
 * 这句话的内容」时，会以为资料被查过、确实没有；读到「这次没有查资料」时才知道
 * 资料这一轮压根没被打开过。两种情况用户下一步该做的事完全不同：前者只能接受
 * 资料里本来就没有，后者可以重新提问、重新上传资料再试一次。
 *
 * `not_required` 仍然不单独显示。它说的是「这次问题不需要资料」，而读者真正
 * 需要知道的是「为什么不需要」——那个理由由 reason 那一行说出来就够了，所以
 * 依据 + 说明两行已经讲清了整件事，多加一行只会重复。
 */

/** The answer's basis. The server's own list, in the server's own words. */
const ANSWER_BASIS_ZH = Object.freeze({
  conversation: '来自之前的对话',
  personal_knowledge: '来自你上传的资料',
  general_knowledge: '来自通用知识',
  refused: '没有作答',
})

/** The state of the knowledge behind the answer. */
const KNOWLEDGE_STATUS_ZH = Object.freeze({
  grounded: '答案能在你的资料里找到出处',
  no_evidence: '资料里没有找到支撑这句话的内容',
  unavailable: '这次没能查到资料',
  invalid_grounding: '引用和原文对不上',
})

/** Why the knowledge behind the answer is in that state. */
const KNOWLEDGE_REASON_ZH = Object.freeze({
  search_required: '这个问题需要先查资料',
  no_active_document: '目前没有可用的文档',
  no_candidate: '资料里没有匹配的段落',
  model_abstained: '模型没有给出答案',
  embedding_unavailable: '向量化服务暂时不可用',
  vector_unavailable: '向量检索暂时不可用',
  lexical_unavailable: '关键词检索暂时不可用',
  index_unavailable: '检索索引暂时不可用',
  document_metadata_unavailable: '文档信息读取失败',
  invalid_citation: '引用的位置对不上原文',
  missing_citation: '引用缺少位置信息',
  fallback_citation: '引用使用了降级位置',
  // The turn never looked at the learner's material; the answer is the
  // model's own general knowledge. It is told apart from `no_candidate`
  // ("the material was searched and had nothing") because a learner can act
  // on the two differently: ask again, or upload something, when nothing was
  // searched - but not when the material was searched and stayed silent.
  search_not_performed: '这次没有查资料',
})

/** The wording used for a value this table does not know. */
const UNNAMED_BASIS = '未说明'
const UNNAMED_STATUS = '情况未说明'
const UNNAMED_REASON = '原因未说明'

/**
 * The values that mean "there is nothing here to report".
 *
 * `not_required` and `none` are the server saying there was nothing. The third
 * one, `legacy_unspecified`, is what the server writes for an answer it stored
 * before it kept these fields (`answer_policy.go`: the answer basis and the
 * knowledge status both default to it). Such an answer has no basis recorded,
 * so it has no basis to show — and saying "未说明" about it would be inventing a
 * sentence about a fact nobody wrote down. It belongs with the other two.
 *
 * `search_not_performed` is not one of them: it names a fact the learner can
 * act on - the material was never opened, so asking again or uploading
 * something is worth doing - and the reason line beside it says so.
 */
const NOTHING_TO_REPORT = Object.freeze({
  answerBasis: new Set(['legacy_unspecified']),
  knowledgeStatus: new Set(['not_required', 'legacy_unspecified']),
  knowledgeReason: new Set(['none', 'legacy_unspecified']),
})

/**
 * The Chinese wording for one value, or `''` when the value says nothing.
 *
 * The lookup ignores case and surrounding spaces, the way the server resolves a
 * name: a value that arrived padded or capitalised is the same value. A value
 * the table does not know comes back as the neutral wording, never as itself.
 */
function wording(table, value, unnamed) {
  if (typeof value !== 'string') return ''
  const key = value.trim().toLowerCase()
  if (key === '') return ''
  return Object.hasOwn(table, key) ? table[key] : unnamed
}

/** The key a value stands for, the way the server resolves it. `''` when there is none. */
function keyOf(value) {
  if (typeof value !== 'string') return ''
  return value.trim().toLowerCase()
}

/** Is this value one of the ones that mean "there is nothing here to report"? */
function saysNothing(field, value) {
  const key = keyOf(value)
  if (key === '') return true
  return NOTHING_TO_REPORT[field].has(key)
}

/** How the answer was based. `''` when the server recorded nothing. */
export function answerBasisText(value) {
  if (saysNothing('answerBasis', value)) return ''
  return wording(ANSWER_BASIS_ZH, value, UNNAMED_BASIS)
}

/** What the knowledge behind the answer was like. `''` when there was none to report. */
export function knowledgeStatusText(value) {
  if (saysNothing('knowledgeStatus', value)) return ''
  return wording(KNOWLEDGE_STATUS_ZH, value, UNNAMED_STATUS)
}

/** Why the knowledge behind the answer was in that state. `''` when there was no reason. */
export function knowledgeReasonText(value) {
  if (saysNothing('knowledgeReason', value)) return ''
  return wording(KNOWLEDGE_REASON_ZH, value, UNNAMED_REASON)
}

/**
 * The lines of the learner-facing card, in the order they are read.
 *
 * The card is a card about one thing: where this answer came from, and what the
 * learner's own material had to say about it. A line exists for every field the
 * server filled in, and for no field it left empty or marked as having nothing
 * to report.
 */
export function agentResultRows(result) {
  const rows = []
  const basis = answerBasisText(result?.answerBasis)
  if (basis) rows.push({ key: 'answerBasis', label: '依据', text: basis })
  const status = knowledgeStatusText(result?.knowledgeStatus)
  if (status) rows.push({ key: 'knowledgeStatus', label: '资料', text: status })
  const reason = knowledgeReasonText(result?.knowledgeReason)
  if (reason) rows.push({ key: 'knowledgeReason', label: '说明', text: reason })
  return rows
}

/**
 * Whether the card has anything to show at all.
 *
 * When the server recorded none of the three fields, or recorded only "there was
 * nothing here", the card would open onto an empty box. That is not a card: it
 * is a heading about a fact that was never written down, so it is not shown.
 */
export function hasAgentResultContent(result) {
  return agentResultRows(result).length > 0
}
