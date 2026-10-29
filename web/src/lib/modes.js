import { recordDevIssue } from './devlog.js'

/**
 * The product has exactly two user-facing session modes. "Knowledge Q&A" is a
 * per-turn option inside Chat, not a third mode, so nothing here may grow a
 * `knowledge_qa` mode value.
 *
 * The mode is a server-owned, immutable session fact; the client reads it back
 * and never infers it from which button the user last pressed.
 */
export const SessionMode = {
  Chat: 'chat',
  LearningAgent: 'learning_agent',
}

export const SESSION_MODES = [SessionMode.Chat, SessionMode.LearningAgent]

const LABELS = {
  [SessionMode.Chat]: '对话',
  [SessionMode.LearningAgent]: '学习 Agent',
}

export const UNKNOWN_MODE_LABEL = '模式未识别'

/** Shown when the server returns a session with no usable title. */
export const UNTITLED_SESSION = '未命名会话'

/**
 * Titles the composer offers at creation time. They say which mode a session
 * is, so a session still carrying one of them has not been named by anything
 * meaningful yet and may be relabelled from its first question.
 */
export const DEFAULT_SESSION_TITLES = {
  [SessionMode.Chat]: '新对话',
  [SessionMode.LearningAgent]: '学习 Agent',
}

export function isDefaultTitle(mode, title) {
  return title === UNTITLED_SESSION || title === DEFAULT_SESSION_TITLES[mode]
}

export function isKnownMode(value) {
  return typeof value === 'string' && SESSION_MODES.includes(value)
}

/**
 * @returns {string} a known mode, or '' when the server sent nothing we
 * recognise. '' is deliberately *not* mapped onto Learning Agent: the agent
 * path is the one with tools and memory attached, so guessing wrong there would
 * grant a session capabilities nobody authorised.
 */
export function normalizeMode(raw) {
  const value = typeof raw === 'string' ? raw.trim().toLowerCase() : ''
  if (isKnownMode(value)) return value
  if (value !== '') {
    recordDevIssue('session.mode.unknown', { received: raw })
  }
  return ''
}

export function modeLabel(mode) {
  return LABELS[mode] ?? UNKNOWN_MODE_LABEL
}

/** '' behaves like plain chat with no optional capabilities attached. */
export function isChatLike(mode) {
  return mode === SessionMode.Chat
}

export function isLearningAgent(mode) {
  return mode === SessionMode.LearningAgent
}

/**
 * Build the `POST /api/v1/sessions` body. `mode` is required by the contract, so
 * a missing or unknown value is a programming error rather than something to
 * paper over with a default.
 */
export function createSessionBody({ title, mode }) {
  if (!isKnownMode(mode)) {
    recordDevIssue('session.create.invalid_mode', { mode })
    throw new Error('新建会话必须指定已知模式。')
  }
  const body = { mode }
  const cleanTitle = typeof title === 'string' ? title.trim() : ''
  if (cleanTitle) body.title = cleanTitle
  return body
}
