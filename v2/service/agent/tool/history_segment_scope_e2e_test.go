package agenttool

import (
	"context"
	"strings"
	"testing"
	"time"

	summarysegmentdao "GopherAI/dao/summarysegment"
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"GopherAI/service/episode"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// The p2-open-03-r1 shape, through the REAL tool chain.
//
// The card search offers the target conversation. The segment search then
// returns a stretch of ANOTHER conversation's words. The knowledge base is
// empty, and the product's knowledge boundary ends the turn early. The turn
// must still ask which conversation was meant: the other conversation's
// segment is not evidence of the target, and the unconfirmed-target rule must
// not be bypassed by the early ending.
//
// This uses the real card and segment tool registries (with fake indexes);
// only the knowledge tool is a minimal stand-in that reports an empty base.

type e2eKnowledgeOutput struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Results []any  `json:"results"`
}

func mustE2EEmptyKnowledgeTool(t *testing.T) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool("knowledge_search", "search the learner's documents",
		func(context.Context, *struct{ Query string }) (*e2eKnowledgeOutput, error) {
			return &e2eKnowledgeOutput{Status: "no_candidates", Summary: "the knowledge base is empty", Results: []any{}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

type e2eSteps struct{}

func (e2eSteps) AppendStep(context.Context, string, *model.AgentStep) error { return nil }

type e2eChatModel struct {
	calls int
}

func (m *e2eChatModel) next() (*schema.Message, error) {
	m.calls++
	switch m.calls {
	case 1:
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-cards", Type: "function",
			Function: schema.FunctionCall{Name: "search_session_cards", Arguments: `{"query":"接口读写拆分"}`},
		}}), nil
	case 2:
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-segments", Type: "function",
			Function: schema.FunctionCall{Name: "search_history_segments", Arguments: `{"query":"读写拆分"}`},
		}}), nil
	case 3:
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-knowledge", Type: "function",
			Function: schema.FunctionCall{Name: "knowledge_search", Arguments: `{"query":"接口读写拆分"}`},
		}}), nil
	default:
		return schema.AssistantMessage("上次那套接口的读写拆分没有定下来。", nil), nil
	}
}

func (m *e2eChatModel) Generate(_ context.Context, _ []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	return m.next()
}

func (m *e2eChatModel) Stream(_ context.Context, _ []*schema.Message,
	_ ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.next()
	if err != nil {
		return nil, err
	}
	if message.ResponseMeta == nil {
		message.ResponseMeta = &schema.ResponseMeta{}
	}
	if message.ResponseMeta.FinishReason == "" {
		message.ResponseMeta.FinishReason = "stop"
		if len(message.ToolCalls) > 0 {
			message.ResponseMeta.FinishReason = "tool_calls"
		}
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	writer.Send(message, nil)
	writer.Close()
	return reader, nil
}

func (m *e2eChatModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

// otherConversationSegment is the other conversation's body the segment search
// hands over. It must never become the answer basis for the target question.
const otherConversationSegment = "别的会话的段落：这里聊的是 HTTP 连接复用和分块传输。"

func TestTheOpenEndShapeAsksBeforeTheOtherConversationBecomesTheAnswer(t *testing.T) {
	cards := &fakeThinCardRetriever{candidates: []episode.ThinCardCandidate{{
		SessionID: "p2c-12", CardID: "card-12", CardVersion: 1, Title: "Evaluation conversation",
	}}}
	cardRegistry, err := NewSearchSessionCardsRegistry(cards)
	if err != nil {
		t.Fatal(err)
	}
	segmentRegistry, err := NewSearchHistorySegmentsRegistry(&fakeSegmentFinder{
		hits: []summarysegmentdao.UserSegmentHit{{
			SessionID: "p2c-03", Seq: 1, Topic: "HTTP 连接、跳转与传输练习",
			Content: otherConversationSegment, EndMessageID: 10, EndAt: time.Now().UTC(), Score: 9,
		}},
	}, nil, cards)
	if err != nil {
		t.Fatal(err)
	}
	run := serviceagent.RunContext{RunID: "run-e2e", UserID: 7, UserName: "learner",
		SessionID: "current-session", Question: "上次那套接口的读写拆分，最后定下来了吗？"}
	cardTools, err := cardRegistry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	segmentTools, err := segmentRegistry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	registered := append([]tool.BaseTool{}, cardTools...)
	registered = append(registered, segmentTools...)
	registered = append(registered, mustE2EEmptyKnowledgeTool(t))

	runner, err := serviceagent.NewRunner(&e2eChatModel{}, serviceagent.StaticToolRegistry{Registered: registered},
		e2eSteps{}, serviceagent.MinimalContextBuilder{}, serviceagent.DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), run, serviceagent.DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification == nil {
		t.Fatalf("the early knowledge ending bypassed the clarification: %q", result.Answer)
	}
	if result.Answer != serviceagent.WaitingChoiceAnswer {
		t.Fatalf("the answer is %q, want the waiting sentence", result.Answer)
	}
	if strings.Contains(result.Answer, "别的会话的段落") {
		t.Fatal("another conversation's segment became the answer")
	}
	if len(result.Clarification.Candidates) != 1 || result.Clarification.Candidates[0].SessionID != "p2c-12" {
		t.Fatalf("the card offers %+v", result.Clarification.Candidates)
	}
	if !result.Clarification.AllowHistoryBrowse || !result.Clarification.AllowFreeText {
		t.Fatal("the card does not offer the learner's own list and words")
	}
}
