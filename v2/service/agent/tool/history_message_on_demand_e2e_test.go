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

// The p2-acc-04-r1 read-back path, through the REAL tools.
//
// The search returns a snippet that is TRUNCATED before the answer text; the
// message id is granted by the search; read_history_message must be on the
// table, read the message in full by that id, and the full text must reach the
// next model request. An id the search did not return, and a grant from another
// session, must still be refused.

const e2eVerbatimTail = "答案就在后半段：动作、提问、验收。"

type e2eHistoryReader struct {
	messages []model.Message
}

func (r *e2eHistoryReader) SearchOwnedMessagesBySession(_ context.Context, _ int64,
	sessionID, _ string, _ int) ([]model.Message, error) {
	var hits []model.Message
	for _, message := range r.messages {
		if message.SessionID == sessionID {
			hits = append(hits, message)
		}
	}
	return hits, nil
}

func (r *e2eHistoryReader) ReadOwnedMessages(_ context.Context, _ int64, ids []uint) ([]model.Message, error) {
	wanted := make(map[uint]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	var found []model.Message
	for _, message := range r.messages {
		if _, ok := wanted[message.ID]; ok {
			found = append(found, message)
		}
	}
	return found, nil
}

func (r *e2eHistoryReader) ReadOwnedMessagesBySession(_ context.Context, _ int64,
	idsBySession map[string][]uint) ([]model.Message, error) {
	var found []model.Message
	for _, message := range r.messages {
		for _, id := range idsBySession[message.SessionID] {
			if id == message.ID {
				found = append(found, message)
				break
			}
		}
	}
	return found, nil
}

type e2eVerbatimChatModel struct {
	calls    int
	requests [][]*schema.Message
}

func (m *e2eVerbatimChatModel) next() (*schema.Message, error) {
	m.calls++
	switch m.calls {
	case 1:
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-search", Type: "function",
			Function: schema.FunctionCall{Name: "search_session_history",
				Arguments: `{"session":"p2c-32","query":"练习单子 哪几样东西 分开列"}`},
		}}), nil
	case 2:
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-read", Type: "function",
			Function: schema.FunctionCall{Name: "read_history_message",
				Arguments: `{"message_ids":[4190,4191]}`},
		}}), nil
	default:
		return schema.AssistantMessage("清单是按动作、提问、验收分开列的。", nil), nil
	}
}

func (m *e2eVerbatimChatModel) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.requests = append(m.requests, append([]*schema.Message{}, input...))
	return m.next()
}

