import { computed, ref, watch } from 'vue'

import {
  createSession as apiCreateSession,
  deleteSession as apiDeleteSession,
  listMessages as apiListMessages,
  listSessions as apiListSessions,
  normalizeMessage,
  normalizeSession,
  readList,
} from '../api/sessions.js'
import { streamMessage as apiStreamMessage } from '../api/stream.js'
import { GENERIC_ERROR, isAbort, safeErrorMessage } from '../api/http.js'
import {
  abortRun,
  createRunState,
  failRun,
  isTerminalPhase,
  reduceRun,
  RunPhase,
  setPublishedText,
  toMessageStatus,
} from '../lib/chatStream.js'
import { localId } from '../lib/id.js'
import { deriveAgentStage } from '../lib/agentRun.js'
import {
  applyStepEvent,
  completeSteps,
  createStepState,
  finishSteps,
  StepStatus,
  stepRows,
} from '../lib/agentSteps.js'
import {
  applyTextBlockEvent,
  confirmAnswerBlock,
  createBlockState,
  finishBlocks,
  turnTranscript,
} from '../lib/agentTextBlocks.js'
import { useKnowledge } from './useKnowledge.js'
import {
  emptyKnowledgeDraft,
  isKnowledgeUsable,
} from '../lib/knowledge.js'
import {
  DEFAULT_SESSION_TITLES,
  isChatLike,
  isDefaultTitle,
  isKnownMode,
  isLearningAgent,
  SessionMode,
} from '../lib/modes.js'
import { validateMessageContent } from '../lib/validation.js'

function nowIso() {
  return new Date().toISOString()
}

/**
 * Owns everything the chat screen can do: the session list, the transcript, and
 * exactly one in-flight answer.
 *
 * Two identity ideas do the heavy lifting. Each answer gets a local
 * `clientRunId` whose only job is to mark which callback still owns the run, so
 * a cancelled request that keeps emitting cannot write into its successor. Each
 * history read gets a generation number, so opening A then B can never let A's
 * slow response replace B's transcript.
 *
 * The transcript itself is written optimistically (question + placeholder are
 * appended before the network call) and the placeholder is addressed by its
 * local id for the whole run. Nothing here rewrites a message other than the
 * current placeholder, which is what keeps a delayed or duplicated event from
 * scrambling the conversation.
 */
