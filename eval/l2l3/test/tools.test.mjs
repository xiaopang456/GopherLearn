import test from 'node:test'
import assert from 'node:assert/strict'

import { Capability, capabilityOf } from '../lib/capabilities.mjs'
import {
  docConstraint,
  toolAssertions,
  toolsBudget,
  toolsConsistency,
  toolsForbidden,
  toolsRequired,
} from '../lib/tools.mjs'
import { label, record, withTools } from './helpers.mjs'

test('names resolve to capabilities, whichever of the two namings the server used', () => {
  assert.equal(capabilityOf('knowledge_search'), Capability.KnowledgeSearch)
  assert.equal(capabilityOf('search_uploaded_documents'), Capability.KnowledgeSearch)
  assert.equal(capabilityOf('  Memory_Search '), Capability.EpisodeSearch)
  assert.equal(capabilityOf('search_past_sessions'), Capability.EpisodeSearch)
  assert.equal(capabilityOf('save_memory_draft'), Capability.MemoryDraft)
  assert.equal(capabilityOf('read_history_message'), Capability.HistoryRead)
  assert.equal(capabilityOf('some_future_tool'), Capability.Unknown)
  assert.equal(capabilityOf('constructor'), Capability.Unknown)
  assert.equal(capabilityOf(undefined), Capability.Unknown)
})

test('tools_required counts only a step that completed', () => {
  const need = label({ expect: ['knowledge_search'] })
  assert.equal(toolsRequired(withTools('knowledge_search'), need).verdict, 'pass')
  assert.equal(toolsRequired(withTools('search_uploaded_documents'), need).verdict, 'pass') // the other naming
  const failedOnly = toolsRequired(withTools(['knowledge_search', 'failed']), need)
  assert.equal(failedOnly.verdict, 'fail')
  assert.equal(failedOnly.label, 'missing_required_tool')
  assert.equal(toolsRequired(withTools(), need).verdict, 'fail')
  assert.equal(toolsRequired(withTools('document_read'), need).verdict, 'fail')
  assert.equal(toolsRequired(withTools(), label()).verdict, 'pass') // nothing required
})

test('tools_required wants every expected capability, in any order', () => {
  const need = label({ expect: ['knowledge_search', 'document_read'] })
  assert.equal(toolsRequired(withTools('document_read', 'knowledge_search'), need).verdict, 'pass')
  assert.equal(toolsRequired(withTools('knowledge_search'), need).missing[0], 'document_read')
})

test('tools_forbidden fails on any use, including a call that failed', () => {
  const ban = label({ forbid: ['knowledge_search', 'document_read'] })
  assert.equal(toolsForbidden(withTools(), ban).verdict, 'pass')
  assert.equal(toolsForbidden(withTools('memory_search'), ban).verdict, 'pass')
  assert.equal(toolsForbidden(withTools('knowledge_search'), ban).verdict, 'fail')
  const failedCall = toolsForbidden(withTools(['search_uploaded_documents', 'failed']), ban)
  assert.equal(failedCall.verdict, 'fail') // a failed call was still a call
  assert.equal(failedCall.label, 'forbidden_tool_used')
  assert.deepEqual(failedCall.used, ['knowledge_search'])
  assert.equal(toolsForbidden(withTools('brand_new_tool'), ban).verdict, 'pass') // unknown tools are neither for nor against
})

test('tools_budget compares the run\'s own tool count with the label, then the default', () => {
  assert.equal(toolsBudget(withTools('knowledge_search'), label({ max_tool_calls: 1 }), 3).verdict, 'pass')
  const over = toolsBudget(withTools('knowledge_search', 'document_read'), label({ max_tool_calls: 1 }), 3)
  assert.equal(over.verdict, 'fail')
  assert.equal(over.label, 'over_tool_budget')
  assert.equal(toolsBudget(withTools(), label({ max_tool_calls: 0 }), 3).verdict, 'pass')
  assert.equal(toolsBudget(withTools('knowledge_search'), label({ max_tool_calls: 0 }), 3).verdict, 'fail')
  assert.equal(toolsBudget(record({ toolCalls: 4 }), { expect: [], forbid: [] }, 3).verdict, 'fail') // falls back to the default
  assert.equal(toolsBudget(record({ toolCalls: 4 }), { expect: [], forbid: [] }, undefined).informational, true)
})

test('tools_consistency: the step list must agree with the run\'s own count, and be readable', () => {
  assert.equal(toolsConsistency(withTools('knowledge_search')).verdict, 'pass')
  const disagree = toolsConsistency(record({ toolCalls: 2 }))
  assert.equal(disagree.verdict, 'error')
  assert.equal(disagree.label, 'steps_disagree')
  const unreadable = toolsConsistency(record({ stepsError: 'http 404' }))
  assert.equal(unreadable.verdict, 'error')
  assert.equal(unreadable.label, 'steps_unreadable')
})

test('an unreliable step list stops the other tool checks: they would judge the wrong thing', async () => {
  const out = await toolAssertions(record({ toolCalls: 5 }), label({ expect: ['knowledge_search'] }), { defaultMaxToolCalls: 3 })
  assert.deepEqual(out.map((a) => a.name), ['tools_consistency'])
})

test('toolAssertions returns the full set in a fixed order', async () => {
  const out = await toolAssertions(withTools('knowledge_search'), label({ expect: ['knowledge_search'], forbid: ['memory_draft'] }), { defaultMaxToolCalls: 3 })
  assert.deepEqual(out.map((a) => [a.name, a.verdict]), [
    ['tools_consistency', 'pass'],
    ['tools_required', 'pass'],
    ['tools_forbidden', 'pass'],
    ['tools_budget', 'pass'],
  ])
})

test('doc_constraint: a document read must come from this turn\'s own search', async () => {
  const need = label({ doc_constraint: true })
  const inspector = (steps) => ({ inspect: async () => ({ steps }) })
  const good = inspector([
    { sequence: 2, capability: 'document_read', docIds: ['doc-1'] }, // listed first, ordered by sequence below
    { sequence: 1, capability: 'knowledge_search', docIds: ['doc-1', 'doc-2'] },
  ])
  assert.equal((await docConstraint(record(), need, good)).verdict, 'pass')

  const stray = await docConstraint(record(), need, inspector([
    { sequence: 1, capability: 'knowledge_search', docIds: ['doc-1'] },
    { sequence: 2, capability: 'document_read', docIds: ['doc-9'] },
  ]))
  assert.equal(stray.verdict, 'fail')
  assert.equal(stray.label, 'document_not_retrieved')

  // A read before any search has nothing to come from.
  const early = await docConstraint(record(), need, inspector([
    { sequence: 1, capability: 'document_read', docIds: ['doc-1'] },
    { sequence: 2, capability: 'knowledge_search', docIds: ['doc-1'] },
  ]))
  assert.equal(early.verdict, 'fail')
})

test('doc_constraint is not evaluated without an inspector, and an inspector that fails is an error', async () => {
  assert.equal(await docConstraint(record(), label(), undefined), null) // no constraint asked
  const skipped = await docConstraint(record(), label({ doc_constraint: true }), undefined)
  assert.equal(skipped.notEvaluated, true)
  assert.equal(skipped.informational, true)
  const broken = await docConstraint(record(), label({ doc_constraint: true }), { inspect: async () => { throw new Error('database is down') } })
  assert.equal(broken.verdict, 'error')
  assert.equal(broken.label, 'inspector_failed')
})
