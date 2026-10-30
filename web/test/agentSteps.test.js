import { describe, expect, it } from 'vitest'

import {
  applyStepEvent,
  completeSteps,
  createStepState,
  finishSteps,
  formatStepDuration,
  isStepEventName,
  stepRows,
  StepStatus,
  toStepEvent,
} from '../src/lib/agentSteps.js'

/**
 * The step channel, tested as its own state machine.
 *
 * What is being pinned here is not "a list renders". The protocol reports four frames
 * and **no frame that closes a step**, so every rule about when a step stops running is
 * the client's inference, and an inference a test does not name is an inference somebody
 * can quietly widen later. The four that matter most:
 *
 *  - a step is closed by the next one opening, or by its own result — never by the
 *    client's clock, and never by a frame that does not exist;
 *  - a repeated frame changes nothing, so a socket that delivered the same fact twice
 *    cannot show it twice;
 *  - a frame from another turn is dropped, because the server numbers sequences per turn;
 *  - nothing the server did not send becomes text on screen: no status sentence is
 *    attached to a step that never reported, and no identifier is read that never arrived.
 */

const RUN = 'run-7'

function frame(event, payload) {
  return { event, data: JSON.stringify(payload), id: '' }
}

function step(event, payload) {
  return toStepEvent(frame(event, payload))
}

function started(sequence, stepType, message) {
  return step('step_started', { run_id: RUN, sequence, step_type: stepType, message })
}

function toolStarted(sequence, toolName) {
  return step('tool_started', { run_id: RUN, sequence, tool_name: toolName })
}

function toolResult(sequence, toolName, content, durationMillis) {
  return step('tool_result', {
    run_id: RUN,
    sequence,
    tool_name: toolName,
    content,
    duration_millis: durationMillis,
  })
}

/** The card a `waiting_confirmation` frame carries: what the server wrote, projected. */
function card(draftId, scope = 'global', content = '用户偏好先看代码再读解释') {
  return { draft_id: draftId, scope, content }
}

function awaiting(sequence, message, draft) {
  return step('waiting_confirmation', {
    run_id: RUN,
    sequence,
    message,
    ...(draft ? { memory_draft: draft } : {}),
  })
}

function play(events) {
  return events.reduce((state, event) => applyStepEvent(state, event), createStepState({ clientRunId: 'run-1' }))
}

