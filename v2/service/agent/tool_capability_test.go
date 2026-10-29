package agent

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"GopherAI/common/memoryrecall"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

// STABILIZE-061, the mapping layer.
//
// These tests are about one property: that renaming a tool cannot make it lose
// its budget, its duplicate check, its confirmation card or its step summary.
// Before the capability map that property held only by seven separate switch
// statements agreeing, and nothing checked that they did.

// 1. Every name either surface can show maps to a capability, and the two
// surfaces never disagree about what a name means.
func TestEveryToolNameOnEverySurfaceHasACapability(t *testing.T) {
	for _, surface := range []ToolSurface{SurfaceLegacy, SurfaceExplicitActions, SurfaceExplicitSources} {
		names := ToolNamesOn(surface)
		if len(names) == 0 {
			t.Fatalf("surface %s exposes no names", surface)
		}
		for _, name := range names {
			if CapabilityOf(name) == CapabilityUnknown {
				t.Errorf("%s on the %s surface maps to no capability", name, surface)
			}
		}
	}
}

// 2. The mapping round-trips: a capability's name on a surface reads back as
// that capability. This is what stops a rename from being half-applied.
func TestCapabilityNamesRoundTrip(t *testing.T) {
	capabilities := []MemoryToolCapability{
		CapabilityEpisodeSearch, CapabilityTopicMemoryRead, CapabilityMemoryDraft,
		CapabilityKnowledgeSearch, CapabilityDocumentRead, CapabilityHistoryRead,
	}
	for _, surface := range []ToolSurface{SurfaceLegacy, SurfaceExplicitActions, SurfaceExplicitSources} {
		for _, capability := range capabilities {
			name, err := ToolNameFor(capability, surface)
			if err != nil {
				t.Fatalf("%s has no name on %s: %v", capability, surface, err)
			}
			if got := CapabilityOf(name); got != capability {
				t.Errorf("%s on %s is called %s, which reads back as %s", capability, surface, name, got)
			}
		}
	}
}

// 3. The two surfaces share one namespace, so CapabilityOf can take a bare
// name. That is only safe while the names that differ are actually disjoint.
func TestTheTwoSurfacesDoNotReuseANameForDifferentCapabilities(t *testing.T) {
	if MustToolNameFor(CapabilityEpisodeSearch, SurfaceLegacy) ==
		MustToolNameFor(CapabilityEpisodeSearch, SurfaceExplicitActions) {
		t.Fatal("the explicit surface did not rename the episode search")
	}
	if MustToolNameFor(CapabilityMemoryDraft, SurfaceLegacy) ==
		MustToolNameFor(CapabilityMemoryDraft, SurfaceExplicitActions) {
		t.Fatal("the explicit surface did not rename the memory draft")
	}
	// And the new names are not the old names of some other capability, which
	// would silently reroute a call.
	for _, name := range []string{SaveMemoryDraftToolName, SearchPastSessionsToolName} {
		if _, frozen := map[string]bool{
			memorySearchToolName: true, proposeMemoryToolName: true, topicMemoryReadToolName: true,
			knowledgeSearchToolName: true, documentReadToolName: true, readHistoryMessageToolName: true,
		}[name]; frozen {
			t.Errorf("%s collides with a legacy tool name", name)
		}
	}
}

// 4. Unknown names stay unknown rather than falling into a capability. A
// deployment may register tools this package has never heard of.
func TestUnknownToolNamesGetNoCapability(t *testing.T) {
	for _, name := range []string{"", "  ", "memory", "save_memory", "search_memory", "propose"} {
		if got := CapabilityOf(name); got != CapabilityUnknown {
			t.Errorf("CapabilityOf(%q) = %q, want unknown", name, got)
		}
	}
	if CapabilityOf("  Save_Memory_Draft  ") != CapabilityMemoryDraft {
		t.Fatal("a padded, capitalised name lost its capability")
	}
}

