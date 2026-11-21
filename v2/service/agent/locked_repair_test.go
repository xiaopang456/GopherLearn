package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// A locked turn that produces no usable answer.
//
// The acceptance retest showed the shape: the learner clicked a conversation,
// the lock resolved, and the follow-up turn's last generation wrote a tool
// call into its answer. The old ending was the fixed "could not call a tool,
// please retry" sentence - a sentence that says nothing about the
// conversation the learner chose and invites a retry of a turn that is not
// the learner's fault. These tests pin the replacement: one bounded repair
// pass (the model may call the history tools again, which can only read the
// locked conversation), and an honest sentence if that also fails.

const lockedRepairAnswer = "先看语句本身和执行计划，再决定动哪里。"

type lockedWindowBuilder struct {
	locked *LockedSession
}

func (b lockedWindowBuilder) Build(_ context.Context, input RunContext) (ContextWindow, error) {
	window := ContextWindow{
		Messages:    []*schema.Message{schema.SystemMessage(agentSystemPrompt), schema.UserMessage(input.Question)},
		SessionLock: b.locked,
	}
	window.Budget.System = messageTokenCost(agentSystemPrompt)
	window.Budget.CurrentQuestion = messageTokenCost(input.Question)
	window.Budget.Assembled = window.Budget.System + window.Budget.CurrentQuestion
	return window, nil
}

type lockedRepairMatch struct {
	MessageID int    `json:"message_id"`
	Role      string `json:"role"`
	SentAt    string `json:"sent_at"`
	Text      string `json:"text"`
}

type lockedRepairOutput struct {
	Status            string              `json:"status"`
	Reason            string              `json:"reason"`
	Matches           []lockedRepairMatch `json:"matches"`
	ObservationTokens int                 `json:"observation_tokens"`
}

// mustLockedRepairSearchTool answers the first call with a miss and the
// second one with the message that carries the answer.
func mustLockedRepairSearchTool(t *testing.T, executions *atomic.Int32) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool("search_session_history", "search one earlier conversation",
		func(context.Context, *lockedRepairQuery) (*lockedRepairOutput, error) {
			if executions != nil && executions.Add(1) == 1 {
				return &lockedRepairOutput{Status: "no_evidence", Reason: "no_match", Matches: []lockedRepairMatch{}}, nil
			}
			return &lockedRepairOutput{
				Status: "ok", Reason: "none",
				Matches: []lockedRepairMatch{{
					MessageID: 2, Role: "assistant", SentAt: "2026-04-02T01:01:00Z",
					Text: "先看语句本身和执行计划，比凭感觉加索引靠谱。",
				}},
			}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

type lockedRepairQuery struct {
	Query string `json:"query"`
}

func lockedRepairToolCall(id, query string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID: id, Function: schema.FunctionCall{
			Name: "search_session_history", Arguments: `{"query":"` + query + `"}`,
		},
	}})
}

func TestALockedTurnRepairsItsUnusableAnswerOnce(t *testing.T) {
	var executions atomic.Int32
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return lockedRepairToolCall("call-1", "加索引之前 先看"), nil
		case 2:
			return schema.AssistantMessage("<｜DSML｜ invoke name=\"search_session_history\">", nil), nil
		case 3:
			return lockedRepairToolCall("call-2", "先看"), nil
		default:
			return schema.AssistantMessage(lockedRepairAnswer, nil), nil
		}
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustLockedRepairSearchTool(t, &executions)}}, newMemoryStepRepository(),
		lockedWindowBuilder{locked: &LockedSession{SessionID: "p2c-01", Title: "Evaluation conversation"}},
		DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer == malformedToolCallAnswer || result.Answer == LockedEvidenceIncompleteAnswer {
		t.Fatalf("a locked turn ended with %q instead of a repaired answer", result.Answer)
	}
	if !strings.Contains(result.Answer, "语句本身和执行计划") {
		t.Fatalf("the repaired answer is %q", result.Answer)
	}
	if executions.Load() != 2 {
		t.Fatalf("the history tool ran %d times, want the first try and the repair", executions.Load())
	}
	if result.Clarification != nil {
		t.Fatal("a locked turn handed the question back as a clarification")
	}
}

