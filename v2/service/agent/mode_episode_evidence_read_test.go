package agent

import (
	"GopherAI/common/memoryrecall"
	"GopherAI/model"
	"slices"
	"strings"
	"testing"
	"time"
)

// STABILIZE-064. v3 exists because a read-only audit of the seam found that v1 and
// v2 never received an Episode card at all: both identities register memory_search,
// AutoInjectsTopicAndEpisodeMemory is !UsesMemorySearchTool, and the builder then
// takes its "global only" branch. So the episode half of HistoryMessageGrant was
// unreachable in the very experiment that claims to measure compressed-memory
// read-back. These tests pin what v3 does differently, and pin that v1 and v2 keep
// doing what their published numbers were taken under.

const (
	v3RunSession     = "session-current"
	v3EpisodeSession = "episode-source-episode-1"
)

func v3ContextConfig() ContextConfig {
	config := DefaultContextConfig()
	config.MemoryReadMode = memoryrecall.ModeAgentMemorySourcePriorityV3
	config.MaxSummaryTokens = 400
	// The mode's own contract requires this: without the inject flag the builder
	// never asks the episode provider, and a test that forgot it would report a
	// configuration detail as a permission result.
	config.EpisodeRecallMode = EpisodeRecallInject
	config.MaxEpisodeTokens = 1000
	return config
}

// v3Episode is one stored card: active, indexed, version 4, describing an earlier
// session, citing 11 (decision), 13 (open loop) and 15 (user correction, which the
// renderer does not print).
func v3Episode(watermark uint, extraOpenLoops ...uint) model.EpisodeMemory {
	episode := testEpisode("episode-1", "第一次讨论粘包", time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC))
	episode.Version = 4
	episode.Status = model.EpisodeStatusActive
	episode.IndexStatus = model.EpisodeIndexIndexed
	episode.SourceLastMessageID = watermark
	episode.Document.OpenLoops = []model.EpisodeEvidence{
		{Text: "补充半包测试", EvidenceMessageIDs: append([]uint{13}, extraOpenLoops...)},
	}
	return episode
}

func v3Snapshot(t *testing.T) SummarySnapshot {
	t.Helper()
	return SummarySnapshot{
		Version: 3, CoveredMessageID: 40,
		Content: summaryDocumentWithGroupsJSON(t, "长度字段拆包", []uint{21}, []uint{23}, []uint{25}),
	}
}

func v3Run() RunContext {
	return RunContext{UserID: 7, SessionID: v3RunSession, Question: "我当时是怎么说的？",
		EpisodeRecallReady: true}
}

func grantIDs(grants []HistoryMessageGrant, kind string) []uint {
	out := make([]uint, 0, len(grants))
	for _, grant := range grants {
		if grant.SourceKind == kind {
			out = append(out, grant.MessageID)
		}
	}
	slices.Sort(out)
	return out
}

