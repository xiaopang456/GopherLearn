package agent

import (
	"context"
	"strings"
	"testing"

	"GopherAI/common/memoryrecall"
	"GopherAI/model"
)

// The clarification's continuation rule.
//
// It has to name the history-reading tools THIS turn offers, and nothing
// else. The summary browse is registered only when the summary is in the
// applied mode, and the segment search only when the segment index is
// there - so a rule that named both unconditionally would be a rule the
// model could not obey half the time, and a model told to call a tool it
// cannot see calls it anyway and the turn fails.
//
// The rule is a separate block and not a line of the base prompt: the base
// prompt's bytes are the ones the published measurements were taken against,
// and a rule that matters only on a turn that continues a clarification must
// not move them.

// clarificationWindow builds a window for a turn that continues a
// clarification, with the tools this turn offers.
func clarificationWindow(t *testing.T, tools []string) ContextWindow {
	t.Helper()
	clarification := &model.SessionClarification{
		Question: "上次你给我排的那三道题里，第二道练的是什么？",
		Candidates: []model.SessionClarificationCandidate{
			{SessionID: "session-a", Title: "洄漪"},
		},
	}
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 1, IsUser: false, Content: "你在问哪一次？", Clarification: clarification},
		{ID: 2, IsUser: true, Content: "2"},
	}}
	config := testContextConfig()
	config.ModelWindowTokens = 32768
	config.OutputReserveTokens = 4000
	config.InputTargetTokens = 20000
	config.EvidenceReserve = 6500
	config.MaxGlobalUserMemoryTokens = 800
	config.MaxTopicUserMemoryTokens = 1500
	config.MaxSummaryTokens = 2000
	config.MaxMasteryTokens = 900
	builder, err := NewContextBuilder(config, history, staticTaskState("active learning task"),
		nil, staticMemories{"prefers examples"}, staticSummary("earlier decision"))
	if err != nil {
		t.Fatal(err)
	}
	window, err := builder.Build(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "2", AvailableTools: tools})
	if err != nil {
		t.Fatalf("build the window: %v", err)
	}
	return window
}

// The rule that keeps a tool read from being refused as somebody else's.
//
// The acceptance run's follow-up turns named the conversation by its TITLE in
// the tool call ("Evaluation conversation"), and the tools refused it with
// session_not_allowed: the lookup is by conversation id. The continuation rule
// has to say so, because the lock block shows both the title and the id.
func TestTheContinuationRuleSaysToPassTheConversationID(t *testing.T) {
	window := clarificationWindow(t, []string{
		"browse_summary_segments", "search_history_segments", "search_session_history",
		"read_session_whiteboard",
	})
	prompt := window.Messages[0].Content
	if !strings.Contains(prompt, "conversation ID") {
		t.Fatalf("the prompt does not say to pass the conversation id: %s", prompt)
	}
	if !strings.Contains(prompt, "not the conversation's title") {
		t.Fatalf("the prompt does not warn against the title: %s", prompt)
	}
}

// The prompt of a turn that continues a clarification names the tools this
// turn offers, and only those.
func TestTheContinuationRuleNamesOnlyTheToolsThisTurnOffers(t *testing.T) {
	for _, test := range []struct {
		name  string
		tools []string
		want  []string
		deny  []string
	}{
		{
			name:  "the summary browse is there",
			tools: []string{"browse_summary_segments"},
			want:  []string{"browse_summary_segments"},
			deny:  []string{"search_history_segments"},
		},
		{
			name:  "the segment search is there",
			tools: []string{"search_history_segments"},
			want:  []string{"search_history_segments"},
			deny:  []string{"browse_summary_segments"},
		},
		{
			name:  "both are there",
			tools: []string{"browse_summary_segments", "search_history_segments"},
			want:  []string{"browse_summary_segments", "search_history_segments"},
			deny:  []string{},
		},
		{
			name:  "neither is there",
			tools: []string{"search_session_cards"},
			want:  []string{},
			deny:  []string{"browse_summary_segments", "search_history_segments"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			window := clarificationWindow(t, test.tools)
			prompt := window.Messages[0].Content
			for _, tool := range test.want {
				if !strings.Contains(prompt, tool) {
					t.Fatalf("the prompt does not name %q: %s", tool, prompt)
				}
			}
			for _, tool := range test.deny {
				if strings.Contains(prompt, tool) {
					t.Fatalf("the prompt names %q and this turn does not have it: %s", tool, prompt)
				}
			}
		})
	}
}

