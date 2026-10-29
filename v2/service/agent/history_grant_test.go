package agent

import (
	"GopherAI/common/memoryrecall"
	"GopherAI/model"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// snapshotSummaryProvider is the rolling-summary reader with a fixed answer, so
// a test can say exactly which summary version and watermark this turn saw.
type snapshotSummaryProvider struct{ snapshot SummarySnapshot }

func (p snapshotSummaryProvider) SessionSummary(context.Context, RunContext) (SummarySnapshot, error) {
	return p.snapshot, nil
}

// summaryDocumentJSON encodes a real v2 summary, so a grant can only come from
// what model.DecodeSessionSummaryDocument actually parses.
func summaryDocumentJSON(t *testing.T, goal string, decisions map[string][]uint) string {
	t.Helper()
	document := model.SessionSummaryDocument{
		SchemaVersion: model.SessionSummarySchemaV2,
		CurrentGoal:   goal,
	}
	for text, ids := range decisions {
		document.ConfirmedDecisions = append(document.ConfirmedDecisions,
			model.SessionSummaryEvidence{Text: text, EvidenceMessageIDs: ids})
	}
	slices.SortFunc(document.ConfirmedDecisions, func(a, b model.SessionSummaryEvidence) int {
		return strings.Compare(a.Text, b.Text)
	})
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// summaryReadContextConfig is the one mode that may turn a summary's evidence ids
// into grants. Every other mode is exercised with its own config to prove it does
// not.
//
// It is sized from DefaultContextConfig rather than testContextConfig because the
// source-priority family carries the longest system prompt in the codebase: under
// the 2048-token test target that prompt alone leaves no room for a summary block
// to be injected at all, which would make every case below pass for the wrong
// reason.
func summaryReadContextConfig() ContextConfig {
	config := DefaultContextConfig()
	config.MemoryReadMode = memoryrecall.ModeAgentMemorySourcePriorityV2
	config.MaxSummaryTokens = 400
	return config
}

// buildSummaryWindow assembles a window under any read mode, wiring the readers
// that mode's own contract requires. The memory-tool family demands a global
// reader and a run evidence registry before anything reaches the summary path,
// and a test that skipped them would be reporting a build error as a permission
// result.
func buildSummaryWindow(t *testing.T, config ContextConfig, snapshot SummarySnapshot,
	episodes []model.EpisodeMemory, input RunContext) ContextWindow {
	t.Helper()
	builder, err := NewContextBuilder(config, &fakeHistoryReader{}, EmptyTaskStateProvider{},
		EmptyMasteryProvider{}, staticMemories{"prefers examples"}, snapshotSummaryProvider{snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if config.MemoryReadMode.UsesMemorySearchTool() {
		builder.UseGlobalMemoryProvider(GlobalMemoryProviderFunc(func(context.Context, RunContext) ([]string, error) {
			return []string{"讲解偏好：先说结论"}, nil
		}))
		input.MemoryEvidence = NewMemoryEvidenceRegistry()
	}
	if config.MemoryReadMode.UsesAuthoritativeTopicBootstrap() {
		builder.UseAuthoritativeTopicMemoryProvider(&recordingTopicBootstrap{})
	}
	if len(episodes) > 0 {
		builder.UseEpisodeProvider(EpisodeProviderFunc(func(context.Context, RunContext) ([]model.EpisodeMemory, error) {
			return episodes, nil
		}))
	}
	window, err := builder.Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return window
}

func summaryRun() RunContext {
	return RunContext{UserID: 7, SessionID: "session-current", Question: "我当时是怎么说的？"}
}

// summaryDocumentWithGroupsJSON serialises a v2 summary whose three evidence
// groups cite distinct ids. Field order is confirmed_decisions, completed_items,
// open_loops, user_corrections, so the citations appear in a known sequence and a
// prefix cut can be reasoned about.
func summaryDocumentWithGroupsJSON(t *testing.T, goal string,
	decisions, openLoops, corrections []uint) string {
	t.Helper()
	document := model.SessionSummaryDocument{
		SchemaVersion: model.SessionSummarySchemaV2,
		CurrentGoal:   goal,
	}
	if len(decisions) > 0 {
		document.ConfirmedDecisions = model.SessionSummaryEvidenceList{
			{Text: "决定采用长度字段拆包，不用分隔符", EvidenceMessageIDs: decisions},
		}
	}
	if len(openLoops) > 0 {
		document.OpenLoops = model.SessionSummaryEvidenceList{
			{Text: "还需要补半包场景的回归测试", EvidenceMessageIDs: openLoops},
		}
	}
	if len(corrections) > 0 {
		document.UserCorrections = model.SessionSummaryEvidenceList{
			{Text: "用户纠正了关于超时单位的说法", EvidenceMessageIDs: corrections},
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func buildWindow(t *testing.T, config ContextConfig, snapshot SummarySnapshot, input RunContext) ContextWindow {
	return buildSummaryWindow(t, config, snapshot, nil, input)
}

// The summary is a compression of messages the model can no longer see, and its
// evidence ids are the only way back. Nothing else about a stored summary may
// open a read: not the watermark, not the row's existence, and not a block the
// budget trimmed away.
func TestContextBuilderGrantsSummaryEvidenceOnlyWhenInjectedAndDecodable(t *testing.T) {
	const watermark = uint(40)
	legible := summaryDocumentJSON(t, "把 RPC 拆包改成方案 B", map[string][]uint{
		"决定用长度字段": {11},
		"否决了分隔符":  {13, 20},
	})
	aboveWatermark := summaryDocumentJSON(t, "把 RPC 拆包改成方案 B", map[string][]uint{
		"决定用长度字段":  {11},
		"引用了水位线之上": {41, 900},
	})
	noEvidence := summaryDocumentJSON(t, "把 RPC 拆包改成方案 B", nil)
	legacyText := "早前我们决定了用长度字段拆包"

	for _, test := range []struct {
		name     string
		snapshot SummarySnapshot
		// configured as MaxSummaryTokens; zero means the block cannot fit.
		summaryTokens int
		wantIDs       []uint
		// Whether the summary text itself reached the window. Losing the read-back
		// permission must not also lose the compression: that is the difference
		// between a narrower tool and a worse answer.
		wantSummaryRendered bool
	}{
		{"no summary at all", SummarySnapshot{}, 180, nil, false},
		{"summary without coverage", SummarySnapshot{Content: legible, Version: 3}, 180, nil, true},
		{"summary without a version", SummarySnapshot{Content: legible, CoveredMessageID: watermark}, 180, nil, true},
		{"legacy plain-text summary", SummarySnapshot{Content: legacyText, CoveredMessageID: watermark, Version: 1}, 180, nil, true},
		{"v2 summary citing nothing", SummarySnapshot{Content: noEvidence, CoveredMessageID: watermark, Version: 3}, 180, nil, true},
		{"summary trimmed out of the window", SummarySnapshot{Content: legible, CoveredMessageID: watermark, Version: 3}, 0, nil, false},
		{"ids above the watermark dropped", SummarySnapshot{Content: aboveWatermark, CoveredMessageID: watermark, Version: 3}, 180, []uint{11}, true},
		{"legible injected summary grants its ids", SummarySnapshot{Content: legible, CoveredMessageID: watermark, Version: 3}, 180, []uint{11, 13, 20}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := summaryReadContextConfig()
			config.MaxSummaryTokens = test.summaryTokens
			window := buildWindow(t, config, test.snapshot, summaryRun())

			if got := grantedMessageIDs(window.SummaryHistoryGrants); !slices.Equal(got, test.wantIDs) {
				t.Fatalf("summary grants=%v want %v", got, test.wantIDs)
			}
			if rendered := window.Budget.Summary > 0; rendered != test.wantSummaryRendered {
				t.Fatalf("summary rendered=%t want %t (budget=%d)",
					rendered, test.wantSummaryRendered, window.Budget.Summary)
			}
			// The ordinary conversation is intact either way: a turn that cannot
			// read its summary back is still a turn that can be answered.
			if len(window.Messages) < 2 {
				t.Fatalf("window carries %d messages", len(window.Messages))
			}
		})
	}
}

// A grant is only worth what its metadata says. The watermark, the version and
// the session are what bound a summary read, so a grant missing any of them
// authorises nothing even when the id itself looks plausible.
func TestSummaryGrantCarriesVersionWatermarkAndCurrentSession(t *testing.T) {
	window := buildWindow(t, summaryReadContextConfig(), SummarySnapshot{
		Content:          summaryDocumentJSON(t, "目标", map[string][]uint{"决定": {11}}),
		CoveredMessageID: 40, Version: 7,
	}, summaryRun())

	if len(window.SummaryHistoryGrants) != 1 {
		t.Fatalf("grants=%+v", window.SummaryHistoryGrants)
	}
	grant := window.SummaryHistoryGrants[0]
	if grant.MessageID != 11 || grant.SourceKind != HistorySourceSummary ||
		grant.SourceSessionID != "session-current" || grant.SummaryVersion != 7 || grant.SummaryWatermark != 40 {
		t.Fatalf("grant=%+v want the frozen summary identity", grant)
	}
	if !grant.UsableIn(summaryRun()) {
		t.Fatal("the builder's own grant is not usable in the run that built it")
	}
	// The same grant seen by a different session authorises nothing there.
	if grant.UsableIn(RunContext{UserID: 7, SessionID: "another-session"}) {
		t.Fatal("a summary grant crossed sessions")
	}
}

// Episodes describe earlier conversations, so their evidence lives in the session
// the episode was projected from. Binding an episode grant to the session asking
// the question would leave the episode path with nothing readable.
func TestEpisodeGrantsKeepTheEpisodeSourceSession(t *testing.T) {
	config := testContextConfig()
	config.EpisodeRecallMode = EpisodeRecallInject
	config.MaxEpisodeTokens = 1000
	builder, err := NewContextBuilder(config, &fakeHistoryReader{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseEpisodeProvider(EpisodeProviderFunc(func(context.Context, RunContext) ([]model.EpisodeMemory, error) {
		return []model.EpisodeMemory{testEpisode("episode-1", "第一次讨论", time.Unix(0, 0))}, nil
	}))
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 7, SessionID: "session-current", Question: "上次我们决定了什么？", EpisodeRecallReady: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// This mode's published answer is every id the stored card cites, including 15,
	// which renderEpisode does not print. v3 narrows that to the visible citations and
	// bounds them by the card's watermark; this mode does not, and UsesEpisodeEvidenceRead
	// is the predicate that says which is which. TestV3NarrowsEpisodeGrantsWhereOldModesDoNot
	// pins the difference, so this line records a deliberate mode boundary rather than an
	// oversight someone will "fix" by editing it.
	if got := grantedMessageIDs(window.EpisodeHistoryGrants); !slices.Equal(got, []uint{11, 13, 15}) {
		t.Fatalf("episode grants=%v want every id the injected card cited", got)
	}
	for _, grant := range window.EpisodeHistoryGrants {
		if grant.SourceKind != HistorySourceEpisode {
			t.Fatalf("grant=%+v want the episode kind", grant)
		}
		if grant.SourceSessionID != "episode-source-episode-1" {
			t.Fatalf("grant=%+v want the episode's own session, not the run's", grant)
		}
		if grant.EpisodeID != "episode-1" || grant.EpisodeVersion != 1 {
			t.Fatalf("grant=%+v want the episode version bound", grant)
		}
		if grant.SummaryVersion != 0 || grant.SummaryWatermark != 0 {
			t.Fatalf("grant=%+v冒充了摘要来源", grant)
		}
	}
	// The two sources stay recorded apart: an episode turn grants no summary ids.
	if len(window.SummaryHistoryGrants) != 0 {
		t.Fatalf("summary grants=%v want none", window.SummaryHistoryGrants)
	}
}

// The window is the freeze. A newer summary that lands after it was assembled
// must not widen what this run may read, and must not move its watermark either.
func TestSummaryGrantsAreFrozenWhenANewerSummaryLandsMidRun(t *testing.T) {
	oldSummary := SummarySnapshot{
		Content:          summaryDocumentJSON(t, "旧目标", map[string][]uint{"旧决定": {11}}),
		CoveredMessageID: 40, Version: 3,
	}
	window := buildWindow(t, summaryReadContextConfig(), oldSummary, summaryRun())

	// Compaction advances while the answer is still being assembled: the next turn
	// sees a summary with a new version, a higher watermark and an id this run was
	// never shown.
	later := SummarySnapshot{
		Content:          summaryDocumentJSON(t, "新目标", map[string][]uint{"新决定": {77}}),
		CoveredMessageID: 90, Version: 9,
	}
	laterWindow := buildWindow(t, summaryReadContextConfig(), later, summaryRun())
	if got := grantedMessageIDs(window.SummaryHistoryGrants); !slices.Equal(got, []uint{11}) {
		t.Fatalf("grants changed with the store: %v want the ids this window was built on", got)
	}
	grant := window.SummaryHistoryGrants[0]
	if grant.SummaryVersion != 3 || grant.SummaryWatermark != 40 {
		t.Fatalf("grant=%+v want the version and watermark this run was built on", grant)
	}
	if got := grantedMessageIDs(laterWindow.SummaryHistoryGrants); !slices.Equal(got, []uint{77}) {
		t.Fatalf("the later turn did not get its own grants: %v", got)
	}

	// The tool set this run registers from its own window cannot read the new id.
	set := NewHistoryGrantSet(summaryRun(), MergeHistoryMessageGrants(window.SummaryHistoryGrants))
	if len(set.Grants(77)) != 0 {
		t.Fatal("a summary the model never saw granted a read")
	}
	if len(set.Grants(11)) != 1 {
		t.Fatalf("provenance for the frozen id=%v", set.Grants(11))
	}
}

func TestMergeHistoryMessageGrantsKeepsBothProvenancesForOneID(t *testing.T) {
	shared := uint(11)
	fromSummary := []HistoryMessageGrant{{MessageID: shared, SourceKind: HistorySourceSummary,
		SourceSessionID: "session-current", SummaryVersion: 3, SummaryWatermark: 40}}
	fromEpisode := []HistoryMessageGrant{{MessageID: shared, SourceKind: HistorySourceEpisode,
		SourceSessionID: "session-episode", EpisodeID: "episode-1", EpisodeVersion: 2}}

	merged := MergeHistoryMessageGrants(fromSummary, fromEpisode, fromSummary)
	if len(merged) != 2 {
		t.Fatalf("merged=%+v want both provenances and no exact repeat", merged)
	}
	if merged[0].SourceKind != HistorySourceSummary || merged[1].SourceKind != HistorySourceEpisode {
		t.Fatalf("merged=%+v want list order preserved", merged)
	}

	set := NewHistoryGrantSet(summaryRun(), merged)
	if set.Len() != 1 {
		t.Fatalf("distinct readable messages=%d want one", set.Len())
	}
	if got := len(set.Grants(shared)); got != 2 {
		t.Fatalf("provenances=%d want the read to stay attributed to both cards", got)
	}
	if !set.Covers(shared, "session-current") || !set.Covers(shared, "session-episode") {
		t.Fatal("a granted session was lost")
	}
	if set.Covers(shared, "session-third") {
		t.Fatal("an ungranted session was covered")
	}
}

// The runner's first-tool rule may only force a tool the model can actually use:
// registered, with at least one id behind it.
func TestForcedHistoryReadNeedsARegisteredToolAndAGrant(t *testing.T) {
	question := "我当时的原话是什么？"
	if !requiresExactHistoryEvidence(question) {
		t.Fatal("the question no longer asks for exact history evidence")
	}
	state := &runState{
		config: AgentMemoryToolRunnerConfig(),
		input:  RunContext{Question: question},
	}
	if got := state.forcedToolName(); got != "" {
		t.Fatalf("forced tool=%q want none while read_history_message is not registered", got)
	}
	state.historyReadAvailable = true
	if got := state.forcedToolName(); got != readHistoryMessageToolName {
		t.Fatalf("forced tool=%q want %q", got, readHistoryMessageToolName)
	}
	// An answer that already carries verbatim learner words needs no second read.
	state.historyEvidence = true
	if got := state.forcedToolName(); got != "" {
		t.Fatalf("forced tool=%q want none once history evidence exists", got)
	}
	// And a question that does not ask for exact words must not be forced either.
	state.historyEvidence = false
	state.input.Question = "解释一下 TCP 粘包"
	if got := state.forcedToolName(); got != "" {
		t.Fatalf("forced tool=%q want none for an ordinary question", got)
	}
}

// Two calls is what the two evidence sources need separately. The third is the
// model re-asking, and it is refused without spending the run's whole tool budget.
func TestHistoryReadPerToolBudgetRefusesTheThirdCall(t *testing.T) {
	config := AgentMemorySourcePriorityV2RunnerConfig()
	if config.PerTool.HistoryRead != DefaultHistoryReadCalls {
		t.Fatalf("history read limit=%d want %d", config.PerTool.HistoryRead, DefaultHistoryReadCalls)
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	state := &runState{config: config, perToolCalls: map[string]int{}}
	for call := 1; call <= DefaultHistoryReadCalls; call++ {
		if _, _, _, err := state.reserveTool(readHistoryMessageToolName, `{"message_ids":[11]}`); err != nil {
			t.Fatalf("call %d refused: %v", call, err)
		}
		state.lastFingerprint = ""
	}
	_, _, _, err := state.reserveTool(readHistoryMessageToolName, `{"message_ids":[13]}`)
	if err == nil {
		t.Fatal("a third history read was allowed")
	}
	if !strings.Contains(err.Error(), readHistoryMessageToolName) {
		t.Fatalf("err=%v want the refusal to name the capped tool", err)
	}
	if state.toolCalls != DefaultHistoryReadCalls {
		t.Fatalf("whole-run tool calls=%d want the refused call uncharged", state.toolCalls)
	}
	// The cap belongs to this capability alone: knowledge_search still has room.
	if _, _, _, err := state.reserveTool(knowledgeSearchToolName, `{"query":"粘包"}`); err != nil {
		t.Fatalf("knowledge_search blocked by the history cap: %v", err)
	}
}

// The renderer cuts a long summary to the remaining token budget, and a cut
// document still decodes fine from the stored row. So visibility must be decided
// against the rendered block, per id: an id below the cut was never shown, and
// granting it would let the model read back a citation it could not have seen.
func TestVisibleSummaryEvidenceIDsCutsWithTheRenderedText(t *testing.T) {
	cited := []uint{11, 12, 13, 33, 112}
	for _, test := range []struct {
		name     string
		rendered string
		want     []uint
	}{
		{"complete arrays", `{"current_goal":"目标","confirmed_decisions":[{"text":"甲","evidence_message_ids":[11,12]},{"text":"乙","evidence_message_ids":[13]}]}`, []uint{11, 12, 13}},
		{"pretty printed arrays still parse", "{\n  \"open_loops\": [\n    {\n      \"text\": \"甲\",\n      \"evidence_message_ids\": [ 11 , 12 ]\n    }\n  ]\n}", []uint{11, 12}},
		{"cut inside the key", `{"open_loops":[{"text":"甲","evidence_message_i`, nil},
		{"cut after the key", `{"open_loops":[{"text":"甲","evidence_message_ids"`, nil},
		{"cut after the colon", `{"open_loops":[{"text":"甲","evidence_message_ids":`, nil},
		{"cut after the bracket", `{"open_loops":[{"text":"甲","evidence_message_ids":[`, nil},
		{"cut mid first number", `{"open_loops":[{"text":"甲","evidence_message_ids":[1`, nil},
		{"cut mid second number", `{"open_loops":[{"text":"甲","evidence_message_ids":[11,1`, []uint{11}},
		{"cut right after a comma", `{"open_loops":[{"text":"甲","evidence_message_ids":[11,`, []uint{11}},
		{"empty array", `{"open_loops":[{"text":"甲","evidence_message_ids":[]}]}`, nil},
		{"digits outside an evidence array authorise nothing", `{"current_goal":"第 11 条和第 13 条都要重写","key_errors":["半包"]}`, nil},
		{"a longer number is not its prefix", `{"open_loops":[{"text":"甲","evidence_message_ids":[112]}]}`, []uint{112}},
		{"second group cut, first group kept", `{"confirmed_decisions":[{"text":"甲","evidence_message_ids":[33]}],"open_loops":[{"text":"乙","evidence_message_ids":[11`, []uint{33}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := visibleSummaryEvidenceIDs(test.rendered, cited)
			if !slices.Equal(got, test.want) {
				t.Fatalf("visible=%v want %v", got, test.want)
			}
		})
	}
}

// Sweeping the summary budget walks the cut through the serialised document. Two
// things must hold at every step: nothing may be granted before the citation that
// precedes it in the text, and the tail id must disappear while the block is still
// being injected - which is precisely what granting from the decoded document
// alone got wrong.
func TestSummaryGrantsNeverOutrunTheRenderedBlock(t *testing.T) {
	content := summaryDocumentWithGroupsJSON(t, "把 RPC 拆包改成方案 B，并补半包测试",
		[]uint{21}, []uint{22}, []uint{33})
	early := summaryDocumentWithGroupsJSON(t, "把 RPC 拆包改成方案 B，并补半包测试", []uint{21}, nil, nil)
	if len(early) >= len(content) {
		t.Fatal("the fixture does not put the later citations after the earlier ones")
	}

	sawPartial, sawFull := false, false
	for budget := 1; budget <= 600; budget++ {
		config := summaryReadContextConfig()
		config.MaxSummaryTokens = budget
		window := buildWindow(t, config, SummarySnapshot{
			Content: content, CoveredMessageID: 40, Version: 3,
		}, summaryRun())
		granted := grantedMessageIDs(window.SummaryHistoryGrants)
		for _, id := range granted {
			switch id {
			case 21:
				// The head citation: the only id grantable on its own.
			case 22:
				if !slices.Contains(granted, uint(21)) {
					t.Fatalf("budget=%d granted 22 without 21: %v", budget, granted)
				}
			case 33:
				if !slices.Contains(granted, uint(21)) || !slices.Contains(granted, uint(22)) {
					t.Fatalf("budget=%d granted the tail without its prefix: %v", budget, granted)
				}
				sawFull = true
			default:
				t.Fatalf("budget=%d granted an id the document does not cite: %v", budget, granted)
			}
		}
		if slices.Contains(granted, uint(21)) && !slices.Contains(granted, uint(33)) {
			sawPartial = true
			// The block really reached the window, so this is a visible-tail case
			// rather than the whole block being dropped.
			if window.Budget.Summary == 0 {
				t.Fatalf("budget=%d reported no summary block yet granted 21", budget)
			}
		}
		if slices.Contains(granted, uint(33)) {
			break
		}
	}
	if !sawPartial {
		t.Fatal("no budget granted the head citation while the tail stayed ungranted")
	}
	if !sawFull {
		t.Fatal("no budget ever granted the fully rendered tail citation, so the filter is too strict")
	}

	// And an id below the cut is not merely absent from the window: it is absent
	// from the set the tool consults before any database access, which is what
	// turns it into a refusal rather than a read.
	config := summaryReadContextConfig()
	config.MaxSummaryTokens = 120
	window := buildWindow(t, config, SummarySnapshot{
		Content: content, CoveredMessageID: 40, Version: 3,
	}, summaryRun())
	set := NewHistoryGrantSet(summaryRun(), MergeHistoryMessageGrants(window.SummaryHistoryGrants))
	if slices.Contains(grantedMessageIDs(window.SummaryHistoryGrants), uint(33)) {
		t.Fatalf("the narrow budget still granted the tail: %+v", window.SummaryHistoryGrants)
	}
	if len(set.Grants(33)) != 0 {
		t.Fatalf("the read tool could still reach the unrendered id: %+v", set.Grants(33))
	}
}

func grantedMessageIDs(grants []HistoryMessageGrant) []uint {
	ids := make([]uint, 0, len(grants))
	for _, grant := range grants {
		ids = append(ids, grant.MessageID)
	}
	slices.Sort(ids)
	return ids
}
