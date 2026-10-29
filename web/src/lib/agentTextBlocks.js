/**
 * One agent turn's public text, kept as the blocks it was published in.
 *
 * A turn's text no longer arrives as one string: every model call publishes its own
 * fragment stream, and three frames say what is happening to it —
 *
 *   agent.text.started  a block opened, naming itself and the round it came from
 *   agent.text.delta    one fragment of that block, as the provider wrote it
 *   agent.text.ended    the block is over, and says which of three things it turned out to be
 *
 * The `kind` of a finished block is the whole point of the protocol, and it is a closed set:
 *
 *   process           the round asked for a tool. Its text is what the model said while it
 *                     decided — worth showing, and never the answer.
 *   answer_candidate  the round finished without asking for anything. This is the text the
 *                     server intends to store, but it is a *candidate* until
 *                     `message.completed` arrives and names this block as the answer.
 *   failed            the round's stream broke. Whatever was already written stays written,
 *                     labelled incomplete, and is never confirmed.
 *
 * So the confirmation rule is on the page, not in the naming: a block becomes the answer only
 * when the completed frame points at it, and the frame's `final_block_id` is the only thing
 * allowed to do that. A text frame is never a claim about identity either — the block id and
 * the round number correlate frames with each other, and neither is shown to a learner.
 *
 * Identity is bound the same way the step tracker does it: the first `run_id` seen owns the
 * turn, and a frame from another run is counted and dropped. And once the turn has reached a
 * terminal state the tracker is sealed: text that arrives on a socket the learner stopped
 * watching cannot grow a paragraph the page has already finished explaining.
 */

export const TextBlockPhase = {
  Started: 'agent.text.started',
  Delta: 'agent.text.delta',
  Ended: 'agent.text.ended',
}

/** The server's closed vocabulary for a finished block. Anything else is dropped, not guessed. */
export const TextBlockKind = {
  Process: 'process',
  Candidate: 'answer_candidate',
  Failed: 'failed',
}

const PHASES = new Set(Object.values(TextBlockPhase))
const KINDS = new Set(Object.values(TextBlockKind))

export function isTextBlockEventName(name) {
  return PHASES.has(name)
}

/**
 * Read one text frame's payload, or `null` when it is not a text frame this page understands.
 *
 * Only the keys named below become properties, so a payload that grew an extra field cannot
 * reach a component through here.
 */
export function toTextBlockEvent(frame) {
  const phase = frame && typeof frame.event === 'string' ? frame.event : ''
  if (!PHASES.has(phase)) return null
  let payload = {}
  try {
    payload = JSON.parse(frame.data || '{}')
  } catch {
    return null
  }
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) return null
  const blockId = typeof payload.block_id === 'string' ? payload.block_id.trim() : ''
  if (blockId === '') return null
  const round = Number.isInteger(payload.round) && payload.round > 0 ? payload.round : 0
  return {
    phase,
    blockId,
    round,
    runId: typeof payload.run_id === 'string' ? payload.run_id : '',
    content: typeof payload.content === 'string' ? payload.content : '',
    kind: (() => {
      const raw = typeof payload.kind === 'string' ? payload.kind : ''
      return KINDS.has(raw) ? raw : ''
    })(),
  }
}

/**
 * @param {string} clientRunId the local id of the request that owns this turn
 */
export function createBlockState({ clientRunId } = {}) {
  return {
    clientRunId: String(clientRunId ?? ''),
    serverRunId: '',
    /** In the order the server opened them, which is the order a learner read them. */
    blocks: [],
    /** The block `message.completed` confirmed as this turn's answer. */
    confirmedId: '',
    ignoredEvents: 0,
    /** Set once the turn reached a terminal state: no frame may edit the page after it. */
    sealed: false,
  }
}

function drop(state) {
  return { ...state, ignoredEvents: state.ignoredEvents + 1 }
}

function indexOfBlock(blocks, blockId) {
  for (let index = 0; index < blocks.length; index += 1) {
    if (blocks[index].blockId === blockId) return index
  }
  return -1
}

function cloneBlocks(state) {
  return state.blocks.map((block) => ({ ...block }))
}

