import { describe, expect, it } from 'vitest'

import {
  agentResultRows,
  answerBasisText,
  knowledgeReasonText,
  knowledgeStatusText,
} from '../src/lib/agentResultText.js'

/**
 * A turn that answered from the model's own general knowledge without ever
 * opening the learner's material. The server tells the two facts apart
 * (`answer_policy.go`: KnowledgeReasonSearchNotPerformed), and the card has to
 * show them apart too: "the notes had nothing to say" and "the notes were
 * never looked at" call for different things from the learner.
 */
const NOT_SEARCHED = {
  answerBasis: 'general_knowledge',
  knowledgeStatus: 'not_required',
  knowledgeReason: 'search_not_performed',
}

describe('the answer-card wording', () => {
  it('says what a known state means', () => {
    expect(answerBasisText('personal_knowledge')).toBe('来自你上传的资料')
    expect(knowledgeStatusText('no_evidence')).toBe('资料里没有找到支撑这句话的内容')
    expect(knowledgeReasonText('no_candidate')).toBe('资料里没有匹配的段落')
  })

  it('names a turn that never searched the material, instead of hiding it', () => {
    // It is not "there is nothing to report": it is a fact the learner has to
    // be told, so it gets a sentence of its own rather than being swallowed
    // by the values that mean "nothing here".
    expect(knowledgeReasonText('search_not_performed')).toBe('这次没有查资料')
    expect(knowledgeReasonText('search_not_performed')).not.toBe('')
    // The values that really do mean "nothing here" still say nothing.
    expect(knowledgeReasonText('none')).toBe('')
    expect(knowledgeReasonText('legacy_unspecified')).toBe('')
  })

  it('shows the basis and the reason of a turn that never searched', () => {
    // Two lines, not three: "not_required" says the question did not call for
    // the material, and the reason beside it says why. A third line saying the
    // same thing in other words would be noise on a card that already says
    // what the answer was based on.
    const rows = agentResultRows(NOT_SEARCHED)
    expect(rows).toHaveLength(2)
    expect(rows.map((row) => row.key)).toEqual(['answerBasis', 'knowledgeReason'])
    expect(rows.find((row) => row.key === 'answerBasis').text).toBe('来自通用知识')
    expect(rows.find((row) => row.key === 'knowledgeReason').text).toBe('这次没有查资料')
  })

  it('says nothing at all about an answer the server recorded nothing for', () => {
    expect(agentResultRows({ answerBasis: 'legacy_unspecified', knowledgeStatus: 'legacy_unspecified', knowledgeReason: 'none' })).toEqual([])
    expect(agentResultRows({})).toEqual([])
  })
})
