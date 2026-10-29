package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"GopherAI/common/memoryrecall"
	"GopherAI/model"
)

// The shipped default does not move because a mode was added. Every mode so far
// has ended its task with candidacy at most, and the one string that decides
// what production runs is the one this asserts.
func TestShippedDefaultRemainsLegacyV2(t *testing.T) {
	if memoryrecall.DefaultReadMode() != memoryrecall.ModeLegacyV2 {
		t.Fatalf("default read mode = %s", memoryrecall.DefaultReadMode())
	}
	if got := DefaultContextConfig().MemoryReadMode; got != memoryrecall.ModeLegacyV2 {
		t.Fatalf("default context mode = %s", got)
	}
	if got := DefaultRunnerConfig(); got.MaxToolCalls != 3 || got.MaxModelCalls != 4 || got.MaxGraphSteps != 10 {
		t.Fatalf("default runner budget = %#v", got)
	}
}

// The new mode builds none of the three read chains that came before it. This
// is asserted on the predicates because they are what the wiring branches on:
// a mode that answered "yes" to UsesReadPlan would construct the v3 retriever
// in main.go before anything else could stop it.
func TestAgentMemoryToolModeConstructsNoOlderReadChain(t *testing.T) {
	mode := memoryrecall.ModeAgentMemoryToolV1
	if !mode.Valid() || !mode.UsesMemorySearchTool() {
		t.Fatalf("mode %s is not a usable memory-tool mode", mode)
	}
	if mode.UsesReadPlan() || mode.UsesTopicResolver() || mode.UsesSemanticTopicLayer() ||
		mode.UsesModelPlanner() || mode.UsesRecallCycle() {
		t.Fatalf("mode %s builds an older read chain", mode)
	}
	if mode.AutoInjectsTopicAndEpisodeMemory() {
		t.Fatalf("mode %s still auto-injects topic and episode memory", mode)
	}
	// STABILIZE-039 added a second mode that registers the tool. The invariant
	// that still has to hold is the one this test was written for: none of the
	// modes measured before the tool existed may acquire it, and none of them
	// may stop injecting the memory their results were produced with.
	// STABILIZE-050 adds a fifth. It registers the tool for episodic history
	// only - it cannot serve semantic kinds at all - but it does register it,
	// and the invariant this list protects is unchanged: no mode measured
	// before the tool existed may acquire it.
	// STABILIZE-058 adds a sixth, which extends the fifth: it registers the
	// tool and governs what a card is made of. The invariant is unchanged and
	// still the only thing this list protects.
	// STABILIZE-061 adds a seventh, which is the sixth under two different tool
	// names. It registers the tool - under the name search_past_sessions - and
	// the invariant is still the only thing this list protects: no mode
	// measured before the tool existed may acquire it, whatever the tool is
	// called in the mode that has it.
	toolModes := map[memoryrecall.ReadMode]bool{
		memoryrecall.ModeAgentMemoryToolV1: true, memoryrecall.ModeAgentMemoryToolV2: true,
		memoryrecall.ModeAgentMemoryToolV3: true, memoryrecall.ModeAgentMemoryToolV4: true,
		memoryrecall.ModeAgentMemoryUserConfirmedV1:   true,
		memoryrecall.ModeEpisodeAssertionGovernedV11:  true,
		memoryrecall.ModeAgentMemoryExplicitActionsV1: true,
		memoryrecall.ModeAgentMemoryExplicitSourcesV1: true,
	}
	for _, other := range memoryrecall.ModeNames() {
		if toolModes[other] {
			continue
		}
		if other.UsesMemorySearchTool() {
			t.Fatalf("mode %s would register memory_search", other)
		}
		if !other.AutoInjectsTopicAndEpisodeMemory() {
			t.Fatalf("mode %s stopped auto-injecting memory", other)
		}
		if other.UsesAuthoritativeTopicBootstrap() {
			t.Fatalf("mode %s acquired the authoritative topic bootstrap", other)
		}
	}
	// And v1 in particular did not acquire it. This is the whole reason v2 is a
	// separate mode rather than a flag: v1 has measured results, and results
	// belong to the window they were produced under.
	if memoryrecall.ModeAgentMemoryToolV1.UsesAuthoritativeTopicBootstrap() {
		t.Fatal("agent_memory_tool_v1 acquired the authoritative topic bootstrap")
	}
	// STABILIZE-040 adds a third. The same rule applies to the narrow tool: no
	// mode measured before it existed may register it, and the two modes that
	// preceded it must not have acquired the shared dedup either, because that
	// changes what a second memory_search may return.
	for _, other := range memoryrecall.ModeNames() {
		if other == memoryrecall.ModeAgentMemoryToolV3 || other == memoryrecall.ModeAgentMemoryToolV4 {
			continue
		}
		if other.UsesTopicMemoryReadTool() {
			t.Fatalf("mode %s would register topic_memory_read", other)
		}
		if other.SharesIssuedRecordsAcrossMemoryTools() {
			t.Fatalf("mode %s acquired the shared issued-record dedup", other)
		}
	}
	// And v3 in particular does not bootstrap. It reads the same rows by the
	// same key; the entire difference from v2 is that the Agent has to ask.
	v3 := memoryrecall.ModeAgentMemoryToolV3
	if v3.UsesAuthoritativeTopicBootstrap() || !v3.AutoInjectsTopicAndEpisodeMemory() == false {
		t.Fatalf("agent_memory_tool_v3 injects memory on its own: %#v", v3)
	}
	if !v3.UsesMemorySearchTool() || !v3.UsesTopicMemoryReadTool() ||
		!v3.SharesIssuedRecordsAcrossMemoryTools() {
		t.Fatal("agent_memory_tool_v3 does not register both memory tools")
	}
	if v3.UsesReadPlan() || v3.UsesTopicResolver() || v3.UsesSemanticTopicLayer() ||
		v3.UsesModelPlanner() || v3.UsesRecallCycle() {
		t.Fatal("agent_memory_tool_v3 builds an older read chain")
	}
	v4 := memoryrecall.ModeAgentMemoryToolV4
	if !v4.UsesMemorySearchTool() || !v4.UsesTopicMemoryReadTool() ||
		!v4.UsesTopicMemoryReadObservationV4() || !v4.SharesIssuedRecordsAcrossMemoryTools() ||
		v4.UsesAuthoritativeTopicBootstrap() || v4.AutoInjectsTopicAndEpisodeMemory() {
		t.Fatal("agent_memory_tool_v4 does not preserve the isolated two-tool boundary")
	}
}

