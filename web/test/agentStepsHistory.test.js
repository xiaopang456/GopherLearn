import { beforeEach, describe, expect, it } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory } from 'vue-router'

import ChatView from '../src/views/ChatView.vue'
import { createAppRouter } from '../src/router/index.js'
import { authStore } from '../src/auth/store.js'
import { setTokenProvider } from '../src/api/http.js'
import {
  applyStepEvent,
  completeSteps,
  createStepState,
  finishSteps,
  restoreStepState,
  StepStatus,
  stepRows,
  toStepEvent,
} from '../src/lib/agentSteps.js'
import { listSessionRuns, NATIVE_RECORD_NOT_FOUND } from '../src/api/agentRuns.js'
import { normalizeMessage } from '../src/api/sessions.js'
import { installFetchStub, jsonResponse } from './helpers/transport.js'

/**
 * A turn that is read back after the fact.
 *
 * The point of these cases is that there is only one trail. A turn that was watched
 * and a turn that is opened again must show the same steps, with the same wording and
 * the same status — otherwise a learner who reloads the page is shown a different
 * history from the one they just watched, and neither can be called wrong.
 *
 * The way that is kept is not a second renderer. The stored rows are turned into the
 * frames they stand for and replayed through the same reducer the live panel uses,
 * so the only thing to prove is that the replay and the live stream agree — which is
 * what the first block below does, field by field.
 *
 * The frames written by hand in this file are the ones the server actually sends.
 * The live progress stream publishes the sequence, the step kind, the tool name and
 * the duration, and nothing else: a step's own sentence and the tool's own sentence
 * never reach the wire, so the read-back cannot carry them either.
 */

const RUN = 'run-4'

function frame(event, payload) {
  return toStepEvent({ event, data: JSON.stringify(payload), id: '' })
}

/** The frames the server sends while a turn runs. */
function liveFrames(runId = RUN) {
  return [
    frame('step_started', { run_id: runId, sequence: 1, step_type: 'plan' }),
    frame('step_started', { run_id: runId, sequence: 2, step_type: 'tool' }),
    frame('tool_started', { run_id: runId, sequence: 2, tool_name: 'knowledge_search' }),
    frame('tool_result', {
      run_id: runId,
      sequence: 2,
      tool_name: 'knowledge_search',
      duration_millis: 412,
    }),
    frame('step_started', { run_id: runId, sequence: 3, step_type: 'tool' }),
    frame('tool_started', { run_id: runId, sequence: 3, tool_name: 'document_read' }),
    // No result for the call that failed: the server sends none for a call that did
    // not finish, and a restored turn must not invent one.
  ]
}

/** The rows the store kept for that same turn. */
function storedRows() {
  return [
    { sequence: 1, stepType: 'plan', toolName: '', durationMillis: 0, status: 'completed' },
    {
      sequence: 2,
      stepType: 'tool',
      toolName: 'knowledge_search',
      durationMillis: 412,
      status: 'completed',
    },
    {
      sequence: 3,
      stepType: 'tool',
      toolName: 'document_read',
      durationMillis: 88,
      status: 'failed',
    },
  ]
}

/** The live panel's state, after the turn ended the way it ended. */
function liveState(frames, terminal) {
  let state = createStepState({ clientRunId: 'run-1' })
  for (const event of frames) state = applyStepEvent(state, event)
  if (terminal === 'completed') return completeSteps(state)
  if (terminal === 'failed') return finishSteps(state, StepStatus.Failed)
  if (terminal === 'cancelled') return finishSteps(state, StepStatus.Cancelled)
  return state
}

