import { RunPhase } from './chatStream.js'

/**
 * The Learning Agent's visible turn state.
 *
 * This is a *projection* of the existing conversation state machine, not a new
 * protocol: every stage below is derived from the four SSE events the backend
 * already sends (`message.started` / `delta` / `completed` / `failed`) or from
 * the local cancel. Nothing here invents a step, and nothing here is timed or
 * animated on the client's own clock — until the Phase 4B.1 agent event
 * contract is frozen there is simply no such data to show, so the panel that
 * would carry it stays unrendered (see `AgentRunPanel.vue`).
 */
export const AgentStage = {
  None: 'none',
  Waiting: 'waiting',
  Generating: 'generating',
  Completed: 'completed',
  Failed: 'failed',
  Cancelled: 'cancelled',
}

const STAGES = new Set(Object.values(AgentStage))

/** @returns {string} one of `AgentStage`, from the run of the open session. */
export function deriveAgentStage(run) {
  if (!run) return AgentStage.None
  switch (run.phase) {
    case RunPhase.Idle:
      // Request issued, no event back yet. Reporting it as "generating" would
      // claim a server response that has not arrived.
      return AgentStage.Waiting
    case RunPhase.Streaming:
      return AgentStage.Generating
    case RunPhase.Completed:
      return AgentStage.Completed
    case RunPhase.Failed:
      return AgentStage.Failed
    case RunPhase.Aborted:
      return AgentStage.Cancelled
    default:
      return AgentStage.None
  }
}

export function isAgentStage(value) {
  return STAGES.has(value)
}

/** True only while a turn is actually in progress. */
export function isStageActive(stage) {
  return stage === AgentStage.Waiting || stage === AgentStage.Generating
}

const TITLES = {
  [AgentStage.None]: '',
  [AgentStage.Waiting]: '已发出请求，等待服务开始回答。',
  [AgentStage.Generating]: '正在接收回答。',
  [AgentStage.Completed]: '本轮已完成。',
  [AgentStage.Failed]: '本轮未完成。',
  [AgentStage.Cancelled]: '本轮已取消。',
}

export function agentStageTitle(stage) {
  return TITLES[stage] ?? ''
}

/**
 * Anything the stage title does not already say. Returns '' when the title
 * would only be repeated — the panel then drops the second line entirely, so a
 * completed turn reads as one sentence rather than the same one twice.
 */
export function agentStageDetail(stage, run) {
  if (stage === AgentStage.Failed || stage === AgentStage.Cancelled) {
    const message = run?.error?.message ?? ''
    return message === TITLES[stage] ? '' : message
  }
  if (stage === AgentStage.Generating && run?.text) {
    return '内容会随到达的片段逐步显示。'
  }
  return ''
}
