import { describe, expect, it } from 'vitest'

import {
  abortRun,
  createRunState,
  failRun,
  isTerminalPhase,
  reduceRun,
  RunPhase,
  toMessageStatus,
} from '../src/lib/chatStream.js'

const BASE = { clientRunId: 'run-1', placeholderId: 'ph-1' }

function started(extra = {}) {
  return { kind: 'started', runId: '', messageId: '', ...extra }
}
function delta(text, extra = {}) {
  return { kind: 'delta', delta: text, runId: '', messageId: '', ...extra }
}
function completed(content = '', extra = {}) {
  return { kind: 'completed', content, runId: '', messageId: '', ...extra }
}
function failure(message = 'boom', extra = {}) {
  return {
    kind: 'failed',
    message,
    code: 'model_timeout',
    runId: '',
    messageId: '',
    ...extra,
  }
}

describe('reduceRun transitions', () => {
  it('idle -> streaming -> completed', () => {
    let state = createRunState(BASE)
    state = reduceRun(state, started())
    expect(state.phase).toBe(RunPhase.Streaming)
    state = reduceRun(state, completed('最终答案'))
    expect(state.phase).toBe(RunPhase.Completed)
    expect(state.text).toBe('最终答案')
    expect(isTerminalPhase(state.phase)).toBe(true)
  })

  it('concatenates deltas in arrival order, including multi-byte text', () => {
    const steps = ['你', '好', '，', 'world 🦫\n']
    const state = steps.reduce(
      (acc, piece) => reduceRun(acc, delta(piece)),
      createRunState(BASE),
    )
    expect(state.text).toBe('你好，world 🦫\n')
    expect(state.ignoredEvents).toBe(0)
  })

  it('does not mutate the previous state', () => {
    const before = createRunState(BASE)
    const after = reduceRun(before, delta('x'))
    expect(before.text).toBe('')
    expect(after).not.toBe(before)
  })
})

describe('terminal states are final', () => {
  const finish = (path) => path.reduce((state, event) => reduceRun(state, event), createRunState(BASE))

  it('a late message.failed cannot overwrite a completed answer', () => {
    const done = finish([started(), delta('完整回答'), completed()])
    const after = reduceRun(done, failure('connection reset'))

    expect(after.phase).toBe(RunPhase.Completed)
    expect(after.text).toBe('完整回答')
    expect(after.error).toBe(null)
    expect(after.ignoredEvents).toBe(done.ignoredEvents + 1)
  })

  it('a late completed cannot resurrect a failed run', () => {
    const failed = finish([delta('部分'), failure()])
    const after = reduceRun(failed, completed('覆盖'))
    expect(after.phase).toBe(RunPhase.Failed)
    expect(after.text).toBe('部分')
  })

  it('a failed run keeps whatever text already arrived', () => {
    const state = finish([delta('第一段 '), delta('第二段'), failure()])
    expect(state.phase).toBe(RunPhase.Failed)
    expect(state.text).toBe('第一段 第二段')
    expect(state.error).toEqual({ code: 'model_timeout', message: 'boom' })
  })

  it('aborted ignores further deltas and failures', () => {
    let state = finish([delta('half')])
    state = abortRun(state)
    expect(state.phase).toBe(RunPhase.Aborted)
    state = reduceRun(state, delta(' more'))
    state = reduceRun(state, failure())
    expect(state.text).toBe('half')
    expect(state.phase).toBe(RunPhase.Aborted)
  })

  it('abortRun and failRun are idempotent once terminal', () => {
    const done = reduceRun(reduceRun(createRunState(BASE), completed('ok')), completed('nope'))
    expect(done.text).toBe('ok')
    expect(failRun(done, 'x', 'y')).toBe(done)
    expect(abortRun(done)).toBe(done)
  })

  it('a duplicate started never resets streamed text', () => {
    let state = reduceRun(createRunState(BASE), started())
    state = reduceRun(state, delta('abc'))
    state = reduceRun(state, started())
    expect(state.text).toBe('abc')
    expect(state.phase).toBe(RunPhase.Streaming)
    expect(state.ignoredEvents).toBe(0)
  })
})