/** Apply one text frame. Pure: every branch returns a new object. */
export function applyTextBlockEvent(state, event) {
  if (!event || typeof event !== 'object' || !PHASES.has(event.phase)) return drop(state)
  if (state.sealed) return drop(state)
  if (event.runId) {
    if (!state.serverRunId) state = { ...state, serverRunId: event.runId }
    else if (event.runId !== state.serverRunId) return drop(state)
  }
  const blocks = cloneBlocks(state)
  const existing = indexOfBlock(blocks, event.blockId)

  switch (event.phase) {
    case TextBlockPhase.Started: {
      if (existing >= 0) return drop(state)
      blocks.push({
        blockId: event.blockId,
        round: event.round,
        text: '',
        ended: false,
        kind: '',
      })
      return { ...state, blocks }
    }

    case TextBlockPhase.Delta: {
      if (existing >= 0) {
        // A block that already finished cannot receive text: the server said what it had to
        // say, and appending afterwards would edit a paragraph the learner has filed away.
        if (blocks[existing].ended || !event.content) return drop(state)
        blocks[existing] = { ...blocks[existing], text: blocks[existing].text + event.content }
        return { ...state, blocks }
      }
      // The opening frame is missing — a reconnect, or a server that started mid-stream. The
      // fragment is still the server's own text, so it is shown rather than discarded, in a
      // block opened on the evidence of the fragment itself.
      blocks.push({
        blockId: event.blockId,
        round: event.round,
        text: event.content,
        ended: false,
        kind: '',
      })
      return { ...state, blocks }
    }

    case TextBlockPhase.Ended: {
      if (existing < 0 || blocks[existing].ended) return drop(state)
      if (!KINDS.has(event.kind)) return drop(state)
      blocks[existing] = { ...blocks[existing], ended: true, kind: event.kind }
      return { ...state, blocks }
    }

    default:
      return drop(state)
  }
}

/**
 * Confirm the block `message.completed` names. A block that does not exist, never finished, or
 * ended as anything but a candidate cannot be promoted: the fold and the answer on screen are
 * decided here, so guessing would be the page inventing which paragraph it was answering.
 */
export function confirmAnswerBlock(state, blockId) {
  if (state.sealed) return drop(state)
  if (state.confirmedId === blockId && blockId !== '') return state
  if (state.confirmedId) return drop(state)
  const index = indexOfBlock(state.blocks, blockId)
  if (index < 0) return drop(state)
  const block = state.blocks[index]
  if (!block.ended || block.kind !== TextBlockKind.Candidate) return drop(state)
  const blocks = cloneBlocks(state)
  blocks[index] = { ...block, confirmed: true }
  return { ...state, blocks, confirmedId: blockId }
}

/**
 * Close the turn's unfinished blocks. A stream that ended without saying so — a break, a
 * cancel, a socket closed early — leaves text on screen that is not an answer, and this is what
 * labels it instead of letting it sit there looking finished.
 */
export function finishBlocks(state) {
  if (!state.blocks.some((block) => !block.ended)) return { ...state, sealed: true }
  const blocks = cloneBlocks(state).map((block) =>
    block.ended ? block : { ...block, ended: true, kind: TextBlockKind.Failed },
  )
  return { ...state, blocks, sealed: true }
}

/** No frame may edit the page after this. */
export function sealBlocks(state) {
  if (state.sealed) return state
  return finishBlocks(state)
}

function blockById(state, blockId) {
  const index = indexOfBlock(state.blocks, blockId)
  return index < 0 ? null : state.blocks[index]
}

/** The confirmed answer's text, or '' while nothing has been confirmed. */
export function confirmedText(state) {
  const block = blockById(state, state.confirmedId)
  return block ? block.text : ''
}

/** Every block's text in publication order — what the learner has watched being written. */
export function allText(state) {
  return state.blocks.map((block) => block.text).join('')
}

/** The rounds that were notes rather than answers, oldest first. */
export function processNotes(state) {
  return state.blocks
    .filter((block) => block.kind === TextBlockKind.Process && block.text !== '')
    .map((block) => ({ blockId: block.blockId, round: block.round, text: block.text }))
}

/** True while a block is still being written, which is what a "generating" label reads. */
export function isBuilding(state) {
  return state.blocks.some((block) => !block.ended)
}

/**
 * What the transcript entry for this turn shows.
 *
 * Before the completed frame, everything the model published is one growing body of text: the
 * learner watches one sentence after another arrive, and nothing has been withdrawn. After it,
 * the confirmed block is the answer and the notes become what they always were — the reasoning
 * the model showed on its way to a tool — so they move out of the body and into the fold. With
 * no blocks at all (a chat turn, or an agent turn on the legacy channel) this reports no text,
 * and the caller keeps using what `chatStream.js` accumulated.
 */
export function turnTranscript(state) {
  if (state.confirmedId) {
    return {
      content: confirmedText(state),
      processNotes: processNotes(state),
      confirmed: true,
    }
  }
  if (state.blocks.length === 0) return { content: '', processNotes: [], confirmed: false }
  // Nothing has been separated yet, so nothing is folded: showing a note beside a body that
  // still contains it would put the same sentence on the page twice.
  return { content: allText(state), processNotes: [], confirmed: false }
}
