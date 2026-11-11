import crypto from 'node:crypto'
import { foldText, normalizeValue } from './normalize.mjs'

export const categories = new Set(['early_fact', 'constraint', 'stale', 'no_fabrication', 'irrelevant'])
// The second round adds six categories under a schema version of their own.
//
//   A1  assistant artifact recall      the answer lives in an assistant message
//   A2  assistant proposal, changed     the new value is said by the user
//   A3  assistant proposal, refused     nothing was decided; the assistant only proposed
//   A4  assistant echo, not a second fact  the echo must not become its own value
//   P1  profile against this session    a seeded global memory conflicts with the session
//   F1  multi-turn follow-up            several questions in one scenario, each with its key
//
// They are accepted by name and by schema version, the way the draft
// categories R1..R6 are. Every rule of the first round still runs on them:
// the second round adds checks, it does not replace any.
export const secondRoundCategories = new Set(['A1', 'A2', 'A3', 'A4', 'P1', 'F1'])
export const secondRoundSchema = 'l4.scenario.draft.v4'
export function splitForId(id) {
  const n = crypto.createHash('sha256').update(id, 'utf8').digest().readBigUInt64BE()
  return n * 5n < 3n * (1n << 64n) ? 'dev' : 'sealed'
}
// factValueKey is how the answer key compares its own values: the same normalization
// the scorer applies before it looks for a mention. So "9道" and "九道" are one value
// here as well, and a key that accepts one of them cannot forbid the other.
function factValueKey(value) {
  return normalizeValue(value)
}

// The second round's own comparison of a value against a text uses foldText, the
// folding of normalize.mjs: the same folding the answer and the values of the
// answer key go through.
//
// It is that folding and not the scorer's normalization, because it is used to
// ask "is this value in this text", and the scorer's normalization answers a
// different question: it names a quantity canonically instead of folding a
// whole sentence. A value that is not an expression therefore has to be looked
// for literally in the folded text, which is what the second-round dataset was
// written against.

// The product's own token estimator, mirrored.
//
// The product counts CJK characters and punctuation one by one and ASCII
// words four characters to a token (v2/common/rag/token.go). The whitespace
// set is the Unicode White_Space property, which is what the product's
// unicode.IsSpace accepts, so the two agree on every character a dialogue
// can hold.
const WHITESPACE = new Set([
  '\t', '\n', '\v', '\f', '\r', ' ', '\u0085', '\u00a0', '\u1680',
  '\u2000', '\u2001', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006', '\u2007',
  '\u2008', '\u2009', '\u200a', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000',
])
export function estimateTokens(text) {
  const runes = [...String(text ?? '')]
  let tokens = 0
  for (let i = 0; i < runes.length;) {
    const r = runes[i]
    if (r === '\n') { tokens++; i++; continue }
    if (WHITESPACE.has(r)) {
      tokens++
      while (i < runes.length && runes[i] !== '\n' && WHITESPACE.has(runes[i])) i++
      continue
    }
    if (r <= '\x7f' && /[A-Za-z0-9_]/.test(r)) {
      const start = i
      while (i < runes.length && runes[i] <= '\x7f' && /[A-Za-z0-9_]/.test(runes[i])) i++
      tokens += Math.ceil((i - start) / 4)
      continue
    }
    tokens++; i++
  }
  return tokens
}

// tailTokens is the number of tokens between a turn and the end of the
// history. The rule is "at least 900 tokens from the key turn to the
// question", so this counts the key turn itself and everything after it, both
// messages of every turn.
export function tailTokens(turns, fromTurn) {
  let total = 0
  for (let index = fromTurn - 1; index < turns.length; index += 1) {
    total += estimateTokens(turns[index].user) + estimateTokens(turns[index].assistant)
  }
  return total
}

