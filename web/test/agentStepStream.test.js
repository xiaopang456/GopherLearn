import { beforeEach, describe, expect, it } from 'vitest'

import { streamMessage } from '../src/api/stream.js'
import { setTokenProvider } from '../src/api/http.js'
import { installFetchStub, sseFrame, sseResponse } from './helpers/transport.js'

/**
 * The transport's share of the step protocol: one request flag, two channels out.
 *
 * The rule that matters is separation, not parsing. A progress frame must never reach the
 * answer callback — `chatStream.js` binds a run's identity from the first non-empty
 * `run_id` it sees and drops any later frame carrying a different one, so a step routed
 * into it would make the real deltas look foreign and the learner would watch an answer
 * that never starts. The two channels also explain what a caller that did not ask for
 * progress receives: nothing, and in particular not a counted, half-interpreted frame.
 */

let bodies
let events
let steps

beforeEach(() => {
  bodies = []
  events = []
  steps = []
  setTokenProvider(() => 'tok.steps')
})

function serve(streamFrames, extra = {}) {
  installFetchStub((url, init) => {
    bodies.push({ url: String(url), body: init?.body ? JSON.parse(init.body) : null, method: init?.method ?? 'GET' })
    return sseResponse(streamFrames, extra)
  })
}

async function run(sessionId, options = {}) {
  await streamMessage({
    sessionId,
    content: '讲讲 RDB 与 AOF',
    signal: undefined,
    onEvent: (event) => events.push(event),
    onStep: (step) => steps.push(step),
    ...options,
  })
}

describe('the request', () => {
  it('adds stream_steps only when the caller asked to watch', async () => {
    serve([sseFrame('message.completed', { message_id: 'a-1', content: '答案' })])
    await run('ag-1', { streamSteps: true })

    expect(bodies[0].method).toBe('POST')
    expect(bodies[0].body).toEqual({ content: '讲讲 RDB 与 AOF', stream_steps: true })
  })

  it('sends the body it always sent when it did not', async () => {
    serve([sseFrame('message.completed', { message_id: 'a-1', content: '答案' })])
    await run('ch-1')

    expect(Object.keys(bodies[0].body)).toEqual(['content'])
    expect(bodies[0].body).not.toHaveProperty('stream_steps')
  })

  it('does not smuggle the flag in as false, which is a different request', async () => {
    serve([sseFrame('message.completed', { message_id: 'a-1', content: '答案' })])
    await run('ch-1', { streamSteps: false })

    expect(bodies[0].body).toEqual({ content: '讲讲 RDB 与 AOF' })
  })
})

describe('the two channels', () => {
  const WATCHED = [
    sseFrame('message.started', { message_id: 'a-1', run_id: 'run-4' }),
    sseFrame('step_started', { run_id: 'run-4', sequence: 1, step_type: 'plan', message: 'model generation 1' }),
    sseFrame('message.delta', { run_id: 'run-4', message_id: 'a-1', delta: '一半' }),
    sseFrame('step_started', { run_id: 'run-4', sequence: 2, step_type: 'tool', message: 'tool call 1' }),
    sseFrame('tool_started', { run_id: 'run-4', sequence: 2, tool_name: 'knowledge_search' }),
    sseFrame('tool_result', {
      run_id: 'run-4',
      sequence: 2,
      tool_name: 'knowledge_search',
      content: '本轮检索了你的笔记。',
      duration_millis: 9,
    }),
    sseFrame('message.completed', { run_id: 'run-4', message_id: 'a-1', content: '一半，另一半' }),
  ]

  it('keeps message frames on the answer channel in their original order', async () => {
    serve(WATCHED)
    await run('ag-1', { streamSteps: true })

    expect(events.map((event) => event.kind)).toEqual(['started', 'delta', 'completed'])
    expect(events[events.length - 1].content).toBe('一半，另一半')
  })

  it('reports progress on its own channel, in arrival order', async () => {
    serve(WATCHED)
    await run('ag-1', { streamSteps: true })

    expect(steps.map((step) => `${step.phase}#${step.sequence}`)).toEqual([
      'step_started#1',
      'step_started#2',
      'tool_started#2',
      'tool_result#2',
    ])
  })

  it('never hands a progress frame to the answer callback, so identity cannot bind there', async () => {
    serve(WATCHED)
    await run('ag-1', { streamSteps: true })

    expect(events.some((event) => String(event.kind).includes('step') || String(event.kind).includes('tool')))
      .toBe(false)
    // The answer callback also never sees a frame it would have to count as ignored.
    expect(events.every((event) => event.kind !== 'unknown')).toBe(true)
  })

  it('leaves a caller that did not ask for progress hearing only the answer', async () => {
    serve(WATCHED)
    await streamMessage({
      sessionId: 'ch-1',
      content: '讲讲 RDB 与 AOF',
      onEvent: (event) => events.push(event),
    })

    expect(events.map((event) => event.kind)).toEqual(['started', 'delta', 'completed'])
    expect(steps).toHaveLength(0)
  })

  it('drops an event on neither channel rather than guessing which one it belongs to', async () => {
    serve([
      sseFrame('message.started', { message_id: 'a-1' }),
      sseFrame('memory_written', { memory_id: 'mem-9' }),
      sseFrame('message.completed', { message_id: 'a-1', content: '答案' }),
    ])
    await run('ag-1', { streamSteps: true })

    expect(events.map((event) => event.kind)).toEqual(['started', 'completed'])
    expect(steps).toHaveLength(0)
    expect(JSON.stringify({ events, steps })).not.toContain('mem-9')
  })

  it('survives a progress frame whose body cannot be read', async () => {
    serve([
      sseFrame('step_started', { run_id: 'run-4', sequence: 1, step_type: 'plan', message: 'model generation 1' }),
      'event: tool_result\ndata: {not json\n\n',
      sseFrame('message.completed', { message_id: 'a-1', content: '答案' }),
    ])
    await run('ag-1', { streamSteps: true })

    expect(steps.map((step) => step.phase)).toEqual(['step_started'])
    expect(events.map((event) => event.kind)).toEqual(['completed'])
  })
})

describe('frames arriving in pieces', () => {
  it('reassembles a progress frame split across reads, including mid-character', async () => {
    const text = sseFrame('tool_result', {
      run_id: 'run-4',
      sequence: 2,
      tool_name: 'memory_search',
      content: '本轮已查阅长期记忆。',
      duration_millis: 7,
    })
    const bytes = new TextEncoder().encode(text)
    // Cut one byte inside the three-byte encoding of a Han character: the decoder must
    // carry it into the next read rather than turn it into U+FFFD.
    const cut = bytes.indexOf(new TextEncoder().encode('本')) + 2
    installFetchStub(() => sseResponse([bytes.slice(0, cut), bytes.slice(cut)]))

    await run('ag-1', { streamSteps: true })

    expect(steps).toHaveLength(1)
    expect(steps[0].content).toBe('本轮已查阅长期记忆。')
    expect(steps[0].durationMillis).toBe(7)
  })
})
