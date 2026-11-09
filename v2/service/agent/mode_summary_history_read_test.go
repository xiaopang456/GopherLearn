package agent

import (
	"GopherAI/common/memoryrecall"
	"GopherAI/model"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// Summary read-back is a mode, not a tool upgrade. The tool implementation is
// shared with every mode that existed before it, so what keeps the
// published numbers describing the source-priority mode is this file: the bytes each older
// mode sends as its system prompt, the predicates each older mode answers, and
// the grant set each older mode can assemble.

// TestOlderModePromptsKeepTheirFrozenBytes pins a digest per mode.
//
// A digest rather than a substring, because the failure this guards is a sentence
// added for a new mode landing in a string all modes share. Substring assertions
// survive exactly that.
//
// Every value below was verified against the same computation on the commit this
// round started from, so the table records what the older modes sent rather than
// what they send now. The last row is the new identity, pinned so a later edit to
// its paragraph is a visible change of method.
func TestOlderModePromptsKeepTheirFrozenBytes(t *testing.T) {
	// The five modes whose prompt is the base text and nothing else share one
	// digest, and that is the point of listing them together.
	//
	// Every digest below changed on 2026-11-07, and deliberately: the base
	// text now carries the rule that tells the model not to write bracketed
	// numbers when the answer rests on the conversation, and every mode's
	// prompt is the base text plus its own rules. That is a change of method
	// for all of them - the answer prompt each one sends is different from
	// the one its published numbers were measured against - so the new values
	// are recorded here rather than the test being loosened. The values were
	// computed by the same function the table checks.
	const basePrompt = "6f45160d3846f466e7c043f97e6343dc5b14ec7889a672f8a44d00ea2a362140"
	for _, frozen := range []struct {
		mode   memoryrecall.ReadMode
		digest string
	}{
		{memoryrecall.ModeLegacyV2, basePrompt},
		{memoryrecall.ModeEpisodeV3, basePrompt},
		{memoryrecall.ModeEpisodeV3Lexical, basePrompt},
		{memoryrecall.ModeEpisodeV3Semantic, basePrompt},
		{memoryrecall.ModeModelPlannedV1, basePrompt},
		{memoryrecall.ModeAgentMemoryToolV1, "a30ba8b983eb4cc0c5fa01841f3c80710a0c133fd9ea52187c030e285e5904d0"},
		{memoryrecall.ModeAgentMemoryToolV2, "e4282b7928e4dfdfe979e1bc49d39cc16909dbaedb7d73f114b9b16e4943ae15"},
		{memoryrecall.ModeAgentMemoryToolV3, "f6ce9df396fdf635d18fb2e11b4ddb0f266e46226d9f1a204117484b725994db"},
		{memoryrecall.ModeAgentMemoryToolV4, "69f53953243ae66e6a126db9db09e871249c10a9d7442c7c9f6960e5d204b699"},
		{memoryrecall.ModeAgentMemoryUserConfirmedV1, "bf4235a5071547c284dab703e5429cd36e5d7af982f142185273c65514f3c798"},
		{memoryrecall.ModeEpisodeAssertionGovernedV11, "bf4235a5071547c284dab703e5429cd36e5d7af982f142185273c65514f3c798"},
		{memoryrecall.ModeAgentMemoryExplicitActionsV1, "50203b8b401ad0c71ca1f478f210fcf9b25d24b387a7161f23058553d7ce8ee8"},
		{memoryrecall.ModeAgentMemoryExplicitSourcesV1, "e42d0aab9e3d8e1eb6d8d91cad395fcd2b27e02c3686accc633c276948305a50"},
		{memoryrecall.ModeAgentMemorySourcePriorityV1, "d60befcfb83512791ae4459856cb2e4542116425283a48624605fa496a2779bf"},
		{memoryrecall.ModeAgentMemorySourcePriorityV2, "9daaaf2eb9972fe56273967b5d0af721cbf1dfb478f6348dab8fa9037c35f3f7"},
		// v3 deliberately runs on v1's bytes: both of its arms share this prompt, and
		// the read-back paragraph would describe a tool the baseline arm does not have.
		{memoryrecall.ModeAgentMemorySourcePriorityV3, "d60befcfb83512791ae4459856cb2e4542116425283a48624605fa496a2779bf"},
	} {
		got := promptDigest(frozen.mode)
		if os.Getenv("PRINT_PROMPT_DIGESTS") != "" {
			t.Logf("%s %s", frozen.mode, got)
		}
		if got != frozen.digest {
			t.Errorf("mode %s prompt digest drifted: got %s want %s", frozen.mode, got, frozen.digest)
		}
	}
}

func promptDigest(mode memoryrecall.ReadMode) string {
	sum := sha256.Sum256([]byte(SystemPromptFor(mode)))
	return hex.EncodeToString(sum[:])
}

// The new mode's prompt is the old one with the read-back paragraph appended, so
// a diff of the two is the whole change and nothing about source priority moved.
func TestSummaryReadPromptIsSourcePriorityPlusReadback(t *testing.T) {
	v1 := SystemPromptFor(memoryrecall.ModeAgentMemorySourcePriorityV1)
	v2 := SystemPromptFor(memoryrecall.ModeAgentMemorySourcePriorityV2)
	if v1 == v2 {
		t.Fatal("the new mode carries no read-back rule")
	}
	if !strings.HasPrefix(v2, v1) {
		t.Fatal("the new mode rewrote the source-priority prompt instead of extending it")
	}
	suffix := strings.TrimPrefix(v2, v1)
	for _, required := range []string{
		"evidence_message_ids", "read_history_message", "never guess one", "bounded fragment",
	} {
		if !strings.Contains(suffix, required) {
			t.Fatalf("the appended paragraph is missing %q", required)
		}
	}
	// An older mode must not be able to reach the same text by accident.
	for _, mode := range []memoryrecall.ReadMode{
		memoryrecall.ModeAgentMemorySourcePriorityV1, memoryrecall.ModeAgentMemoryExplicitSourcesV1,
		memoryrecall.ModeLegacyV2, memoryrecall.ModeAgentMemoryToolV1,
	} {
		if strings.Contains(SystemPromptFor(mode), "evidence_message_ids") {
			t.Fatalf("mode %s was told summary ids are readable", mode)
		}
	}
}

// Everything the mode predicates answer is shared with v1; the one difference is
// the capability this round adds. Written as a table because the claim is about
// every predicate at once, and a list maintained by hand is where a new mode
// silently acquires an older one's behaviour.
func TestNewModeDiffersFromSourcePriorityV1OnlyBySummaryRead(t *testing.T) {
	v1 := memoryrecall.ModeAgentMemorySourcePriorityV1
	v2 := memoryrecall.ModeAgentMemorySourcePriorityV2
	if !v2.Valid() {
		t.Fatal("the new mode does not parse")
	}
	if memoryrecall.DefaultReadMode() != memoryrecall.ModeLegacyV2 ||
		v1 == memoryrecall.DefaultReadMode() || v2 == memoryrecall.DefaultReadMode() {
		t.Fatal("the new mode moved the production default")
	}
	for _, predicate := range []struct {
		name string
		call func(memoryrecall.ReadMode) bool
	}{
		{"UsesMemorySearchTool", memoryrecall.ReadMode.UsesMemorySearchTool},
		{"UsesReadPlan", memoryrecall.ReadMode.UsesReadPlan},
		{"AutoInjectsTopicAndEpisodeMemory", memoryrecall.ReadMode.AutoInjectsTopicAndEpisodeMemory},
		{"UsesUserConfirmedTopicContext", memoryrecall.ReadMode.UsesUserConfirmedTopicContext},
		{"RestrictsMemorySearchToEpisodic", memoryrecall.ReadMode.RestrictsMemorySearchToEpisodic},
		{"GovernsAssertionTime", memoryrecall.ReadMode.GovernsAssertionTime},
		{"UsesExplicitMemoryActionNames", memoryrecall.ReadMode.UsesExplicitMemoryActionNames},
		{"UsesExplicitKnowledgeSourceName", memoryrecall.ReadMode.UsesExplicitKnowledgeSourceName},
		{"OrdersMemoryDraftBeforeMemoryRead", memoryrecall.ReadMode.OrdersMemoryDraftBeforeMemoryRead},
		{"DefersFirstToolChoiceToModel", memoryrecall.ReadMode.DefersFirstToolChoiceToModel},
		{"UsesTopicMemoryReadTool", memoryrecall.ReadMode.UsesTopicMemoryReadTool},
		{"UsesTopicMemoryReadObservationV4", memoryrecall.ReadMode.UsesTopicMemoryReadObservationV4},
		{"UsesAuthoritativeTopicBootstrap", memoryrecall.ReadMode.UsesAuthoritativeTopicBootstrap},
		{"UsesModelPlanner", memoryrecall.ReadMode.UsesModelPlanner},
		{"UsesTopicResolver", memoryrecall.ReadMode.UsesTopicResolver},
		{"UsesSemanticTopicLayer", memoryrecall.ReadMode.UsesSemanticTopicLayer},
		{"UsesRecallCycle", memoryrecall.ReadMode.UsesRecallCycle},
		{"SharesIssuedRecordsAcrossMemoryTools", memoryrecall.ReadMode.SharesIssuedRecordsAcrossMemoryTools},
	} {
		if predicate.call(v1) != predicate.call(v2) {
			t.Errorf("%s disagrees between the two source-priority identities", predicate.name)
		}
	}
	// ...and the one predicate that does differ differs in the expected direction.
	if v1.UsesSummaryHistoryRead() || !v2.UsesSummaryHistoryRead() {
		t.Fatal("UsesSummaryHistoryRead is not the new mode alone")
	}
	for _, mode := range []memoryrecall.ReadMode{
		memoryrecall.ModeLegacyV2, memoryrecall.ModeAgentMemoryToolV1, memoryrecall.ModeAgentMemoryToolV2,
		memoryrecall.ModeAgentMemoryToolV3, memoryrecall.ModeAgentMemoryToolV4,
		memoryrecall.ModeAgentMemoryUserConfirmedV1, memoryrecall.ModeEpisodeAssertionGovernedV11,
		memoryrecall.ModeAgentMemoryExplicitActionsV1, memoryrecall.ModeAgentMemoryExplicitSourcesV1,
	} {
		if mode.UsesSummaryHistoryRead() {
			t.Errorf("mode %s acquired summary read-back", mode)
		}
	}
}

// Names and budgets are governance the model can observe, so they stay as v1
// published them. The single addition is the per-capability history ceiling, and
// it is stated here rather than left to a comment.
func TestNewModeKeepsTheSourcePriorityToolSurfaceAndEnvelope(t *testing.T) {
	v1 := AgentMemorySourcePriorityV1RunnerConfig()
	v2 := AgentMemorySourcePriorityV2RunnerConfig()
	if v1.ToolSurface != v2.ToolSurface || v2.ToolSurface != SurfaceSourcePriority {
		t.Fatalf("surfaces = %q / %q", v1.ToolSurface, v2.ToolSurface)
	}
	if v1.MaxToolCalls != v2.MaxToolCalls || v1.MaxModelCalls != v2.MaxModelCalls ||
		v1.MaxGraphSteps != v2.MaxGraphSteps || v1.Deadline != v2.Deadline {
		t.Fatalf("run envelope moved: %#v vs %#v", v1, v2)
	}
	if v1.ForcesKnowledgeSearchWithMemoryTool != v2.ForcesKnowledgeSearchWithMemoryTool ||
		v1.ForcesMemoryDraftBeforeMemoryRead != v2.ForcesMemoryDraftBeforeMemoryRead {
		t.Fatal("first-tool governance moved")
	}
	if v1.PerTool.MemorySearch != v2.PerTool.MemorySearch ||
		v1.PerTool.KnowledgeSearch != v2.PerTool.KnowledgeSearch ||
		v1.PerTool.DocumentRead != v2.PerTool.DocumentRead ||
		v1.PerTool.ProposeMemory != v2.PerTool.ProposeMemory ||
		v1.PerTool.TopicMemoryRead != v2.PerTool.TopicMemoryRead {
		t.Fatal("an existing per-tool budget moved")
	}
	if v1.PerTool.HistoryRead != 0 || v2.PerTool.HistoryRead != DefaultHistoryReadCalls {
		t.Fatalf("history ceiling = %d / %d", v1.PerTool.HistoryRead, v2.PerTool.HistoryRead)
	}
	if got, want := ToolNamesOn(v2.Surface()), ToolNamesOn(v1.Surface()); !slices.Equal(got, want) {
		t.Fatalf("tool names = %v want %v", got, want)
	}
	if err := v2.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The whole isolation claim, checked where it is decided: the same summary, with
// the same citable ids, in front of every older mode, grants nothing.
//
// The summary cites ids 21 and 22, which no episode fixture cites, so a grant for
// them can only have come from the summary.
func TestOlderModesGrantNoSummaryEvidence(t *testing.T) {
	legible := summaryDocumentJSON(t, "把 RPC 拆包改成方案 B", map[string][]uint{
		"决定用长度字段": {21}, "否决了分隔符": {22},
	})
	snapshot := SummarySnapshot{Content: legible, CoveredMessageID: 40, Version: 3}
	episodes := []model.EpisodeMemory{testEpisode("episode-1", "第一次讨论", time.Unix(0, 0))}
	for _, mode := range []memoryrecall.ReadMode{
		memoryrecall.ModeLegacyV2, memoryrecall.ModeAgentMemoryToolV1, memoryrecall.ModeAgentMemoryToolV2,
		memoryrecall.ModeAgentMemoryToolV3, memoryrecall.ModeAgentMemoryToolV4,
		memoryrecall.ModeAgentMemoryUserConfirmedV1, memoryrecall.ModeEpisodeAssertionGovernedV11,
		memoryrecall.ModeAgentMemoryExplicitActionsV1, memoryrecall.ModeAgentMemoryExplicitSourcesV1,
		memoryrecall.ModeAgentMemorySourcePriorityV1,
	} {
		config := summaryReadContextConfig()
		config.MemoryReadMode = mode
		config.EpisodeRecallMode = EpisodeRecallInject
		input := summaryRun()
		input.EpisodeRecallReady = true
		window := buildSummaryWindow(t, config, snapshot, episodes, input)

		if len(window.SummaryHistoryGrants) != 0 {
			t.Fatalf("mode %s granted summary evidence: %+v", mode, window.SummaryHistoryGrants)
		}
		if window.Budget.Summary == 0 {
			t.Fatalf("mode %s lost the summary block as well: %+v", mode, window.Budget)
		}
		set := NewHistoryGrantSet(input, MergeHistoryMessageGrants(window.EpisodeHistoryGrants, window.SummaryHistoryGrants))
		if len(set.Grants(21)) != 0 || len(set.Grants(22)) != 0 {
			t.Fatalf("mode %s could read a summary id through the shared tool", mode)
		}
		// Narrowing the summary path must not have narrowed anything older. Every
		// mode that pre-injects Episodes still grants what those Episodes cited.
		if mode.AutoInjectsTopicAndEpisodeMemory() {
			if len(window.EpisodeHistoryGrants) != 3 || len(set.Grants(11)) != 1 {
				t.Fatalf("mode %s lost its episode grants: %+v", mode, window.EpisodeHistoryGrants)
			}
		}
	}
}

// Under the new mode the summary's ids become grants with their full provenance,
// and the window says plainly which half it did not produce.
//
// An older round might read the empty episode list below as a bug. It is the
// mode: the source-priority family injects global memory alone and reaches
// Episodes through memory_search, whose cards already carry their own bounded
// quotations. So the summary is precisely the half that had no read-back path,
// and a mixed call across both sources is a property of the tool and the grant
// model rather than of any single mode's window - which is what the read tool's
// own tests cover.
func TestSummaryReadModeGrantsSummaryEvidenceWithFullProvenance(t *testing.T) {
	config := summaryReadContextConfig()
	config.EpisodeRecallMode = EpisodeRecallInject
	window := buildSummaryWindow(t, config, SummarySnapshot{
		Content:          summaryDocumentJSON(t, "目标", map[string][]uint{"决定": {21, 22}}),
		CoveredMessageID: 40, Version: 5,
	}, []model.EpisodeMemory{testEpisode("episode-1", "第一次讨论", time.Unix(0, 0))},
		RunContext{
			UserID: 7, SessionID: "session-current", Question: "我的原话是什么？", EpisodeRecallReady: true,
		})
	if got := grantedMessageIDs(window.SummaryHistoryGrants); !slices.Equal(got, []uint{21, 22}) {
		t.Fatalf("summary grants=%v", got)
	}
	if len(window.EpisodeHistoryGrants) != 0 {
		t.Fatalf("episode grants=%v want none: this mode reaches Episodes through memory_search",
			window.EpisodeHistoryGrants)
	}
	for _, grant := range window.SummaryHistoryGrants {
		if grant.SourceKind != HistorySourceSummary || grant.SourceSessionID != "session-current" ||
			grant.SummaryVersion != 5 || grant.SummaryWatermark != 40 ||
			grant.EpisodeID != "" || grant.EpisodeVersion != 0 {
			t.Fatalf("summary grant lost its provenance: %+v", grant)
		}
		// A grant that carried only a number could not be re-checked against the
		// session, the watermark or the version, which is what both the tool and
		// the query bind to. The owner is deliberately not part of it: that comes
		// from the run, and the query cannot be asked without it.
		if !grant.UsableIn(RunContext{UserID: 7, SessionID: "session-current"}) {
			t.Fatalf("grant unusable in the run that built it: %+v", grant)
		}
	}

	// And a grant set that does carry both halves - which is what a run gets when a
	// pre-injected Episode and a summary cite the same id - keeps them distinct
	// rather than collapsing into an id list.
	merged := MergeHistoryMessageGrants(append([]HistoryMessageGrant(nil), window.SummaryHistoryGrants...),
		[]HistoryMessageGrant{{MessageID: 21, SourceKind: HistorySourceEpisode,
			SourceSessionID: "episode-source-episode-1", EpisodeID: "episode-1", EpisodeVersion: 1}})
	set := NewHistoryGrantSet(RunContext{UserID: 7, SessionID: "session-current"}, merged)
	if set.Len() != 2 {
		t.Fatalf("distinct readable messages=%d want 21 and 22", set.Len())
	}
	if got := len(set.Grants(21)); got != 2 {
		t.Fatalf("provenances for id 21=%d want the summary and the episode", got)
	}
	if !set.Covers(21, "session-current") || !set.Covers(21, "episode-source-episode-1") {
		t.Fatal("one of the two granting sessions was lost")
	}
}