describe('run ownership', () => {
  it('accepts a server run_id that differs from the local clientRunId', () => {
    // The client mints `clientRunId` and never sends it, so a server-generated
    // run_id can never match it. Equality against clientRunId is the bug this
    // guards against: it would discard every single event.
    let state = createRunState(BASE)
    expect(state.clientRunId).toBe('run-1')
    expect(state.serverRunId).toBe('')

    state = reduceRun(state, delta('a', { runId: 'srv-run-77' }))
    state = reduceRun(state, delta('b', { runId: 'srv-run-77' }))
    state = reduceRun(state, completed('ab', { runId: 'srv-run-77' }))

    expect(state.text).toBe('ab')
    expect(state.phase).toBe(RunPhase.Completed)
    expect(state.ignoredEvents).toBe(0)
    expect(state.clientRunId).toBe('run-1')
  })

  it('binds the first non-empty server run_id', () => {
    const state = reduceRun(createRunState(BASE), started({ runId: 'srv-run-77' }))
    expect(state.serverRunId).toBe('srv-run-77')
  })

  it('keeps the first bound server run_id when a later frame repeats another', () => {
    let state = reduceRun(createRunState(BASE), started({ runId: 'srv-A' }))
    state = reduceRun(state, delta('x', { runId: 'srv-B' }))
    expect(state.serverRunId).toBe('srv-A')
    expect(state.text).toBe('')
    expect(state.ignoredEvents).toBe(1)
  })

  it('drops events carrying a different server run_id after binding', () => {
    let state = reduceRun(createRunState(BASE), started({ runId: 'srv-A' }))
    state = reduceRun(state, delta('mine', { runId: 'srv-A' }))
    const after = reduceRun(state, completed('另一个请求的答案', { runId: 'srv-B' }))

    expect(after.text).toBe('mine')
    expect(after.phase).toBe(RunPhase.Streaming)
    expect(after.ignoredEvents).toBe(state.ignoredEvents + 1)
  })

  it('lets a run with no run_id at all stream normally', () => {
    // A server that omits run_id must not be mistaken for a foreign run.
    const state = reduceRun(
      reduceRun(createRunState(BASE), started()),
      completed('no ids anywhere'),
    )
    expect(state.text).toBe('no ids anywhere')
    expect(state.serverRunId).toBe('')
    expect(state.ignoredEvents).toBe(0)
  })

  it('drops deltas addressed to a different assistant message', () => {
    let state = reduceRun(createRunState(BASE), started({ messageId: 'srv-9' }))
    state = reduceRun(state, delta('mine'))
    const after = reduceRun(state, delta('belongs to another request', { messageId: 'srv-10' }))

    expect(after.text).toBe('mine')
    expect(after.ignoredEvents).toBe(state.ignoredEvents + 1)
  })

  it('still accepts frames that carry no run_id once a server run is bound', () => {
    // An absent id is "not stated", which is different from "another run".
    let state = reduceRun(createRunState(BASE), started({ runId: 'srv-A' }))
    state = reduceRun(state, delta('one'))
    state = reduceRun(state, delta(' two', { runId: '' }))
    state = reduceRun(state, completed())
    expect(state.text).toBe('one two')
    expect(state.phase).toBe(RunPhase.Completed)
    expect(state.ignoredEvents).toBe(0)
  })

  it('adopts the server message id from the first event that carries one', () => {
    let state = reduceRun(createRunState(BASE), started({ messageId: 'srv-9' }))
    expect(state.messageId).toBe('srv-9')
    expect(state.placeholderId).toBe('ph-1')

    state = reduceRun(state, delta('more', { messageId: 'srv-9' }))
    expect(state.text).toBe('more')
    expect(state.ignoredEvents).toBe(0)
  })

  it('ignores a malformed event without touching the transcript text', () => {
    const state = reduceRun(createRunState(BASE), delta('keep'))
    expect(reduceRun(state, null).text).toBe('keep')
    expect(reduceRun(state, { kind: 'nonsense' }).text).toBe('keep')
  })

  it('accepts deltas before any started event', () => {
    // A server that omits message.started must still render.
    const state = reduceRun(createRunState(BASE), delta('no start'))
    expect(state.phase).toBe(RunPhase.Streaming)
    expect(state.text).toBe('no start')
  })
})