func TestALockedTurnThatStillCannotAnswerSaysSoInsteadOfAskingToRetry(t *testing.T) {
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return lockedRepairToolCall("call-1", "加索引之前 先看"), nil
		}
		return schema.AssistantMessage("<｜DSML｜ invoke name=\"browse_summary_segments\">", nil), nil
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustLockedRepairSearchTool(t, &atomic.Int32{})}}, newMemoryStepRepository(),
		lockedWindowBuilder{locked: &LockedSession{SessionID: "p2c-01", Title: "Evaluation conversation"}},
		DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != LockedEvidenceIncompleteAnswer {
		t.Fatalf("the locked turn ended with %q, want the honest sentence", result.Answer)
	}
	if strings.Contains(result.Answer, "请重试") {
		t.Fatal("a locked turn still asks the learner to retry")
	}
}

func TestALockedTurnWithAUsableAnswerIsNotRepaired(t *testing.T) {
	var executions atomic.Int32
	chat := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage(lockedRepairAnswer, nil), nil
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustLockedRepairSearchTool(t, &executions)}}, newMemoryStepRepository(),
		lockedWindowBuilder{locked: &LockedSession{SessionID: "p2c-01"}},
		DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != lockedRepairAnswer {
		t.Fatalf("a usable answer was rewritten: %q", result.Answer)
	}
	if executions.Load() != 0 {
		t.Fatal("a usable locked answer triggered a repair search")
	}
}

// The repair request must still carry the evidence the turn already read.
//
// The repair starts from the original window; if it did not carry this turn's
// tool results, it would re-run the same first attempt with the same
// information and the evidence already paid for would be lost. The request is
// asserted on the model's actual input, and the turn's reads are asserted to
// name only the locked conversation.
func TestTheLockedRepairRequestCarriesTheEvidenceAlreadyRead(t *testing.T) {
	var toolCalls atomic.Int32
	sessions := make([]string, 0, 2)
	searchTool, err := toolutils.InferTool("search_session_history", "search one earlier conversation",
		func(_ context.Context, input *lockedRepairQueryWithSession) (*lockedRepairOutput, error) {
			sessions = append(sessions, input.Session)
			if toolCalls.Add(1) == 1 {
				return &lockedRepairOutput{
					Status: "ok", Reason: "none",
					Matches: []lockedRepairMatch{{
						MessageID: 2, Role: "assistant", SentAt: "2026-04-02T01:01:00Z",
						Text: "先看语句本身和执行计划，比凭感觉加索引靠谱。",
					}},
				}, nil
			}
			return &lockedRepairOutput{Status: "no_evidence", Reason: "no_match",
				Matches: []lockedRepairMatch{}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return schema.AssistantMessage("", []schema.ToolCall{{ID: "call-1",
				Function: schema.FunctionCall{Name: "search_session_history",
					Arguments: `{"session":"p2c-01","query":"先看"}`}}}), nil
		case 2:
			return schema.AssistantMessage("<｜DSML｜ invoke name=\"search_session_history\">", nil), nil
		default:
			return schema.AssistantMessage(lockedRepairAnswer, nil), nil
		}
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{Registered: []tool.BaseTool{searchTool}},
		newMemoryStepRepository(),
		lockedWindowBuilder{locked: &LockedSession{SessionID: "p2c-01", Title: "Evaluation conversation"}},
		DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Answer, "语句本身和执行计划") {
		t.Fatalf("the repaired answer is %q", result.Answer)
	}
	for _, session := range sessions {
		if session != "p2c-01" {
			t.Fatalf("a tool read %q, and this turn may only read the locked conversation", session)
		}
	}
	// The repair generation is the third model call (the first turn's tool
	// decision, its malformed answer, then the repair). Its request must
	// carry the evidence the first turn's tool returned.
	chat.mu.Lock()
	received := append([][]*schema.Message{}, chat.received...)
	chat.mu.Unlock()
	if len(received) < 3 {
		t.Fatalf("the model was called %d times, want the repair generation too", len(received))
	}
	repairRequest := received[2]
	var joined strings.Builder
	for _, message := range repairRequest {
		if message != nil {
			joined.WriteString(message.Content)
			joined.WriteString("\n")
		}
	}
	requestText := joined.String()
	if !strings.Contains(requestText, "先看语句本身和执行计划") {
		t.Fatalf("the repair request does not carry the evidence already read: %s", requestText)
	}
	if !strings.Contains(requestText, "只能读被锁定的那个会话") {
		t.Fatalf("the repair request does not say it may only read the locked conversation: %s", requestText)
	}
}

type lockedRepairQueryWithSession struct {
	Session string `json:"session"`
	Query   string `json:"query"`
}
