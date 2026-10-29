package agent

import (
	"fmt"
	"sort"
	"strings"
)

// STABILIZE-061: the one place that knows which tool name means which
// capability.
//
// Before this file the mapping existed seven times - the per-capability budget
// switch, the repeated-tool ledger's read-only test, three availability probes,
// the memory-search step summary and the draft receipt - each written as a
// switch on a string constant. That was survivable while there was exactly one
// name per capability. STABILIZE-061 introduces a second surface, where the
// same two implementations are exposed under names that say what they do, and a
// seventh copy of the mapping is seven chances for a renamed tool to keep
// working in the model's eyes while silently losing its budget, its duplicate
// check or its confirmation card.
//
// So the name stops being the thing the Runner switches on. The capability is.
// A name is looked up once, and everything downstream asks the capability.
type MemoryToolCapability string

const (
	// CapabilityUnknown is what an unregistered or unrecognised name maps to.
	// It is not an error here: a deployment may register tools this package has
	// never heard of, and they simply get no memory-specific handling.
	CapabilityUnknown MemoryToolCapability = ""
	// CapabilityEpisodeSearch reads long-term memory. Under the episodic-only
	// modes that means earlier sessions and nothing else.
	CapabilityEpisodeSearch MemoryToolCapability = "episode_search"
	// CapabilityTopicMemoryRead reads the server-held topic by exact key.
	CapabilityTopicMemoryRead MemoryToolCapability = "topic_memory_read"
	// CapabilityMemoryDraft creates a pending draft from the current user
	// message. It is the only memory capability with a write side, and it
	// writes nothing a user has not confirmed.
	CapabilityMemoryDraft MemoryToolCapability = "memory_draft"
	// CapabilityKnowledgeSearch searches the learner's uploaded documents.
	CapabilityKnowledgeSearch MemoryToolCapability = "knowledge_search"
	// CapabilityDocumentRead reads one document.
	CapabilityDocumentRead MemoryToolCapability = "document_read"
	// CapabilityHistoryRead reads verbatim messages of an earlier session.
	CapabilityHistoryRead MemoryToolCapability = "history_read"
)

// ToolSurface selects which names the model sees for the same capabilities.
//
// It is a property of the read mode, not of a tool: the implementations behind
// both surfaces are the same code, and that is the point. STABILIZE-060 found
// three different ways of saying "remember this" all routed into Episode
// search, on a surface where the two memory tools were called propose_memory
// and memory_search - names that describe the same family rather than two
// exclusive actions. The explicit surface changes what the model reads and
// nothing else, so a difference in behaviour is attributable to the contract.
type ToolSurface string

const (
	// SurfaceLegacy is every mode that existed before STABILIZE-061. Its names
	// are frozen: modes with published measurements must keep exposing the
	// byte-identical schema those measurements describe.
	SurfaceLegacy ToolSurface = "legacy"
	// SurfaceExplicitActions is agent_memory_explicit_actions_v1 and nothing
	// else.
	SurfaceExplicitActions ToolSurface = "explicit_actions"
	// SurfaceExplicitSources keeps the explicit memory actions and also names
	// document retrieval by the source it can actually reach.
	SurfaceExplicitSources ToolSurface = "explicit_sources"
	// SurfaceSourcePriority is STABILIZE-063's source-priority contract. The
	// names are identical to explicit_sources - a name is not the variable this
	// round studies - while the descriptions the model reads state each
	// source's priority, and the server defers the non-save first tool choice.
	SurfaceSourcePriority ToolSurface = "source_priority"
)

func (s ToolSurface) Valid() bool {
	return s == SurfaceLegacy || s == SurfaceExplicitActions || s == SurfaceExplicitSources ||
		s == SurfaceSourcePriority
}

// The explicit surface's names. They are constants here, beside the mapping,
// rather than in the tool package, because this file is what makes them mean
// anything to the Runner.
const (
	// SaveMemoryDraftToolName says both halves of what the tool does: it saves
	// a draft, and a draft is not a memory. STABILIZE-060's failures were all
	// requests to save, and a model that reads "propose_memory" beside
	// "memory_search" has no contract-level reason to prefer one.
	SaveMemoryDraftToolName = "save_memory_draft"
	// SearchPastSessionsToolName names the only thing it can reach. It does not
	// contain the word memory, deliberately: under the episodic-only contract
	// it cannot read the profile, the topic notes or the learning state, and a
	// name that said "memory" would keep inviting the call this round exists to
	// stop.
	SearchPastSessionsToolName = "search_past_sessions"
	// SearchUploadedDocumentsToolName cannot be mistaken for personal history:
	// it reaches only the learner's uploaded RAG documents.
	SearchUploadedDocumentsToolName = "search_uploaded_documents"
)