describe('toStepEvent', () => {
  it('claims only the four names the protocol has', () => {
    expect(isStepEventName('step_started')).toBe(true)
    expect(isStepEventName('tool_started')).toBe(true)
    expect(isStepEventName('tool_result')).toBe(true)
    expect(isStepEventName('waiting_confirmation')).toBe(true)
    expect(isStepEventName('message.delta')).toBe(false)
    expect(isStepEventName('agent.step.completed')).toBe(false)
    expect(toStepEvent(frame('message.delta', { delta: 'x' }))).toBeNull()
  })

  it('reads only the whitelisted keys, so an identifier cannot ride in on a payload', () => {
    const event = toStepEvent(
      frame('tool_result', {
        run_id: RUN,
        sequence: 3,
        tool_name: 'memory_search',
        content: '本轮已查阅长期记忆。',
        duration_millis: 7,
        memory_id: 'mem-77',
        source_id: 'src-9',
        session_id: 'session-2',
        user_id: 'user-1',
        arguments: { query: 'Redis 持久化' },
        observation: '分块原文',
      }),
    )

    expect(Object.keys(event).sort()).toEqual([
      'content',
      'draft',
      'durationMillis',
      'message',
      'phase',
      'runId',
      'sequence',
      'stepType',
      'toolName',
    ])
    // A draft only arrives on the frame that asks about one.
    expect(event.draft).toBeNull()
    const serialized = JSON.stringify(event)
    for (const secret of ['mem-77', 'src-9', 'session-2', 'user-1', 'Redis 持久化', '分块原文']) {
      expect(serialized).not.toContain(secret)
    }
  })

  it('reads a draft card field by field, so nothing else on it can reach the page', () => {
    const event = toStepEvent(
      frame('waiting_confirmation', {
        run_id: RUN,
        sequence: 4,
        message: '本回合生成了 1 条待确认记忆草稿，请在当前回复下方的确认卡片中处理。',
        memory_draft: {
          draft_id: 'mem-77',
          scope: 'topic',
          topic_key: 'redis-持久化',
          content: '用户偏好先看代码再读解释',
          // Everything below is what the stored row also has. None of it is on the list
          // this function reads, so none of it can arrive in a card by accident.
          user_id: 'user-1',
          source_session_id: 'session-2',
          source_message_ids: ['message-9'],
          confidence: 1,
          status: 'pending',
          created_at: '2026-09-28T00:00:00Z',
        },
      }),
    )

    expect(event.draft).toEqual({
      draftId: 'mem-77',
      scope: 'topic',
      topicKey: 'redis-持久化',
      content: '用户偏好先看代码再读解释',
    })
    const serialized = JSON.stringify(event)
    for (const secret of ['user-1', 'session-2', 'message-9', 'confidence', 'created_at', 'pending']) {
      expect(serialized).not.toContain(secret)
    }
  })

  it('degrades a malformed body and a non-numeric sequence instead of guessing', () => {
    // Unparseable JSON is not a step frame at all.
    expect(toStepEvent({ event: 'step_started', data: '{oops', id: '' })).toBeNull()
    expect(toStepEvent({ event: 'step_started', data: '[1,2]', id: '' })).toBeNull()
    // A body-less frame still *is* one, and carries no sequence: the state machine drops
    // it, because a step nobody can number cannot be correlated with its result.
    expect(toStepEvent({ event: 'step_started', data: '', id: '' })).toMatchObject({ phase: 'step_started', sequence: 0 })
    expect(toStepEvent(frame('step_started', { run_id: RUN, sequence: 'two' }))).toMatchObject({ sequence: 0 })
  })

  it('keeps an unrecognised step_type empty rather than labelling it', () => {
    expect(toStepEvent(frame('step_started', { run_id: RUN, sequence: 1, step_type: 'retrieve' })).stepType).toBe('')
  })
})

describe('the frames as they actually arrive', () => {
  it('turns one watched turn into two rows: a model step, then a tool step with its result', () => {
    const state = play([
      started(1, 'plan', 'model generation 1'),
      started(2, 'tool', 'tool call 1'),
      toolStarted(2, 'memory_search'),
      toolResult(2, 'memory_search', '本轮已查阅长期记忆。', 7),
      started(3, 'plan', 'model generation 2'),
    ])
    const rows = stepRows(state)

    expect(rows.map((row) => row.sequence)).toEqual([1, 2, 3])
    expect(rows[0]).toMatchObject({ label: 'model generation 1', status: StepStatus.Completed })
    // The label is the server's own sentence with the tool's display name appended; the client
    // never rewrites "tool call 1" into a word of its own.
    expect(rows[1]).toMatchObject({
      label: 'tool call 1 · 查找记忆',
      status: StepStatus.Completed,
      detail: '本轮已查阅长期记忆。',
      durationMillis: 7,
    })
    expect(rows[2]).toMatchObject({ label: 'model generation 2', status: StepStatus.Running })
  })

  it('closes the previous step when a new one opens — there is no closing frame to wait for', () => {
    let state = applyStepEvent(createStepState(), started(1, 'plan', 'model generation 1'))
    expect(stepRows(state)[0].status).toBe(StepStatus.Running)

    state = applyStepEvent(state, started(2, 'tool', 'tool call 1'))
    expect(stepRows(state)[0].status).toBe(StepStatus.Completed)
    expect(stepRows(state)[1].status).toBe(StepStatus.Running)
  })

  it('a result closes only its own step, by sequence', () => {
    const state = play([
      started(1, 'plan', 'model generation 1'),
      started(2, 'tool', 'tool call 1'),
      toolStarted(2, 'knowledge_search'),
      toolResult(2, 'knowledge_search', '本轮检索了资料。', 12),
      started(3, 'plan', 'model generation 2'),
    ])

    expect(stepRows(state).find((row) => row.sequence === 2).status).toBe(StepStatus.Completed)
    expect(stepRows(state).find((row) => row.sequence === 3).status).toBe(StepStatus.Running)
  })

  it('carries the tool name from tool_started onto the step step_started opened', () => {
    const state = play([started(2, 'tool', 'tool call 1'), toolStarted(2, 'memory_search')])

    expect(stepRows(state)).toHaveLength(1)
    expect(stepRows(state)[0].label).toBe('tool call 1 · 查找记忆')
    expect(stepRows(state)[0].status).toBe(StepStatus.Running)
  })

  it('treats a tool frame with no opening step as the real progress it reports', () => {
    const state = play([toolStarted(4, 'memory_search')])

    expect(stepRows(state)[0]).toMatchObject({ sequence: 4, status: StepStatus.Running })
    // The tool is shown by its display name, not by the raw name the server sent.
    expect(stepRows(state)[0].label).toContain('查找记忆')
  })
})