func TestV3InjectsBothCompressionsAndGrantsBothSources(t *testing.T) {
	episode := v3Episode(0)
	window := buildSummaryWindow(t, v3ContextConfig(), v3Snapshot(t),
		[]model.EpisodeMemory{episode}, v3Run())

	if window.Budget.Summary == 0 {
		t.Fatal("v3 injected no session summary")
	}
	if window.Budget.EpisodeMemory == 0 || window.Budget.EpisodeCount != 1 {
		t.Fatalf("v3 injected no episode card: %+v", window.Budget)
	}
	summary := grantIDs(window.SummaryHistoryGrants, HistorySourceSummary)
	episodeIDs := grantIDs(window.EpisodeHistoryGrants, HistorySourceEpisode)
	// The rendered card shows decision 11 and open_loop 13; the correction's 15 is
	// cited in the store but never printed, so it is not citable in this turn.
	if !slices.Equal(episodeIDs, []uint{11, 13}) {
		t.Fatalf("episode grants=%v want exactly the visible 11 and 13", episodeIDs)
	}
	if len(summary) == 0 {
		t.Fatal("v3 produced no summary grants")
	}
	for _, id := range summary {
		if id > 40 {
			t.Errorf("summary grant %d is past the watermark 40", id)
		}
	}

	// One tool, two sources: the merged set the run hands the registry carries both,
	// each keeping its own provenance rather than collapsing into one id list.
	merged := MergeHistoryMessageGrants(window.EpisodeHistoryGrants, window.SummaryHistoryGrants)
	set := NewHistoryGrantSet(v3Run(), merged)
	if set.Len() != len(summary)+len(episodeIDs) {
		t.Fatalf("grant set covers %d ids, want %d+%d", set.Len(), len(summary), len(episodeIDs))
	}
	for _, id := range episodeIDs {
		found := false
		for _, grant := range set.Grants(id) {
			if grant.SourceKind == HistorySourceEpisode && grant.SourceSessionID == v3EpisodeSession &&
				grant.EpisodeID == episode.ID && grant.EpisodeVersion == episode.Version {
				found = true
			}
		}
		if !found {
			t.Errorf("episode grant %d lost its card identity, source session or version: %+v",
				id, set.Grants(id))
		}
	}
	for _, id := range summary {
		for _, grant := range set.Grants(id) {
			if grant.SourceKind == HistorySourceSummary && grant.SourceSessionID != v3RunSession {
				t.Errorf("summary grant %d carries a foreign session %q", id, grant.SourceSessionID)
			}
		}
	}
}

// An id that is cited in the stored document but absent from the text this turn sent
// must not be grantable, or the model could read back a citation it was never shown.
func TestV3GrantsOnlyEvidenceTheRenderedCardShows(t *testing.T) {
	window := buildSummaryWindow(t, v3ContextConfig(), v3Snapshot(t),
		[]model.EpisodeMemory{v3Episode(0)}, v3Run())
	rendered := ""
	for _, message := range window.Messages {
		if strings.Contains(message.Content, "Relevant cross-session episodes") {
			rendered = message.Content
		}
	}
	if rendered == "" {
		t.Fatal("no episode block was rendered, so this proved nothing")
	}
	if strings.Contains(rendered, "15") {
		t.Fatal("the fixture no longer matches the renderer: corrections were printed")
	}
	if slices.Contains(grantIDs(window.EpisodeHistoryGrants, HistorySourceEpisode), 15) {
		t.Error("an id cited only in the un-rendered user_corrections group was granted")
	}
}

// The card's own source watermark bounds its citations, not the run's: a stale card
// may not authorise rows written after the version that cited them.
func TestV3EpisodeWatermarkBoundsCitations(t *testing.T) {
	// The card cites 13 and 60 but says its session had only reached 20.
	window := buildSummaryWindow(t, v3ContextConfig(), v3Snapshot(t),
		[]model.EpisodeMemory{v3Episode(20, 60)}, v3Run())
	granted := grantIDs(window.EpisodeHistoryGrants, HistorySourceEpisode)
	if !slices.Contains(granted, 13) {
		t.Fatalf("a cited id inside the watermark was not granted: %v", granted)
	}
	if slices.Contains(granted, 60) {
		t.Errorf("id 60 is past the card's watermark 20 and must not be granted: %v", granted)
	}
	for _, grant := range window.EpisodeHistoryGrants {
		if grant.EpisodeWatermark != 20 {
			t.Errorf("grant %d carries watermark %d, want the card's own 20",
				grant.MessageID, grant.EpisodeWatermark)
		}
	}
}