// 5. The per-capability budget follows the capability, not the name. A renamed
// tool budgeted at zero looks exactly like a tool the model never called.
func TestPerToolBudgetChargesTheCapabilityUnderEitherName(t *testing.T) {
	limits := ToolCallLimits{MemorySearch: 2, TopicMemoryRead: 1, KnowledgeSearch: 3,
		DocumentRead: 2, ProposeMemory: 1}
	for name, want := range map[string]int{
		memorySearchToolName:            2,
		SearchPastSessionsToolName:      2,
		proposeMemoryToolName:           1,
		SaveMemoryDraftToolName:         1,
		knowledgeSearchToolName:         3,
		SearchUploadedDocumentsToolName: 3,
		documentReadToolName:            2,
		topicMemoryReadToolName:         1,
		"something_else":                0,
	} {
		if got := limits.limitFor(name); got != want {
			t.Errorf("limitFor(%s) = %d, want %d", name, got, want)
		}
	}
}

// 6. The repeated-tool progress ledger charges the renamed read tool and still
// leaves the write tool alone.
func TestProgressLedgerFollowsTheCapability(t *testing.T) {
	for name, want := range map[string]bool{
		memorySearchToolName:       true,
		SearchPastSessionsToolName: true,
		topicMemoryReadToolName:    true,
		proposeMemoryToolName:      false,
		SaveMemoryDraftToolName:    false,
		knowledgeSearchToolName:    false,
		readHistoryMessageToolName: false,
	} {
		if got := readOnlyMemoryTool(name); got != want {
			t.Errorf("readOnlyMemoryTool(%s) = %v, want %v", name, got, want)
		}
	}
}

// 7. Availability is detected by capability, so the explicit surface's tools
// are seen as present.
func TestAvailabilityProbesSeeRenamedTools(t *testing.T) {
	registered := []tool.BaseTool{
		stubNamedTool(t, SearchPastSessionsToolName), stubNamedTool(t, SaveMemoryDraftToolName),
	}
	for capability, want := range map[MemoryToolCapability]bool{
		CapabilityEpisodeSearch:   true,
		CapabilityMemoryDraft:     true,
		CapabilityKnowledgeSearch: false,
		CapabilityHistoryRead:     false,
	} {
		got, err := hasRegisteredCapability(context.Background(), registered, capability)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s available = %v, want %v", capability, got, want)
		}
	}
}

