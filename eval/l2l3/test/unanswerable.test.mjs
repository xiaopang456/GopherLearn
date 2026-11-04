import test from 'node:test'
import assert from 'node:assert/strict'

import { unanswerableAssertion } from '../lib/unanswerable.mjs'
import { record } from './helpers.mjs'

const handle = (overrides) => unanswerableAssertion(record(overrides))

test('a stated "no evidence" is a structural pass, scored in the abstained tier', () => {
  const r = handle({ answerBasis: 'refused', knowledgeStatus: 'no_evidence', knowledgeReason: 'no_candidate', sources: [] })
  assert.equal(r.verdict, 'pass')
  assert.equal(r.tier, 'abstained')
})

test('a grounded answer with citations is left to the judge, never failed on structure', () => {
  // The trial's "grounded negative answer": honest, cited, and indistinguishable by state
  // from an irrelevant passage taken for the answer.
  const r = handle({})
  assert.equal(r.verdict, 'needs_judge')
  assert.equal(r.tier, 'pending')
  assert.equal(r.label, 'grounded_answer')
  assert.equal(r.cited, 1)
})

test('general knowledge after a stated "no evidence" is left to the judge too', () => {
  const r = handle({ answerBasis: 'general_knowledge', knowledgeStatus: 'no_evidence', knowledgeReason: 'no_candidate', sources: [] })
  assert.equal(r.verdict, 'needs_judge')
  assert.equal(r.label, 'disclosed_general_knowledge')
})

test('general knowledge without any search is left to the judge, and counted apart', () => {
  // The material was never consulted, which is a different fact from "the
  // material was searched and had nothing". The state still cannot tell
  // whether skipping the search was the right thing to do.
  const r = handle({ answerBasis: 'general_knowledge', knowledgeStatus: 'not_required', knowledgeReason: 'search_not_performed', sources: [] })
  assert.equal(r.verdict, 'needs_judge')
  assert.equal(r.tier, 'pending')
  assert.equal(r.label, 'general_knowledge_without_search')
})

test('"searched and found nothing" is still the disclosed-general-knowledge case', () => {
  // The new branch must not swallow the old one: a search that ran and found
  // nothing keeps its own label.
  const r = handle({ answerBasis: 'general_knowledge', knowledgeStatus: 'no_evidence', knowledgeReason: 'no_candidate', sources: [] })
  assert.equal(r.verdict, 'needs_judge')
  assert.equal(r.tier, 'pending')
  assert.equal(r.label, 'disclosed_general_knowledge')
})

test('"not required" without the no-search reason is not read as a no-search turn', () => {
  // The reason is what says the material was never consulted. On its own,
  // `not_required` must not borrow the new branch's verdict.
  const r = handle({ answerBasis: 'general_knowledge', knowledgeStatus: 'not_required', knowledgeReason: 'none', sources: [] })
  assert.equal(r.verdict, 'fail')
  assert.equal(r.label, 'unexpected_basis')
})

test('a service that could not answer is an error, not an abstention and not a fabrication', () => {
  for (const status of ['unavailable', 'invalid_grounding']) {
    const r = handle({ answerBasis: 'refused', knowledgeStatus: status, knowledgeReason: 'index_unavailable' })
    assert.equal(r.verdict, 'error', status)
    assert.equal(r.label, status)
  }
})

test('an unknown state value is an error about the record', () => {
  assert.equal(handle({ answerBasis: 'improvised' }).verdict, 'error')
  assert.equal(handle({ knowledgeReason: 'whim' }).label, 'invalid_state')
})

test('an outcome a knowledge question should not have is a failure of a named kind', () => {
  for (const overrides of [
    { answerBasis: 'conversation', knowledgeStatus: 'not_required' },
    { answerBasis: 'legacy_unspecified', knowledgeStatus: 'legacy_unspecified' },
    { answerBasis: 'personal_knowledge', knowledgeStatus: 'no_evidence' },
    { answerBasis: 'general_knowledge', knowledgeStatus: 'grounded' },
  ]) {
    const r = handle(overrides)
    assert.equal(r.verdict, 'fail', JSON.stringify(overrides))
    assert.equal(r.label, 'unexpected_basis')
  }
})

test('the verdict ignores the wording of the answer', () => {
  const refused = handle({ answerBasis: 'refused', knowledgeStatus: 'no_evidence', sources: [], answer: '根据现有文档没有相关内容' })
  const same = handle({ answerBasis: 'refused', knowledgeStatus: 'no_evidence', sources: [], answer: '' })
  assert.equal(refused.verdict, same.verdict)
  const grounded = handle({ answer: '我不知道' })
  assert.equal(grounded.verdict, 'needs_judge')
})
