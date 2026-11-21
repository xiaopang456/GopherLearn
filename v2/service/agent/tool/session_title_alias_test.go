package agenttool

import (
	"context"
	"strings"
	"testing"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// p2-absent-04-r2: the learner picked p2c-25, and the model then named it by
// the title the locked block showed it. The id is what authorises a read, so
// the search was refused with session_not_allowed and the turn answered from
// the compressed block alone.
//
// These tests pin the narrow alias: the LOCKED title resolves to the LOCKED id
// for the session-scoped tools; any other title still gets nothing.

func titleAliasRun() serviceagent.RunContext {
	run := serviceagent.RunContext{RunID: "run-title-alias", UserID: 7, UserName: "learner",
		SessionID: "current-session", LockedSessionID: "p2c-25",
		LockedSessionTitle: "Evaluation conversation",
		Question:           "上次我们讨论过数据库的事务隔离级别吗？定下来用哪一档了吗？"}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	return run
}

func TestSearchingByTheLockedConversationsTitleReadsTheLockedConversation(t *testing.T) {
	searcher := &fakeSessionSearcher{messages: []model.Message{
		{ID: 501, SessionID: "p2c-25", Content: "从濮潽换到 WezTerm 之后顺手多了。"},
		{ID: 502, SessionID: "p2c-13", Content: "隔离级别到底怎么定，还是没底。"},
	}}
	registry, err := NewSearchSessionHistoryRegistry(searcher)
	if err != nil {
		t.Fatal(err)
	}
	run := titleAliasRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("the search tool is not invokable")
	}
	raw, err := invokable.InvokableRun(context.Background(),
		`{"session":"Evaluation conversation","query":"隔离级别"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "session_not_allowed") {
		t.Fatalf("the locked title was refused as another conversation: %s", raw)
	}
	if searcher.calls != 1 {
		t.Fatalf("the locked conversation was not read (searcher calls %d)", searcher.calls)
	}
	// p2c-25 itself has no 隔离级别; p2c-13 does. A hit would mean the search
	// leaked into another conversation.
	if strings.Contains(raw, "502") {
		t.Fatalf("the search read a conversation outside the lock: %s", raw)
	}
}

func TestATitleOfAnotherConversationIsStillRefused(t *testing.T) {
	searcher := &fakeSessionSearcher{}
	registry, err := NewSearchSessionHistoryRegistry(searcher)
	if err != nil {
		t.Fatal(err)
	}
	run := titleAliasRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("the search tool is not invokable")
	}
	raw, err := invokable.InvokableRun(context.Background(),
		`{"session":"Rust 学习","query":"隔离级别"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "session_not_allowed") {
		t.Fatalf("another conversation's title was accepted under the lock: %s", raw)
	}
	if searcher.calls != 0 {
		t.Fatalf("a refused session still reached the store (calls %d)", searcher.calls)
	}
}

func TestBrowsingByTheLockedConversationsTitleOpensIt(t *testing.T) {
	segment := browseSegment(t, 1, 1, 500, "终端多窗口习惯",
		[]model.SessionSummarySegmentPoint{{Text: "换到 WezTerm 之后顺手多了。"}})
	registry, err := NewBrowseSummarySegmentsRegistry(&fakeSegmentBrowser{
		segments: []model.SessionSummarySegment{segment}})
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSessionParameter(true)
	run := titleAliasRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("the browse tool is not invokable")
	}
	raw, err := invokable.InvokableRun(context.Background(),
		`{"session":"Evaluation conversation","query":"终端"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "session_not_allowed") {
		t.Fatalf("the locked title was refused by the browse tool: %s", raw)
	}
	if !strings.Contains(raw, "WezTerm") {
		t.Fatalf("the locked conversation was not browsed: %s", raw)
	}
	raw, err = invokable.InvokableRun(context.Background(),
		`{"session":"Rust 学习","query":"终端"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "session_not_allowed") {
		t.Fatalf("another title opened a conversation under the lock: %s", raw)
	}
}

func TestReadingTheWhiteboardByTheLockedConversationsTitleWorks(t *testing.T) {
	board := storedBoard(t, "p2c-25", model.SessionWhiteboardDocument{SchemaVersion: 3})
	registry, err := NewReadSessionWhiteboardRegistry(&fakeWhiteboardReader{board: board})
	if err != nil {
		t.Fatal(err)
	}
	run := titleAliasRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("the whiteboard tool is not invokable")
	}
	raw, err := invokable.InvokableRun(context.Background(), `{"session":"Evaluation conversation"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "session_not_allowed") {
		t.Fatalf("the locked title was refused by the whiteboard tool: %s", raw)
	}
	raw, err = invokable.InvokableRun(context.Background(), `{"session":"Rust 学习"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "session_not_allowed") {
		t.Fatalf("another title read the whiteboard under the lock: %s", raw)
	}
}

// The model's actual request must see the locked conversation's no_match
// result, not a session_not_allowed refusal.
type titleAliasChatModel struct {
	calls    int
	requests [][]*schema.Message
}

func (m *titleAliasChatModel) next() (*schema.Message, error) {
	m.calls++
	if m.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-search", Type: "function",
			Function: schema.FunctionCall{Name: "search_session_history",
				Arguments: `{"session":"Evaluation conversation","query":"隔离级别"}`},
		}}), nil
	}
	return schema.AssistantMessage("那次会话的原话里没有数据库或隔离级别的内容，我不能确认讨论过。", nil), nil
}

func (m *titleAliasChatModel) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.requests = append(m.requests, append([]*schema.Message{}, input...))
	return m.next()
}

func (m *titleAliasChatModel) Stream(_ context.Context, input []*schema.Message,
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

func (m *titleAliasChatModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

func TestTheLockedTitleSearchReachesTheModelAsNoMatch(t *testing.T) {
	searcher := &fakeSessionSearcher{messages: []model.Message{
		{ID: 501, SessionID: "p2c-25", Content: "从濮潽换到 WezTerm 之后顺手多了。"},
	}}
	registry, err := NewSearchSessionHistoryRegistry(searcher)
	if err != nil {
		t.Fatal(err)
	}
	run := titleAliasRun()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	chat := &titleAliasChatModel{}
	runner, err := serviceagent.NewRunner(chat, serviceagent.StaticToolRegistry{Registered: tools},
		e2eVerbatimSteps{}, e2eVerbatimWindowBuilder{locked: &serviceagent.LockedSession{
			SessionID: "p2c-25", Title: "Evaluation conversation",
		}}, serviceagent.DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), run, serviceagent.DiscardEvents); err != nil {
		t.Fatal(err)
	}
	if len(chat.requests) < 2 {
		t.Fatalf("the model was called %d times", len(chat.requests))
	}
	var toolObservation string
	for _, message := range chat.requests[1] {
		if message != nil && message.Role == schema.Tool {
			toolObservation += message.Content
		}
	}
	if strings.Contains(toolObservation, "session_not_allowed") {
		t.Fatalf("the model received a refusal instead of the locked search result: %s", toolObservation)
	}
	if !strings.Contains(toolObservation, "no_match") && !strings.Contains(toolObservation, "no_evidence") {
		t.Fatalf("the locked search result did not reach the model: %s", toolObservation)
	}
}
