// Builders for the tests: a good record that each test then spoils in one way.

import { makeCorpus } from '../lib/citations.mjs'

export const corpus = makeCorpus({ files: { 'a.md': 100, 'b.md': 50, 'notes/c.md': 30 } })

/** A grounded, well-cited answer with one completed knowledge search. */
export function record(overrides = {}) {
  return {
    schema: 'record.v1',
    arm: 'arm-a',
    qid: 'q1',
    rep: 0,
    env: 'fake',
    http: 200,
    statusCode: 1000,
    replayed: false,
    runId: 'run-1',
    answer: 'an answer',
    answerBasis: 'personal_knowledge',
    knowledgeStatus: 'grounded',
    knowledgeReason: 'none',
    sources: [{ file_name: 'a.md', doc_id: 'doc-a', start_line: 10, end_line: 14, snippet: '', score: 0.9 }],
    toolCalls: 1,
    modelCalls: 2,
    steps: [
      { sequence: 1, step_type: 'plan', tool_name: '', duration_millis: 5, status: 'completed' },
      { sequence: 2, step_type: 'tool', tool_name: 'knowledge_search', duration_millis: 20, status: 'completed' },
    ],
    durationMs: 100,
    ...overrides,
  }
}

/** A record with the given tool steps (a name, or [name, status]) and a matching toolCalls. */
export function withTools(...tools) {
  const steps = [{ sequence: 1, step_type: 'plan', tool_name: '', duration_millis: 1, status: 'completed' }]
  tools.forEach((tool, index) => {
    const [name, status = 'completed'] = Array.isArray(tool) ? tool : [tool]
    steps.push({ sequence: index + 2, step_type: 'tool', tool_name: name, duration_millis: 10, status })
  })
  return record({ steps, toolCalls: tools.length })
}

export const gold = [[{ file: 'a.md', start: 10, end: 14 }]]

export function answerable(overrides = {}) {
  return {
    id: 'q1',
    text: 'a question',
    answerable: true,
    evidenceSets: gold,
    cluster: 'a.md',
    category: 'single_fact',
    label: null,
    layers: { l2: true, l3: false },
    ...overrides,
  }
}

export function label(overrides = {}) {
  return { id: 'q1', expect: [], forbid: [], max_tool_calls: 3, author: 'human', ...overrides }
}
