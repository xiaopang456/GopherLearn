package agenttool

import (
	"context"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// The p2-hist-03-r2 shape, through the REAL browse tool.
//
// The conversation is confirmed (a card pointed at it, and the model browses
// it by session). Its summary stops at the early proposal ("18 个要点"); the
// later decision ("改成 27 个要点") exists only as words AFTER the summary's
// high-water mark. The browse call now carries those words, oldest first, so
// the model's request contains the old proposal, the later decision and their
// order - using the learner's original question.

type fakeSummaryTailReader struct {
	messages []model.Message
	calls    int
	afterID  uint
}

func (f *fakeSummaryTailReader) ListOwnedWordsAfter(_ context.Context, _ int64,
	_ string, afterID uint, limit int) ([]model.Message, error) {
	f.calls++
	f.afterID = afterID
	var out []model.Message
	for _, message := range f.messages {
		if message.ID <= afterID {
			continue
		}
		out = append(out, message)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

type tailBrowseChatModel struct {
	calls    int
	requests [][]*schema.Message
}

func (m *tailBrowseChatModel) next() (*schema.Message, error) {
	m.calls++
	if m.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-browse", Type: "function",
			Function: schema.FunctionCall{Name: "browse_summary_segments",
				Arguments: `{"session":"p2c-11","query":"复盘 每次拆多少个要点"}`},
		}}), nil
	}
	return schema.AssistantMessage("上次最后改成了一次 27 个要点。", nil), nil
}

func (m *tailBrowseChatModel) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.requests = append(m.requests, append([]*schema.Message{}, input...))
	return m.next()
}

func (m *tailBrowseChatModel) Stream(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	m.requests = append(m.requests, append([]*schema.Message{}, input...))
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

func (m *tailBrowseChatModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

func TestABrowsedConversationCarriesTheWordsAfterItsSummaryInOrder(t *testing.T) {
	segment := browseSegment(t, 1, 1, 1422, "复盘写法与结构讨论",
		[]model.SessionSummarySegmentPoint{{
			Text: "先按一次拆 18 个要点（助手提议，用户未表示同意） [message_ids=1422]",
		}})
	browser := &fakeSegmentBrowser{segments: []model.SessionSummarySegment{segment}}
	tailMessages := make([]model.Message, 0, 36)
	for id := uint(1423); id <= 1454; id++ {
		tailMessages = append(tailMessages, model.Message{ID: id, SessionID: "p2c-11", IsUser: true,
			Content: "这一段是摘要之后、定案之前的连续性对话。", CreatedAt: time.Now().UTC()})
	}
	tailMessages = append(tailMessages,
		model.Message{ID: 1455, SessionID: "p2c-11", IsUser: true,
			Content: "这次复盘要覆盖两个模块，改成一次 27 个要点吧", CreatedAt: time.Now().UTC()},
		model.Message{ID: 1456, SessionID: "p2c-11",
			Content: "行，改成一次 27 个要点，两个模块的要点我分开列。", CreatedAt: time.Now().UTC()},
	)
	tail := &fakeSummaryTailReader{messages: tailMessages}
	registry, err := NewBrowseSummarySegmentsRegistry(browser)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSessionParameter(true)
	registry.UseTailReader(tail)

	run := serviceagent.RunContext{RunID: "run-tail", UserID: 7, UserName: "learner",
		SessionID: "current-session", Question: "上次说每次复盘拆多少个要点？"}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	run.GrantSet.ExtendCardScope(run, serviceagent.CardScope{
		SessionID: "p2c-11", CardID: "card-11", CardVersion: 2, CardWatermark: 1462,
	})
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	chat := &tailBrowseChatModel{}
	runner, err := serviceagent.NewRunner(chat, serviceagent.StaticToolRegistry{Registered: tools},
		e2eVerbatimSteps{}, serviceagent.MinimalContextBuilder{}, serviceagent.DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), run, serviceagent.DiscardEvents); err != nil {
		t.Fatal(err)
	}
	if tail.calls != 1 || tail.afterID != 1422 {
		t.Fatalf("the tail read ran %d times after id %d, want once after the summary's 1422", tail.calls, tail.afterID)
	}
	if len(chat.requests) < 2 {
		t.Fatalf("the model was called %d times, want the answer generation too", len(chat.requests))
	}
	var request strings.Builder
	for _, message := range chat.requests[1] {
		if message != nil {
			request.WriteString(message.Content)
			request.WriteString("\n")
		}
	}
	text := request.String()
	old := strings.Index(text, "18 个要点")
	latest := strings.Index(text, "27 个要点")
	if old < 0 || latest < 0 {
		t.Fatalf("the request does not carry both values: %s", text)
	}
	if old > latest {
		t.Fatalf("the old proposal came after the later decision: %s", text)
	}
	if !strings.Contains(text, "later_messages") || !strings.Contains(text, "1455") {
		t.Fatalf("the tail's message ids are not in the request: %s", text)
	}
}

// A browse of THIS conversation does not duplicate the window with a tail read.
func TestBrowsingThisConversationDoesNotReadATail(t *testing.T) {
	segment := browseSegment(t, 1, 1, 10, "本会话", nil)
	registry, err := NewBrowseSummarySegmentsRegistry(&fakeSegmentBrowser{
		segments: []model.SessionSummarySegment{segment}})
	if err != nil {
		t.Fatal(err)
	}
	tail := &fakeSummaryTailReader{}
	registry.UseTailReader(tail)
	run := serviceagent.RunContext{UserID: 7, SessionID: "session", Question: "拆包"}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("the browse tool is not invokable")
	}
	if _, err := invokable.InvokableRun(context.Background(), `{"query":"拆包"}`); err != nil {
		t.Fatal(err)
	}
	if tail.calls != 0 {
		t.Fatalf("this conversation's browse read a tail %d times", tail.calls)
	}
}
