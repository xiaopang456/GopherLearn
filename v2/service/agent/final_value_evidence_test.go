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

// A final-agreement question answered without conversation evidence.
//
// The card search only located a conversation; the model answered from the
// summary or board, where the old value still sits. The product reads the
// confirmed conversation once before publishing, and if no evidence arrives it
// says what is missing instead of presenting the old value as the current one.

type finalValueBrowseQuery struct {
	Session string `json:"session"`
	Query   string `json:"query"`
}

type finalValueBrowseMessage struct {
	MessageID int    `json:"message_id"`
	Role      string `json:"role"`
	Text      string `json:"text"`
}

type finalValueBrowseOutput struct {
	Status        string                    `json:"status"`
	Reason        string                    `json:"reason"`
	Segments      []map[string]any          `json:"segments"`
	LaterMessages []finalValueBrowseMessage `json:"later_messages"`
}

func mustFinalValueBrowseTool(t *testing.T, executions *atomic.Int32) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool("browse_summary_segments", "browse one conversation",
		func(context.Context, *finalValueBrowseQuery) (*finalValueBrowseOutput, error) {
			executions.Add(1)
			return &finalValueBrowseOutput{
				Status:   "ok",
				Reason:   "none",
				Segments: []map[string]any{{"seq": 1, "topic": "读书节奏", "start_message_id": 785, "end_message_id": 848}},
				LaterMessages: []finalValueBrowseMessage{{
					MessageID: 877, Role: "user", Text: "「芨菁」这两周加班多，改成每次 37 分钟吧。",
				}, {
					MessageID: 878, Role: "assistant", Text: "好，改成每次 37 分钟，笔记里的标注我一并更新。",
				}},
			}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func finalValueCandidate() []handoffCandidate {
	return []handoffCandidate{{
		SessionID: "p2c-07", Title: "Evaluation conversation",
		Topics: []string{"读书习惯梳理"}, LastTalkedAt: "2026-04-10T03:00:00Z",
	}}
}

func TestAFinalValueQuestionWithoutEvidenceReadsTheConversationFirst(t *testing.T) {
	var browseCalls atomic.Int32
	var cardCalls atomic.Int32
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return handoffCardSearchCallWithArgs("读书 每次 多久"), nil
		case 2:
			return schema.AssistantMessage("每次 22 分钟。", nil), nil
		case 3:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "call-browse", Type: "function",
				Function: schema.FunctionCall{Name: "browse_summary_segments",
					Arguments: `{"session":"p2c-07","query":"每次 读多久"}`},
			}}), nil
		default:
			return schema.AssistantMessage("最后改成了每次 37 分钟；22 分钟是旧值。", nil), nil
		}
	}}
	runner := mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustHandoffCardTool(t, &cardCalls, "", finalValueCandidate()),
		mustFinalValueBrowseTool(t, &browseCalls),
	}}, newMemoryStepRepository(), DefaultRunnerConfig())
	input := testRunContext()
	input.Question = "上次我们说好的每次读多久？最后定的是多少？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if browseCalls.Load() != 1 {
		t.Fatalf("the evidence read ran %d times, want once", browseCalls.Load())
	}
	if !strings.Contains(result.Answer, "37 分钟") {
		t.Fatalf("the update did not reach the answer: %q", result.Answer)
	}
	if result.ModelCalls < 4 {
		t.Fatalf("the turn was not continued once (model calls %d)", result.ModelCalls)
	}
}

// With evidence already delivered, the direct answer is not re-run.
func TestAFinalValueQuestionWithEvidenceKeepsTheDirectAnswer(t *testing.T) {
	var browseCalls atomic.Int32
	var cardCalls atomic.Int32
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return handoffCardSearchCallWithArgs("读书 每次 多久"), nil
		case 2:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "call-browse", Type: "function",
				Function: schema.FunctionCall{Name: "browse_summary_segments",
					Arguments: `{"session":"p2c-07","query":"每次 读多久"}`},
			}}), nil
		default:
			return schema.AssistantMessage("最后是每次 37 分钟。", nil), nil
		}
	}}
	runner := mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustHandoffCardTool(t, &cardCalls, "", finalValueCandidate()),
		mustFinalValueBrowseTool(t, &browseCalls),
	}}, newMemoryStepRepository(), DefaultRunnerConfig())
	input := testRunContext()
	input.Question = "上次我们说好的每次读多久？最后定的是多少？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.ModelCalls != 3 {
		t.Fatalf("a turn with evidence was re-run (%d model calls)", result.ModelCalls)
	}
	if browseCalls.Load() != 1 || !strings.Contains(result.Answer, "37 分钟") {
		t.Fatalf("answer=%q browse=%d", result.Answer, browseCalls.Load())
	}
}

// A non-final-value pointer question keeps its direct path.
func TestANonFinalValueQuestionWithoutEvidenceKeepsItsAnswer(t *testing.T) {
	var cardCalls atomic.Int32
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return handoffCardSearchCallWithArgs("事务 清单"), nil
		}
		return schema.AssistantMessage("看慢查询清单和事务记录。", nil), nil
	}}
	runner := mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustHandoffCardTool(t, &cardCalls, "", finalValueCandidate()),
	}}, newMemoryStepRepository(), DefaultRunnerConfig())
	input := testRunContext()
	input.Question = "上次你说怎么看出哪些语句被包在同一个事务里跑得久，是看哪两样东西？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "看慢查询清单和事务记录。" {
		t.Fatalf("a non-final-value answer was rewritten: %q", result.Answer)
	}
	if result.ModelCalls != 2 {
		t.Fatalf("the turn was continued unnecessarily (%d model calls)", result.ModelCalls)
	}
}

// If the bounded re-query still finds no evidence, the answer names what is
// missing instead of repeating the old value as the current one.
func TestAFinalValueQuestionWithNoEvidenceSaysWhatIsMissing(t *testing.T) {
	var cardCalls atomic.Int32
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return handoffCardSearchCallWithArgs("读书 每次 多久"), nil
		}
		return schema.AssistantMessage("每次 22 分钟。", nil), nil
	}}
	runner := mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustHandoffCardTool(t, &cardCalls, "", finalValueCandidate()),
	}}, newMemoryStepRepository(), DefaultRunnerConfig())
	input := testRunContext()
	input.Question = "上次我们说好的每次读多久？最后定的是多少？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != InsufficientConversationEvidenceAnswer {
		t.Fatalf("answer=%q, want the missing-evidence sentence", result.Answer)
	}
	if strings.Contains(result.Answer, "22 分钟") {
		t.Fatalf("the old value was presented as the current one: %q", result.Answer)
	}
}
