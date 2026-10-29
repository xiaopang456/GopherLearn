package agent

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"GopherAI/common/memoryrecall"
)

// STABILIZE-063 source-priority contract: the three halves that must move
// together are pinned here - the prompt states the priority at runtime, the
// server defers the non-save first tool choice through the config field, and
// the tool names stay source-specific. The prompt assertions deliberately read
// SystemPromptFor at runtime so a broken replacement cannot hide behind a
// constant that was never applied.

func TestSourcePriorityModePredicates(t *testing.T) {
	mode := memoryrecall.ModeAgentMemorySourcePriorityV1
	if !mode.UsesExplicitKnowledgeSourceName() {
		t.Fatal("source-priority mode must use the explicit knowledge source name")
	}
	if !mode.UsesExplicitMemoryActionNames() {
		t.Fatal("source-priority mode must use the explicit memory action names")
	}
	if !mode.UsesMemorySearchTool() {
		t.Fatal("source-priority mode must register memory_search")
	}
	if !mode.RestrictsMemorySearchToEpisodic() {
		t.Fatal("source-priority mode must restrict memory search to episodic")
	}
	if !mode.GovernsAssertionTime() {
		t.Fatal("source-priority mode must govern assertion time")
	}
	if !mode.OrdersMemoryDraftBeforeMemoryRead() {
		t.Fatal("source-priority mode must order the draft before the read")
	}
	// The mode predicate exists so wiring can choose the right config; the
	// deferral itself is the config field ForcesKnowledgeSearchWithMemoryTool.
	if !AgentMemorySourcePriorityV1RunnerConfig().DefersNonSaveFirstTool() {
		t.Fatal("the source-priority config must defer the non-save first tool")
	}
	// The production default is untouched.
	if memoryrecall.DefaultReadMode() != memoryrecall.ModeLegacyV2 {
		t.Fatal("the production default moved")
	}
}

func TestSourcePrioritySurfaceKeepsSourceSpecificNames(t *testing.T) {
	if MustToolNameFor(CapabilityEpisodeSearch, SurfaceSourcePriority) != SearchPastSessionsToolName {
		t.Fatal("episode search must stay search_past_sessions on the source-priority surface")
	}
	if MustToolNameFor(CapabilityKnowledgeSearch, SurfaceSourcePriority) != SearchUploadedDocumentsToolName {
		t.Fatal("document retrieval must stay search_uploaded_documents on the source-priority surface")
	}
	if MustToolNameFor(CapabilityMemoryDraft, SurfaceSourcePriority) != SaveMemoryDraftToolName {
		t.Fatal("the draft must stay save_memory_draft on the source-priority surface")
	}
	if !SurfaceSourcePriority.Valid() {
		t.Fatal("the source-priority surface must be valid")
	}
	// The legacy surface keeps its frozen names.
	if MustToolNameFor(CapabilityKnowledgeSearch, SurfaceLegacy) != "knowledge_search" {
		t.Fatal("the legacy knowledge_search name moved")
	}
}

// sourcePriorityOnlyFragments are the sentences the STABILIZE-063 contract
// adds. They must be present in the source-priority prompt and absent from the
// explicit-sources prompt, which is the byte-visible difference between the
// two. The explicit prompt is a rename of the same memory-tool base, so
// generic phrases like "the learner's own past" appear in both and cannot be
// used to tell them apart.
var sourcePriorityOnlyFragments = []string{
	"decide which source a question needs by what it asks about",
	"A technical subject alone is not a reason to search uploaded documents",
	"A question that genuinely needs both may call both tools",
	"it is the authoritative source for that and no document can replace it",
}

// sourcePriorityGeneralFragments must exist in the source-priority prompt but
// are not specific to it.
var sourcePriorityGeneralFragments = []string{
	"the learner's own past",
	"search_past_sessions",
	"search_uploaded_documents",
	"uploaded documents",
}

// The explicit-source prompt was already a mechanical rename, so the sentence
// that slipped through to v1.4.3 reads "call search_uploaded_documents before
// answering substantive knowledge..." - the technical-first trigger. A
// source-priority prompt must not contain it.
const technicalFirstSentence = "call search_uploaded_documents before answering substantive knowledge, technical, project, or learning questions"

// containsFold is case-insensitive containment: a rule fragment written in
// sentence case must match the prompt whether it lands at the start of a
// sentence (capitalised) or mid-sentence.
func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func TestSourcePriorityPromptStatesTheWholeContract(t *testing.T) {
	prompt := SystemPromptFor(memoryrecall.ModeAgentMemorySourcePriorityV1)
	for _, fragment := range sourcePriorityOnlyFragments {
		if !containsFold(prompt, fragment) {
			t.Fatalf("the source-priority prompt is missing %q", fragment)
		}
	}
	for _, fragment := range sourcePriorityGeneralFragments {
		if !containsFold(prompt, fragment) {
			t.Fatalf("the source-priority prompt is missing %q", fragment)
		}
	}
	if containsFold(prompt, technicalFirstSentence) {
		t.Fatalf("the source-priority prompt kept the technical-first sentence: %q", technicalFirstSentence)
	}

	// The explicit-sources prompt is unchanged: it still carries the frozen
	// technical-first sentence and none of the source-priority-only rules.
	explicit := SystemPromptFor(memoryrecall.ModeAgentMemoryExplicitSourcesV1)
	if !containsFold(explicit, technicalFirstSentence) {
		t.Fatal("the explicit-sources prompt lost its frozen technical-first sentence")
	}
	for _, fragment := range sourcePriorityOnlyFragments {
		if containsFold(explicit, fragment) {
			t.Fatalf("the explicit-sources prompt gained the source-priority fragment %q", fragment)
		}
	}

	// And the two prompts are actually different bytes.
	digest := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return fmt.Sprintf("%x", sum[:])
	}
	if digest(prompt) == digest(explicit) {
		t.Fatal("the source-priority prompt is byte-identical to the explicit-sources prompt")
	}
}

func TestSourcePriorityConfigDefersKnowledgeForcing(t *testing.T) {
	config := AgentMemorySourcePriorityV1RunnerConfig()
	if config.ForcesKnowledgeSearchWithMemoryTool {
		t.Fatal("the source-priority config must not force knowledge first")
	}
	if !config.ForcesMemoryDraftBeforeMemoryRead {
		t.Fatal("the source-priority config must keep the draft-first rule")
	}
	if config.ToolSurface != SurfaceSourcePriority {
		t.Fatalf("surface = %q, want source_priority", config.ToolSurface)
	}
	// The envelope's fuse values are inherited unchanged from STABILIZE-061.
	base := AgentMemoryExplicitSourcesV1RunnerConfig()
	if config.MaxModelCalls != base.MaxModelCalls || config.MaxToolCalls != base.MaxToolCalls ||
		config.PerTool != base.PerTool {
		t.Fatal("the source-priority envelope changed a fuse value")
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("the source-priority config does not validate: %v", err)
	}
	// The explicit-sources config is untouched.
	if !AgentMemoryExplicitSourcesV1RunnerConfig().ForcesKnowledgeSearchWithMemoryTool {
		t.Fatal("the explicit-sources config lost its knowledge forcing")
	}
}
