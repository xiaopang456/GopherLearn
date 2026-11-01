// The scripts the end-to-end tests and the README demo play against the synthetic corpus
// in eval/testdata (three short documents, six questions). Two arms are scripted:
//
//   good    answers everything correctly, every time.
//   flawed  has one fault of every kind the assertions can tell apart — a wrong
//           citation, a refusal, a flaky third repeat, a transport failure, an
//           unanswerable question taken for answerable, a forbidden search, a missing
//           save, a tool under its other name.
//
// What each trial must come out as is worked out by hand in test/e2e.test.mjs.

const grounded = (sources, extra = {}) => ({
  answer: 'a grounded answer',
  answerBasis: 'personal_knowledge',
  knowledgeStatus: 'grounded',
  knowledgeReason: 'none',
  sources,
  toolCalls: 1,
  modelCalls: 2,
  contextBudget: { projectedInput: 1000 },
  ...extra,
})

const noEvidence = () => ({
  answer: 'there is nothing about that in your notes',
  answerBasis: 'refused',
  knowledgeStatus: 'no_evidence',
  knowledgeReason: 'no_candidate',
  sources: [],
  toolCalls: 1,
  modelCalls: 2,
  contextBudget: { projectedInput: 900 },
})

const conversation = (toolCalls) => ({
  answer: 'a conversational answer',
  answerBasis: 'conversation',
  knowledgeStatus: 'not_required',
  knowledgeReason: 'none',
  sources: [],
  toolCalls,
  modelCalls: 1,
  contextBudget: { projectedInput: 300 },
})

const cite = (file_name, start_line, end_line) => ({ file_name, doc_id: `doc-${file_name}`, start_line, end_line, snippet: '', score: 0.9 })
const plan = { sequence: 1, step_type: 'plan', tool_name: '', duration_millis: 4, status: 'completed' }
const tool = (name, status = 'completed', sequence = 2) => ({ sequence, step_type: 'tool', tool_name: name, duration_millis: 30, status })
const rep = (body, ...tools) => ({ body, steps: [plan, ...tools.map((t, i) => (typeof t === 'string' ? tool(t, 'completed', i + 2) : t))] })

export const QUESTIONS = {
  neap: 'How does the tidal range behave during neap tides?',
  bar: 'How long before high water must a keel boat cross the Grey Point bar?',
  wick: 'Why does a pointed wick blacken the chimney, and how is the chimney washed?',
  fuel: 'Which fuel does the lantern take, and when may it be topped up?',
  tungsten: 'What is the boiling point of tungsten?',
  chat: 'Tell me something cheerful.',
  mem: 'Please save my preference for tables.',
  hist: 'What did we discuss about tides last week?',
}

const TIDE = 'tide-tables.md'
const LANTERN = 'lantern-maintenance.md'

export function goodScript() {
  return {
    scenarios: [
      { question: QUESTIONS.neap, reps: [rep(grounded([cite(TIDE, 14, 16)]), 'knowledge_search')] },
      { question: QUESTIONS.bar, reps: [rep(grounded([cite(TIDE, 20, 21)]), 'knowledge_search')] },
      { question: QUESTIONS.wick, reps: [rep(grounded([cite(LANTERN, 8, 9), cite(LANTERN, 13, 14)], { toolCalls: 2 }), 'knowledge_search', 'document_read')] },
      { question: QUESTIONS.fuel, reps: [rep(grounded([cite(LANTERN, 16, 19)]), 'search_uploaded_documents')] },
      { question: QUESTIONS.tungsten, reps: [rep(noEvidence(), 'knowledge_search')] },
      { question: QUESTIONS.chat, reps: [rep(conversation(0))] },
      { question: QUESTIONS.mem, reps: [rep(conversation(1), 'propose_memory')] },
      { question: QUESTIONS.hist, reps: [rep(conversation(1), 'memory_search')] },
    ],
  }
}

export function flawedScript() {
  return {
    scenarios: [
      // A citation to the wrong document: L2 fails, L3 is fine.
      { question: QUESTIONS.neap, reps: [rep(grounded([cite(LANTERN, 1, 5)]), 'knowledge_search')] },
      // Refused although the answer is in the notes.
      { question: QUESTIONS.bar, reps: [rep(noEvidence(), 'knowledge_search')] },
      // Right twice, then the service fails once.
      {
        question: QUESTIONS.wick,
        reps: [
          rep(grounded([cite(LANTERN, 8, 9), cite(LANTERN, 13, 14)], { toolCalls: 2 }), 'knowledge_search', 'document_read'),
          rep(grounded([cite(LANTERN, 8, 9), cite(LANTERN, 13, 14)], { toolCalls: 2 }), 'knowledge_search', 'document_read'),
          { http: 500, body: { status_code: 4001 } },
        ],
      },
      // Right twice, then a citation past the end of the document (24 lines).
      {
        question: QUESTIONS.fuel,
        reps: [
          rep(grounded([cite(LANTERN, 16, 19)]), 'knowledge_search'),
          rep(grounded([cite(LANTERN, 16, 19)]), 'knowledge_search'),
          rep(grounded([cite(LANTERN, 20, 30)]), 'knowledge_search'),
        ],
      },
      // An unanswerable question answered from a passage that is not an answer.
      { question: QUESTIONS.tungsten, reps: [rep(grounded([cite(TIDE, 1, 3)]), 'knowledge_search')] },
      // Chit-chat that searched the notes.
      { question: QUESTIONS.chat, reps: [rep(conversation(1), 'knowledge_search')] },
      // Asked to save, saved nothing.
      { question: QUESTIONS.mem, reps: [rep(conversation(0))] },
      // The past-sessions search under its other name: still the right capability.
      { question: QUESTIONS.hist, reps: [rep(conversation(1), 'search_past_sessions')] },
    ],
  }
}