export function useChat() {
  const sessions = ref([])
  const activeId = ref('')
  const messages = ref([])
  const draft = ref('')

  const sessionsLoading = ref(false)
  const sessionsError = ref('')
  const historyLoading = ref(false)
  const historyError = ref('')
  const sendError = ref('')

  const run = ref(null)
  let streamController = null
  let creating = false
  const pendingCreate = ref(null)
  const pendingSend = ref(null)
  const retryableSend = computed(() => !!pendingSend.value && !streaming.value
    && pendingSend.value.sessionId === activeId.value
    && run.value && [RunPhase.Failed, RunPhase.Aborted].includes(run.value.phase))

  // History requests are keyed by a generation counter plus their own
  // AbortController. Whichever `openSession`/`closeSession`/`send` call runs
  // last owns the transcript, so a slow response for A can never land on top of
  // B — see `loadHistory`.
  let historyGeneration = 0
  let historyController = null

  const streaming = computed(() => !!run.value && !isTerminalPhase(run.value.phase))
  const ready = computed(() => !historyLoading.value)
  const activeSession = computed(() =>
    sessions.value.find((item) => item.id === activeId.value) ?? null,
  )

  /**
   * Collections come from the shared knowledge store, never a local copy: the
   * management page and this composer have to agree on which ids exist, or the
   * composer can send one the user just deleted.
   */
  const { collections, collectionIds } = useKnowledge()

  // Per-session, in-memory only: a half-finished toggle on session A must not
  // follow the user into session B, and nothing here survives a reload.
  const knowledgeDrafts = ref({})

  watch(collectionIds, (ids) => {
    // A collection the user picked may have disappeared from the list; the
    // draft has to lose it rather than keep sending a dead id. With nothing
    // left to choose, the toggle goes back off to match its disabled state.
    const known = new Set(ids)
    let changed = false
    const next = {}
    for (const [id, draft] of Object.entries(knowledgeDrafts.value)) {
      const kept = draft.selectedCollectionIds.filter((value) => known.has(value))
      if (!ids.length && !draft.enabled) {
        next[id] = draft
        continue
      }
      if (kept.length === draft.selectedCollectionIds.length && ids.length > 0) {
        next[id] = draft
        continue
      }
      changed = true
      next[id] =
        kept.length > 0 && draft.enabled && ids.length > 0
          ? { enabled: true, selectedCollectionIds: kept }
          : emptyKnowledgeDraft()
    }
    if (changed) knowledgeDrafts.value = next
  })

  /** The server's own answer, never the button the user last pressed. */
  const activeMode = computed(() => activeSession.value?.mode ?? '')
  const isChatSession = computed(() => isChatLike(activeMode.value))
  const isAgentSession = computed(() => isLearningAgent(activeMode.value))

  /**
   * Learning Agent turn state. Scoped to this composable instance on purpose —
   * like the per-session knowledge drafts, it is destroyed when the chat screen
   * unmounts (which every sign-out route transition causes), so it cannot be
   * carried into the next account either.
   *
   * `agentStage` is a projection of `run`, so there is no second copy of the
   * truth to fall out of sync. `agentPanelOpen` is the only piece of genuine
   * local state, and `resetAgentState` clears it at every session boundary.
   */
  const agentPanelOpen = ref(false)
  const agentStage = computed(() => deriveAgentStage(run.value))

  /**
   * The turn's progress frames, kept beside the answer run and never inside it.
   *
   * Two separate state machines is the design, not duplication: `chatStream.js` owns what
   * the answer is, this owns what the agent was doing while it became that. They share
   * one identity on purpose — the server puts the same `run_id` on `message.started` and
   * on every step of a turn — but a step frame must not pass through the answer reducer,
   * where binding that id would make the real deltas look foreign.
   *
   * Read through `run`, so a session boundary that drops the run also drops the steps:
   * there is no second place to remember to clear. Cancel keeps them (the run survives as
   * aborted) because "本轮已取消" is a fact about the steps too.
   */
  const stepsByRun = ref(createStepState())

  const agentSteps = computed(() => (run.value === null ? [] : stepRows(stepsByRun.value.state)))

  /** Move the open steps to match where the answer run ended. Idempotent. */
  function syncStepsWithRun(state) {
    if (!state) return
    if (state.phase === RunPhase.Completed) {
      stepsByRun.value = { ...stepsByRun.value, state: completeSteps(stepsByRun.value.state) }
      return
    }
    if (state.phase === RunPhase.Failed) {
      stepsByRun.value = {
        ...stepsByRun.value,
        state: finishSteps(stepsByRun.value.state, StepStatus.Failed),
      }
      return
    }
    if (state.phase === RunPhase.Aborted) {
      stepsByRun.value = {
        ...stepsByRun.value,
        state: finishSteps(stepsByRun.value.state, StepStatus.Cancelled),
      }
    }
  }

  /**
   * The turn's published text, held as the blocks it arrived in.
   *
   * A third machine, for a third kind of fact: `chatStream.js` owns what the answer is,
   * `agentSteps.js` owns what the agent was doing, and this owns the characters the model put
   * on screen one round at a time. They share one `run_id` and one liveness rule, and none of
   * them reads the other's frames — which is why a note about a tool call can never be
   * mistaken for the answer, and why a draft card and the text beside it always belong to the
   * same turn.
   */
  const blocksByRun = ref({ clientRunId: '', state: createBlockState() })

  /**
   * Move the open text blocks to match where the answer run ended.
   *
   * Confirmation is read off the run's own state rather than re-derived from the frame: a block
   * becomes the answer only on a run the answer reducer has *completed*, and `reduceRun` enters
   * that phase for a frame it accepted and for no other. A cancelled run, a frame carrying a
   * foreign `run_id`, and a second terminal event are all dropped there, so none of them folds a
   * paragraph or promotes one here. The block tracker refuses a confirmation after it sealed,
   * which is the same boundary stated where the text is written rather than a new rule.
   */
  function syncBlocksWithRun(state, event = null) {
    if (!state) return
    if (event && event.kind === 'completed' && event.finalBlockId
      && state.phase === RunPhase.Completed
      && blocksByRun.value.state.blocks.some((block) => block.blockId === event.finalBlockId
        && block.text === state.text)) {
      // Only the completed frame the service actually sent gets to say which paragraph was
      // the answer. Until it names one, every block is still just what the model wrote.
      blocksByRun.value = {
        ...blocksByRun.value,
        state: confirmAnswerBlock(blocksByRun.value.state, event.finalBlockId),
      }
    }
    if (isTerminalPhase(state.phase)) {
      // Terminal means final: a fragment that arrives on a socket the learner stopped
      // watching cannot grow a paragraph the page has already explained.
      blocksByRun.value = {
        ...blocksByRun.value,
        state: finishBlocks(blocksByRun.value.state),
      }
    }
  }

  function resetAgentState() {
    agentPanelOpen.value = false
  }

  function setAgentPanelOpen(open) {
    agentPanelOpen.value = !!open
  }

  function knowledgeFor(id) {
    const existing = id ? knowledgeDrafts.value[id] : null
    if (!existing) return emptyKnowledgeDraft()
    return { enabled: existing.enabled, selectedCollectionIds: [...existing.selectedCollectionIds] }
  }

  const activeKnowledge = computed(() => knowledgeFor(activeId.value))
  const canUseKnowledge = computed(
    () => isChatSession.value && collections.value.length > 0,
  )

  function modeOf(id) {
    return sessions.value.find((item) => item.id === id)?.mode ?? ''
  }

  /**
   * The only writer of knowledge state. It re-checks the target session's mode
   * and the server's collection list rather than trusting the caller, so an
   * Agent session can never end up holding a grounding option.
   *
   * "Switched on, nothing picked yet" is a state the user can be left in on
   * purpose — the toggle stays lit so the picker is reachable — while
   * `toWireKnowledge` still refuses to send anything until a real id is chosen.
   */
  function setKnowledge(id, next) {
    if (!id) return
    const allowed = isChatLike(modeOf(id)) && collections.value.length > 0
    if (!allowed) {
      knowledgeDrafts.value = { ...knowledgeDrafts.value, [id]: emptyKnowledgeDraft() }
      return
    }
    const previous = knowledgeFor(id)
    const requested = Array.isArray(next?.selectedCollectionIds)
      ? next.selectedCollectionIds
      : previous.selectedCollectionIds
    const known = new Set(collections.value.map((item) => String(item.id)))
    const selected = [...new Set(requested.map(String))].filter((value) =>
      known.has(value),
    )
    knowledgeDrafts.value = {
      ...knowledgeDrafts.value,
      [id]:
        next?.enabled === true
          ? { enabled: true, selectedCollectionIds: selected }
          : emptyKnowledgeDraft(),
    }
  }

  function forgetKnowledge(id) {
    if (!id || !(id in knowledgeDrafts.value)) return
    const rest = { ...knowledgeDrafts.value }
    delete rest[id]
    knowledgeDrafts.value = rest
  }

  /** Events are consumed by the reducer; the transcript mirrors its output. */
  function mirrorToTranscript(state) {
    const target = messages.value.find((item) => item.id === state.placeholderId)
    if (!target) return // the entry is gone (navigated away); drop the update
    const view = toMessageStatus(state)
    // One body of text, whoever wrote it. An agent turn's blocks are folded into `state.text`
    // as they arrive (see setPublishedText), so the transcript, the caret and the autoscroll
    // watcher all read the same string the learner is looking at instead of keeping a second
    // copy that could disagree mid-turn.
    target.content = state.text
    const published = turnTranscript(blocksByRun.value.state)
    // The rounds that were notes rather than answers fold out of the body once the completed
    // frame has confirmed one. They are the model's own words, so they stay readable - they
    // just stop posing as the reply.
    target.processNotes = published.processNotes
    target.status = view.status
    target.error = view.error
    // Citations follow the same containment as the text: they land on this run's
    // placeholder and nowhere else.
    target.sources = state.sources
    if (state.serverRunId) target.runId = state.serverRunId
    // The draft that is waiting on this reply, if the server offered one. It crosses here
    // rather than beside here so the card and the text share one binding: the run that
    // wrote this placeholder is the only run whose step state is read, and a step frame
    // from another turn was dropped by its own run id before it could reach this state.
    target.memoryDraft = stepsByRun.value.state.draft
    if (state.messageId) target.serverId = state.messageId
  }

  /**
   * `message.failed` carries a server string. The machine code is shown as-is
   * because it is enumerable, but the prose goes through the same whitelist as
   * every other error so an internal detail cannot reach the card.
   */
  function sanitizeEvent(event) {
    if (event.kind !== 'failed') return event
    return {
      ...event,
      message: safeErrorMessage(
        { code: event.code, message: event.message },
        '回答生成失败，请重试。',
      ),
    }
  }

  /** Is the run that `clientRunId` belongs to still the live one? */
  function isLiveRun(clientRunId) {
    return !!run.value && run.value.clientRunId === clientRunId
  }

  /**
   * A callback bound to one request. A cancelled run keeps draining its socket
   * for a while; without this closure the orphaned callback would feed a dead
   * request's events into whatever run is current by then.
   */
  function createEventHandler(clientRunId) {
    return function onStreamEvent(event) {
      if (!isLiveRun(clientRunId)) return
      const previous = run.value
      const next = reduceRun(previous, sanitizeEvent(event))
      run.value = next
      if (event.kind === 'completed' && next.phase === RunPhase.Completed
        && !isTerminalPhase(previous.phase) && next.ignoredEvents === previous.ignoredEvents) {
        const target = messages.value.find((item) => item.id === next.placeholderId)
        if (target && event.originalV2) target.originalV2 = event.originalV2
        pendingSend.value = null
      }
      syncStepsWithRun(next)
      // Whether this frame was taken is decided by the state it produced: only a run the
      // answer reducer completed can confirm a block, and a frame the reducer dropped leaves
      // the phase where it was.
      syncBlocksWithRun(next, event)
      mirrorToTranscript(next)
    }
  }

  /**
   * Progress belongs to the request that asked for it. A cancelled run keeps draining its
   * socket, and without the same liveness guard an orphaned frame would draw a dead
   * turn's steps onto the screen the next turn owns.
   */
  function createStepHandler(clientRunId) {
    return function onStreamStep(step) {
      if (!isLiveRun(clientRunId)) return
      // A terminal run is final for its trail too. A frame that reaches us on a socket the
      // learner already stopped — or after the answer landed — cannot add a step the screen
      // would then claim had happened.
      if (isTerminalPhase(run.value.phase)) return
      if (stepsByRun.value.clientRunId !== clientRunId) return
      stepsByRun.value = {
        clientRunId,
        state: applyStepEvent(stepsByRun.value.state, step),
      }
      // A step can change what this reply *is*, not only what the panel says about it: the
      // draft a `waiting_confirmation` frame brought rides on the reply, so it crosses on
      // the same mirror as the text. The mirror reads the run's own placeholder, so a frame
      // for a turn that has already left the screen has nowhere to write.
      mirrorToTranscript(run.value)
    }
  }

  /**
   * Published text belongs to the request that asked to watch for it, under the same two
   * guards as progress: a run that is no longer live has no screen to write on, and a
   * terminal run has nothing left to add. Without the second one, a fragment still draining
   * from a cancelled socket would lengthen an answer the page has already called off.
   */
  function createTextBlockHandler(clientRunId) {
    return function onStreamText(block) {
      if (!isLiveRun(clientRunId)) return
      if (isTerminalPhase(run.value.phase)) return
      if (blocksByRun.value.clientRunId !== clientRunId) return
      const state = applyTextBlockEvent(blocksByRun.value.state, block)
      blocksByRun.value = { clientRunId, state }
      // Growing the block and growing the answer are one update, not two copies of it: the
      // body a learner watches an agent turn write is the body every other consumer of this
      // run reads.
      const published = turnTranscript(state)
      if (published.content !== '') run.value = setPublishedText(run.value, published.content)
      // The text a block carries is the reply itself, so it crosses on the same mirror as
      // the answer status - one writer, one transcript entry, one run.
      mirrorToTranscript(run.value)
    }
  }

  /** Called once the stream settled; only the owning run may write the result. */
  function finalize(clientRunId, error) {
    if (!isLiveRun(clientRunId)) return
    let state = run.value
    if (isAbort(error)) state = abortRun(state)
    else if (error) {
      state = failRun(
        state,
        typeof error.code === 'string' ? error.code : 'stream_failed',
        safeErrorMessage(error, GENERIC_ERROR),
      )
    } else if (!isTerminalPhase(state.phase)) {
      // The socket closed without `message.completed`; say so rather than
      // leaving a spinner on screen forever.
      state = failRun(state, 'stream_closed', '连接已结束，但没有收到完成事件。')
    }
    run.value = state
    syncStepsWithRun(state)
    syncBlocksWithRun(state)
    mirrorToTranscript(state)
  }

  async function loadSessions() {
    sessionsLoading.value = true
    sessionsError.value = ''
    try {
      const payload = await apiListSessions()
      sessions.value = readList(payload, 'sessions').map(normalizeSession)
    } catch (error) {
      sessionsError.value = safeErrorMessage(error, '会话列表加载失败。')
    } finally {
      sessionsLoading.value = false
    }
  }

  /** Make any in-flight history request inert, and stop reading its socket. */
  function cancelPendingHistory() {
    historyGeneration += 1
    if (historyController) {
      const stale = historyController
      historyController = null
      stale.abort()
    }
    historyLoading.value = false
  }

  async function loadHistory(id) {
    const generation = historyGeneration + 1
    historyGeneration = generation
    if (historyController) historyController.abort()
    const controller = new AbortController()
    historyController = controller

    messages.value = []
    historyError.value = ''
    historyLoading.value = true

    try {
      const payload = await apiListMessages(id, { signal: controller.signal })
      // A newer navigation, or a close, owns the transcript now.
      if (generation !== historyGeneration || activeId.value !== id) return
      messages.value = readList(payload, 'messages').map((raw, index) =>
        normalizeMessage(raw, `hist-${id}-${index}`),
      )
    } catch (error) {
      if (isAbort(error)) return
      if (generation !== historyGeneration || activeId.value !== id) return
      historyError.value = safeErrorMessage(error, '历史消息加载失败。')
    } finally {
      if (historyController === controller) historyController = null
      if (generation === historyGeneration) historyLoading.value = false
    }
  }

  async function openSession(id) {
    if (!id) return
    const alreadyShown = id === activeId.value && messages.value.length > 0
    if (streaming.value) cancel()
    run.value = null
    resetAgentState()
    activeId.value = id
    // A deep link or a reload can beat the list request. Without the row we do
    // not know the mode, so fetch the list to find out rather than guessing one.
    if (!sessions.value.some((item) => item.id === id) && !sessionsLoading.value) {
      loadSessions()
    }
    if (alreadyShown) return
    await loadHistory(id)
  }

  function closeSession() {
    if (streaming.value) cancel()
    cancelPendingHistory()
    resetAgentState()
    activeId.value = ''
    messages.value = []
    run.value = null
    historyError.value = ''
  }

  /**
   * Creating a session always names its mode — there is no mode-less create any
   * more, because `POST /api/v1/sessions` requires one.
   */
  async function createSession(mode = SessionMode.Chat, retry = false) {
    if (creating) return ''
    if (!isKnownMode(mode)) {
      sessionsError.value = '只能创建对话或学习 Agent 两种会话。'
      return ''
    }
    sessionsError.value = ''
    const spec = retry ? pendingCreate.value : {
      mode, title: DEFAULT_SESSION_TITLES[mode],
      ...(isLearningAgent(mode) ? { requestId: localId('create') } : {}),
    }
    if (!spec) return ''
    pendingCreate.value = spec
    creating = true
    try {
      const payload = await apiCreateSession({ ...spec })
      const created = normalizeSession(payload || {})
      if (!created.id) {
        sessionsError.value = '服务没有返回新会话的标识。'
        return ''
      }
      // Trust the mode the server echoed back, not the one we asked for: if
      // they disagree, the stored session is the one the transcript must match.
      pendingCreate.value = null
      sessions.value = [created, ...sessions.value.filter((item) => item.id !== created.id)]
      closeSession()
      activeId.value = created.id
      return created.id
    } catch (error) {
      sessionsError.value = safeErrorMessage(error, '新建会话失败。')
      if (isLearningAgent(spec.mode) && /^agent_(request_conflict|request_finished|run_busy)$/.test(error.code || '')) {
        sessionsError.value += ` (${error.code})`
      }
      return ''
    } finally {
      creating = false
    }
  }

  async function removeSession(id) {
    if (!id) return
    try {
      await apiDeleteSession(id)
    } catch (error) {
      sessionsError.value = safeErrorMessage(error, '删除会话失败。')
      return
    }
    sessions.value = sessions.value.filter((item) => item.id !== id)
    forgetKnowledge(id)
    if (id === activeId.value) closeSession()
  }

  /** Cosmetic only: keeps a fresh session distinguishable in the sidebar. */
  function labelSession(id, content) {
    const target = sessions.value.find((item) => item.id === id)
    if (!target || !isDefaultTitle(target.mode, target.title)) return
    target.title = content.replace(/\s+/g, ' ').trim().slice(0, 24)
  }

  async function send(text, retryOperation = null, agentFields = {}) {
    const content = retryOperation ? retryOperation.content : String(text ?? draft.value).trim()
    if (!content || streaming.value) return
    const tooLong = validateMessageContent(content)
    if (tooLong) {
      sendError.value = tooLong
      return
    }

    sendError.value = ''
    let sessionId = retryOperation ? retryOperation.sessionId : activeId.value
    if (!sessionId) {
      // Typing into an empty screen is a question, not an agent invocation, so
      // the session it creates is a plain Chat one.
      sessionId = await createSession(SessionMode.Chat)
      if (!sessionId) return
    }

    draft.value = ''
    labelSession(sessionId, content)

    // Read after the session exists: the grounding option belongs to the
    // resolved session, and an Agent session has none to read.
    const mode = modeOf(sessionId)
    const knowledge = isChatLike(mode) ? knowledgeFor(sessionId) : null
    const operation = retryOperation || (isLearningAgent(mode) ? {
      sessionId, content, requestId: localId('send'),
      ...(agentFields.knowledgePolicy !== undefined ? { knowledgePolicy: structuredClone(agentFields.knowledgePolicy) } : {}),
      ...(agentFields.topicConsent !== undefined ? { topicConsent: structuredClone(agentFields.topicConsent) } : {}),
    } : null)
    if (operation) pendingSend.value = operation

    // From here the transcript is ours to write; a history request still in
    // flight would replace it wholesale.
    cancelPendingHistory()

    const placeholderId = retryOperation?.placeholderId || localId('asst')
    if (operation) operation.placeholderId = placeholderId
    if (retryOperation && messages.value.some((item) => item.id === placeholderId)) {
      const target = messages.value.find((item) => item.id === placeholderId)
      Object.assign(target, { content: '', status: 'pending', error: null, sources: [], processNotes: [], memoryDraft: null, originalV2: null })
    } else {
      messages.value.push({
        id: localId('msg'),
        role: 'user',
        content,
        createdAt: nowIso(),
        status: 'done',
        error: null,
        sources: [],
      })
      messages.value.push({
        id: placeholderId,
        role: 'assistant',
        content: '',
        createdAt: nowIso(),
        status: 'pending',
        error: null,
        sources: [],
        // The draft this turn stored, once it says so. It hangs off the reply it came from
        // rather than off the panel, because the decision is about that sentence.
        memoryDraft: null,
        // Presentation provenance from the persisted session's mode, never user text.
        memoryContract: isLearningAgent(mode) ? 'v2' : 'clean',
        // The rounds the model narrated on the way to a tool, once this reply has an answer to
        // be separate from. They belong to this reply for the same reason the draft does.
        processNotes: [],
      })

    }

    const clientRunId = localId('run')
    run.value = createRunState({ clientRunId, placeholderId })
    stepsByRun.value = { clientRunId, state: createStepState({ clientRunId }) }
    blocksByRun.value = { clientRunId, state: createBlockState({ clientRunId }) }
    // A new turn never inherits the previous turn's opened panel.
    resetAgentState()
    const controller = new AbortController()
    streamController = controller

    // Only an agent turn asks to be watched. The mode is the stored session's, read
    // above, and the server refuses `stream_steps` on a turn whose strategy has no
    // steps — so a Chat or knowledge request keeps carrying the body it always did.
    const watchingSteps = isLearningAgent(mode)

    try {
      await apiStreamMessage({
        sessionId,
        content,
        ...(operation ? { requestId: operation.requestId,
          ...(operation.knowledgePolicy !== undefined ? { knowledgePolicy: operation.knowledgePolicy } : {}),
          ...(operation.topicConsent !== undefined ? { topicConsent: operation.topicConsent } : {}),
        } : {}),
        knowledge: isKnowledgeUsable(knowledge) ? knowledge : null,
        signal: controller.signal,
        onEvent: createEventHandler(clientRunId),
        streamSteps: watchingSteps,
        onStep: watchingSteps ? createStepHandler(clientRunId) : undefined,
        // The same opt-in carries the text-block frames: a turn that asked to watch an agent
        // is a turn whose text arrives as blocks, and a turn that did not ask keeps receiving
        // `message.delta` like any other.
        onText: watchingSteps ? createTextBlockHandler(clientRunId) : undefined,
      })
      finalize(clientRunId, null)
    } catch (error) {
      finalize(clientRunId, error)
    } finally {
      if (streamController === controller) streamController = null
    }
  }

  function retryCreate() {
    return pendingCreate.value ? createSession(pendingCreate.value.mode, true) : Promise.resolve('')
  }

  function retrySend() {
    return retryableSend.value ? send(undefined, pendingSend.value) : Promise.resolve()
  }

  function cancel() {
    const controller = streamController
    if (controller) {
      streamController = null
      controller.abort()
    }
    if (run.value) {
      run.value = abortRun(run.value)
      // Stopping is the learner's own fact, so the trail reads cancelled from the moment
      // they press the button rather than waiting for a socket that may keep draining.
      syncStepsWithRun(run.value)
      mirrorToTranscript(run.value)
    }
    // The stage itself must stay and read "本轮已取消"; only the expansion is
    // transient, so collapsing is the whole of the cleanup here.
    resetAgentState()
  }

  return {
    sessions,
    sessionsLoading,
    sessionsError,
    pendingCreate,
    retryCreate,
    retryableSend,
    retrySend,
    activeId,
    activeSession,
    activeMode,
    isChatSession,
    isAgentSession,
    collections,
    messages,
    historyLoading,
    historyError,
    draft,
    sendError,
    run,
    streaming,
    ready,
    activeKnowledge,
    canUseKnowledge,
    setKnowledge,
    knowledgeFor,
    agentStage,
    agentSteps,
    agentPanelOpen,
    setAgentPanelOpen,
    resetAgentState,
    loadSessions,
    openSession,
    closeSession,
    createSession,
    removeSession,
    send,
    cancel,
  }
}
