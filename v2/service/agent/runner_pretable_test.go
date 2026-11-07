package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"GopherAI/model"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type syncAtomicInt32 = atomic.Int32

// recordingBuilder wraps the real builder and keeps the window it built, so a
// test can see what the runner measured rather than what it sent.
type recordingBuilder struct {
	inner ContextBuilder
	built []ContextWindow
}

func (b *recordingBuilder) Build(ctx context.Context, input RunContext) (ContextWindow, error) {
	window, err := b.inner.Build(ctx, input)
	if err == nil {
		b.built = append(b.built, window)
	}
	return window, err
}

// The runner knows the turn's tool table before it assembles the window.
//
// The prompt is written as the union of every rule any turn might need, and
// the rules for the tools a turn does not have are dropped before the prompt
// is sent. The window used to be assembled against the longer prompt with the
// rules dropped afterwards, which reserved room nobody spent and took it out
// of the history: on a turn with one tool the difference is hundreds of
// tokens, more than a whole compressed block.
//
// This is the runner-level statement of the guarantee the builder's own tests
// make: the turn says what it will offer, and the window is measured against
// the prompt that is sent.
func TestTheRunnerSizesTheWindowAgainstTheTurnsToolTable(t *testing.T) {
	messages := make([]model.Message, 0, 120)
	for index := 1; index <= 120; index++ {
		messages = append(messages, model.Message{ID: uint(index), IsUser: index%2 == 1,
			Content: strings.Repeat("每天读 15 页，材料提前 5 天。", 3)})
	}
	history := &fakeHistoryReader{messages: messages}
	config := testContextConfig()
	config.InputTargetTokens = 4096
	config.EvidenceReserve = 500
	real, err := NewContextBuilder(config, history, staticTaskState(""), nil, nil, EmptySummaryProvider{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordingBuilder{inner: real}

	chat := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("final answer", nil), nil
	}}
	var executions syncAtomicInt32
	registry := StaticToolRegistry{Registered: []tool.BaseTool{mustFakeTool(t, &executions)}}
	runner, err := NewRunner(chat, registry, newMemoryStepRepository(), recorder, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), testRunContext(), DiscardEvents); err != nil {
		t.Fatal(err)
	}
	if len(recorder.built) == 0 {
		t.Fatal("the runner never built a window")
	}
	window := recorder.built[0]
	sent := window.Messages[0].Content
	// The budget describes the prompt the window carries.
	if window.Budget.System != messageTokenCost(sent) {
		t.Fatalf("the budget says the system prompt costs %d and the prompt in the window costs %d",
			window.Budget.System, messageTokenCost(sent))
	}
	// And that prompt is the one this turn's table needs: it names no tool
	// the turn does not have, and re-rendering it changes nothing.
	capabilities := CapabilitiesFromToolNames([]string{"lookup"})
	capabilities.MemoryIntent = ExplicitMemoryIntent("question")
	if again := RenderSystemPrompt(sent, capabilities); again != sent {
		t.Fatal("the window's prompt still carries rules for tools the turn does not have")
	}
	// The unrendered prompt is longer, so the turn really did save room for
	// the history - this is the number the defect was about.
	unrendered := SystemPromptFor(config.MemoryReadMode)
	if messageTokenCost(unrendered) <= messageTokenCost(sent) {
		t.Fatal("the rendered prompt is not shorter than the prompt as written; the table changed nothing")
	}
	if window.Budget.RecentMessages == 0 {
		t.Fatal("the shorter prompt did not reach the history")
	}
}
