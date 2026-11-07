package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"GopherAI/model"
)

// The window is measured against the prompt that is sent.
//
// The prompt is written as the union of every rule any turn might need, and
// the rules for the tools a turn does not have are dropped before the prompt
// is sent: a rule that names a tool the model cannot see is a rule it cannot
// obey. The measurement has to happen after that dropping, because the
// measurement is what decides how much of the conversation fits.
//
// A window measured against the longer prompt reserves room for text that
// nobody is sent, and the history pays for it: the raw conversation and the
// compressed blocks are measured against a target the model never fills. On a
// turn that offers two tools the difference is hundreds of tokens - more than
// the whole compressed block the turn was supposed to carry.
func TestTheWindowIsMeasuredAgainstThePromptItSends(t *testing.T) {
	messages := make([]model.Message, 0, 200)
	for index := 1; index <= 200; index++ {
		messages = append(messages, model.Message{ID: uint(index), IsUser: index%2 == 1,
			Content: fmt.Sprintf("第 %d 条：每天读 15 页，后来改成 30 页，材料提前 5 天，编号 B 短号总结", index)})
	}
	history := &fakeHistoryReader{messages: messages}
	config := testContextConfig()
	config.InputTargetTokens = 4096
	config.EvidenceReserve = 500

	builder, err := NewContextBuilder(config, history, nil, nil, nil, staticSummary(""))
	if err != nil {
		t.Fatal(err)
	}
	question := "我最初每天读多少页"
	// The turn that knows its table: one tool, so the knowledge rules and the
	// memory rules go.
	rendered, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "session",
		Question: question, AvailableTools: []string{"read_history_message"}})
	if err != nil {
		t.Fatal(err)
	}
	// The turn that does not know it yet: the prompt is measured as it stands.
	unmeasured, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "session",
		Question: question})
	if err != nil {
		t.Fatal(err)
	}

	// The budget describes the prompt the window carries.
	sent := rendered.Messages[0].Content
	if rendered.Budget.System != messageTokenCost(sent) {
		t.Fatalf("the budget says the system prompt costs %d and the prompt in the window costs %d",
			rendered.Budget.System, messageTokenCost(sent))
	}
	// And that prompt is the one this turn's table needs.
	capabilities := CapabilitiesFromToolNames([]string{"read_history_message"})
	capabilities.MemoryIntent = ExplicitMemoryIntent(question)
	if again := RenderSystemPrompt(sent, capabilities); again != sent {
		t.Fatal("the window's prompt still carries rules for tools the turn does not have")
	}
	if unmeasured.Budget.System <= rendered.Budget.System {
		t.Fatalf("the rendered prompt costs %d and the prompt as written costs %d; the rendering changed nothing",
			rendered.Budget.System, unmeasured.Budget.System)
	}
	// The room the prompt gave back is the room the history got.
	if rendered.Budget.RecentMessages <= unmeasured.Budget.RecentMessages {
		t.Fatalf("the rendered window carries %d tokens of recent messages and the unmeasured one %d; "+
			"the shorter prompt did not reach the history",
			rendered.Budget.RecentMessages, unmeasured.Budget.RecentMessages)
	}
}

// A turn that offers no tools is a turn whose prompt says nothing about them.
//
// The builder reads an empty table as "no tools" and a missing one as "the
// table is not known yet", and the two are not the same: the first drops
// every tool rule, the second keeps them all.
func TestATurnThatOffersNoToolsCarriesNoToolRules(t *testing.T) {
	history := &fakeHistoryReader{messages: []model.Message{{ID: 1, IsUser: true, Content: "older user"}}}
	builder, err := NewContextBuilder(testContextConfig(), history, nil, nil, nil, staticSummary(""))
	if err != nil {
		t.Fatal(err)
	}
	question := "我最初每天读多少页"
	window, err := builder.Build(context.Background(), RunContext{UserID: 7, SessionID: "session",
		Question: question, AvailableTools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(window.Messages[0].Content, "browse_summary_segments") {
		t.Fatal("the prompt names a tool the turn does not have")
	}
	capabilities := CapabilitiesFromToolNames([]string{})
	capabilities.MemoryIntent = ExplicitMemoryIntent(question)
	if again := RenderSystemPrompt(window.Messages[0].Content, capabilities); again != window.Messages[0].Content {
		t.Fatal("the window's prompt is not the one a turn without tools needs")
	}
}
