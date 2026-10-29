package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/model"
)

// The tool ceiling bounds executions, not the ability to answer from the
// evidence already collected: once it is spent, the next call forbids tool
// choice and what the model produces is the turn's answer.
func TestBudgetCloseoutAnswersAfterTheToolBudgetIsSpent(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", "knowledge_search", `{"query":"one"}`),
		toolCallReply("call-2", "knowledge_search", `{"query":"two"}`),
		// The closeout round: the model answers from what it already has.
		answerReply("两次检索的证据已经足够，答案是……"),
	}}
	search := &recordingTool{name: "knowledge_search", observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{MaxModelCalls: 4, MaxToolCalls: 2, MaxSteps: 8, Deadline: time.Second}, search)

	answer, spent, err := harness.Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatalf("the closeout turn failed: %v", err)
	}
	if answer.Content == "" {
		t.Fatal("the closeout produced no answer")
	}
	if spent.ToolCalls != 2 {
		t.Fatalf("spent %d tool calls, want the two executions", spent.ToolCalls)
	}
	// The third call is the closeout: it must have gone out with tool choice
	// forbidden, tools still offered.
	last := chat.requests[len(chat.requests)-1]
	if last.ToolChoice != model.ToolChoiceNone {
		t.Fatalf("the closeout request carried tool choice %q, want none", last.ToolChoice)
	}
	if len(last.Tools) == 0 {
		t.Fatal("the closeout request dropped the tool offers")
	}
}

// A provider that violates tool_choice=none is stopped by the hard check, the
// way the reference project stops it: no further execution, no answer.
func TestBudgetCloseoutStopsAProviderThatViolatesNone(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", "knowledge_search", `{"query":"one"}`),
		toolCallReply("call-2", "knowledge_search", `{"query":"two"}`),
		toolCallReply("call-3", "knowledge_search", `{"query":"three"}`),
	}, repeatLast: true, varyRepeat: true}
	search := &recordingTool{name: "knowledge_search", observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{MaxModelCalls: 6, MaxToolCalls: 2, MaxSteps: 8, Deadline: time.Second}, search)

	_, spent, err := harness.Run(context.Background(), baseRequest())
	if !errors.Is(err, ErrToolCallBudget) {
		t.Fatalf("error = %v, want ErrToolCallBudget", err)
	}
	if spent.ToolCalls != 2 {
		t.Fatalf("spent %d tool calls, want the two before the violation", spent.ToolCalls)
	}
}

// An exact consecutive repeat - same tool, same canonical arguments - is
// refused before it runs, and it ends the turn: the reference project
// terminates rather than answers a model that asks the same question twice in
// a row.
func TestConsecutiveFingerprintRefusesTheIdenticalRepeat(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", "knowledge_search", `{"query":"fsync","top_k":5}`),
		// Same call, different key order: canonicalisation makes this the
		// identical repeat it is.
		toolCallReply("call-2", "knowledge_search", `{"top_k":5,"query":"fsync"}`),
	}}
	search := &recordingTool{name: "knowledge_search", observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{MaxModelCalls: 6, MaxToolCalls: 8, MaxSteps: 8, Deadline: time.Second}, search)

	_, spent, err := harness.Run(context.Background(), baseRequest())
	if !errors.Is(err, ErrRepeatedToolCall) {
		t.Fatalf("error = %v, want ErrRepeatedToolCall", err)
	}
	if len(search.calls) != 1 {
		t.Fatalf("the repeat executed %d times, want once", len(search.calls))
	}
	if spent.ToolCalls != 1 {
		t.Fatalf("spent %d tool calls, want the one execution", spent.ToolCalls)
	}
}

// Two calls of one round that differ are both executed: the fingerprint is
// consecutive, not a history.
func TestConsecutiveFingerprintAllowsADifferentNextCall(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", "knowledge_search", `{"query":"one"}`),
		toolCallReply("call-2", "knowledge_search", `{"query":"two"}`),
		answerReply("两条证据都用上了。"),
	}}
	search := &recordingTool{name: "knowledge_search", observations: []Observation{{Text: "资料", Found: true}}}
	harness := newHarness(t, chat, Budget{MaxModelCalls: 4, MaxToolCalls: 4, MaxSteps: 8, Deadline: time.Second}, search)

	if _, spent, err := harness.Run(context.Background(), baseRequest()); err != nil {
		t.Fatal(err)
	} else if spent.ToolCalls != 2 {
		t.Fatalf("spent %d tool calls, want both", spent.ToolCalls)
	}
}