// 8. The mapping has exactly one source. This is a structural assertion because
// the property is about the codebase rather than about a run: a third copy of
// "memory_search means episode search" added anywhere in service/agent would
// compile, pass every behavioural test, and drift on the next rename.
func TestOnlyTheCapabilityMapKnowsWhichNameMeansWhat(t *testing.T) {
	fileSet := token.NewFileSet()
	packageFiles, err := parser.ParseDir(fileSet, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Comparing a tool-name constant against something is how the old switches
	// were written; the map file is where that is now allowed to happen.
	names := map[string]bool{
		"memorySearchToolName": true, "proposeMemoryToolName": true,
		"SaveMemoryDraftToolName": true, "SearchPastSessionsToolName": true,
	}
	for _, parsed := range packageFiles {
		for path, file := range parsed.Files {
			if strings.HasSuffix(path, "tool_capability.go") {
				continue
			}
			ast.Inspect(file, func(node ast.Node) bool {
				binary, ok := node.(*ast.BinaryExpr)
				if !ok || (binary.Op != token.EQL && binary.Op != token.NEQ) {
					return true
				}
				for _, side := range []ast.Expr{binary.X, binary.Y} {
					identifier, ok := side.(*ast.Ident)
					if ok && names[identifier.Name] {
						t.Errorf("%s compares %s directly; ask CapabilityOf instead",
							fileSet.Position(binary.Pos()), identifier.Name)
					}
				}
				return true
			})
		}
	}
}

// 9. The new mode is a mode, complete and separate, and it does not become the
// default by existing.
func TestExplicitActionsModeIsCompleteAndNotDefault(t *testing.T) {
	mode := memoryrecall.ModeAgentMemoryExplicitActionsV1
	if !mode.Valid() {
		t.Fatal("the mode does not validate")
	}
	parsed, err := memoryrecall.ParseReadMode("agent_memory_explicit_actions_v1")
	if err != nil || parsed != mode {
		t.Fatalf("ParseReadMode = %v, %v", parsed, err)
	}
	if memoryrecall.DefaultReadMode() == mode {
		t.Fatal("the new mode became the production default")
	}
	listed := false
	for _, name := range memoryrecall.ModeNames() {
		if name == mode {
			listed = true
		}
	}
	if !listed {
		t.Fatal("the mode is not listed in ModeNames")
	}
	for name, got := range map[string]bool{
		"registers the memory tool":  mode.UsesMemorySearchTool(),
		"episodic only":              mode.RestrictsMemorySearchToEpisodic(),
		"governs assertion time":     mode.GovernsAssertionTime(),
		"user-confirmed topic":       mode.UsesUserConfirmedTopicContext(),
		"explicit action names":      mode.UsesExplicitMemoryActionNames(),
		"orders the draft first":     mode.OrdersMemoryDraftBeforeMemoryRead(),
		"injects nothing on its own": !mode.AutoInjectsTopicAndEpisodeMemory(),
	} {
		if !got {
			t.Errorf("the mode %s = false", name)
		}
	}
	if mode.DefersFirstToolChoiceToModel() {
		t.Fatal("the mode suppressed the server's knowledge-first rule")
	}
	if mode.UsesTopicMemoryReadTool() {
		t.Fatal("the mode registered topic_memory_read, which STABILIZE-060 did not")
	}
}

// 10. Every mode that existed before this round keeps the names, the surface
// and the ordering its published measurements describe.
func TestOlderModesKeepTheirToolSurface(t *testing.T) {
	for _, mode := range memoryrecall.ModeNames() {
		if mode == memoryrecall.ModeAgentMemoryExplicitActionsV1 ||
			mode == memoryrecall.ModeAgentMemoryExplicitSourcesV1 {
			continue
		}
		if mode.UsesExplicitMemoryActionNames() {
			t.Errorf("%s was moved onto the explicit surface", mode)
		}
		if mode.OrdersMemoryDraftBeforeMemoryRead() {
			t.Errorf("%s had its first tool reordered", mode)
		}
	}
}

func TestExplicitSourcesModeChangesOnlyTheKnowledgeSurface(t *testing.T) {
	mode := memoryrecall.ModeAgentMemoryExplicitSourcesV1
	if !mode.Valid() || memoryrecall.DefaultReadMode() == mode {
		t.Fatal("explicit-sources mode is invalid or became the default")
	}
	parsed, err := memoryrecall.ParseReadMode(string(mode))
	if err != nil || parsed != mode {
		t.Fatalf("ParseReadMode = %v, %v", parsed, err)
	}
	if !mode.UsesExplicitMemoryActionNames() || !mode.UsesExplicitKnowledgeSourceName() ||
		!mode.OrdersMemoryDraftBeforeMemoryRead() || !mode.RestrictsMemorySearchToEpisodic() ||
		!mode.GovernsAssertionTime() || !mode.UsesUserConfirmedTopicContext() {
		t.Fatal("explicit-sources mode lost an inherited boundary")
	}
	old := AgentMemoryExplicitActionsV1RunnerConfig()
	got := AgentMemoryExplicitSourcesV1RunnerConfig()
	old.ToolSurface, got.ToolSurface = "", ""
	if old != got {
		t.Fatalf("run envelope changed beyond the surface: %+v vs %+v", old, got)
	}
	if AgentMemoryExplicitSourcesV1RunnerConfig().Surface() != SurfaceExplicitSources {
		t.Fatal("new mode did not select the explicit-sources surface")
	}
}

// 11. The run envelope of the new mode is the old one with two fields added and
// no number changed. A round that also moved the fuses could not say which
// change moved the result.
func TestExplicitActionsEnvelopeChangesNoBudget(t *testing.T) {
	base := AgentMemoryToolRunnerConfig()
	explicit := AgentMemoryExplicitActionsV1RunnerConfig()
	if explicit.PerTool != base.PerTool {
		t.Fatalf("per-tool budget changed: %+v vs %+v", explicit.PerTool, base.PerTool)
	}
	if explicit.MaxToolCalls != base.MaxToolCalls || explicit.MaxModelCalls != base.MaxModelCalls ||
		explicit.MaxGraphSteps != base.MaxGraphSteps || explicit.Deadline != base.Deadline {
		t.Fatalf("run fuses changed: %+v vs %+v", explicit, base)
	}
	if explicit.Surface() != SurfaceExplicitActions || !explicit.ForcesMemoryDraftBeforeMemoryRead {
		t.Fatal("the explicit envelope does not select the explicit surface")
	}
	if err := explicit.Validate(); err != nil {
		t.Fatalf("the explicit envelope does not validate: %v", err)
	}
}

// 12. The ordering rule cannot be switched on where it would change a frozen
// mode, and cannot be switched on without a draft budget to spend.
func TestOrderingRuleFailsClosedOffTheExplicitSurface(t *testing.T) {
	config := AgentMemoryToolRunnerConfig()
	config.ForcesMemoryDraftBeforeMemoryRead = true
	if err := config.Validate(); err == nil {
		t.Fatal("the legacy surface accepted the draft-first rule")
	}
	config = AgentMemoryExplicitActionsV1RunnerConfig()
	config.PerTool.ProposeMemory = 0
	if err := config.Validate(); err == nil {
		t.Fatal("the draft-first rule was accepted with no draft budget")
	}
	config = AgentMemoryToolRunnerConfig()
	config.ToolSurface = "something_else"
	if err := config.Validate(); err == nil {
		t.Fatal("an unknown tool surface was accepted")
	}
	if AgentMemoryToolRunnerConfig().Surface() != SurfaceLegacy {
		t.Fatal("the zero surface is not the legacy surface")
	}
}

func stubNamedTool(t *testing.T, name string) tool.BaseTool {
	t.Helper()
	built, err := toolutils.InferTool(name, "stub",
		func(context.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	return built
}

// 18. The prompt is the same prompt with the two names substituted, and nothing
// else. Replacing the new names back must reproduce the old prompt byte for
// byte: that is what makes this round's variable exactly one thing.
func TestExplicitActionPromptIsAPureSubstitution(t *testing.T) {
	prompt := SystemPromptFor(memoryrecall.ModeAgentMemoryExplicitActionsV1)
	confirmed := SystemPromptFor(memoryrecall.ModeEpisodeAssertionGovernedV11)
	if prompt == confirmed {
		t.Fatal("the explicit mode reuses a prompt naming tools it does not register")
	}
	restored := strings.NewReplacer(
		SaveMemoryDraftToolName, proposeMemoryToolName,
		SearchPastSessionsToolName, memorySearchToolName,
	).Replace(prompt)
	if restored != confirmed {
		t.Fatal("the explicit prompt is not a pure rename of the confirmed prompt")
	}
	// And it names no tool the mode does not register.
	for _, gone := range []string{proposeMemoryToolName, memorySearchToolName, topicMemoryReadToolName} {
		if strings.Contains(prompt, gone) {
			t.Errorf("the explicit prompt still names %s", gone)
		}
	}
	for _, present := range []string{SaveMemoryDraftToolName, SearchPastSessionsToolName} {
		if !strings.Contains(prompt, present) {
			t.Errorf("the explicit prompt never names %s", present)
		}
	}
}

func TestExplicitSourcePromptIsAThirdPureSubstitution(t *testing.T) {
	prompt := SystemPromptFor(memoryrecall.ModeAgentMemoryExplicitSourcesV1)
	previous := SystemPromptFor(memoryrecall.ModeAgentMemoryExplicitActionsV1)
	restored := strings.ReplaceAll(prompt, SearchUploadedDocumentsToolName, knowledgeSearchToolName)
	if restored != previous {
		t.Fatal("explicit-source prompt changes more than the document-search name")
	}
	for _, required := range []string{SaveMemoryDraftToolName, SearchPastSessionsToolName,
		SearchUploadedDocumentsToolName} {
		if !strings.Contains(prompt, required) {
			t.Errorf("explicit-source prompt never names %s", required)
		}
	}
	if strings.Contains(prompt, knowledgeSearchToolName) {
		t.Fatal("explicit-source prompt still names knowledge_search")
	}
}