describe('duplicates and foreign frames', () => {
  it('changes nothing when the same frame is delivered twice', () => {
    const once = play([started(1, 'plan', 'model generation 1'), toolResult(1, 'memory_search', '本轮已查阅长期记忆。', 7)])
    const twice = applyStepEvent(once, toolResult(1, 'memory_search', '本轮已查阅长期记忆。', 7))

    expect(stepRows(twice)).toEqual(stepRows(once))
    expect(twice.ignoredEvents).toBe(once.ignoredEvents)
  })

  it('counts a frame from another turn without drawing it', () => {
    let state = applyStepEvent(createStepState(), started(1, 'plan', 'model generation 1'))
    const other = step('tool_started', { run_id: 'run-8', sequence: 9, tool_name: 'memory_search' })
    state = applyStepEvent(state, other)

    expect(stepRows(state)).toHaveLength(1)
    expect(state.ignoredEvents).toBe(1)
  })

  it('drops a frame with no sequence, since nothing can be correlated to it', () => {
    const state = applyStepEvent(
      applyStepEvent(createStepState(), started(1, 'plan', 'model generation 1')),
      step('tool_result', { run_id: RUN, tool_name: 'memory_search', content: 'x', duration_millis: 2 }),
    )

    expect(stepRows(state)).toHaveLength(1)
    expect(state.ignoredEvents).toBe(1)
  })

  it('ignores a frame the protocol does not have', () => {
    const state = applyStepEvent(createStepState(), { phase: 'step_completed', sequence: 1, runId: RUN })

    expect(stepRows(state)).toHaveLength(0)
    expect(state.ignoredEvents).toBe(1)
  })
})