type recordingMemoryProvider struct {
	memoryCalls   atomic.Int32
	episodeCalls  atomic.Int32
	topicCalls    atomic.Int32
	plannedCalls  atomic.Int32
	globalCalls   atomic.Int32
	globalMemory  []string
	topicMemory   []string
	episodeMemory []model.EpisodeMemory
	globalErr     error
}

func (p *recordingMemoryProvider) RelevantMemories(context.Context, RunContext) (MemoryRecall, error) {
	p.memoryCalls.Add(1)
	return MemoryRecall{Global: p.globalMemory, Topic: p.topicMemory}, nil
}

func (p *recordingMemoryProvider) RelevantEpisodes(context.Context, RunContext) ([]model.EpisodeMemory, error) {
	p.episodeCalls.Add(1)
	return p.episodeMemory, nil
}

func (p *recordingMemoryProvider) ProposeTopicKeys(context.Context, RunContext) ([]string, error) {
	p.topicCalls.Add(1)
	return []string{"topic"}, nil
}

func (p *recordingMemoryProvider) PlanAndVerify(context.Context, RunContext) (PlannedMemoryRecall, error) {
	p.plannedCalls.Add(1)
	return PlannedMemoryRecall{}, nil
}

func (p *recordingMemoryProvider) GlobalMemories(context.Context, RunContext) ([]string, error) {
	p.globalCalls.Add(1)
	return p.globalMemory, p.globalErr
}

type stubHistory struct{ rows []model.Message }

func (s stubHistory) ListRecentAfter(context.Context, string, uint, int) ([]model.Message, error) {
	return s.rows, nil
}

func memoryToolContextConfig() ContextConfig {
	config := DefaultContextConfig()
	config.MemoryReadMode = memoryrecall.ModeAgentMemoryToolV1
	config.EpisodeRecallMode = EpisodeRecallInject
	return config
}