// A read-only memory call that already ran and found nothing is answered from
// the ledger: it neither reaches storage again nor spends the budget, and the
// observation tells the model to answer now.
func TestProgressLedgerSuppressesAFruitlessRepeat(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", MemorySearchName, `{"query":"记忆"}`),
		toolCallReply("call-2", MemorySearchName, `{"query":"记忆"}`),
		answerReply("没有找到相关记录。"),
	}}
	reader := &scriptedMemories{}
	memory := newMemoryTool(t, reader, MemorySearchOptions{})
	harness := newHarness(t, chat, Budget{MaxModelCalls: 4, MaxToolCalls: 4, MaxSteps: 8, Deadline: time.Second}, memory)

	_, spent, err := harness.Run(context.Background(), baseRequest())
	if err != nil {
		t.Fatalf("the fruitless repeat must be answered, not fatal: %v", err)
	}
	if spent.ToolCalls != 1 {
		t.Fatalf("spent %d tool calls, want only the first search charged", spent.ToolCalls)
	}
	// The suppressed call reached the model as a stop_searching observation.
	last := chat.requests[len(chat.requests)-1]
	var stop string
	for _, message := range last.Messages {
		if message.Role == model.RoleTool && strings.Contains(message.Content, "stop_searching") {
			stop = message.Content
		}
	}
	if stop == "" {
		t.Fatal("the model was never told to stop searching")
	}
	if !strings.Contains(stop, "不要再用同义改写重试") {
		t.Fatalf("the observation lost its guidance:\n%s", stop)
	}
}

// The no-progress allowance is three: three attempts that achieved nothing
// exhaust the turn's memory reading, and every further call is answered as
// exhausted.
func TestProgressLedgerExhaustsAfterThreeFruitlessAttempts(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", MemorySearchName, `{"query":"第一"}`),
		toolCallReply("call-2", MemorySearchName, `{"query":"第二"}`),
		toolCallReply("call-3", MemorySearchName, `{"query":"第三"}`),
		toolCallReply("call-4", MemorySearchName, `{"query":"第四"}`),
		answerReply("本轮记忆检索没有进展。"),
	}}
	reader := &scriptedMemories{}
	memory := newMemoryTool(t, reader, MemorySearchOptions{})
	harness := newHarness(t, chat, Budget{MaxModelCalls: 8, MaxToolCalls: 8, MaxSteps: 12, Deadline: time.Second}, memory)

	if _, spent, err := harness.Run(context.Background(), baseRequest()); err != nil {
		t.Fatalf("exhaustion is an observation, not a failure: %v", err)
	} else if spent.ToolCalls != 3 {
		t.Fatalf("spent %d tool calls, want the three executed attempts", spent.ToolCalls)
	}
	last := chat.requests[len(chat.requests)-1]
	var exhausted bool
	for _, message := range last.Messages {
		if message.Role == model.RoleTool && strings.Contains(message.Content, "没有进展空间") {
			exhausted = true
		}
	}
	if !exhausted {
		t.Fatal("the fourth call was not answered as exhausted")
	}
}

// A refused read-only memory call never reached storage, so it is refunded:
// the budget, the per-tool count and the fingerprint all give the attempt
// back, and an honest corrected retry stays possible.
func TestProgressLedgerRefundsARefusedMemoryCall(t *testing.T) {
	chat := &scriptedToolModel{replies: []model.Completion{
		toolCallReply("call-1", MemorySearchName, `{"query":"记忆"}`),
		toolCallReply("call-2", MemorySearchName, `{"query":"记忆重试"}`),
		answerReply("找到了。"),
	}}
	memory := &recordingTool{name: MemorySearchName, observations: []Observation{
		// The first call is refused before storage, through the observation's
		// refusal seam; the second reaches the reader and returns.
		{Text: "本轮不允许读取", Found: false, Refused: true, Reason: "semantic_not_permitted"},
		{Text: "读到一条", Found: true},
	}}
	harness := newHarness(t, chat, Budget{MaxModelCalls: 4, MaxToolCalls: 4, MaxSteps: 8, Deadline: time.Second}, memory)

	if _, spent, err := harness.Run(context.Background(), baseRequest()); err != nil {
		t.Fatal(err)
	} else if spent.ToolCalls != 1 {
		t.Fatalf("spent %d tool calls, want the refused call refunded", spent.ToolCalls)
	}
}