describe('the read-back trail and the watched one', () => {
  it('says exactly the same things, field by field', () => {
    const restored = stepRows(
      restoreStepState({ runId: RUN, rows: storedRows(), runStatus: 'failed' }),
    )
    const watched = stepRows(liveState(liveFrames(), 'failed'))

    expect(restored).toEqual(watched)
    // The comparison is on the whole row, not on a summary of it.
    expect(restored.map((row) => row.sequence)).toEqual([1, 2, 3])
    expect(restored[0]).toMatchObject({ label: '模型规划', status: StepStatus.Completed })
    expect(restored[1]).toMatchObject({
      label: '检索知识库',
      status: StepStatus.Completed,
      durationMillis: 412,
    })
  })

  it('agrees on a turn that ended well, too', () => {
    const rows = storedRows().filter((row) => row.status !== 'failed')
    const frames = liveFrames().slice(0, 4)

    const restored = stepRows(restoreStepState({ runId: RUN, rows, runStatus: 'completed' }))
    const watched = stepRows(liveState(frames, 'completed'))

    expect(restored).toEqual(watched)
    expect(restored.every((row) => row.status === StepStatus.Completed)).toBe(true)
  })

  it('shows a step the server recorded as failed as failed', () => {
    const rows = stepRows(
      restoreStepState({ runId: RUN, rows: storedRows(), runStatus: 'failed' }),
    )

    expect(rows[2]).toMatchObject({ status: StepStatus.Failed })
    expect(rows[2].status).not.toBe(StepStatus.Completed)
    // A call that failed never sent a result, so there is no duration to show and
    // no result to claim: the live panel shows neither either.
    expect(rows[2].durationMillis).toBeNull()
    expect(rows[2].detail).toBe('')
  })

  it('shows a step the server recorded as failed even when the turn looks finished', () => {
    const rows = stepRows(
      restoreStepState({ runId: RUN, rows: storedRows(), runStatus: 'completed' }),
    )

    expect(rows[2].status).toBe(StepStatus.Failed)
  })

  it('leaves out the rows the live stream never announced', () => {
    // The answer of a turn is written to the store as a row of its own. It was never
    // a step on screen, so it is not one now either.
    const rows = stepRows(
      restoreStepState({
        runId: RUN,
        rows: [...storedRows(), { sequence: 4, stepType: 'answer', toolName: '', durationMillis: 0, status: 'completed' }],
        runStatus: 'completed',
      }),
    )

    expect(rows.map((row) => row.sequence)).toEqual([1, 2, 3])
  })

  it('has nothing to show for a turn that took no steps', () => {
    const state = restoreStepState({ runId: RUN, rows: [], runStatus: 'completed' })

    expect(stepRows(state)).toEqual([])
  })

  it('reads nothing but the fields the read-back carries', () => {
    const state = restoreStepState({
      runId: RUN,
      rows: storedRows(),
      runStatus: 'failed',
    })

    expect(Object.keys(state.steps[0]).sort()).toEqual([
      'detail',
      'durationMillis',
      'kind',
      'label',
      'sequence',
      'status',
      'toolName',
    ])
  })
})

describe('reading the store', () => {
  let calls

  beforeEach(() => {
    calls = []
    setTokenProvider(() => 'tok.runs')
  })

  it('asks the native surface, with the bearer in a header', async () => {
    const stub = installFetchStub((url, init) => {
      calls.push({ url: String(url), init })
      return jsonResponse({ status_code: 1000, runs: [], truncated: false })
    })

    await listSessionRuns('s-1')

    expect(calls[0].url).toBe('/api/v1/AI/agent/sessions/s-1/runs')
    expect(calls[0].url).not.toContain('tok.runs')
    expect(calls[0].init.headers.authorization).toBe('Bearer tok.runs')
    stub.fetch
  })

  it('escapes the ids it puts in the path', async () => {
    const stub = installFetchStub((url) => {
      calls.push({ url: String(url) })
      return jsonResponse({ status_code: 1000, runs: [], truncated: false })
    })

    await listSessionRuns('a/b')

    expect(calls[0].url).toBe('/api/v1/AI/agent/sessions/a%2Fb/runs')
    stub.fetch
  })

  it('reads the runs and the steps they took', async () => {
    installFetchStub(() =>
      jsonResponse({
        status_code: 1000,
        truncated: true,
        runs: [
          {
            run_id: RUN,
            status: 'completed',
            execution_kind: 'agent_chat',
            started_at: '2026-10-30T12:00:00Z',
            steps: [
              {
                sequence: 1,
                step_type: 'tool',
                tool_name: 'knowledge_search',
                duration_millis: 412,
                status: 'completed',
              },
            ],
          },
        ],
      }),
    )

    const payload = await listSessionRuns('s-1')

    expect(payload.truncated).toBe(true)
    expect(payload.runs).toHaveLength(1)
    expect(payload.runs[0]).toMatchObject({ runId: RUN, status: 'completed' })
    expect(payload.runs[0].steps[0]).toMatchObject({
      sequence: 1,
      stepType: 'tool',
      toolName: 'knowledge_search',
      durationMillis: 412,
      status: 'completed',
    })
  })

  it('reports a session that is not the caller’s as not found, not as a 404', async () => {
    // The native surface answers every outcome with HTTP 200 and a status code, so
    // "not yours" and "does not exist" are the same answer and never a missing route.
    installFetchStub(() => jsonResponse({ status_code: 2009, status_msg: '记录不存在' }))

    const error = await listSessionRuns('s-1').then(
      () => null,
      (thrown) => thrown,
    )

    expect(error).not.toBeNull()
    expect(error.nativeCode).toBe(NATIVE_RECORD_NOT_FOUND)
    expect(error.status).toBe(200)
  })
})