// The audit finding, pinned: v1 and v2 cannot reach an Episode card at all. If a later
// change made them inject one, their published measurements would stop describing the
// window they were taken under, and this is where that would surface.
func TestSourcePriorityV1AndV2ReceiveNoEpisodeCard(t *testing.T) {
	for _, mode := range []memoryrecall.ReadMode{
		memoryrecall.ModeAgentMemorySourcePriorityV1, memoryrecall.ModeAgentMemorySourcePriorityV2,
	} {
		config := v3ContextConfig()
		config.MemoryReadMode = mode
		window := buildSummaryWindow(t, config, v3Snapshot(t),
			[]model.EpisodeMemory{v3Episode(0)}, v3Run())
		if len(window.EpisodeIDs) != 0 || len(window.EpisodeHistoryGrants) != 0 {
			t.Errorf("mode %s received an episode card: ids=%v grants=%v",
				mode, window.EpisodeIDs, window.EpisodeHistoryGrants)
		}
		if mode == memoryrecall.ModeAgentMemorySourcePriorityV1 && len(window.SummaryHistoryGrants) != 0 {
			t.Errorf("v1 acquired summary grants: %+v", window.SummaryHistoryGrants)
		}
		if mode == memoryrecall.ModeAgentMemorySourcePriorityV2 && len(window.SummaryHistoryGrants) == 0 {
			t.Error("v2 lost the summary grants its published run measured")
		}
	}
}

// v3 runs on v1's published prompt bytes. The reason is the experiment: both arms
// share this prompt, so the paragraph telling the model it may read evidence back
// cannot be in it - it would hand the arm without the tool a promise it cannot act
// on, and the arm difference would stop being the tool.
func TestV3PromptIsThePublishedSourcePriorityBytes(t *testing.T) {
	v1 := SystemPromptFor(memoryrecall.ModeAgentMemorySourcePriorityV1)
	v3 := SystemPromptFor(memoryrecall.ModeAgentMemorySourcePriorityV3)
	if v1 != v3 {
		t.Error("v3 does not run on v1's published prompt bytes")
	}
	if strings.Contains(v3, "evidence_message_ids") {
		t.Error("v3's prompt promises read-back the baseline arm may not have")
	}
	if v3 == SystemPromptFor(memoryrecall.ModeAgentMemorySourcePriorityV2) {
		t.Error("v3 and v2 must not share a prompt: v2's paragraph is a different method")
	}
}

func TestV3PredicatesAreTheNewIsolation(t *testing.T) {
	v3 := memoryrecall.ModeAgentMemorySourcePriorityV3
	if !v3.Valid() {
		t.Fatal("v3 does not parse")
	}
	if memoryrecall.DefaultReadMode() != memoryrecall.ModeLegacyV2 {
		t.Fatal("the production default moved")
	}
	// The whole point of the mode: outside the memory-search family, so the builder's
	// auto-injection branch runs and an Episode card reaches the window.
	if v3.UsesMemorySearchTool() || !v3.AutoInjectsTopicAndEpisodeMemory() {
		t.Errorf("v3 must not register memory_search and must inject cards: %t/%t",
			v3.UsesMemorySearchTool(), v3.AutoInjectsTopicAndEpisodeMemory())
	}
	if !v3.UsesSourcePrioritySurface() || !v3.UsesExplicitKnowledgeSourceName() ||
		!v3.UsesExplicitMemoryActionNames() || !v3.OrdersMemoryDraftBeforeMemoryRead() {
		t.Error("v3 lost the source-priority tool naming")
	}
	if v3.UsesSummaryHistoryRead() {
		t.Error("v3 must not answer v2's published predicate")
	}
	if !v3.ReadsEvidenceBack() {
		t.Error("v3 does not read evidence back")
	}
	if v3.UsesUserConfirmedTopicContext() || v3.RestrictsMemorySearchToEpisodic() ||
		v3.GovernsAssertionTime() || v3.UsesModelPlanner() {
		t.Error("v3 picked up a behaviour belonging to another identity")
	}
	// Leaving the memory-search family has one consequence worth stating rather than
	// discovering: UsesReadPlan is defined as "not legacy, not planned, not a memory
	// tool mode", so v3 runs the multi-axis read plan like the older injecting modes do.
	// Both arms share it, so it cannot move the pair comparison, but a reader of the
	// freeze should know v3 is an injecting mode with everything that comes with it.
	if !v3.UsesReadPlan() {
		t.Error("v3 stopped being an injecting mode")
	}
	// Nothing may leak backwards.
	v1, v2 := memoryrecall.ModeAgentMemorySourcePriorityV1, memoryrecall.ModeAgentMemorySourcePriorityV2
	if v1.UsesSummaryHistoryRead() || !v2.UsesSummaryHistoryRead() {
		t.Error("UsesSummaryHistoryRead drifted between v1 and v2")
	}
	if v1.ReadsEvidenceBack() || !v2.ReadsEvidenceBack() {
		t.Error("ReadsEvidenceBack drifted for the published identities: v1 no, v2 yes")
	}
	if v1.AutoInjectsTopicAndEpisodeMemory() || v2.AutoInjectsTopicAndEpisodeMemory() {
		t.Error("v1/v2 started injecting cards, which would change what their run measured")
	}
	if !v1.UsesMemorySearchTool() || !v2.UsesMemorySearchTool() {
		t.Error("v1/v2 stopped registering memory_search")
	}
}