// A turn with no clarification to continue carries no continuation rule.
//
// The rule is about a turn that continues a question, and a turn that
// continues nothing must not pay for the paragraph.
func TestATurnWithNoClarificationCarriesNoContinuationRule(t *testing.T) {
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 1, IsUser: false, Content: "一句普通的话"},
	}}
	config := testContextConfig()
	builder, err := NewContextBuilder(config, history, staticTaskState("active learning task"),
		nil, staticMemories{"prefers examples"}, staticSummary("earlier decision"))
	if err != nil {
		t.Fatal(err)
	}
	window, err := builder.Build(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "普通的问题",
			AvailableTools: []string{"browse_summary_segments", "search_history_segments"}})
	if err != nil {
		t.Fatalf("build the window: %v", err)
	}
	if strings.Contains(window.Messages[0].Content, "continuing a question the learner asked in an earlier turn") {
		t.Fatalf("an ordinary turn carries the continuation rule: %s", window.Messages[0].Content)
	}
}

// The base prompt's bytes do not move.
//
// The published measurements were taken against them, and a rule that
// matters only on a turn that continues a clarification must not move them.
func TestTheBasePromptIsUnchangedByTheContinuationRule(t *testing.T) {
	for _, mode := range []memoryrecall.ReadMode{
		memoryrecall.ModeLegacyV2, memoryrecall.ModeAgentMemoryToolV1,
		memoryrecall.ModeAgentMemoryToolV3, memoryrecall.ModeAgentMemoryToolV4,
	} {
		base := SystemPromptFor(mode)
		if strings.Contains(base, "continuing a question the learner asked in an earlier turn") {
			t.Fatalf("the base prompt of %s carries the continuation rule", mode)
		}
		capabilities := CapabilitiesFromToolNames([]string{
			"knowledge_search", "document_read", "read_history_message",
			"memory_search", "topic_memory_read", "propose_memory",
			"browse_summary_segments", "search_session_cards",
			"search_history_segments", "read_session_whiteboard",
			"ask_which_conversation",
		})
		// The save rule is the one place where the prompt depends on the
		// question rather than on the tool table, so it is carried here
		// explicitly: the property under test is about the TOOLS.
		capabilities.MemoryIntent = true
		if rendered := RenderSystemPrompt(base, capabilities); rendered != base {
			t.Fatalf("the rendered prompt of %s is not the base prompt byte for byte", mode)
		}
	}
}

// The rule that reaches a detail neither the summary nor the segments kept.
//
// p2-acc-04-r1 answered from the locked summary and missed the message that
// said "动作、提问、验收" - it was inside the summary's range but dropped by
// compression. The continuation rule has to name the raw-word search for that
// case, and must be dropped when this turn does not offer it.
func TestTheContinuationRuleNamesTheRawWordSearch(t *testing.T) {
	window := clarificationWindow(t, []string{
		"browse_summary_segments", "search_history_segments", "search_session_history",
	})
	prompt := window.Messages[0].Content
	for _, phrase := range []string{"search_session_history", "short keyword"} {
		if !strings.Contains(prompt, phrase) {
			t.Fatalf("the continuation rule does not say %q: %s", phrase, prompt)
		}
	}
}

func TestTheContinuationRuleDropsTheRawWordSearchWhenTheTurnDoesNotOfferIt(t *testing.T) {
	window := clarificationWindow(t, []string{"browse_summary_segments", "search_history_segments"})
	prompt := window.Messages[0].Content
	if strings.Contains(prompt, "search_session_history") {
		t.Fatalf("the continuation rule names a tool this turn does not offer: %s", prompt)
	}
}

// The window keeps the question the learner asked before the clarification
// reply: the continuation's own Question is only the session id.
func TestTheContinuationWindowCarriesTheOriginalQuestion(t *testing.T) {
	window := clarificationWindow(t, []string{"browse_summary_segments"})
	const original = "上次你给我排的那三道题里，第二道练的是什么？"
	if window.OriginalQuestion != original {
		t.Fatalf("window original question = %q, want %q", window.OriginalQuestion, original)
	}
}

// A normal turn has no clarification to recover a question from.
func TestANormalWindowHasNoOriginalQuestion(t *testing.T) {
	config := testContextConfig()
	builder, err := NewContextBuilder(config, &fakeHistoryReader{}, staticTaskState("active learning task"),
		nil, staticMemories{"prefers examples"}, staticSummary("earlier decision"))
	if err != nil {
		t.Fatal(err)
	}
	window, err := builder.Build(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "这次聊什么"})
	if err != nil {
		t.Fatal(err)
	}
	if window.OriginalQuestion != "" {
		t.Fatalf("a normal turn carried an original question: %q", window.OriginalQuestion)
	}
}