describe('waiting confirmation', () => {
  it('keeps the draft the frame named, and nothing else about it', () => {
    const state = play([
      started(2, 'tool', 'tool call 1'),
      toolStarted(2, 'propose_memory'),
      toolResult(2, 'propose_memory', '新增 1 条待确认记忆草稿', 3),
      awaiting(2, '本回合生成了 1 条待确认记忆草稿，请在当前回复下方的确认卡片中处理。', {
        draft_id: 'mem-3',
        scope: 'global',
        content: '用户偏好先看代码再读解释',
      }),
    ])

    expect(state.draft).toEqual({
      draftId: 'mem-3',
      scope: 'global',
      topicKey: '',
      content: '用户偏好先看代码再读解释',
    })
    // Four names, and no room for anything the stored row also carries.
    expect(Object.keys(state.draft).sort()).toEqual(['content', 'draftId', 'scope', 'topicKey'])
    // A global card has no subject to show.
    expect(state.draft.topicKey).toBe('')
  })

  it('holds one card per turn, because the turn stored one draft', () => {
    const once = applyStepEvent(createStepState(), awaiting(2, 'a', card('mem-3')))
    const twice = applyStepEvent(once, awaiting(2, 'a', card('mem-4')))

    // The second is not kept and not swapped in: the first frame is the row this turn wrote,
    // and a card that changed id after the fact would be a different promise.
    expect(twice.draft).toEqual(once.draft)
    expect(twice.draft.draftId).toBe('mem-3')
    expect(twice.ignoredEvents).toBe(1)
  })

  it('invents no card when the frame carries nothing to decide on', () => {
    // A sentence with no draft, and a draft that cannot be addressed: both leave the screen
    // as it was. The alternative — looking the newest pending draft up — is what would
    // attach another conversation's row to this reply.
    const noCard = play([started(1, 'plan', 'model generation 1'), awaiting(1, '有一条草稿在等待确认。')])
    expect(noCard.draft).toBeNull()
    expect(noCard.ignoredEvents).toBe(1)

    const blankId = play([awaiting(1, 'a', { draft_id: '', scope: 'global', content: '一句话' })])
    expect(blankId.draft).toBeNull()

    const unknownScope = play([awaiting(1, 'a', { draft_id: 'mem-3', scope: 'preferences', content: '一句话' })])
    expect(unknownScope.draft).toBeNull()

    const emptyContent = play([awaiting(1, 'a', { draft_id: 'mem-3', scope: 'global', content: '   ' })])
    expect(emptyContent.draft).toBeNull()

    // A topic card needs its subject: "主题记忆" with nothing after it is not a decision.
    const noSubject = play([awaiting(1, 'a', { draft_id: 'mem-3', scope: 'topic', content: '一句话' })])
    expect(noSubject.draft).toBeNull()

    const notAnObject = play([awaiting(1, 'a', 'mem-3')])
    expect(notAnObject.draft).toBeNull()
  })

  it('carries nothing when no draft was left', () => {
    expect(play([started(1, 'plan', 'model generation 1')]).draft).toBeNull()
  })
})

describe('the end of a turn', () => {
  it('message.completed closes an open step, without claiming it produced a result', () => {
    const open = play([started(1, 'plan', 'model generation 1'), started(2, 'tool', 'tool call 1')])
    const done = completeSteps(open)

    expect(stepRows(done).every((row) => row.status === StepStatus.Completed)).toBe(true)
    // No detail appeared: the step never reported one.
    expect(stepRows(done).every((row) => row.detail === '')).toBe(true)
  })

  it('is idempotent, so a late frame after the answer cannot reopen anything', () => {
    const done = completeSteps(play([started(1, 'plan', 'model generation 1')]))

    expect(stepRows(completeSteps(done))).toEqual(stepRows(done))
    expect(stepRows(finishSteps(done, StepStatus.Failed)).map((r) => r.status)).toEqual([StepStatus.Completed])
  })

  it('a failed turn marks the steps failed and fabricates no tool result', () => {
    const state = finishSteps(
      play([started(1, 'plan', 'model generation 1'), started(2, 'tool', 'tool call 1'), toolStarted(2, 'memory_search')]),
      StepStatus.Failed,
    )
    const rows = stepRows(state)

    expect(rows[1]).toMatchObject({ status: StepStatus.Failed, detail: '', durationMillis: null })
    // The server sends no result frame for a call that did not finish; neither may we.
    expect(state.ignoredEvents).toBe(0)
  })

  it('a cancelled turn reads as cancelled, not as completed', () => {
    const rows = stepRows(finishSteps(play([started(1, 'plan', 'model generation 1')]), StepStatus.Cancelled))

    expect(rows[0].status).toBe(StepStatus.Cancelled)
  })
})

describe('durations', () => {
  it('reports what the server measured and nothing when it did not', () => {
    expect(formatStepDuration(7)).toBe('7 毫秒')
    expect(formatStepDuration(1500)).toBe('1.5 秒')
    expect(formatStepDuration(0)).toBe('0 毫秒')
    expect(formatStepDuration(null)).toBe('')
    expect(formatStepDuration(undefined)).toBe('')
  })

  it('omits the duration line when the frame carried none', () => {
    const state = play([started(2, 'tool', 'tool call 1'), toolStarted(2, 'memory_search')])

    expect(stepRows(state)[0].durationMillis).toBeNull()
  })
})