describe('the run id on a message', () => {
  it('is read when the server sent one', () => {
    expect(normalizeMessage({ role: 'assistant', run_id: RUN }, 'm-1').runId).toBe(RUN)
  })

  it('is empty for a message of an ordinary conversation', () => {
    expect(normalizeMessage({ role: 'assistant' }, 'm-1').runId).toBe('')
  })
})

const AGENT = { id: 'ag-1', title: '学习会话', mode: 'learning_agent' }

/** The history of one session, and the runs the store kept for it. */
function serve({ messages, runs, status = 1000, truncated = false }) {
  installFetchStub((url) => {
    const path = new URL(String(url), 'https://api.test').pathname
    if (path === '/api/v1/sessions') {
      return jsonResponse({ data: { items: [AGENT] }, error: null })
    }
    if (/\/messages$/.test(path)) {
      return jsonResponse({ data: { items: messages }, error: null })
    }
    if (/\/runs$/.test(path)) {
      return jsonResponse({ status_code: status, runs, truncated })
    }
    return jsonResponse({ data: null, error: { code: 'not_found', message: 'no' } }, { status: 404 })
  })
}

async function openSession() {
  const router = createAppRouter({ authStore, history: createMemoryHistory() })
  await router.push('/chat/ag-1')
  await router.isReady()
  const app = mount(ChatView, { attachTo: document.body, global: { plugins: [router] } })
  await flushPromises()
  await flushPromises()
  return app
}