// The envelope is v2's, number for number: the experiment's only variable is the
// second compression, so a budget difference would be a fourth one.
func TestV3EnvelopeMatchesV2(t *testing.T) {
	v2 := AgentMemorySourcePriorityV2RunnerConfig()
	v3 := AgentMemorySourcePriorityV3RunnerConfig()
	if v3.ToolSurface != v2.ToolSurface || v3.ToolSurface != SurfaceSourcePriority {
		t.Errorf("surface %q vs %q", v3.ToolSurface, v2.ToolSurface)
	}
	if v3.MaxToolCalls != v2.MaxToolCalls || v3.MaxModelCalls != v2.MaxModelCalls ||
		v3.MaxGraphSteps != v2.MaxGraphSteps || v3.Deadline != v2.Deadline {
		t.Errorf("envelope moved: %+v vs %+v", v3, v2)
	}
	if v3.PerTool != v2.PerTool {
		t.Errorf("per-capability ceilings moved: %+v vs %+v", v3.PerTool, v2.PerTool)
	}
	if v3.PerTool.HistoryRead != DefaultHistoryReadCalls {
		t.Errorf("history read ceiling %d, want the production default %d",
			v3.PerTool.HistoryRead, DefaultHistoryReadCalls)
	}
	if err := v3.Validate(); err != nil {
		t.Errorf("v3 envelope does not validate: %v", err)
	}
}