// Under the new mode the window carries global memory and nothing else, and the
// other readers are not merely skipped — they are never asked. A builder that
// fetched topic notes and then discarded them would pay for the retrieval this
// mode exists to move behind a tool.
func TestMemoryToolContextInjectsGlobalOnly(t *testing.T) {
	provider := &recordingMemoryProvider{
		globalMemory:  []string{"讲解偏好：先说结论"},
		topicMemory:   []string{"主题笔记：TCP 是没有刻度的水管"},
		episodeMemory: []model.EpisodeMemory{{ID: "e-1", Title: "复习消息边界", LastMessageAt: time.Now()}},
	}
	builder, err := NewContextBuilder(memoryToolContextConfig(), stubHistory{}, nil, nil, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseEpisodeProvider(provider)
	builder.UseTopicKeyResolver(provider)
	builder.UsePlannedMemoryProvider(provider)
	builder.UseGlobalMemoryProvider(provider)

	evidence := NewMemoryEvidenceRegistry()
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 1, SessionID: "s-1", Question: "我之前是怎么理解消息边界的？", EpisodeRecallReady: true,
		MemoryEvidence: evidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	assembled := renderWindow(window)
	if !strings.Contains(assembled, "先说结论") {
		t.Fatalf("global memory was not injected: %s", assembled)
	}
	if strings.Contains(assembled, "没有刻度的水管") || strings.Contains(assembled, "复习消息边界") {
		t.Fatalf("topic or episode memory was auto-injected: %s", assembled)
	}
	if window.Budget.TopicUserMemory != 0 || window.Budget.EpisodeMemory != 0 || len(window.EpisodeIDs) != 0 {
		t.Fatalf("budget = %#v episodes=%v", window.Budget, window.EpisodeIDs)
	}
	if len(window.EpisodeHistoryGrants) != 0 || len(window.SummaryHistoryGrants) != 0 {
		t.Fatalf("a history read allow-list was pre-filled: %v %v",
			window.EpisodeHistoryGrants, window.SummaryHistoryGrants)
	}
	if provider.globalCalls.Load() != 1 {
		t.Fatalf("global reader calls = %d, want 1", provider.globalCalls.Load())
	}
	if provider.memoryCalls.Load() != 0 || provider.episodeCalls.Load() != 0 ||
		provider.topicCalls.Load() != 0 || provider.plannedCalls.Load() != 0 {
		t.Fatalf("an older reader ran: %#v", provider)
	}
	if window.RecallEmbeddingCalls != 0 {
		t.Fatalf("the context bought %d embeddings", window.RecallEmbeddingCalls)
	}
}

// A mode whose whole purpose is memory must not serve turns with none of it
// because a dependency was left unwired.
func TestMemoryToolContextRefusesWithoutAGlobalReader(t *testing.T) {
	builder, err := NewContextBuilder(memoryToolContextConfig(), stubHistory{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Build(context.Background(), RunContext{
		UserID: 1, SessionID: "s-1", Question: "问题", MemoryEvidence: NewMemoryEvidenceRegistry(),
	}); err == nil {
		t.Fatalf("the builder served a turn with no global memory reader")
	}
	// And a run with no registry cannot label what it injects, so it fails here
	// rather than at the final citation check.
	withReader, err := NewContextBuilder(memoryToolContextConfig(), stubHistory{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	withReader.UseGlobalMemoryProvider(&recordingMemoryProvider{globalMemory: []string{"讲解偏好"}})
	if _, err := withReader.Build(context.Background(), RunContext{
		UserID: 1, SessionID: "s-1", Question: "问题",
	}); err == nil {
		t.Fatalf("the builder served a turn with no evidence registry")
	}
}

// A global reader failure costs personalisation, not the turn.
func TestMemoryToolContextDegradesWhenGlobalMemoryFails(t *testing.T) {
	provider := &recordingMemoryProvider{globalErr: errors.New("memory store down")}
	builder, err := NewContextBuilder(memoryToolContextConfig(), stubHistory{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseGlobalMemoryProvider(provider)
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 1, SessionID: "s-1", Question: "问题", MemoryEvidence: NewMemoryEvidenceRegistry(),
	})
	if err != nil {
		t.Fatalf("a degraded memory read failed the turn: %v", err)
	}
	if window.Budget.GlobalUserMemory != 0 {
		t.Fatalf("budget = %#v", window.Budget)
	}
}

// The prompt is the base prompt plus the four boundaries, and the budget is
// computed on the prompt the turn actually carries.
func TestSystemPromptSelectionMatchesTheMode(t *testing.T) {
	base := SystemPromptFor(memoryrecall.ModeLegacyV2)
	tool := SystemPromptFor(memoryrecall.ModeAgentMemoryToolV1)
	if base == tool || !strings.HasPrefix(tool, base) {
		t.Fatalf("the memory-tool prompt is not the base prompt plus its own rules")
	}
	for _, phrase := range []string{"memory_search", "knowledge_search", "propose_memory", "Recent Messages", "[E1]", "[G1]", "[C1]", "[S1]"} {
		if !strings.Contains(tool, phrase) {
			t.Fatalf("the prompt does not draw the %s boundary", phrase)
		}
	}
	if strings.Contains(base, "memory_search") {
		t.Fatalf("the base prompt names a tool no other mode registers")
	}

	provider := &recordingMemoryProvider{}
	builder, err := NewContextBuilder(memoryToolContextConfig(), stubHistory{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseGlobalMemoryProvider(provider)
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 1, SessionID: "s", Question: "问题", MemoryEvidence: NewMemoryEvidenceRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if window.Budget.System != messageTokenCost(tool) {
		t.Fatalf("the budget was computed for a different prompt: %d", window.Budget.System)
	}
	if got := window.Messages[0].Content; got != tool {
		t.Fatalf("the window carries a different prompt")
	}
}

func TestV4PromptIsIsolatedFromTheFrozenV3Prompt(t *testing.T) {
	const frozenV3 = "f8659230993117be9f98b99d0b7399bd5f824327ca064a746f7640605f163496"
	v3 := SystemPromptFor(memoryrecall.ModeAgentMemoryToolV3)
	sum := sha256.Sum256([]byte(v3))
	if got := hex.EncodeToString(sum[:]); got != frozenV3 {
		t.Fatalf("v3 prompt drifted: %s", got)
	}
	v4 := SystemPromptFor(memoryrecall.ModeAgentMemoryToolV4)
	if v4 == v3 || !strings.Contains(v4, "current_topic_keys") || !strings.Contains(v4, "complete") {
		t.Fatal("v4 prompt does not carry its isolated observation rules")
	}
}

func renderWindow(window ContextWindow) string {
	var out strings.Builder
	for _, message := range window.Messages {
		out.WriteString(message.Content)
		out.WriteString("\n")
	}
	return out.String()
}
