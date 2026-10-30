import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'

import AgentRunPanel from '../src/components/AgentRunPanel.vue'
import AgentStepList from '../src/components/AgentStepList.vue'
import { AgentStage } from '../src/lib/agentRun.js'
import {
  applyStepEvent,
  completeSteps,
  createStepState,
  restoreStepState,
  stepRows,
  toStepEvent,
} from '../src/lib/agentSteps.js'
import { TOOL_NAME_ZH, toolDisplayName } from '../src/lib/toolNames.js'

/**
 * A learner reads the capability, not the identifier the model was given.
 *
 * Every name the server can register is listed here by hand, so adding a tool without
 * adding its display name is a decision someone has to make visibly: the table below is
 * the contract, and the last case pins what happens to a name nobody has listed.
 */
const KNOWN = [
  // the integrated entry and the native entry
  ['knowledge_search', '检索知识库'],
  ['document_read', '阅读文档原文'],
  ['memory_search', '查找记忆'],
  ['topic_memory_read', '读取主题记忆'],
  ['propose_memory', '提议保存记忆'],
  ['read_history_message', '读取历史消息'],
  // the second set of names the native entry shows the model in some modes
  ['search_uploaded_documents', '检索知识库'],
  ['search_past_sessions', '查找记忆'],
  ['save_memory_draft', '提议保存记忆'],
  // the integrated entry's own long-term search
  ['episode_search', '查找记忆'],
]

describe('the tool display table', () => {
  it('names each known tool in plain Chinese', () => {
    for (const [name, text] of KNOWN) {
      expect(toolDisplayName(name), name).toBe(text)
    }
  })

  it('lists exactly the tools above, so a new entry has to be argued for here too', () => {
    expect(Object.keys(TOOL_NAME_ZH).sort()).toEqual(KNOWN.map(([name]) => name).sort())
  })

  it('gives the two spellings of one capability the same words', () => {
    expect(toolDisplayName('knowledge_search')).toBe(toolDisplayName('search_uploaded_documents'))
    expect(toolDisplayName('memory_search')).toBe(toolDisplayName('search_past_sessions'))
    expect(toolDisplayName('propose_memory')).toBe(toolDisplayName('save_memory_draft'))
  })

  it('resolves a name the way the server does: ignoring case and padding', () => {
    expect(toolDisplayName('  Knowledge_Search ')).toBe('检索知识库')
    expect(toolDisplayName('DOCUMENT_READ')).toBe('阅读文档原文')
  })

  it('shows a name nobody has listed exactly as the server sent it', () => {
    expect(toolDisplayName('future_tool')).toBe('future_tool')
    expect(toolDisplayName('Future_Tool ')).toBe('Future_Tool ')
    expect(toolDisplayName('read_everything')).toBe('read_everything')
  })

  it('never turns a name into something blank, and does not trip over inherited keys', () => {
    expect(toolDisplayName('')).toBe('')
    expect(toolDisplayName(undefined)).toBe('')
    expect(toolDisplayName(null)).toBe('')
    expect(toolDisplayName(42)).toBe('')
    for (const name of ['constructor', 'toString', '__proto__', 'hasOwnProperty']) {
      expect(toolDisplayName(name)).toBe(name)
    }
  })

  it('is read-only', () => {
    expect(Object.isFrozen(TOOL_NAME_ZH)).toBe(true)
  })
})

const RUN = 'run-zh'

function frame(event, payload) {
  return toStepEvent({ event, data: JSON.stringify(payload), id: '' })
}

function liveFrames(toolName) {
  return [
    frame('step_started', { run_id: RUN, sequence: 1, step_type: 'plan' }),
    frame('step_started', { run_id: RUN, sequence: 2, step_type: 'tool' }),
    frame('tool_started', { run_id: RUN, sequence: 2, tool_name: toolName }),
    frame('tool_result', { run_id: RUN, sequence: 2, tool_name: toolName, duration_millis: 120 }),
  ]
}

function storedRows(toolName) {
  return [
    { sequence: 1, stepType: 'plan', toolName: '', durationMillis: 0, status: 'completed' },
    { sequence: 2, stepType: 'tool', toolName, durationMillis: 120, status: 'completed' },
  ]
}

function watched(toolName) {
  let state = createStepState({ clientRunId: RUN })
  for (const event of liveFrames(toolName)) state = applyStepEvent(state, event)
  return completeSteps(state)
}

function readBack(toolName) {
  return restoreStepState({ runId: RUN, rows: storedRows(toolName), runStatus: 'completed' })
}

describe('the live panel and the read-back trail', () => {
  it('show every known tool by the same Chinese name, row for row', () => {
    for (const [name, text] of KNOWN) {
      const live = stepRows(watched(name))
      const history = stepRows(readBack(name))

      expect(history, name).toEqual(live)
      expect(live[1].label, name).toBe(`工具调用 · ${text}`)
      expect(live[1].label, name).not.toContain(name)
    }
  })

  it('show an unknown tool as the server named it, in both places', () => {
    const live = stepRows(watched('future_tool'))
    const history = stepRows(readBack('future_tool'))

    expect(history).toEqual(live)
    expect(live[1].label).toBe('工具调用 · future_tool')
  })

  it('leave a step with no tool as it was', () => {
    const rows = stepRows(watched('knowledge_search'))
    expect(rows[0].label).toBe('模型规划')
  })

  it('render the same words in the strip and in the trail', async () => {
    for (const [name, text] of [...KNOWN, ['future_tool', 'future_tool']]) {
      const panel = mount(AgentRunPanel, {
        props: { stage: AgentStage.Completed, steps: stepRows(watched(name)), expanded: true },
      })
      const trail = mount(AgentStepList, { props: { steps: stepRows(readBack(name)) } })
      // The trail is closed until asked for.
      await trail.get('[data-test="agent-history-steps-toggle"]').trigger('click')

      const live = panel.findAll('[data-test="agent-steps"] li')[1].text()
      const history = trail.findAll('[data-test="agent-history-step-list"] li')[1].text()

      expect(history, name).toBe(live)
      expect(live, name).toContain(text)
      if (text !== name) expect(live, name).not.toContain(name)
    }
  })
})