// A proposal the user refused must not be preceded by acceptance.
//
// The markers are phrases, not single characters: "好" alone occurs inside
// "好像" and "好久", and a marker that fires on a bare substring would
// report every ordinary sentence. They are written as the way a user
// actually accepts, with the punctuation that goes with them.
//
// The first list is checked in the turns right after the proposal, where the
// user's answer to the proposal lives. The second list is checked over the
// whole history: it is the wording that means acceptance wherever it
// appears, so a refusal cannot turn into a decision further down.
const ACCEPTANCE_NEARBY = [
  '好的', '好吧', '好，', '好。', '行，', '行。', '行吧', '可以，', '可以。',
  '就这样', '就这么定', '就这么办', '按你说的来', '按这个来', '就按', '同意', '没问题',
  '决定了', '就定', '采纳', '认可', '照你说的',
]
const ACCEPTANCE_STRONG = [
  '就这么定', '就这么办', '按你说的来', '同意了', '决定了', '就按这个来', '采纳', '认可', '照你说的',
]
const POLITE_REFUSALS = ['谢谢', '我想想', '再想想', '再看看', '先放一放', '再说', '回头', '我考虑', '先不']

export function acceptanceMarkersIn(text) {
  const found = []
  for (const marker of ACCEPTANCE_NEARBY) {
    if (text.includes(marker)) found.push(marker)
  }
  return found
}

export function strongAcceptanceMarkersIn(text) {
  const found = []
  for (const marker of ACCEPTANCE_STRONG) {
    // A marker inside a negation is not an acceptance: "我确实没认可" says the
    // opposite. The window before the marker decides.
    let at = text.indexOf(marker)
    while (at >= 0) {
      const before = text.slice(Math.max(0, at - 6), at)
      if (!/[没不未别无勿莫]/.test(before)) { found.push(marker); break }
      at = text.indexOf(marker, at + 1)
    }
  }
  return found
}

export function refusalMarkersIn(text) {
  const found = []
  for (const marker of POLITE_REFUSALS) {
    if (text.includes(marker)) found.push(marker)
  }
  return found
}

// The questions of a scenario, and the attempt each of them is.
//
// A scenario of the first round has one question and one answer key, and its
// attempt is the scenario. A scenario of the second round's F1 shape has
// several questions, each with its own key, and each question is scored on
// its own: the attempt of a question is the scenario's id with the question's
// number on it, "l4r2-...-f1-01#q2". The number is one based, so it reads the
// way the question is written.
export function isMultiQuestion(scenario) {
  return Array.isArray(scenario?.questions) && scenario.questions.length > 0
}
export function attemptIdOf(scenario, index) {
  if (!isMultiQuestion(scenario)) return scenario?.id
  return `${scenario?.id}#q${index + 1}`
}
export function scenarioQuestions(scenario) {
  const questions = Array.isArray(scenario?.questions) ? scenario.questions : []
  if (!questions.length) {
    return [{ index: 0, attempt: scenario?.id, question: scenario?.question, answer: scenario?.answer, turn: null }]
  }
  return questions.map((question, index) => ({
    index,
    attempt: attemptIdOf(scenario, index),
    question: question?.question,
    answer: question?.answer,
    // The turn is the round of the conversation the question was asked at. It
    // is what a record names its question by, so it travels with the question.
    turn: Number.isInteger(question?.turn) ? question.turn : null,
  }))
}
export function attemptIds(scenario) {
  return scenarioQuestions(scenario).map(question => question.attempt)
}
// scenarioIdOf is the scenario an attempt belongs to. An attempt of a
// multi-question scenario carries its question number; every other attempt is
// the scenario's own id.
export function scenarioIdOf(attempt) {
  const text = String(attempt ?? '')
  const at = text.lastIndexOf('#q')
  if (at < 0) return text
  const suffix = text.slice(at + 2)
  return /^\d+$/.test(suffix) ? text.slice(0, at) : text
}
// questionIndexOf is the question an attempt answers, counted from zero.
export function questionIndexOf(attempt) {
  const text = String(attempt ?? '')
  const at = text.lastIndexOf('#q')
  if (at < 0) return null
  const suffix = text.slice(at + 2)
  return /^\d+$/.test(suffix) ? Number(suffix) - 1 : null
}
// resolveAttempt is the question a record answers.
//
// A record of a multi-question scenario has to name its question, either by
// the attempt id or by the question index. A record that names a question
// that does not exist is not resolved: the caller turns that into an
// identity error rather than scoring it against the wrong key.
//
// The question index of a record is the turn the question was asked at, when
// the scenario says which turn that is. The turn is the better name: it says
// where in the conversation the question was asked, while the position in the
// list says only where in the file it was written down.
//
// A scenario that names the turns is read strictly by the turn. A record that
// carries the position instead is not resolved: it names a question of a
// scenario that names its turns, and the two readings are different facts. A
// position of zero is the first question of the list, and the first question
// of the list was asked at its turn, not at the zero-th round of the
// conversation - a record that carries the position has been written by
// something that did not read the scenario.
//
// A scenario that does not name the turns keeps the position, because the
// position is the only thing it offers.
export function resolveAttempt(scenario, record) {
  const list = scenarioQuestions(scenario)
  if (!isMultiQuestion(scenario)) {
    const only = list[0]
    if (record?.qid !== only.attempt) return null
    return only
  }
  const named = list.find(question => question.attempt === record?.qid)
  if (named) {
    if (Number.isInteger(record?.question_index) && !namesThisQuestion(named, record.question_index)) return null
    return named
  }
  if (record?.qid === scenario?.id) {
    const index = Number.isInteger(record?.question_index) ? record.question_index : 0
    if (namesTurns(list)) {
      // The scenario says which round each question was asked at, so the
      // record has to say the same. The position is not a second reading of
      // the turn: it is a different fact, and reading it as one would score
      // the answer against a key it does not belong to.
      return list.find(question => question.turn === index) ?? null
    }
    return list[index] ?? null
  }
  return null
}

