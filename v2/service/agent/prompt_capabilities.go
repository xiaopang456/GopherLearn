package agent

import "strings"

// The prompt has to agree with the tool table.
//
// The system prompt names tools. The tool table holds tools. For a long time
// nothing tied the two together, and the difference was invisible in the
// product and loud in the logs: the prompt told the model to call
// read_history_message, the table did not hold it, and the model - which had
// been told the tool existed - called it anyway. The call was refused, and the
// refusal failed the whole turn, including whatever evidence the earlier calls
// of the same turn had brought back.
//
// So the prompt is rendered per turn, from what the turn actually holds. A
// paragraph that talks about a tool this turn does not have is dropped, and
// what is left says only what the model can act on.
//
// Two properties matter and they pull in opposite directions:
//
//   - The base prompt must not change when everything is available. The modes
//     with published measurements were measured against these exact bytes, and
//     a prompt that changes under them would move every published number onto
//     bytes nobody measured. So the renderer only ever removes paragraphs: with
//     every capability available, the rendered prompt is the base prompt byte
//     for byte.
//   - The renderer must be able to say "this turn has no document tools". The
//     document tools are removed from the table for two reasons - the question
//     is about this conversation, and the learner said not to look in their
//     material - and in both cases the prompt that talks about searching their
//     documents is a prompt the model cannot obey.
//
// The filtering is per line. The base prompt is written one rule per line, and
// a rule that names a tool this turn does not have is a rule the model cannot
// follow. Filtering per sentence instead would keep more of the surrounding
// rules, but it would also split rules that only make sense whole - the
// source-priority block puts two source rules in one line, and losing both
// because one of them does not apply is a known cost of this choice.

// PromptCapabilities is what one turn's tool table actually holds.
//
// It is computed from the registered tool names, so it cannot disagree with
// the table: the names are the table.
type PromptCapabilities struct {
	// Available is the set of capabilities the registered tools provide.
	Available map[MemoryToolCapability]bool
	// MemoryIntent says the learner asked to save something this turn.
	//
	// The draft tool is on the table every turn, but the paragraph that tells
	// the model to propose saving is only carried when the learner asked for
	// it. Without this the model is prompted to notice saveable moments in
	// every ordinary message, and a learner gets a draft card they did not
	// ask for.
	MemoryIntent bool
}

// CapabilitiesFromToolNames reads the capabilities out of a tool table.
//
// The names are the table, so this cannot drift from it. A name this package
// has never heard of maps to no capability and contributes nothing: a
// deployment may register tools here that have never seen, and a tool nobody
// recognises has no paragraph to remove.
func CapabilitiesFromToolNames(names []string) PromptCapabilities {
	capabilities := PromptCapabilities{Available: map[MemoryToolCapability]bool{}}
	for _, name := range names {
		if capability := CapabilityOf(name); capability != CapabilityUnknown {
			capabilities.Available[capability] = true
		}
	}
	return capabilities
}

// provides reports whether the turn holds a tool for the capability.
func (c PromptCapabilities) provides(capability MemoryToolCapability) bool {
	return c.Available[capability]
}

// promptToolMarkers maps the phrases the prompt uses to the capability each of
// them depends on.
//
// The phrases are the names the tools have on either surface, because the
// prompt has been renamed along with the surface it belongs to. Two entries
// are not tool names: "document_ids" is a parameter of the search tool, and
// "PERSONAL_KB_INSUFFICIENT_EVIDENCE" is the sentinel the search contract
// defines - both belong to the search capability, and a prompt that mentions
// them while the search tool is gone is a prompt the model cannot obey.
var promptToolMarkers = []struct {
	phrase     string
	capability MemoryToolCapability
}{
	{"knowledge_search", CapabilityKnowledgeSearch},
	{"search_uploaded_documents", CapabilityKnowledgeSearch},
	{"document_ids", CapabilityKnowledgeSearch},
	{"PERSONAL_KB_INSUFFICIENT_EVIDENCE", CapabilityKnowledgeSearch},
	{"document_read", CapabilityDocumentRead},
	{"read_history_message", CapabilityHistoryRead},
	{"propose_memory", CapabilityMemoryDraft},
	{"save_memory_draft", CapabilityMemoryDraft},
	{"memory_search", CapabilityEpisodeSearch},
	{"search_past_sessions", CapabilityEpisodeSearch},
	{"topic_memory_read", CapabilityTopicMemoryRead},
	{"browse_summary_segments", CapabilityBrowseSummarySegments},
}

// RenderSystemPrompt drops from the base prompt every rule that talks about a
// capability this turn does not have.
//
// With every capability available the result is the base prompt unchanged,
// byte for byte. That is the property that keeps the modes with published
// measurements on the bytes they were measured against.
func RenderSystemPrompt(base string, capabilities PromptCapabilities) string {
	if base == "" {
		return base
	}
	lines := strings.Split(base, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if promptLineApplies(line, capabilities) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// promptLineApplies reports whether one line of the prompt survives this
// turn's capabilities.
//
// A line survives when it names no capability, and when every capability it
// does name is one the turn holds. The draft capability has a second
// condition: the rule that tells the model to propose saving is only carried
// when the learner asked for something to be saved, because the tool being
// available is not the same thing as being told to use it.
func promptLineApplies(line string, capabilities PromptCapabilities) bool {
	for _, marker := range promptToolMarkers {
		if !strings.Contains(line, marker.phrase) {
			continue
		}
		if marker.capability == CapabilityMemoryDraft {
			if !capabilities.MemoryIntent {
				return false
			}
			if !capabilities.provides(CapabilityMemoryDraft) {
				return false
			}
			continue
		}
		if !capabilities.provides(marker.capability) {
			return false
		}
	}
	return true
}
