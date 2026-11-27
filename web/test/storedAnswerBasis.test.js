import { describe, expect, it } from 'vitest'

import { normalizeMessage } from '../src/api/sessions.js'

/**
 * The basis of an answer has to survive a reload.
 *
 * The server keeps the basis on the message row, and the history read returns
 * it. Without that the "回答依据" card is a live-turn-only thing: a learner
 * who opens the conversation again sees an answer with no basis at all, and
 * the topic note they picked looks like it was never the source.
 */
describe('the basis a stored message carries', () => {
  it('is read back into the same shape the live turn carries', () => {
    const message = normalizeMessage({
      id: 'm-1',
      role: 'assistant',
      content: '现值看白板，旧值翻档案。',
      answer_basis: 'topic_memory',
      knowledge_status: 'not_used',
      knowledge_reason: 'not_cited',
      answer_basis_topics: ['分层摘要'],
    })

    expect(message.originalV2).toEqual({
      answerBasis: 'topic_memory',
      knowledgeStatus: 'not_used',
      knowledgeReason: 'not_cited',
      answerBasisTopics: ['分层摘要'],
    })
  })

  it('carries no subject when the server named none', () => {
    const message = normalizeMessage({
      id: 'm-2',
      role: 'assistant',
      content: '好。',
      answer_basis: 'general_knowledge',
      knowledge_status: 'not_required',
      knowledge_reason: 'search_not_performed',
    })

    expect(message.originalV2.answerBasis).toBe('general_knowledge')
    expect(message.originalV2.answerBasisTopics).toBeUndefined()
  })

  it('is absent on a row the server stored nothing for', () => {
    const message = normalizeMessage({ id: 'm-3', role: 'assistant', content: '好。' })
    expect(message.originalV2).toBeNull()
  })
})
