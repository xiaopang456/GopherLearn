import test from 'node:test'
import assert from 'node:assert/strict'

import {
  answerableAssertions,
  answeredFromKnowledge,
  citationCoverage,
  citationHitsGold,
  citationLegal,
  citationPresent,
  fallbackCitation,
  stateValid,
} from '../lib/answer.mjs'
import { makeCorpus } from '../lib/citations.mjs'
import { answerable, corpus, gold, record } from './helpers.mjs'

const src = (file_name, start_line, end_line) => ({ file_name, doc_id: 'd', start_line, end_line, snippet: '', score: 1 })

test('state_valid accepts the product enums and calls anything else an error, not a failure', () => {
  assert.equal(stateValid(record()).verdict, 'pass')
  for (const bad of [{ answerBasis: 'made_up' }, { knowledgeStatus: 'sort_of' }, { knowledgeReason: 'because' }, { answerBasis: undefined }]) {
    const r = stateValid(record(bad))
    assert.equal(r.verdict, 'error', JSON.stringify(bad))
    assert.equal(r.label, 'invalid_state')
  }
})

test('answered_from_knowledge names each way of not answering from knowledge', () => {
  const cases = [
    [{}, 'pass', undefined],
    [{ answerBasis: 'refused', knowledgeStatus: 'no_evidence' }, 'fail', 'refused'],
    [{ answerBasis: 'general_knowledge', knowledgeStatus: 'no_evidence' }, 'fail', 'general_knowledge'],
    [{ answerBasis: 'personal_knowledge', knowledgeStatus: 'no_evidence' }, 'fail', 'no_evidence'],
    [{ answerBasis: 'conversation', knowledgeStatus: 'not_required' }, 'fail', 'unexpected_basis'],
    [{ answerBasis: 'general_knowledge', knowledgeStatus: 'unavailable', knowledgeReason: 'vector_unavailable' }, 'error', 'unavailable'],
    [{ answerBasis: 'refused', knowledgeStatus: 'invalid_grounding', knowledgeReason: 'invalid_citation' }, 'error', 'invalid_grounding'],
  ]
  for (const [overrides, verdict, label] of cases) {
    const r = answeredFromKnowledge(record(overrides))
    assert.equal(r.verdict, verdict, JSON.stringify(overrides))
    assert.equal(r.label, label, JSON.stringify(overrides))
  }
})

test('citation_present needs at least one citation', () => {
  assert.equal(citationPresent(record()).verdict, 'pass')
  assert.equal(citationPresent(record({ sources: [] })).verdict, 'fail')
  assert.equal(citationPresent(record({ sources: undefined })).label, 'no_citation')
})

test('citation_legal: the document is in the corpus and the lines are inside it', () => {
  const ok = (sources) => citationLegal(record({ sources }), corpus)
  assert.equal(ok([src('a.md', 1, 100)]).verdict, 'pass')
  assert.equal(ok([src('a.md', 10, 14), src('notes/c.md', 30, 30)]).verdict, 'pass')

  const cases = [
    [[src('ghost.md', 1, 2)], 'citation_unknown_doc'],
    [[src('a.md', 1, 101)], 'citation_out_of_range'],
    [[src('a.md', 0, 5)], 'citation_out_of_range'],
    [[src('a.md', 9, 8)], 'citation_out_of_range'],
    [[src('a.md', 1.5, 3)], 'citation_out_of_range'],
    [[{ file_name: 'a.md', start_line: '3', end_line: '4' }], 'citation_out_of_range'],
    [[src('a.md', 10, 14), src('b.md', 49, 51)], 'citation_out_of_range'],
    // An unknown document outranks a bad range as the label, because the range cannot even be checked.
    [[src('ghost.md', 0, 1000)], 'citation_unknown_doc'],
  ]
  for (const [sources, label] of cases) {
    const r = ok(sources)
    assert.equal(r.verdict, 'fail', JSON.stringify(sources))
    assert.equal(r.label, label, JSON.stringify(sources))
  }
  assert.equal(ok([]).label, 'no_citation')
})