func (m *e2eVerbatimChatModel) Stream(_ context.Context, input []*schema.Message,
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

func (m *e2eVerbatimChatModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

type e2eVerbatimSteps struct{}

func (e2eVerbatimSteps) AppendStep(context.Context, string, *model.AgentStep) error { return nil }

// e2eVerbatimWindowBuilder carries the learner's pick, the way the real
// context builder does: the lock is what tells the runner this turn may only
// read the chosen conversation.
type e2eVerbatimWindowBuilder struct {
	locked *serviceagent.LockedSession
}

func (b e2eVerbatimWindowBuilder) Build(_ context.Context, input serviceagent.RunContext) (serviceagent.ContextWindow, error) {
	window := serviceagent.ContextWindow{
		Messages:    []*schema.Message{schema.SystemMessage("You are GopherAI."), schema.UserMessage(input.Question)},
		SessionLock: b.locked,
	}
	window.Budget.System = 10
	window.Budget.CurrentQuestion = 10
	window.Budget.Assembled = 20
	return window, nil
}

func TestATruncatedSearchHitIsReadBackInFullAndReachesTheModel(t *testing.T) {
	longMessage := model.Message{ID: 4190, SessionID: "p2c-32", IsUser: false,
		Content:   strings.Repeat("截断前的内容。", 45) + e2eVerbatimTail,
		CreatedAt: time.Now().UTC()}
	otherSession := model.Message{ID: 4191, SessionID: "p2c-03", IsUser: false,
		Content: "另一会话的原话。", CreatedAt: time.Now().UTC()}
	reader := &e2eHistoryReader{messages: []model.Message{longMessage, otherSession}}

	run := serviceagent.RunContext{RunID: "run-verbatim", UserID: 7, UserName: "learner",
		SessionID: "current-session", LockedSessionID: "p2c-32",
		Question: "上次那份练习单子，你当时按哪几样东西分开列的？"}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)

	searchRegistry, err := NewSearchSessionHistoryRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	searchRegistry.UseAssistantContent(true)
	readRegistry, err := NewHistoryMessageRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	readRegistry.UseAssistantContent(true)
	readRegistry.UseOnDemandSearchGrants(true)
	searchTools, err := searchRegistry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	readTools, err := readRegistry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	registered := append([]tool.BaseTool{}, searchTools...)
	registered = append(registered, readTools...)

	chat := &e2eVerbatimChatModel{}
	runner, err := serviceagent.NewRunner(chat, serviceagent.StaticToolRegistry{Registered: registered},
		e2eVerbatimSteps{}, e2eVerbatimWindowBuilder{locked: &serviceagent.LockedSession{SessionID: "p2c-32"}},
		serviceagent.DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), run, serviceagent.DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Answer, "动作、提问、验收") {
		t.Fatalf("the answer is %q", result.Answer)
	}
	if len(chat.requests) < 3 {
		t.Fatalf("the model was called %d times", len(chat.requests))
	}
	// The search result the model saw must be the TRUNCATED one: the tail is
	// beyond the snippet cap, so seeing it in the answer request can only come
	// from the read-back.
	var searchResult string
	for _, message := range chat.requests[1] {
		if message != nil && message.Role == schema.Tool && strings.Contains(message.Content, "4190") {
			searchResult = message.Content
		}
	}
	if !strings.Contains(searchResult, `"truncated":true`) {
		t.Fatalf("the search did not return a truncated snippet: %s", searchResult)
	}
	if strings.Contains(searchResult, e2eVerbatimTail) {
		t.Fatalf("the answer text was already in the snippet, so the read-back is not proven: %s", searchResult)
	}
	var answerRequest strings.Builder
	for _, message := range chat.requests[2] {
		if message != nil {
			answerRequest.WriteString(message.Content)
			answerRequest.WriteString("\n")
		}
	}
	if !strings.Contains(answerRequest.String(), e2eVerbatimTail) {
		t.Fatalf("the full message did not reach the model: %s", answerRequest.String())
	}
	// The id the search returned is readable; the id it did not return is
	// refused, even though the reader holds that message.
	var readResult string
	for _, message := range chat.requests[2] {
		if message != nil && message.Role == schema.Tool && strings.Contains(message.Content, "4190") {
			readResult = message.Content
		}
	}
	if readResult == "" {
		t.Fatal("the read-back result is not in the model request")
	}
	if !strings.Contains(readResult, "4190") || !strings.Contains(readResult, "4191") {
		t.Fatalf("the read-back result is %q", readResult)
	}
	if !strings.Contains(readResult, "refused") && !strings.Contains(readResult, "Refused") {
		t.Fatalf("an id the search did not return was not refused: %s", readResult)
	}
}