describe('the trail under an old answer', () => {
  beforeEach(() => {
    authStore.adopt('tok.ui', { id: 'user-1', email: 'ui@example.test', displayName: '' })
    setTokenProvider(() => 'tok.ui')
  })

  it('hangs under the answer of its own turn, and only there', async () => {
    serve({
      messages: [
        { id: 'q-1', role: 'user', content: '讲讲 RDB', created_at: '2026-10-30T12:00:00Z', run_id: RUN },
        { id: 'a-1', role: 'assistant', content: 'RDB 是…', created_at: '2026-10-30T12:00:03Z', run_id: RUN },
      ],
      runs: [
        {
          run_id: RUN,
          status: 'completed',
          steps: [
            { sequence: 1, step_type: 'plan', tool_name: '', duration_millis: 0, status: 'completed' },
            {
              sequence: 2,
              step_type: 'tool',
              tool_name: 'knowledge_search',
              duration_millis: 412,
              status: 'completed',
            },
          ],
        },
      ],
    })

    const app = await openSession()

    // The question of that turn carries the same run id, and shows no trail of its
    // own: one turn, one trail.
    expect(app.find('[data-test="message-q-1"]').find('[data-test="agent-history-steps"]').exists()).toBe(false)

    const answer = app.find('[data-test="message-a-1"]')
    const trail = answer.find('[data-test="agent-history-steps"]')
    expect(trail.exists()).toBe(true)
    // Closed until it is asked for.
    expect(trail.find('[data-test="agent-history-step-list"]').exists()).toBe(false)

    await trail.find('[data-test="agent-history-steps-toggle"]').trigger('click')
    await flushPromises()

    const rows = answer.findAll('[data-test="agent-history-step-list"] li')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('模型规划')
    expect(rows[1].text()).toContain('检索知识库')
    expect(rows[1].text()).toContain('已完成')
    expect(rows[1].text()).toContain('412 毫秒')
    app.unmount()
  })

  it('shows nothing for a turn that failed before it produced an answer', async () => {
    // The turn wrote a question and no answer, so its steps have nothing to hang
    // under — the same as the live turn, which never drew them either.
    serve({
      messages: [
        { id: 'q-2', role: 'user', content: '讲讲 AOF', created_at: '2026-10-30T12:10:00Z', run_id: 'run-9' },
      ],
      runs: [
        {
          run_id: 'run-9',
          status: 'failed',
          steps: [
            {
              sequence: 1,
              step_type: 'tool',
              tool_name: 'document_read',
              duration_millis: 88,
              status: 'failed',
            },
          ],
        },
      ],
    })

    const app = await openSession()

    expect(app.find('[data-test="agent-history-steps"]').exists()).toBe(false)
    app.unmount()
  })

  it('shows nothing for a turn that took no steps', async () => {
    serve({
      messages: [
        { id: 'q-3', role: 'user', content: '你好', created_at: '2026-10-30T12:20:00Z', run_id: 'run-8' },
        { id: 'a-3', role: 'assistant', content: '你好。', created_at: '2026-10-30T12:20:01Z', run_id: 'run-8' },
      ],
      runs: [{ run_id: 'run-8', status: 'completed', steps: [] }],
    })

    const app = await openSession()

    expect(app.find('[data-test="agent-history-steps"]').exists()).toBe(false)
    app.unmount()
  })

  it('shows the failed step of a turn as failed', async () => {
    serve({
      messages: [
        { id: 'q-4', role: 'user', content: '讲讲 AOF', created_at: '2026-10-30T12:30:00Z', run_id: 'run-7' },
        { id: 'a-4', role: 'assistant', content: '回答', created_at: '2026-10-30T12:30:02Z', run_id: 'run-7' },
      ],
      runs: [
        {
          run_id: 'run-7',
          status: 'failed',
          steps: [
            {
              sequence: 1,
              step_type: 'tool',
              tool_name: 'document_read',
              duration_millis: 88,
              status: 'failed',
            },
          ],
        },
      ],
    })

    const app = await openSession()
    const trail = app.find('[data-test="message-a-4"]').find('[data-test="agent-history-steps"]')
    await trail.find('[data-test="agent-history-steps-toggle"]').trigger('click')
    await flushPromises()

    const row = app.find('[data-test="agent-history-step-list"] li')
    expect(row.text()).toContain('阅读文档原文')
    expect(row.text()).toContain('未成功')
    expect(row.text()).not.toContain('已完成')
    app.unmount()
  })

  it('leaves the conversation alone when the store hands the session over to nobody', async () => {
    serve({
      messages: [
        { id: 'q-5', role: 'user', content: '你好', created_at: '2026-10-30T12:40:00Z' },
        { id: 'a-5', role: 'assistant', content: '你好。', created_at: '2026-10-30T12:40:01Z' },
      ],
      runs: [],
      status: 2009,
    })

    const app = await openSession()

    // The answers are still there: a trail that cannot be read is not worth an error
    // card over the conversation.
    expect(app.text()).toContain('你好。')
    expect(app.find('[data-test="agent-history-steps"]').exists()).toBe(false)
    app.unmount()
  })

  it('says so when the server left turns out of the list it read back', async () => {
    serve({
      messages: [
        { id: 'q-6', role: 'user', content: '你好', created_at: '2026-10-30T12:50:00Z' },
        { id: 'a-6', role: 'assistant', content: '你好。', created_at: '2026-10-30T12:50:01Z' },
      ],
      runs: [],
      truncated: true,
    })

    const app = await openSession()

    // A session whose older turns were left out must not look like a session that
    // never had any steps.
    expect(app.find('[data-test="runs-truncated"]').exists()).toBe(true)
    expect(app.find('[data-test="runs-truncated"]').text()).toContain('最近 100 轮')
    app.unmount()
  })

  it('says nothing about truncation when the list was the whole story', async () => {
    serve({
      messages: [
        { id: 'q-7', role: 'user', content: '你好', created_at: '2026-10-30T13:00:00Z' },
        { id: 'a-7', role: 'assistant', content: '你好。', created_at: '2026-10-30T13:00:01Z' },
      ],
      runs: [],
    })

    const app = await openSession()

    expect(app.find('[data-test="runs-truncated"]').exists()).toBe(false)
    app.unmount()
  })
})