// namesTurns says whether the scenario names the round each of its questions
// was asked at.
//
// A scenario that names the turns of some of its questions but not of others
// is read by the turn as well: a record of a question whose turn is known
// may only carry that turn, and the questions whose turn is not known keep
// the position.
function namesTurns(list) {
  return list.some(question => question.turn !== null)
}

// namesThisQuestion says whether a record's question index names the question
// it is asked about.
//
// The index is the turn of the question when the scenario names it, and the
// position in the list otherwise. When the scenario names the turn, the
// position is not accepted: the two are different facts, and a record that
// carries the position has not read the scenario.
function namesThisQuestion(question, index) {
  if (question.turn !== null) return index === question.turn
  return index === question.index
}

export function checkScenario(s) {
  return [...new Set([...checkOriginal(s), ...checkSecondRound(s)])]
}

// The original per-scenario rules, unchanged. Every scenario of every set
// still runs through them.
export function checkOriginal(s) {
  const errors = []
  if (!s || typeof s !== 'object') return ['scenario is not an object']
  if (typeof s.id !== 'string' || !/^[A-Za-z0-9_-]+$/.test(s.id)) errors.push('invalid id')
  const secondRound = s.schema_version === secondRoundSchema
  if (!categories.has(s.category) && !(s.schema_version === 'l4.scenario.draft.v3' && /^R[1-6]$/.test(s.category)) && !(secondRound && secondRoundCategories.has(s.category))) errors.push('unknown category')
  if (s.split !== splitForId(String(s.id))) errors.push('split differs from id hash')
  const hasQuestionList = secondRound && isMultiQuestion(s)
  if (typeof s.question !== 'string' || !s.question.trim()) {
    // A second-round F1 scenario carries its questions in a list, and each of
    // them has its own answer key, so there is no single question to check
    // here. The list is checked in the F1 block below.
    if (!hasQuestionList) errors.push('missing question')
  }
  if (!Array.isArray(s.turns) || s.turns.length < 1) errors.push('missing history')
  let last = -Infinity
  for (const t of Array.isArray(s.turns) ? s.turns : []) {
    if (!t || typeof t !== 'object') { errors.push('invalid turn'); continue }
    const u = Date.parse(t.user_at), a = Date.parse(t.assistant_at)
    if (typeof t.user !== 'string' || !t.user.trim() || typeof t.assistant !== 'string' || !t.assistant.trim()) errors.push('empty message')
    if (!Number.isFinite(u) || !Number.isFinite(a) || u <= last || a <= u) errors.push('invalid timestamps')
    last = a
  }
  if (!Array.isArray(s.evidence_turns) || s.evidence_turns.some(n => !Number.isInteger(n) || n < 1 || n > (s.turns?.length ?? 0))) errors.push('invalid evidence turns')
  // A second-round F1 scenario has no top-level answer: every question brings
  // its own key. The keys are checked in the F1 block below.
  if (!hasQuestionList) {
    if (!['fact', 'needs_judge'].includes(s.answer?.mode)) errors.push('unknown scoring mode')
    if (s.answer?.mode === 'fact' && (!Array.isArray(s.answer.accepted) || !s.answer.accepted.length || s.answer.accepted.some(v => typeof v !== 'string' || !v.trim()))) errors.push('missing accepted answers')
    for (const key of ['aliases', 'forbidden']) {
      const values = s.answer?.[key]
      if (values !== undefined && (!Array.isArray(values) || values.some(v => typeof v !== 'string' || !v.trim()))) errors.push(`invalid ${key}`)
    }
  }
  // An answer key that names a value both right and wrong cannot be scored: the
  // scorer would find the same mention acceptable and forbidden, and only the
  // order in which it inspects them would decide the attempt. Full-width digits
  // and spacing are the same value to the scorer, so they are the same value
  // here.
  const keys = list => (Array.isArray(list) ? list : []).map(factValueKey)
  const acceptedKeys = keys(s.answer?.accepted), forbiddenKeys = keys(s.answer?.forbidden), aliasKeys = keys(s.answer?.aliases)
  if (acceptedKeys.some(value => forbiddenKeys.includes(value))) errors.push('accepted value is also forbidden')
  if (aliasKeys.some(value => forbiddenKeys.includes(value))) errors.push('alias is also forbidden')
  if (s.category === 'irrelevant') {
    for (const name of ['control', 'distractor']) {
      const c = s.conditions?.[name]
      if (!c || !Number.isInteger(c.replace_turn) || c.replace_turn < 1 || c.replace_turn > (s.turns?.length ?? 0) || typeof c.user !== 'string' || !c.user.trim() || typeof c.assistant !== 'string' || !c.assistant.trim()) errors.push(`invalid ${name} condition`)
    }
    if (s.conditions?.control?.replace_turn !== s.conditions?.distractor?.replace_turn) errors.push('conditions replace different turns')
  }
  if (s.schema_version === 'l4.scenario.draft.v3') {
    const N = s.turns?.length ?? 0
    if (N < 60 || N > 110) errors.push('invalid v3 history length')
    const valid = n => Number.isInteger(n) && n >= 1 && n <= N
    if (s.category === 'R2' || s.category === 'R3') {
      if (!valid(s.original_turn) || !valid(s.superseded_turn) || s.original_turn <= 25 || s.original_turn > 40 || s.superseded_turn < 45 || s.superseded_turn > 70 || s.original_turn >= s.superseded_turn || N + 1 - s.original_turn < 20 || N + 1 - s.superseded_turn < 20) errors.push('invalid update turns')
      if (typeof s.superseded_value !== 'string' || !s.superseded_value.trim()) errors.push('invalid old value')
      if (s.distance_to_question !== N + 1 - s.superseded_turn) errors.push('invalid distance')
      if (!Array.isArray(s.evidence_turns) || !s.evidence_turns.includes(s.original_turn) || !s.evidence_turns.includes(s.superseded_turn)) errors.push('missing update evidence')
      const values = s.category === 'R2' ? s.answer?.forbidden : s.answer?.accepted
      if (!Array.isArray(values) || !values.includes(s.superseded_value)) errors.push('old value absent from answer key')
    } else {
      if (!valid(s.fact_turn) || s.distance_to_question !== N + 1 - s.fact_turn || (!Array.isArray(s.evidence_turns) || !s.evidence_turns.includes(s.fact_turn))) errors.push('invalid fact turn or distance')
      if (['R1', 'R5'].includes(s.category) && (s.fact_turn < 30 || s.fact_turn > 50 || s.distance_to_question < 20)) errors.push('invalid middle turn')
      if (s.category === 'R4' && ![2, 3].includes(s.distance_to_question)) errors.push('invalid tail turn')
      if (s.category === 'R6' && (s.fact_turn < 3 || s.fact_turn > 10)) errors.push('invalid early turn')
    }
    if (s.category === 'R5' ? s.answer?.mode !== 'needs_judge' : s.answer?.mode !== 'fact') errors.push('invalid category answer mode')
  }
  return [...new Set(errors)]
}