test('citation_legal maps the name the product knows a document by back to the corpus name', () => {
  const mapped = makeCorpus({ files: { 'docs/go/x.md': 40 }, uploadNames: { 'docs__go__x.md': 'docs/go/x.md' } })
  assert.equal(citationLegal(record({ sources: [src('docs__go__x.md', 1, 40)] }), mapped).verdict, 'pass')
  assert.equal(citationLegal(record({ sources: [src('docs__go__x.md', 1, 41)] }), mapped).label, 'citation_out_of_range')
  // The unmapped corpus name is not what the product cites, but it is still a corpus file.
  assert.equal(citationLegal(record({ sources: [src('docs/go/x.md', 1, 2)] }), mapped).verdict, 'pass')
})

test('citation_hits_gold: at least one shared line with any alternative evidence set', () => {
  const q = answerable({ evidenceSets: [[{ file: 'a.md', start: 10, end: 14 }], [{ file: 'b.md', start: 1, end: 2 }]] })
  const hits = (sources) => citationHitsGold(record({ sources }), q, corpus)
  assert.equal(hits([src('a.md', 14, 20)]).verdict, 'pass') // one shared line is enough
  assert.equal(hits([src('b.md', 2, 9)]).verdict, 'pass') // the second alternative counts
  assert.equal(hits([src('a.md', 15, 20)]).label, 'citation_miss')
  assert.equal(hits([src('b.md', 10, 14)]).label, 'citation_miss') // same line numbers, different file
  assert.equal(hits([src('ghost.md', 10, 14)]).label, 'citation_miss') // an unknown document cannot hit
})

test('citation_coverage is the best set covered, with every span of a set required', () => {
  const q = answerable({
    evidenceSets: [
      [{ file: 'a.md', start: 1, end: 2 }, { file: 'b.md', start: 1, end: 2 }],
      [{ file: 'notes/c.md', start: 5, end: 8 }],
    ],
  })
  const cover = (sources) => citationCoverage(record({ sources }), q, corpus).value
  assert.equal(cover([src('a.md', 1, 2)]), 0.5)
  assert.equal(cover([src('a.md', 1, 2), src('b.md', 1, 2)]), 1)
  assert.equal(cover([src('notes/c.md', 5, 6)]), 0.5)
  assert.equal(cover([src('a.md', 1, 2), src('notes/c.md', 5, 8)]), 1) // the second alternative is whole
  assert.equal(cover([src('a.md', 50, 60)]), 0)
  assert.equal(citationCoverage(record(), q, corpus).informational, true)
})

test('fallback_citation is a flag, not a verdict', () => {
  assert.equal(fallbackCitation(record()), null)
  const flagged = fallbackCitation(record({ knowledgeReason: 'fallback_citation' }))
  assert.equal(flagged.flag, true)
  assert.equal(flagged.informational, true)
})

test('answerableAssertions: a whole good answer passes, and each fault shows up on its own assertion', () => {
  const q = answerable()
  const verdicts = (r) => Object.fromEntries(answerableAssertions(r, q, corpus).map((a) => [a.name, a.verdict]))

  assert.deepEqual(verdicts(record()), {
    state_valid: 'pass',
    answered_from_knowledge: 'pass',
    citation_present: 'pass',
    citation_legal: 'pass',
    citation_hits_gold: 'pass',
    citation_coverage: 'pass',
  })
  const wrongDoc = verdicts(record({ sources: [src('b.md', 1, 5)] }))
  assert.equal(wrongDoc.citation_legal, 'pass')
  assert.equal(wrongDoc.citation_hits_gold, 'fail')
  const outOfRange = verdicts(record({ sources: [src('a.md', 10, 500)] }))
  assert.equal(outOfRange.citation_legal, 'fail')
  assert.equal(outOfRange.citation_hits_gold, 'pass')
  assert.equal(verdicts(record({ answerBasis: 'refused', knowledgeStatus: 'no_evidence', sources: [] })).answered_from_knowledge, 'fail')

  // With an unknown state value nothing else is judged: the record is the problem.
  assert.deepEqual(verdicts(record({ knowledgeStatus: 'sort_of' })), { state_valid: 'error' })
  assert.ok(gold.length === 1)
})