// toolCapabilities is the mapping, written once.
//
// The two surfaces share a namespace because their names are disjoint, which is
// checked by a test rather than left to reading. That lets CapabilityOf take a
// bare name: the Runner sees whichever tools were registered and does not need
// to be told which surface produced them.
var toolCapabilities = map[string]MemoryToolCapability{
	memorySearchToolName:            CapabilityEpisodeSearch,
	SearchPastSessionsToolName:      CapabilityEpisodeSearch,
	topicMemoryReadToolName:         CapabilityTopicMemoryRead,
	proposeMemoryToolName:           CapabilityMemoryDraft,
	SaveMemoryDraftToolName:         CapabilityMemoryDraft,
	knowledgeSearchToolName:         CapabilityKnowledgeSearch,
	SearchUploadedDocumentsToolName: CapabilityKnowledgeSearch,
	documentReadToolName:            CapabilityDocumentRead,
	readHistoryMessageToolName:      CapabilityHistoryRead,
}

// surfaceNames is the forward direction, also written once. Every capability
// the model can be given a name for appears under both surfaces; only the two
// memory actions differ between them.
var surfaceNames = map[ToolSurface]map[MemoryToolCapability]string{
	SurfaceLegacy: {
		CapabilityEpisodeSearch:   memorySearchToolName,
		CapabilityTopicMemoryRead: topicMemoryReadToolName,
		CapabilityMemoryDraft:     proposeMemoryToolName,
		CapabilityKnowledgeSearch: knowledgeSearchToolName,
		CapabilityDocumentRead:    documentReadToolName,
		CapabilityHistoryRead:     readHistoryMessageToolName,
	},
	SurfaceExplicitActions: {
		CapabilityEpisodeSearch:   SearchPastSessionsToolName,
		CapabilityTopicMemoryRead: topicMemoryReadToolName,
		CapabilityMemoryDraft:     SaveMemoryDraftToolName,
		CapabilityKnowledgeSearch: knowledgeSearchToolName,
		CapabilityDocumentRead:    documentReadToolName,
		CapabilityHistoryRead:     readHistoryMessageToolName,
	},
	SurfaceExplicitSources: {
		CapabilityEpisodeSearch:   SearchPastSessionsToolName,
		CapabilityTopicMemoryRead: topicMemoryReadToolName,
		CapabilityMemoryDraft:     SaveMemoryDraftToolName,
		CapabilityKnowledgeSearch: SearchUploadedDocumentsToolName,
		CapabilityDocumentRead:    documentReadToolName,
		CapabilityHistoryRead:     readHistoryMessageToolName,
	},
	// The same names as explicit_sources, deliberately: the priority contract
	// varies the descriptions and the server's first-tool behaviour, not the
	// labels. A round that renamed and reprioritised at once could not say
	// which half moved the result.
	SurfaceSourcePriority: {
		CapabilityEpisodeSearch:   SearchPastSessionsToolName,
		CapabilityTopicMemoryRead: topicMemoryReadToolName,
		CapabilityMemoryDraft:     SaveMemoryDraftToolName,
		CapabilityKnowledgeSearch: SearchUploadedDocumentsToolName,
		CapabilityDocumentRead:    documentReadToolName,
		CapabilityHistoryRead:     readHistoryMessageToolName,
	},
}

// CapabilityOf is the only reverse mapping in the codebase.
//
// It normalises the way the repeated-tool ledger already did, because a model
// that pads or capitalises a tool name is still calling that tool and must
// still be charged for it.
func CapabilityOf(name string) MemoryToolCapability {
	return toolCapabilities[strings.ToLower(strings.TrimSpace(name))]
}

