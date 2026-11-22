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

// The real shape: the model reads the old whiteboard (22 minutes) after the
// card search and drafts 22. The board is a material, but it only covers the
// old state; a final-agreement question still has to read the conversation's
// own words where the update lives.

type finalValueBoardQuery struct {
	Session string `json:"session"`
}

type finalValueBoardEntry struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

type finalValueBoardOutput struct {
	Status      string                 `json:"status"`
	Reason      string                 `json:"reason"`
	Session     string                 `json:"session"`
	Constraints []finalValueBoardEntry `json:"constraints"`
	Plan        []finalValueBoardEntry `json:"plan"`
	Observation int                    `json:"observation_tokens"`
}

type finalValueSearchQuery struct {
	Session string `json:"session"`
	Query   string `json:"query"`
}

type finalValueSearchMatch struct {
	MessageID int    `json:"message_id"`
	Role      string `json:"role"`
	Text      string `json:"text"`
}

type finalValueSearchOutput struct {
	Status  string                  `json:"status"`
	Reason  string                  `json:"reason"`
	Matches []finalValueSearchMatch `json:"matches"`
}

func mustFinalValueWhiteboardTool(t *testing.T, calls *atomic.Int32) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool("read_session_whiteboard", "read the board",
		func(_ context.Context, input *finalValueBoardQuery) (*finalValueBoardOutput, error) {
			calls.Add(1)
			return &finalValueBoardOutput{
				Status: "ok", Reason: "none", Session: input.Session,
				Constraints: []finalValueBoardEntry{{
					Key: "session_length", Text: "每次读书先按 22 分钟来",
				}},
				Plan: []finalValueBoardEntry{{Key: "question_resume", Text: "把待续位置和当前问题写在一起"}},
			}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func mustFinalValueSearchTool(t *testing.T, calls *atomic.Int32) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool("search_session_history", "search the conversation's words",
		func(_ context.Context, input *finalValueSearchQuery) (*finalValueSearchOutput, error) {
			calls.Add(1)
			return &finalValueSearchOutput{
				Status: "ok", Reason: "none",
				Matches: []finalValueSearchMatch{
					{MessageID: 877, Role: "user", Text: "「芨菁」这两周加班多，改成每次 37 分钟吧。"},
					{MessageID: 878, Role: "assistant", Text: "好，改成每次 37 分钟，笔记里的标注我一并更新。"},
				},
			}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func TestTheOldWhiteboardTriggersABoundedReQueryInTheSameConversation(t *testing.T) {
	var cardCalls, boardCalls, searchCalls atomic.Int32
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return handoffCardSearchCallWithArgs("上次我们说好的每次读多久"), nil
		case 2:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "call-board", Type: "function",
				Function: schema.FunctionCall{Name: "read_session_whiteboard",
					Arguments: `{"session":"p2c-07"}`},
			}}), nil
		case 3:
			return schema.AssistantMessage("答案是：每次读书先按 22 分钟来。", nil), nil
		case 4:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "call-search", Type: "function",
				Function: schema.FunctionCall{Name: "search_session_history",
					Arguments: `{"session":"p2c-07","query":"每次 读多久"}`},
			}}), nil
		default:
			return schema.AssistantMessage("后来改成了每次 37 分钟；22 分钟是旧值。", nil), nil
		}
	}}
	runner := mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustHandoffCardTool(t, &cardCalls, "", finalValueCandidate()),
		mustFinalValueWhiteboardTool(t, &boardCalls),
		mustFinalValueSearchTool(t, &searchCalls),
	}}, newMemoryStepRepository(), DefaultRunnerConfig())
	input := testRunContext()
	input.Question = "上次我们说好的每次读多久？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if boardCalls.Load() != 1 || searchCalls.Load() != 1 {
		t.Fatalf("board=%d search=%d, want one of each", boardCalls.Load(), searchCalls.Load())
	}
	if !strings.Contains(result.Answer, "37 分钟") {
		t.Fatalf("the update did not reach the final answer: %q", result.Answer)
	}
	// The 4th request only proves the re-query started. The update has to be
	// in a request AFTER the search returned: the tool result carries it.
	var afterSearch bool
	for _, request := range chat.received {
		var text strings.Builder
		for _, message := range request {
			if message != nil {
				text.WriteString(message.Content)
				text.WriteString("\n")
			}
		}
		if strings.Contains(text.String(), "37 分钟") && strings.Contains(text.String(), "877") {
			afterSearch = true
		}
	}
	if !afterSearch {
		t.Fatal("the update original words never entered a model request")
	}
}

// When the conversation's own words were already read, the answer is direct
// and the re-query is not run again.
func TestTheOldWhiteboardIsNotNeededWhenTheUpdateWasAlreadyRead(t *testing.T) {
	var cardCalls, searchCalls atomic.Int32
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return handoffCardSearchCallWithArgs("上次我们说好的每次读多久"), nil
		case 2:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "call-search", Type: "function",
				Function: schema.FunctionCall{Name: "search_session_history",
					Arguments: `{"session":"p2c-07","query":"每次 读多久"}`},
			}}), nil
		default:
			return schema.AssistantMessage("最后是每次 37 分钟。", nil), nil
		}
	}}
	runner := mustRunner(t, chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustHandoffCardTool(t, &cardCalls, "", finalValueCandidate()),
		mustFinalValueSearchTool(t, &searchCalls),
	}}, newMemoryStepRepository(), DefaultRunnerConfig())
	input := testRunContext()
	input.Question = "上次我们说好的每次读多久？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.ModelCalls != 3 {
		t.Fatalf("a turn that already read the update was re-run (%d model calls)", result.ModelCalls)
	}
	if searchCalls.Load() != 1 || !strings.Contains(result.Answer, "37 分钟") {
		t.Fatalf("answer=%q search=%d", result.Answer, searchCalls.Load())
	}
}
