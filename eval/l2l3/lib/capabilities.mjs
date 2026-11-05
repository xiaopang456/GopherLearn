// What a tool is, independent of the name the model was given.
//
// One capability has more than one name: the integrated entry and the native entry
// spell some differently, and the native entry shows the model a second set of
// names in some modes. An expectation such as "this question needs a knowledge
// search" is about the capability, so every assertion maps names through this table
// and never compares a raw name.
//
// This is the one table. It mirrors CapabilityOf in v2/service/agent/tool_capability.go
// and the tool names of the integrated entry in internal/agent; the drift tests read
// those files and fail when a name appears there that is missing here.

export const Capability = Object.freeze({
  KnowledgeSearch: 'knowledge_search',
  DocumentRead: 'document_read',
  EpisodeSearch: 'episode_search',
  TopicMemoryRead: 'topic_memory_read',
  MemoryDraft: 'memory_draft',
  HistoryRead: 'history_read',
  BrowseSummarySegments: 'browse_summary_segments',
  Unknown: 'unknown',
})

export const KNOWN_CAPABILITIES = Object.freeze([
  Capability.KnowledgeSearch,
  Capability.DocumentRead,
  Capability.EpisodeSearch,
  Capability.TopicMemoryRead,
  Capability.MemoryDraft,
  Capability.HistoryRead,
  Capability.BrowseSummarySegments,
])

export const TOOL_CAPABILITIES = Object.freeze({
  knowledge_search: Capability.KnowledgeSearch,
  search_uploaded_documents: Capability.KnowledgeSearch,
  document_read: Capability.DocumentRead,
  memory_search: Capability.EpisodeSearch,
  search_past_sessions: Capability.EpisodeSearch,
  episode_search: Capability.EpisodeSearch,
  topic_memory_read: Capability.TopicMemoryRead,
  propose_memory: Capability.MemoryDraft,
  save_memory_draft: Capability.MemoryDraft,
  read_history_message: Capability.HistoryRead,
  browse_summary_segments: Capability.BrowseSummarySegments,
})

/**
 * The capability a tool name stands for. Names are resolved the way the server
 * resolves them — ignoring case and surrounding spaces — and a name this table does
 * not know is `unknown`: recorded, but never counted for or against an expectation.
 */
export function capabilityOf(name) {
  if (typeof name !== 'string') return Capability.Unknown
  const key = name.trim().toLowerCase()
  return Object.hasOwn(TOOL_CAPABILITIES, key) ? TOOL_CAPABILITIES[key] : Capability.Unknown
}

export function isKnownCapability(value) {
  return KNOWN_CAPABILITIES.includes(value)
}
