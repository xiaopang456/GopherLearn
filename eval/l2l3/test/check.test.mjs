import test from 'node:test'
import assert from 'node:assert/strict'

import { checkLabels, productShortcut } from '../check.mjs'

const label = (id, over = {}) => ({ id, source: 'l3only', category: 'x', expect: [], forbid: [], max_tool_calls: 1, author: 'human', ...over })
const l3q = (id, question, over = {}) => ({ id, question, split: 'dev', l3_only: true, category: 'x', author: 'human', ...over })
const devq = (id, question = `dev ${id}`) => ({ id, question, answerable: true, split: 'dev', evidence_sets: [[{ file: 'a.md', start: 1, end: 2 }]] })

function balanced() {
  const labels = []
  const datasetRows = []
  const l3OnlyRows = []
  for (let i = 0; i < 3; i += 1) {
    labels.push(label(`d${i}`, { source: 'dev', expect: ['knowledge_search'] }))
    datasetRows.push(devq(`d${i}`))
    labels.push(label(`n${i}`))
    l3OnlyRows.push(l3q(`n${i}`, `plain ${i}`))
  }
  return { labels, datasetRows, l3OnlyRows }
}

test('a balanced, well-formed label set passes', () => {
  const result = checkLabels(balanced())
  assert.deepEqual(result.errors, [])
  assert.equal(result.summary.shareShouldSearch, 0.5)
  assert.equal(result.summary.labels, 6)
})

test('searching everything, or nothing, is a balance error', () => {
  const all = balanced()
  all.labels.forEach((l) => { l.expect = ['knowledge_search'] })
  assert.match(checkLabels(all).errors.join('\n'), /expect a knowledge search \(100%\)/)

  const none = balanced()
  none.labels.forEach((l) => { l.expect = [] })
  assert.match(checkLabels(none).errors.join('\n'), /\(0%\)/)

  // 4 of 10 and 6 of 10 are at the edge and allowed.
  const edge = { labels: [], datasetRows: [], l3OnlyRows: [] }
  for (let i = 0; i < 10; i += 1) {
    edge.labels.push(label(`e${i}`, { source: 'dev', expect: i < 6 ? ['knowledge_search'] : [] }))
    edge.datasetRows.push(devq(`e${i}`))
  }
  assert.deepEqual(checkLabels(edge).errors, [])
})

test('each way a label can be malformed is reported against its id', () => {
  const base = balanced()
  base.labels.push(label('bad-cap', { source: 'dev', expect: ['web_search'] }))
  base.datasetRows.push(devq('bad-cap'))
  base.labels.push(label('both', { source: 'dev', expect: ['document_read'], forbid: ['document_read'] }))
  base.datasetRows.push(devq('both'))
  base.labels.push(label('no-budget', { source: 'dev', max_tool_calls: -1 }))
  base.datasetRows.push(devq('no-budget'))
  const text = checkLabels(base).errors.join('\n')
  assert.match(text, /bad-cap: expect names unknown capability "web_search"/)
  assert.match(text, /both: document_read is both expected and forbidden/)
  assert.match(text, /no-budget: max_tool_calls is not a non-negative integer/)
})

test('a label must point at a question that exists, in the right file', () => {
  const r = balanced()
  r.labels.push(label('ghost-dev', { source: 'dev' }))
  r.labels.push(label('ghost-l3', { source: 'l3only' }))
  r.labels.push(label('d0', { source: 'dev' })) // a second label for an existing id
  r.labels.push(label('weird', { source: 'other' }))
  const text = checkLabels(r).errors.join('\n')
  assert.match(text, /ghost-dev: labelled as a dev question, but the dev question file has no such id/)
  assert.match(text, /ghost-l3: labelled as an L3-only question, but the L3-only file has no such id/)
  assert.match(text, /d0: two labels/)
  assert.match(text, /weird: source must be "dev" or "l3only"/)
})

test('a question written for L3 needs a label, the human author, and the l3_only flag', () => {
  const r = balanced()
  r.l3OnlyRows.push(l3q('orphan', 'an unlabelled question'))
  r.l3OnlyRows.push(l3q('n0', 'duplicate id check', { author: 'someone' }))
  r.l3OnlyRows[1].l3_only = false
  const text = checkLabels(r).errors.join('\n')
  assert.match(text, /orphan: an L3-only question with no label/)
  assert.match(text, /n0: a question written for L3 must say author "human"/)
  assert.match(text, /n1: l3_only must be true/)
})

test('wording the product answers by rule is an error for a new question, and a shortcut is a warning', () => {
  const r = balanced()
  r.labels.push(label('h', { source: 'l3only' }), label('k', { source: 'l3only' }), label('s', { source: 'l3only' }), label('g', { source: 'l3only' }))
  r.l3OnlyRows.push(
    l3q('h', '请把我当时怎么说的告诉我'),
    l3q('k', '只根据我的笔记回答这个问题'),
    l3q('s', '请记住我喜欢表格'),
    l3q('g', '你好'),
  )
  const result = checkLabels(r)
  assert.match(result.errors.join('\n'), /h: the wording "当时怎么说" makes the product answer by rule/)
  assert.match(result.errors.join('\n'), /k: the wording "只根据我的笔记"/)
  assert.match(result.warnings.join('\n'), /s: the product has a fixed path for this wording \(session_context\)/)
  assert.match(result.warnings.join('\n'), /g: .*\(direct_greeting\)/)
})

test('the same text under two ids is a warning', () => {
  const r = balanced()
  r.datasetRows.push(devq('dup', 'plain 0'))
  assert.match(checkLabels(r).warnings.join('\n'), /dup and n0 have the same text|n0 and dup have the same text/)
})

test('productShortcut recognises the fixed paths and ignores ordinary questions', () => {
  assert.equal(productShortcut('  Hello! '), 'direct_greeting')
  assert.equal(productShortcut('你好。'), 'direct_greeting')
  assert.equal(productShortcut('本会话里我问过什么'), 'session_context')
  assert.equal(productShortcut('How tall is a typical lighthouse?'), '')
  assert.equal(productShortcut('hello there, how does a mutex work'), '')
})