// The narrowing is v3's, and the list of what changes is stated rather than implied.
// An older injecting mode that silently acquired it would move artefacts nobody is
// re-running; a v3 that silently lost it would measure a citation the model never saw.
func TestV3NarrowsEpisodeGrantsWhereOldModesDoNot(t *testing.T) {
	card := v3Episode(20, 60) // cites 11, 13, 60 (open loop) and 15 (un-printed)
	// Sized from DefaultContextConfig like the v3 fixture: the test-local config carries
	// the shortest token target in the codebase, and under it the older injecting modes'
	// own prompt leaves no room for a card at all, which would make this comparison pass
	// for the wrong reason.
	episodeOnly := DefaultContextConfig()
	episodeOnly.MaxSummaryTokens = 400
	episodeOnly.EpisodeRecallMode = EpisodeRecallInject
	episodeOnly.MaxEpisodeTokens = 1000

	v3Config := v3ContextConfig()
	if !v3Config.MemoryReadMode.UsesEpisodeEvidenceRead() {
		t.Fatal("v3 must be the mode that narrows")
	}
	for _, mode := range []memoryrecall.ReadMode{
		memoryrecall.ModeLegacyV2, memoryrecall.ModeEpisodeV3, memoryrecall.ModeEpisodeV3Semantic,
		memoryrecall.ModeAgentMemorySourcePriorityV1, memoryrecall.ModeAgentMemorySourcePriorityV2,
	} {
		if mode.UsesEpisodeEvidenceRead() {
			t.Errorf("mode %s claims the v3 narrowing", mode)
		}
		config := episodeOnly
		config.MemoryReadMode = mode
		window := buildSummaryWindow(t, config, SummarySnapshot{}, []model.EpisodeMemory{card},
			RunContext{UserID: 7, SessionID: v3RunSession, Question: "上次怎么定的？",
				EpisodeRecallReady: true})
		// Nothing injected at all under the two source-priority identities, and the
		// full stored citation set everywhere else: 11, 13, 15, 60.
		granted := grantIDs(window.EpisodeHistoryGrants, HistorySourceEpisode)
		if mode.UsesMemorySearchTool() {
			if len(granted) != 0 {
				t.Errorf("mode %s was handed episode grants it never injects: %v", mode, granted)
			}
			continue
		}
		if !slices.Equal(granted, []uint{11, 13, 15, 60}) {
			t.Errorf("mode %s episode grants=%v, want the unfiltered stored set", mode, granted)
		}
	}

	window := buildSummaryWindow(t, v3Config, v3Snapshot(t), []model.EpisodeMemory{card}, v3Run())
	granted := grantIDs(window.EpisodeHistoryGrants, HistorySourceEpisode)
	// 15 is never printed, 60 is past the card's own watermark of 20, and 11/13 stay.
	if !slices.Equal(granted, []uint{11, 13}) {
		t.Errorf("v3 episode grants=%v, want the printed citations inside the watermark", granted)
	}
	for _, grant := range window.EpisodeHistoryGrants {
		if grant.EpisodeWatermark != 20 {
			t.Errorf("v3 grant %d must carry the card's watermark, got %d",
				grant.MessageID, grant.EpisodeWatermark)
		}
	}
}

// A v3 grant carries the watermark so the tool can re-check it; an older mode's grant
// leaves the field zero, which its UsableIn check treats as "no bound". Both readings
// must authorise what their own mode decided, and neither may borrow the other's.
func TestEpisodeWatermarkFieldOnlyConstrainsV3(t *testing.T) {
	for _, testCase := range []struct {
		mode     memoryrecall.ReadMode
		grant    HistoryMessageGrant
		usable   bool
		boundary string
	}{
		{memoryrecall.ModeLegacyV2, HistoryMessageGrant{MessageID: 60, SourceKind: HistorySourceEpisode,
			SourceSessionID: v3EpisodeSession, EpisodeID: "episode-1", EpisodeVersion: 4}, true,
			"no watermark recorded, so nothing bounds it"},
		{memoryrecall.ModeAgentMemorySourcePriorityV3, HistoryMessageGrant{MessageID: 60,
			SourceKind: HistorySourceEpisode, SourceSessionID: v3EpisodeSession,
			EpisodeID: "episode-1", EpisodeVersion: 4, EpisodeWatermark: 20}, false,
			"the card's own watermark excludes it"},
		{memoryrecall.ModeAgentMemorySourcePriorityV3, HistoryMessageGrant{MessageID: 11,
			SourceKind: HistorySourceEpisode, SourceSessionID: v3EpisodeSession,
			EpisodeID: "episode-1", EpisodeVersion: 4, EpisodeWatermark: 20}, true,
			"inside the watermark"},
	} {
		if !testCase.mode.UsesEpisodeEvidenceRead() && !testCase.usable {
			t.Errorf("%s: %s", testCase.mode, testCase.boundary)
		}
		usable := testCase.grant.UsableIn(RunContext{UserID: 7, SessionID: v3RunSession})
		if usable != testCase.usable {
			t.Errorf("mode %s grant %d: UsableIn=%t, want %t (%s)",
				testCase.mode, testCase.grant.MessageID, usable, testCase.usable, testCase.boundary)
		}
	}
}