func TestTheOnDemandReadBackStillObeysTheSessionLock(t *testing.T) {
	reader := &e2eHistoryReader{messages: []model.Message{{
		ID: 4190, SessionID: "p2c-03", IsUser: false,
		Content: "另一会话的原话。", CreatedAt: time.Now().UTC(),
	}}}
	run := serviceagent.RunContext{RunID: "run-lock", UserID: 7,
		SessionID: "current-session", LockedSessionID: "p2c-32"}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	run.GrantSet.Extend(run, []serviceagent.HistoryMessageGrant{{
		MessageID: 4190, SourceKind: serviceagent.HistorySourceSessionSearch,
		SourceSessionID: "p2c-03",
	}})
	registry, err := NewHistoryMessageRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseAssistantContent(true)
	registry.UseOnDemandSearchGrants(true)
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("the read-back tool is not on the table under the lock: %d tools", len(tools))
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("the read-back tool is not invokable")
	}
	raw, err := invokable.InvokableRun(context.Background(), `{"message_ids":[4190]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "refused") && !strings.Contains(raw, "Refused") {
		t.Fatalf("a grant from another session was not refused under the lock: %s", raw)
	}
}

// The p2-acc-04-r1 chain with the question's own word.
//
// Searching "练习单子" lands on #4191 ("那就弄成一张练习单子"). The answer
// ("动作、提问、验收") is the message BEFORE it, #4190. Reading the hit brings
// the neighbour too; the neighbour of the other side is in a different
// conversation and must not come along.
type e2eNeighbourChatModel struct {
	calls    int
	requests [][]*schema.Message
}

func (m *e2eNeighbourChatModel) next() (*schema.Message, error) {
	m.calls++
	switch m.calls {
	case 1:
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-search", Type: "function",
			Function: schema.FunctionCall{Name: "search_session_history",
				Arguments: `{"session":"p2c-32","query":"练习单子"}`},
		}}), nil
	case 2:
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-read", Type: "function",
			Function: schema.FunctionCall{Name: "read_history_message",
				Arguments: `{"message_ids":[4191]}`},
		}}), nil
	default:
		return schema.AssistantMessage("清单是按动作、提问、验收分开列的。", nil), nil
	}
}

func (m *e2eNeighbourChatModel) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.requests = append(m.requests, append([]*schema.Message{}, input...))
	return m.next()
}

func (m *e2eNeighbourChatModel) Stream(_ context.Context, input []*schema.Message,
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

func (m *e2eNeighbourChatModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

func TestTheSearchHitBringsItsSameConversationNeighbourNotTheOtherSessions(t *testing.T) {
	reader := &e2eHistoryReader{messages: []model.Message{
		{ID: 4190, SessionID: "p2c-32", Content: "行，我按环节把动作、提问、验收整理成统一的样子。",
			CreatedAt: time.Now().UTC()},
		{ID: 4191, SessionID: "p2c-32", IsUser: true, Content: "那就弄成一张练习单子。",
			CreatedAt: time.Now().UTC()},
		{ID: 4192, SessionID: "p2c-03", Content: "另一会话的相邻消息。",
			CreatedAt: time.Now().UTC()},
	}}
	run := serviceagent.RunContext{RunID: "run-neighbour", UserID: 7, UserName: "learner",
		SessionID: "current-session", LockedSessionID: "p2c-32",
		Question: "上次那份练习单子，你当时按哪几样东西分开列的？"}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)

	searchRegistry, err := NewSearchSessionHistoryRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	searchRegistry.UseAssistantContent(true)
	readRegistry, err := NewHistoryMessageRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	readRegistry.UseAssistantContent(true)
	readRegistry.UseOnDemandSearchGrants(true)
	readRegistry.UseAdjacentRead(true)
	searchTools, err := searchRegistry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	readTools, err := readRegistry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	registered := append([]tool.BaseTool{}, searchTools...)
	registered = append(registered, readTools...)

	chat := &e2eNeighbourChatModel{}
	runner, err := serviceagent.NewRunner(chat, serviceagent.StaticToolRegistry{Registered: registered},
		e2eVerbatimSteps{}, e2eVerbatimWindowBuilder{locked: &serviceagent.LockedSession{SessionID: "p2c-32"}},
		serviceagent.DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), run, serviceagent.DiscardEvents); err != nil {
		t.Fatal(err)
	}
	if len(chat.requests) < 3 {
		t.Fatalf("the model was called %d times", len(chat.requests))
	}
	var answerRequest strings.Builder
	for _, message := range chat.requests[2] {
		if message != nil {
			answerRequest.WriteString(message.Content)
			answerRequest.WriteString("\n")
		}
	}
	text := answerRequest.String()
	if !strings.Contains(text, "动作、提问、验收") {
		t.Fatalf("the neighbour's answer text did not reach the model: %s", text)
	}
	if !strings.Contains(text, `"adjacent":true`) {
		t.Fatalf("the neighbour is not marked adjacent: %s", text)
	}
	if strings.Contains(text, "另一会话的相邻消息") {
		t.Fatalf("another conversation's message came along: %s", text)
	}
}
