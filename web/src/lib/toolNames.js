/**
 * What a tool is called on screen.
 *
 * The server names a tool by what the model is told to call it, and the same capability
 * has more than one such name: the integrated entry and the native one spell some
 * differently, and the native one exposes a second set of names to the model in some
 * modes. A learner reading "knowledge_search" learns nothing, so a step shows the
 * capability in plain Chinese instead.
 *
 * This is the one table. The live panel and the history trail both read their rows from
 * `stepRows`, which asks `toolDisplayName` here, so a tool cannot be called one thing
 * while a turn runs and another after the page is reloaded.
 *
 * The keys are every name the server can register — see the table in the change that
 * introduced this file. A name this table does not know is shown exactly as the server
 * sent it: a new tool should look new, not be silently called something else, and a
 * step must never show a blank where a name was.
 */
export const TOOL_NAME_ZH = Object.freeze({
  // Search the learner's uploaded documents.
  knowledge_search: '检索知识库',
  search_uploaded_documents: '检索知识库',
  // Read one document in full.
  document_read: '阅读文档原文',
  // Look through earlier sessions and notes the learner has kept.
  memory_search: '查找记忆',
  search_past_sessions: '查找记忆',
  episode_search: '查找记忆',
  // Read the notes kept under one topic.
  topic_memory_read: '读取主题记忆',
  // Draft a memory for the learner to confirm. Nothing is saved until they do.
  propose_memory: '提议保存记忆',
  save_memory_draft: '提议保存记忆',
  // Read earlier messages word for word.
  read_history_message: '读取历史消息',
})

/**
 * The text to show for a tool name.
 *
 * The lookup ignores case and surrounding spaces, the way the server resolves a name
 * (a model that pads or capitalises a name is still calling that tool). A name that is
 * not in the table comes back untouched, and a missing name stays missing — the caller
 * decides what a step without a tool looks like.
 */
export function toolDisplayName(name) {
  if (typeof name !== 'string' || name === '') return ''
  const key = name.trim().toLowerCase()
  return Object.hasOwn(TOOL_NAME_ZH, key) ? TOOL_NAME_ZH[key] : name
}