// ToolNameFor is the only forward mapping. Registries and evaluators call it
// rather than each holding their own idea of what a capability is called.
func ToolNameFor(capability MemoryToolCapability, surface ToolSurface) (string, error) {
	names, ok := surfaceNames[surface]
	if !ok {
		return "", fmt.Errorf("unknown tool surface %q", surface)
	}
	name, ok := names[capability]
	if !ok {
		return "", fmt.Errorf("capability %q has no tool name on the %s surface", capability, surface)
	}
	return name, nil
}

// MustToolNameFor is ToolNameFor for the callers that pass compile-time
// constants and have nothing useful to do with an error.
func MustToolNameFor(capability MemoryToolCapability, surface ToolSurface) string {
	name, err := ToolNameFor(capability, surface)
	if err != nil {
		panic(err)
	}
	return name
}

// ToolNamesOn lists a surface's names in a stable order, for freezes and
// reports that have to record what the model was shown.
func ToolNamesOn(surface ToolSurface) []string {
	names := make([]string, 0, len(surfaceNames[surface]))
	for _, name := range surfaceNames[surface] {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ReadsLongTermMemory reports whether a capability only reads long-term memory.
//
// This is the rule the repeated-tool progress ledger runs on. The draft
// capability is excluded because it writes, and the document and knowledge
// capabilities because they have their own budgets and their own meaning of
// progress - charging them by this rule would change modes whose results were
// measured without it.
func (c MemoryToolCapability) ReadsLongTermMemory() bool {
	return c == CapabilityEpisodeSearch || c == CapabilityTopicMemoryRead
}

// ForcedToolNamesFor lists every name this configuration's first-tool rule can
// ever bind, on this configuration's surface.
//
// It exists because of BUG-AGENT-214. The Runner forced a tool by name while
// the registry built the tool table from the capability map, and under a new
// surface those two disagreed: the table held search_uploaded_documents and the
// Runner asked the model to call knowledge_search. The model cannot bind a name
// that is not in front of it, so the turn ended before the first model request
// - and the evaluator, seeing no tool call, recorded it as the model choosing
// not to search.
//
// The fix was to resolve the forced name through the same map as everything
// else. This function is the second half: it lets a caller check, before
// spending anything, that every name the rule could force is actually
// registered. A mismatch is a wiring fault, and it must be discoverable without
// a provider call and without reading an error string.
func ForcedToolNamesFor(config RunnerConfig) []string {
	capabilities := forceableCapabilities(config)
	names := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		name, err := ToolNameFor(capability, config.Surface())
		if err != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// UnregisteredForcedTools names the tools this configuration would force under
// a name the registered set does not contain.
//
// The question is asked per capability, not per name, and only about
// capabilities that are actually on the table. Every branch of the first-tool
// rule is guarded by that capability being available, so a capability nobody
// registered can never be forced and its absence is not a fault. What is a
// fault - and what BUG-AGENT-214 was - is a capability that IS registered,
// under a name the rule would not ask for: the tool is right there, the model
// is told to call something else, and the turn dies before its first model
// request.
//
// Empty is the only acceptable answer before a run.
func UnregisteredForcedTools(config RunnerConfig, registered []string) []string {
	present := make(map[string]struct{}, len(registered))
	byCapability := map[MemoryToolCapability]struct{}{}
	for _, name := range registered {
		normalised := strings.ToLower(strings.TrimSpace(name))
		present[normalised] = struct{}{}
		if capability := CapabilityOf(normalised); capability != CapabilityUnknown {
			byCapability[capability] = struct{}{}
		}
	}
	missing := []string{}
	for _, capability := range forceableCapabilities(config) {
		if _, registeredHere := byCapability[capability]; !registeredHere {
			// Not on the table, so the rule's guard means it is never forced.
			continue
		}
		name, err := ToolNameFor(capability, config.Surface())
		if err != nil {
			continue
		}
		if _, ok := present[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// forceableCapabilities is the set of capabilities the first-tool rule can bind
// under this configuration.
func forceableCapabilities(config RunnerConfig) []MemoryToolCapability {
	capabilities := []MemoryToolCapability{CapabilityKnowledgeSearch, CapabilityHistoryRead}
	if config.ForcesMemoryDraftBeforeMemoryRead {
		capabilities = append(capabilities, CapabilityMemoryDraft)
	}
	return capabilities
}