// The checks the second-round categories add.
//
// The rules that are new here, and why each exists:
//
//   * The answer key of an assistant-side category must not be reachable from
//     the user side. A1 asks what the assistant produced; if the user had
//     repeated the same words, the summary would hold that fact under the
//     user's authority and the question would not test the assistant path.
//   * A3 must carry no acceptance. "好"、"就这样"、"行" in the user's mouth
//     would turn the proposal into a decision.
//   * A2 and A4 need the old value and the new value to be in different
//     roles, or the change is not a change.
//   * Every key turn needs the tail-distance rule: the key turn must be at
//     least 900 tokens from the question, so it has already left the recent
//     window the product keeps verbatim.
//   * P1 needs the seeded memory to exist and to disagree with the session.
export function checkSecondRound(s) {
  const errors = []
  if (!s || typeof s !== 'object') return errors
  if (!secondRoundCategories.has(s.category)) return errors
  const N = s.turns?.length ?? 0
  const turns = Array.isArray(s.turns) ? s.turns : []
  const valid = n => Number.isInteger(n) && n >= 1 && n <= N
  const at = n => turns[n - 1] ?? { user: '', assistant: '' }
  const userAt = n => at(n).user ?? ''
  const assistantAt = n => at(n).assistant ?? ''
  const fold = foldText

  if (N < 60 || N > 110) errors.push('invalid history length')
  if (s.schema_version !== secondRoundSchema) errors.push('unexpected schema version')

  // Every key turn of every second-round scenario needs the tail distance.
  // The rule is the same for all of them: the key turn must have left the
  // window the product keeps verbatim before the question arrives.
  const keyTurns = s.key_turns ?? {}
  for (const [role, turn] of Object.entries(keyTurns)) {
    if (!valid(turn)) { errors.push(`invalid key turn ${role}`); continue }
    const tail = tailTokens(turns, turn)
    if (tail < 900) errors.push(`key turn ${role} has ${tail} tail tokens, needs 900`)
  }
  if (!Array.isArray(s.evidence_turns) || !s.evidence_turns.length) errors.push('missing evidence turns')
  for (const turn of s.evidence_turns ?? []) {
    if (!valid(turn)) errors.push('invalid evidence turn')
  }
  // Every key turn has to be evidence: the answer is only checkable against
  // the turns that carry it.
  for (const [role, turn] of Object.entries(keyTurns)) {
    if (valid(turn) && !(s.evidence_turns ?? []).includes(turn)) errors.push(`key turn ${role} is not evidence`)
  }

  // The question must not carry the answer. For the second-round set this
  // also covers the forbidden values, because a question that names the old
  // value hands the model one half of the comparison.
  const question = String(s.question ?? '')
  const values = [...(s.answer?.accepted ?? []), ...(s.answer?.forbidden ?? []), ...(s.answer?.aliases ?? [])]
  for (const value of values) {
    if (typeof value === 'string' && value.trim() && fold(question).includes(fold(value))) errors.push(`question carries the answer value ${value}`)
  }
  if (/\d/.test(question)) errors.push('question carries a number')

  // The entity name has to appear at the key turn, so a reader can tell
  // which object the question is about.
  const name = String(s.entity_name ?? '')
  if (name && Object.values(keyTurns).some(turn => valid(turn))) {
    const carriers = Object.values(keyTurns).filter(turn => valid(turn)).map(turn => `${userAt(turn)}\u0000${assistantAt(turn)}`).join('\u0000')
    if (!carriers.includes(name)) errors.push('entity name absent from the key turns')
  }
  // The name must not turn up anywhere else: another scenario's name in this
  // history would let the question be answered from the wrong one.
  for (const [index, turn] of turns.entries()) {
    if (name && `${turn.user}\u0000${turn.assistant}`.includes(name)) {
      const isKey = Object.values(keyTurns).includes(index + 1)
      if (!isKey) errors.push(`entity name occurs outside the key turns at turn ${index + 1}`)
    }
  }

  if (s.category === 'A1') {
    // The answer is the assistant's product, so the answer value must be in
    // the assistant's mouth at the key turn and in no user message at all.
    const artifactTurn = keyTurns.artifact_turn
    if (!valid(artifactTurn)) errors.push('A1 needs a valid artifact turn')
    else {
      const accepted = (s.answer?.accepted ?? []).map(fold)
      if (!accepted.length) errors.push('A1 needs an accepted value')
      for (const value of accepted) {
        if (!fold(assistantAt(artifactTurn)).includes(value)) {
          errors.push('A1 accepted value is not in the assistant message of the artifact turn')
        }
      }
      // The forbidden value is the other item of the same list: it must also
      // be in the assistant's list, so the question really has a choice.
      const forbidden = (s.answer?.forbidden ?? []).map(fold)
      for (const value of forbidden) {
        if (!fold(assistantAt(artifactTurn)).includes(value)) {
          errors.push('A1 forbidden value is not in the assistant message of the artifact turn')
        }
      }
      // The user never repeats it: if the user had said the same words, the
      // summary would hold it under the user's authority and the question
      // would not test the assistant path at all.
      for (const [index, turn] of turns.entries()) {
        const lower = fold(turn.user)
        for (const value of accepted) if (lower.includes(value)) errors.push(`A1 answer value is in a user message at turn ${index + 1}`)
        for (const value of forbidden) if (lower.includes(value)) errors.push(`A1 forbidden value is in a user message at turn ${index + 1}`)
      }
    }
  }

  if (s.category === 'A2') {
    // The assistant proposed, the user accepted, the user then changed it.
    // The new value is said by the user; the old value must not be, or the
    // change would be a paraphrase of the user's own statement.
    const proposal = keyTurns.proposal_turn
    const change = keyTurns.change_turn
    if (!valid(proposal) || !valid(change)) errors.push('A2 needs a valid proposal turn and change turn')
    else {
      if (proposal >= change) errors.push('A2 proposal turn must come before the change turn')
      const accepted = (s.answer?.accepted ?? []).map(fold)
      const forbidden = (s.answer?.forbidden ?? []).map(fold)
      const changeUser = fold(userAt(change))
      const changeBoth = fold(`${userAt(change)}\u0000${assistantAt(change)}`)
      for (const value of accepted) {
        if (!changeUser.includes(value)) errors.push('A2 accepted value is not said by the user at the change turn')
      }
      for (const value of forbidden) {
        // The old value was proposed by the assistant. It may be echoed by
        // the user when accepting, but the change turn must carry the new one.
        if (changeBoth.includes(value)) errors.push('A2 forbidden value appears at the change turn')
      }
      // The acceptance has to be there, otherwise there is nothing to change.
      const between = turns.slice(proposal, change).map(t => t.user).join('\n')
      if (!acceptanceMarkersIn(between).length) errors.push('A2 has no acceptance between the proposal and the change')
    }
  }

  if (s.category === 'A3') {
    // The assistant proposed; the user never accepted. Right after the
    // proposal there must be no acceptance, or the proposal would have become
    // a decision there and then. Further down the history the strong
    // acceptance wording is still forbidden, so the refusal cannot be undone
    // later either.
    const proposal = keyTurns.proposal_turn
    if (!valid(proposal)) errors.push('A3 needs a valid proposal turn')
    else {
      for (const [index, turn] of turns.entries()) {
        if (index + 1 <= proposal) continue
        const nearby = index + 1 <= proposal + 2
        const markers = nearby ? acceptanceMarkersIn(turn.user) : strongAcceptanceMarkersIn(turn.user)
        if (markers.length) {
          const where = nearby ? 'right after the proposal' : 'later in the history'
          errors.push(`A3 has an acceptance marker ${markers.join('/')} in a user message ${where} at turn ${index + 1}`)
        }
      }
      // A refusal has to be visible, otherwise the scenario does not test the
      // difference between "proposed" and "decided".
      const after = turns.slice(proposal - 1, proposal + 3).map(t => t.user).join('\n')
      if (!refusalMarkersIn(after).length) errors.push('A3 has no visible refusal after the proposal')
      // The answer key of a refusal cannot be a value: the right answer is
      // "nothing was decided", and the wrong one is a value.
      if (s.answer?.mode !== 'needs_judge') errors.push('A3 must be judged, not matched against a value')
    }
  }

  if (s.category === 'A4') {
    // The user said V1; the assistant repeated it; the user changed it to V2.
    // The echo must be in the assistant's message and must not carry a value
    // of its own: an echo that adds a number would be a second fact.
    const said = keyTurns.said_turn
    const echo = keyTurns.echo_turn
    const change = keyTurns.change_turn
    if (!valid(said) || !valid(echo) || !valid(change)) errors.push('A4 needs valid said, echo and change turns')
    else {
      if (!(said < echo && echo < change)) errors.push('A4 turns must be in the order said, echo, change')
      const accepted = (s.answer?.accepted ?? []).map(fold)
      const forbidden = (s.answer?.forbidden ?? []).map(fold)
      const changeUser = fold(userAt(change))
      for (const value of accepted) if (!changeUser.includes(value)) errors.push('A4 accepted value is not said by the user at the change turn')
      // The echo repeats the old value; it must not introduce the new one.
      const echoBoth = fold(`${userAt(echo)}\u0000${assistantAt(echo)}`)
      for (const value of accepted) if (echoBoth.includes(value)) errors.push('A4 echo carries the new value')
      // The old value was said by the user, and the echo repeats it: that is
      // the whole point of the shape.
      const saidBoth = fold(`${userAt(said)}\u0000${assistantAt(said)}`)
      for (const value of forbidden) if (!saidBoth.includes(value)) errors.push('A4 forbidden value is not said at the first turn')
    }
  }

  if (s.category === 'P1') {
    // A profile memory seeded before the session conflicts with what the
    // session says. The session has to win, so the seeded value must
    // disagree with the value the session states.
    const seeded = Array.isArray(s.seed_memories) ? s.seed_memories : []
    if (!seeded.length) errors.push('P1 needs at least one seeded memory')
    for (const memory of seeded) {
      if (!memory || typeof memory !== 'object') { errors.push('invalid seeded memory'); continue }
      if (memory.scope !== 'global') errors.push('a seeded profile memory must have the global scope')
      if (typeof memory.content !== 'string' || !memory.content.trim()) errors.push('a seeded memory needs content')
      if (typeof memory.subject !== 'string' || !memory.subject.trim()) errors.push('a seeded memory needs a subject')
      if (typeof memory.confirmed !== 'boolean') errors.push('a seeded memory needs a confirmed flag')
      if (!Number.isFinite(Date.parse(memory.saved_at))) errors.push('a seeded memory needs a save date')
    }
    const sessionTurn = keyTurns.session_turn
    if (!valid(sessionTurn)) errors.push('P1 needs a valid session turn')
    else {
      const accepted = (s.answer?.accepted ?? []).map(fold)
      const forbidden = (s.answer?.forbidden ?? []).map(fold)
      const sessionUser = fold(userAt(sessionTurn))
      for (const value of accepted) if (!sessionUser.includes(value)) errors.push('P1 accepted value is not said by the user in the session')
      // The session value must not be the seeded one, and the seeded one
      // must not appear in the session at all: the conflict has to be real.
      for (const value of accepted) if (forbidden.includes(value)) errors.push('P1 accepted and forbidden values are the same')
      const seededText = fold(seeded.map(m => `${m.subject}\u0000${m.content}`).join('\u0000'))
      for (const value of forbidden) if (!seededText.includes(value)) errors.push('P1 forbidden value is not in the seeded memory')
      for (const [index, turn] of turns.entries()) {
        const lower = fold(`${turn.user}\u0000${turn.assistant}`)
        for (const value of forbidden) if (lower.includes(value)) errors.push(`P1 seeded value appears in the history at turn ${index + 1}`)
      }
    }
  }

  if (s.category === 'F1') {
    // One scenario, several questions asked in a row, each with its own key.
    const questions = Array.isArray(s.questions) ? s.questions : []
    if (questions.length < 2 || questions.length > 3) errors.push('F1 needs two or three questions')
    for (const [index, question] of questions.entries()) {
      if (!question || typeof question !== 'object') { errors.push('invalid question'); continue }
      if (typeof question.question !== 'string' || !question.question.trim()) errors.push(`question ${index + 1} has no text`)
      if (!question.answer || typeof question.answer !== 'object') { errors.push(`question ${index + 1} has no answer key`); continue }
      if (!['fact', 'needs_judge'].includes(question.answer.mode)) errors.push(`question ${index + 1} has an unknown scoring mode`)
      if (question.answer.mode === 'fact' && (!Array.isArray(question.answer.accepted) || !question.answer.accepted.length)) errors.push(`question ${index + 1} has no accepted value`)
      // The turn is the round of the conversation the question was asked at.
      // It is what a record names its question by, and it is what says which
      // part of the conversation the answer has to come from.
      if (question.turn === undefined || question.turn === null) {
        errors.push(`question ${index + 1} does not name the turn it was asked at`)
      } else if (!valid(question.turn)) {
        errors.push(`question ${index + 1} names a turn that is not in the history: ${question.turn}`)
      } else if (!(s.evidence_turns ?? []).includes(question.turn)) {
        // The round the question was asked at is a round of evidence: what
        // the assistant said then is part of what the answer may rest on.
        errors.push(`question ${index + 1} is asked at turn ${question.turn}, which is not an evidence turn`)
      }
      // The questions are asked in the order of the conversation, so a later
      // question cannot be asked before an earlier one.
      if (index > 0) {
        const previous = questions[index - 1]
        if (Number.isInteger(previous?.turn) && Number.isInteger(question.turn) && question.turn <= previous.turn) {
          errors.push(`question ${index + 1} is asked at turn ${question.turn}, which is not after the turn of the question before it`)
        }
      }
      // A follow-up leans on the turn before it, so the answers of the
      // questions must not be the same value: otherwise the second question
      // would test nothing the first one did not.
      if (index > 0) {
        const previous = questions[index - 1].answer?.accepted ?? []
        const current = question.answer?.accepted ?? []
        if (previous.some(v => current.includes(v))) errors.push(`question ${index + 1} has the same answer as the one before it`)
      }
      if (/\d/.test(String(question.question ?? ''))) errors.push(`question ${index + 1} carries a number`)
      // The question must not name the answer: each question has to be
      // answerable from the history, not from its own wording.
      const own = [...(question.answer?.accepted ?? []), ...(question.answer?.forbidden ?? []), ...(question.answer?.aliases ?? [])]
      const lower = fold(String(question.question ?? ''))
      for (const value of own) {
        if (typeof value === 'string' && value.trim() && lower.includes(fold(value))) errors.push(`question ${index + 1} carries the answer value ${value}`)
      }
    }
    // The artifact the questions point at has to be far enough back too.
    if (!valid(keyTurns.artifact_turn)) errors.push('F1 needs a valid artifact turn')
  }

  return [...new Set(errors)]
}

export function materialize(s, condition = 'main') {
  const errors = checkScenario(s)
  if (errors.length) throw new Error(errors.join('; '))
  const turns = structuredClone(s.turns)
  if (s.category === 'irrelevant') {
    if (!['control', 'distractor'].includes(condition)) throw new Error('paired scenario requires a condition')
    const c = s.conditions[condition]
    turns[c.replace_turn - 1].user = c.user
    turns[c.replace_turn - 1].assistant = c.assistant
  } else if (condition !== 'main') throw new Error('unexpected condition')
  return turns
}
