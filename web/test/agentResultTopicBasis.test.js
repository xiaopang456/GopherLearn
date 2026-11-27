import { describe, expect, it } from 'vitest'

import {
  agentResultRows,
  answerBasisText,
  answerBasisTopics,
  knowledgeReasonText,
  knowledgeStatusText,
} from '../src/lib/agentResultText.js'

/**
 * A turn that rests on the learner's own confirmed topic note, while the
 * document retrieval ran and returned candidates the answer did not use.
 *
 * The two facts are separate on the server: the basis says what the answer
 * stands on, and the knowledge status says what the retrieval did. The card
 * has to show both, because "my note was not the source" and "your uploads
 * had nothing to say about it" are different facts that call for different
 * things from the learner.
 */
const TOPIC_ANSWER = {
  answerBasis: 'topic_memory',
  answerBasisTopics: ['分层摘要'],
  knowledgeStatus: 'not_used',
  knowledgeReason: 'not_cited',
}

describe('the topic-memory basis', () => {
  it('names the subject the answer came from', () => {
    expect(answerBasisText('topic_memory')).toBe('主题记忆')
    expect(answerBasisTopics(TOPIC_ANSWER)).toEqual(['分层摘要'])
  })

  it('shows the basis with the subject beside it', () => {
    const rows = agentResultRows(TOPIC_ANSWER)
    const basis = rows.find((row) => row.key === 'answerBasis')
    expect(basis.text).toBe('主题记忆 · 分层摘要')
  })

  it('shows the document side on its own lines', () => {
    // The retrieval ran. The answer did not come from it. That is not "the
    // material had nothing to say", and the card must not say so.
    expect(knowledgeStatusText('not_used')).toBe('这次回答没有用到你上传的资料')
    expect(knowledgeReasonText('not_cited')).toBe('答案依据的是你选的主题笔记，不是上传的资料')

    const rows = agentResultRows(TOPIC_ANSWER)
    expect(rows.map((row) => row.key)).toEqual(['answerBasis', 'knowledgeStatus', 'knowledgeReason'])
    expect(rows.find((row) => row.key === 'knowledgeStatus').text).toBe('这次回答没有用到你上传的资料')
  })

  it('falls back to the bare basis when the server named no subject', () => {
    const rows = agentResultRows({ answerBasis: 'topic_memory' })
    expect(rows.find((row) => row.key === 'answerBasis').text).toBe('主题记忆')
  })

  it('ignores an empty subject rather than printing a dangling separator', () => {
    expect(answerBasisTopics({ answerBasisTopics: ['', '  '] })).toEqual([])
    expect(answerBasisTopics({ answerBasisTopics: '分层摘要' })).toEqual([])
    expect(answerBasisTopics({})).toEqual([])
  })

  it('joins several subjects in the order the server named them', () => {
    const rows = agentResultRows({
      answerBasis: 'topic_memory',
      answerBasisTopics: ['分层摘要', '缓存'],
    })
    expect(rows.find((row) => row.key === 'answerBasis').text).toBe('主题记忆 · 分层摘要 / 缓存')
  })
})
