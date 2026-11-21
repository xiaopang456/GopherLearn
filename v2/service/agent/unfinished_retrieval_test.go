package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// A reply that only promises to look is not an answer.
//
// The 7d58767 record has the shape: question about an earlier conversation,
// model replies "I'll look for that earlier conversation.", issues no tool
// call, and the turn ends. Nothing was read and nobody was asked. The product
// must continue the turn once, then ask which conversation was meant, and the
// promise must never stand as the final answer.

const recordPromise = "I'll look for that earlier conversation."

type unfinishedSearchQuery struct {
	Session string `json:"session"`
	Query   string `json:"query"`
}

type unfinishedSearchOutput struct {
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Matches []struct {
		MessageID int    `json:"message_id"`
		Role      string `json:"role"`
		Text      string `json:"text"`
	} `json:"matches"`
}

func mustUnfinishedSearchTool(t *testing.T) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool("search_session_history", "search one earlier conversation",
		func(context.Context, *unfinishedSearchQuery) (*unfinishedSearchOutput, error) {
			out := &unfinishedSearchOutput{Status: "ok", Reason: "none"}
			out.Matches = append(out.Matches, struct {
				MessageID int    `json:"message_id"`
				Role      string `json:"role"`
				Text      string `json:"text"`
			}{MessageID: 2, Role: "assistant", Text: "先看语句本身和执行计划。"})
			return out, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func TestAPromiseToSearchIsNeverTheFinalAnswer(t *testing.T) {
	chat := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage(recordPromise, nil), nil
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{}, newMemoryStepRepository(),
		MinimalContextBuilder{}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = "上次我们讨论过数据库的事务隔离级别吗？定下来用哪一档了吗？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Answer, "I'll look") {
		t.Fatalf("the promise was published as the answer: %q", result.Answer)
	}
	if result.Clarification == nil {
		t.Fatalf("an unfinished cross-session turn was not turned into a clarification: %q", result.Answer)
	}
	if result.Answer != WaitingChoiceAnswer {
		t.Fatalf("answer=%q, want the waiting sentence", result.Answer)
	}
	if result.ModelCalls < 2 {
		t.Fatalf("the turn was not continued once (model calls %d)", result.ModelCalls)
	}
}

func TestAPromiseIsContinuedWithARealSearch(t *testing.T) {
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return schema.AssistantMessage(recordPromise, nil), nil
		case 2:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "call-search", Type: "function",
				Function: schema.FunctionCall{Name: "search_session_history",
					Arguments: `{"session":"p2c-01","query":"先看 索引"}`},
			}}), nil
		default:
			return schema.AssistantMessage("先看语句本身和执行计划。", nil), nil
		}
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{Registered: []tool.BaseTool{mustUnfinishedSearchTool(t)}},
		newMemoryStepRepository(), MinimalContextBuilder{}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = "上次说到数据库这边加索引，你当时建议先看哪两样再动手？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification != nil {
		t.Fatalf("a continued turn that retrieved was turned into a clarification: %+v", result.Clarification)
	}
	if !strings.Contains(result.Answer, "语句本身和执行计划") {
		t.Fatalf("answer=%q, want the retrieved answer", result.Answer)
	}
	if result.ToolCalls != 1 {
		t.Fatalf("the continuation made %d tool calls, want 1", result.ToolCalls)
	}
}

func TestACurrentConversationAnswerIsNotTreatedAsAnUnfinishedTurn(t *testing.T) {
	for _, test := range []struct{ name, question, answer string }{
		{name: "normal answer", question: "你之前说的这个方案可行吗？", answer: "可行，按现在这个方案走。"},
		{name: "promise outside the history path", question: "帮我看下这个函数怎么改", answer: "I'll check that later."},
	} {
		t.Run(test.name, func(t *testing.T) {
			chat := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
				return schema.AssistantMessage(test.answer, nil), nil
			}}
			runner, err := NewRunner(chat, StaticToolRegistry{}, newMemoryStepRepository(),
				MinimalContextBuilder{}, DefaultRunnerConfig())
			if err != nil {
				t.Fatal(err)
			}
			input := testRunContext()
			input.Question = test.question
			result, err := runner.Run(context.Background(), input, DiscardEvents)
			if err != nil {
				t.Fatal(err)
			}
			if result.Clarification != nil {
				t.Fatalf("a current-conversation reply was treated as an unfinished history turn: %+v", result.Clarification)
			}
			if result.Answer != test.answer {
				t.Fatalf("answer=%q, want it unchanged", result.Answer)
			}
		})
	}
}

// A direct answer to an explicit history target is not interrupted: only a
// promise with no retrieval is.
func TestADirectAnswerForAnExplicitHistoryTargetIsNotInterrupted(t *testing.T) {
	const answer = "先看语句本身和执行计划。"
	chat := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage(answer, nil), nil
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{}, newMemoryStepRepository(),
		MinimalContextBuilder{}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = "上次说到数据库这边加索引，你当时建议先看哪两样再动手？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification != nil || result.Answer != answer {
		t.Fatalf("a direct history answer was interrupted: clarification=%v answer=%q", result.Clarification, result.Answer)
	}
}