describe('toMessageStatus', () => {
  it('maps each phase onto the transcript vocabulary', () => {
    const cases = [
      [createRunState(BASE), 'pending'],
      [reduceRun(createRunState(BASE), completed('x')), 'done'],
      [reduceRun(createRunState(BASE), failure('bad')), 'error'],
      [abortRun(reduceRun(createRunState(BASE), delta('y'))), 'stopped'],
    ]
    expect(cases.map(([state]) => toMessageStatus(state).status)).toEqual([
      'pending',
      'done',
      'error',
      'stopped',
    ])
    expect(toMessageStatus(cases[1][0]).streaming).toBe(false)
    expect(toMessageStatus(cases[0][0]).streaming).toBe(true)
    expect(toMessageStatus(cases[2][0]).error.message).toBe('bad')
  })

  it('reports no error for a run that completed', () => {
    const state = reduceRun(
      reduceRun(createRunState(BASE), delta('partial')),
      completed('final'),
    )
    expect(toMessageStatus(state)).toEqual({
      status: 'done',
      streaming: false,
      error: null,
    })
  })
})

/**
 * The property the Agent-step work (Phase 11C-FE) is built on: a name this client
 * does not map becomes `kind: 'unknown'` upstream and must be **counted and
 * discarded** here — never interpreted, never allowed to move the transcript.
 *
 * These cases are deliberately written without naming any future event. The point is
 * not what the server will call its steps; it is that adding them cannot change what
 * `message.started` / `delta` / `completed` / `failed` already do, so Chat keeps its
 * current behaviour whether or not the step surface exists.
 */
describe('unrecognised events are counted, never interpreted', () => {
  function unknown(extra = {}) {
    return { kind: 'unknown', runId: '', messageId: '', delta: '', content: '', sources: [], ...extra }
  }

  it('leaves text, phase and error exactly where they were mid-stream', () => {
    let state = reduceRun(createRunState(BASE), started())
    state = reduceRun(state, delta('已经收到的正文'))
    const before = state
    const after = reduceRun(state, unknown())

    expect(after.text).toBe(before.text)
    expect(after.phase).toBe(RunPhase.Streaming)
    expect(after.error).toBe(null)
    expect(after.sources).toEqual([])
    expect(after.ignoredEvents).toBe(before.ignoredEvents + 1)
  })

  it('does not open or finish a run before the first real event', () => {
    const state = reduceRun(createRunState(BASE), unknown())

    // An unknown frame is not evidence that an answer started, and certainly not
    // evidence that it ended.
    expect(state.phase).toBe(RunPhase.Idle)
    expect(isTerminalPhase(state.phase)).toBe(false)
    expect(state.ignoredEvents).toBe(1)
  })

  it('cannot resurrect or disturb a terminal run', () => {
    const done = reduceRun(
      reduceRun(reduceRun(createRunState(BASE), started()), delta('完整回答')),
      completed(),
    )
    const after = reduceRun(done, unknown({ content: '别的什么', delta: '拼接' }))

    expect(after.phase).toBe(RunPhase.Completed)
    expect(after.text).toBe('完整回答')
    expect(after.ignoredEvents).toBe(done.ignoredEvents + 1)
  })

  it('documents why a future step event must carry the same ids as the message events', () => {
    // Consequence, pinned on purpose: an unmapped frame that arrives first binds the
    // run identity it carries, and a later `message.delta` with a *different* id is
    // then dropped as foreign — the learner would watch an answer that never starts.
    // So whatever the Agent step surface is named, its `run_id` / `message_id` must be
    // the same values the message events use, or be absent. If this rule is ever
    // changed on purpose, this case is the one that has to move.
    let state = reduceRun(createRunState(BASE), unknown({ runId: 'step-run' }))
    state = reduceRun(state, delta('真正的回答', { runId: 'message-run' }))

    expect(state.serverRunId).toBe('step-run')
    expect(state.text).toBe('')
    expect(state.ignoredEvents).toBe(2)
  })
})
