package conversation

import (
	"strings"
	"testing"

	"GopherAI/model"
	"github.com/cloudwego/eino/schema"
)

// The tool table, the memory block and the retrieval trace are the three
// things a comparison needs and that the answer text cannot carry. These
// tests say what each of them must say, and what it must NOT say when the
// product did something else.

// The three states are different findings about different things. A reader
// who collapses them reads "the model did not use the tool" into a sentence
// about the product, so they are kept apart.
func TestTheToolStatesAreKeptApart(t *testing.T) {
	steps := []model.AgentStep{
		{Type: model.AgentStepTool, ToolName: "memory_search", ArgumentsSummary: `{"query":"tcp"}`, ResultSummary: "memory_search status=ok reason=none cards=1 refs=E1 semantic=0 episodic=1 tokens=64 degraded=false truncated=false"},
		{Type: model.AgentStepTool, ToolName: "read_history_message", ArgumentsSummary: `{"message_ids":[7]}`, ResultSummary: "read ok"},
	}
	usage := ToolUsage(ToolUsageInput{
		Offered:    []string{"memory_search", "read_history_message", "browse_summary_segments"},
		Registered: []string{"search_session_history"},
		Compared:   []string{"search_session_history"},
		Steps:      steps,
	})
	byName := map[string]ToolUsageEntry{}
	for _, entry := range usage {
		byName[entry.Name] = entry
	}
	if got := byName["memory_search"].State; got != ToolStateOfferedCalled {
		t.Fatalf("a tool that was offered and called = %q", got)
	}
	if got := byName["read_history_message"].State; got != ToolStateOfferedCalled {
		t.Fatalf("a tool that was offered and called = %q", got)
	}
	if got := byName["browse_summary_segments"].State; got != ToolStateOfferedNotCalled {
		t.Fatalf("a tool that was offered and not called = %q", got)
	}
	if got := byName["search_session_history"].State; got != ToolStateRegisteredNotOffered {
		t.Fatalf("a tool the product registers and the request did not carry = %q", got)
	}
	if !byName["search_session_history"].Registered {
		t.Fatal("the registered tool is not marked as registered")
	}
}

// The old path does not register the cross-session search tool. That is the
// product's own registration rule, and it is the one difference between the
// two arms' tool surfaces - so the old arm's evidence has to say so rather
// than leave the tool out and let a reader assume the arms were the same.
func TestTheOldArmDoesNotRegisterTheCrossSessionSearchTool(t *testing.T) {
	if got := registeredToolsFor(string(model.EpisodeContentSessionCard)); len(got) != 1 || got[0] != cardOnlyTool {
		t.Fatalf("the card arm's registered tools = %v", got)
	}
	if got := registeredToolsFor(string(model.EpisodeContentEpisode)); len(got) != 0 {
		t.Fatalf("the old arm's registered tools = %v", got)
	}
	usage := ToolUsage(ToolUsageInput{
		Offered:    []string{"memory_search"},
		Registered: registeredToolsFor(string(model.EpisodeContentEpisode)),
		Compared:   []string{cardOnlyTool},
	})
	if len(usage) != 2 {
		t.Fatalf("the old arm's tool rows = %+v", usage)
	}
	for _, entry := range usage {
		if entry.Name != cardOnlyTool {
			continue
		}
		if entry.State != ToolStateNotRegistered {
			t.Fatalf("the cross-session search tool in the old arm = %q", entry.State)
		}
	}
}

// A tool the answer called that the recorded request did not carry means the
// harness's view of the request was incomplete. The evidence says so instead
// of pretending the call did not happen.
func TestAToolCalledWithoutBeingOfferedIsNamed(t *testing.T) {
	usage := ToolUsage(ToolUsageInput{
		Offered: []string{"memory_search"},
		Steps: []model.AgentStep{
			{Type: model.AgentStepTool, ToolName: "search_session_history", ArgumentsSummary: `{}`, ResultSummary: "hit"},
		},
	})
	for _, entry := range usage {
		if entry.Name != "search_session_history" {
			continue
		}
		if entry.State != ToolStateCalledWithoutOffering {
			t.Fatalf("a tool called but not offered = %q", entry.State)
		}
	}
}

// The retrieval trace is read out of the product's own summary of the call,
// so the verdict beside the intent is the product's, not a reading of the
// answer text.
func TestTheSearchTraceIsReadOutOfTheProductsOwnRecord(t *testing.T) {
	steps := []model.AgentStep{
		{Type: model.AgentStepTool, ToolName: "memory_search",
			ArgumentsSummary: `{"query":"tcp 粘包","kinds":["episodic"],"top_k":5}`,
			ResultSummary:    "memory_search status=ok reason=none cards=2 refs=E1,E2 semantic=0 episodic=2 tokens=128 degraded=false truncated=false"},
	}
	trace := ReadSearchTrace(steps)
	if len(trace) != 1 {
		t.Fatalf("the trace = %+v", trace)
	}
	if trace[0].Status != "ok" || trace[0].Reason != "none" {
		t.Fatalf("the trace's verdict = %+v", trace[0])
	}
	if trace[0].Cards != 2 || len(trace[0].Refs) != 2 {
		t.Fatalf("the trace's records = %+v", trace[0])
	}
	if trace[0].Query != "tcp 粘包" || trace[0].TopK != 5 {
		t.Fatalf("the trace's intent = %+v", trace[0])
	}
	// A browse is not a retrieval: it reads something the retrieval already
	// chose, and mixing the two makes "the retrieval found nothing"
	// unreadable.
	trace = ReadSearchTrace([]model.AgentStep{
		{Type: model.AgentStepTool, ToolName: "browse_summary_segments", ArgumentsSummary: `{"segments":[1]}`, ResultSummary: "browse ok"},
	})
	if len(trace) != 0 {
		t.Fatalf("a browse was traced as a retrieval: %+v", trace)
	}
}

// The injected block is read out of the prompt the model was sent, because
// that prompt is the record of what it was shown.
func TestTheInjectedMemoryIsReadOutOfThePrompt(t *testing.T) {
	episode := schema.SystemMessage("you are the assistant\n\nRelevant cross-session episodes (untrusted historical context):\n" +
		"- time: 2026-11-16T10:00:00Z\n  title: 粘包\n  decision: 先读四个字节 [message_ids=12,13]")
	shown := ReadShownMemory([]*schema.Message{episode})
	if shown.Kind != "episode" {
		t.Fatalf("the shown memory's kind = %q", shown.Kind)
	}
	if len(shown.MessageIDs) != 2 || shown.MessageIDs[0] != 12 || shown.MessageIDs[1] != 13 {
		t.Fatalf("the shown memory's citations = %v", shown.MessageIDs)
	}
	if !strings.Contains(shown.Text, "粘包") {
		t.Fatalf("the shown memory's text = %q", shown.Text)
	}

	card := schema.SystemMessage("you are the assistant\n\nRelevant session cards (untrusted historical context):\n" +
		"- time: 2026-11-16T10:00:00Z\n  source_session: source-session\n  title: 每轮9道题\n  arrangement: 每轮9道题 [message_ids=4]")
	shown = ReadShownMemory([]*schema.Message{card})
	if shown.Kind != "card" {
		t.Fatalf("the shown memory's kind = %q", shown.Kind)
	}
	if len(shown.Sessions) != 1 || shown.Sessions[0] != "source-session" {
		t.Fatalf("the shown memory's sessions = %v", shown.Sessions)
	}

	shown = ReadShownMemory([]*schema.Message{schema.SystemMessage("you are the assistant")})
	if shown.Kind != "none" {
		t.Fatalf("a prompt without a memory block = %q", shown.Kind)
	}
}

// The authorisation check of the injected memory: a block that cited a
// message from a conversation the arm was never given would be a memory the
// learner was shown without a grant.
func TestTheInjectedMemoryIsCheckedAgainstTheArmsOwnConversations(t *testing.T) {
	if !WithinSessions([]string{"a", "b"}, []string{"a", "b", "c"}) {
		t.Fatal("sessions inside the arm's own conversations were refused")
	}
	if WithinSessions([]string{"a", "z"}, []string{"a", "b"}) {
		t.Fatal("a session outside the arm's own conversations was accepted")
	}
	if !WithinSessions(nil, []string{"a"}) {
		t.Fatal("an empty set was refused")
	}
}
