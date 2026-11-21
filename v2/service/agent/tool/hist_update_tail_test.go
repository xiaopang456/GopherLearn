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
	"github.com/cloudwego/eino/schema"
)

// p2-hist-01: the settled length is 22 minutes in the summary, and a later
// turn changes it to 37 minutes ("「芨菁」这两周加班多，改成每次 37 分钟吧").
// The old tail kept only the newest messages, which cut #877/#878; the
// selection now keeps the messages that mention the question.
//
// These tests use the frozen conversation's shape: one summary segment ending
// at #848, the update at #877/#878, and the newest turns about something else.

const (
	histOldValue = "每次读书先按 22 分钟来"
	histUpdate   = "「芨菁」这两周加班多，改成每次 37 分钟吧。"
	histConfirm  = "好，改成每次 37 分钟，笔记里的标注我一并更新。"
)

func histTailMessages() []model.Message {
	out := make([]model.Message, 0, 80)
	for id := uint(849); id <= 928; id++ {
		message := model.Message{ID: id, SessionID: "p2c-07", IsUser: true,
			Content: "这一段是摘要之后、无关的连续性对话。", CreatedAt: time.Now().UTC()}
		switch id {
		case 877:
			message.Content = histUpdate
		case 878:
			message.IsUser = false
			message.Content = histConfirm
		case 921:
			message.IsUser = false
			message.Content = "第一页会被反复看到，比放在末尾更容易被真正执行。"
		}
		out = append(out, message)
	}
	return out
}

func TestSelectSegmentTailKeepsTheUpdateTheQuestionMentions(t *testing.T) {
	messages := histTailMessages()
	selected := selectSegmentTail(messages, []string{"每次", "读多久"}, 8)
	ids := make([]int, 0, len(selected))
	for _, message := range selected {
		ids = append(ids, int(message.ID))
	}
	joined := ""
	for _, message := range selected {
		joined += message.Content + "\n"
	}
	if !strings.Contains(joined, "37 分钟") {
		t.Fatalf("the update was cut from the tail: %v", ids)
	}
	for index := 1; index < len(ids); index++ {
		if ids[index] <= ids[index-1] {
			t.Fatalf("the tail is not oldest-first: %v", ids)
		}
	}
}

func histBrowseRegistry(t *testing.T) (*BrowseSummarySegmentsRegistry, *fakeSummaryTailReader) {
	t.Helper()
	segment := model.SessionSummarySegment{
		UserID: 7, SessionID: "p2c-07", Seq: 1, StartMessageID: 785, EndMessageID: 848,
		Topic: "读书习惯梳理与「芨菁」节奏定为每次22分钟",
		Content: storedSegment(t, "读书习惯梳理与「芨菁」节奏定为每次22分钟",
			[]model.SessionSummarySegmentPoint{{
				Text: histOldValue + " [message_ids=844,845]", EvidenceMessageIDs: []uint{844, 845},
			}}),
	}
	registry, err := NewBrowseSummarySegmentsRegistry(&fakeSegmentBrowser{
		segments: []model.SessionSummarySegment{segment}})
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSessionParameter(true)
	tail := &fakeSummaryTailReader{messages: histTailMessages()}
	registry.UseTailReader(tail)
	return registry, tail
}

// histWindowBuilder opens p2c-07 through a card, without a lock: this is the
// normal entry the two rounds used.
type histWindowBuilder struct{ watermark uint }

func (b histWindowBuilder) Build(_ context.Context, input serviceagent.RunContext) (serviceagent.ContextWindow, error) {
	watermark := b.watermark
	if watermark == 0 {
		watermark = 1000
	}
	window := serviceagent.ContextWindow{
		Messages: []*schema.Message{schema.SystemMessage("You are GopherAI."), schema.UserMessage(input.Question)},
		SessionCardScopes: []serviceagent.CardScope{{
			SessionID: "p2c-07", CardID: "card-07", CardVersion: 1, CardWatermark: watermark,
		}},
	}
	window.Budget.System = 10
	window.Budget.CurrentQuestion = 10
	window.Budget.Assembled = 20
	return window, nil
}

type histChatModel struct {
	calls    int
	requests [][]*schema.Message
}

func (m *histChatModel) next() (*schema.Message, error) {
	m.calls++
	if m.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-tool", Type: "function",
			Function: schema.FunctionCall{Name: "browse_summary_segments",
				Arguments: `{"session":"p2c-07","query":"每次读多久 22分钟 节奏"}`},
		}}), nil
	}
	return schema.AssistantMessage("上次定的是 22 分钟，后来改成了 37 分钟。", nil), nil
}

func (m *histChatModel) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.requests = append(m.requests, append([]*schema.Message{}, input...))
	return m.next()
}

func (m *histChatModel) Stream(_ context.Context, input []*schema.Message,
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

func (m *histChatModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

func TestTheNormalBrowseEntryCarriesTheUpdateAfterTheSummary(t *testing.T) {
	registry, tail := histBrowseRegistry(t)
	run := serviceagent.RunContext{RunID: "run-hist", UserID: 7, UserName: "learner",
		SessionID: "current-session", Question: "上次我们说好的每次读多久？"}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	var captured serviceagent.RunContext
	chat := &histChatModel{}
	runner, err := serviceagent.NewRunner(chat,
		&capturingRegistry{registry: registry, captured: &captured},
		e2eVerbatimSteps{}, histWindowBuilder{}, serviceagent.DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), run, serviceagent.DiscardEvents); err != nil {
		t.Fatal(err)
	}
	if tail.calls != 1 || tail.afterID != 848 {
		var seen string
		for _, message := range chat.requests[1] {
			if message != nil && message.Role == schema.Tool {
				seen += message.Content
			}
		}
		t.Fatalf("the tail read ran %d times after id %d; observation=%s", tail.calls, tail.afterID, seen)
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
	if !strings.Contains(toolObservation, "37 分钟") {
		t.Fatalf("the update did not reach the model request: %s", toolObservation)
	}
	if !strings.Contains(toolObservation, "22 分钟") {
		t.Fatalf("the old value is missing from the request: %s", toolObservation)
	}
	if strings.Index(toolObservation, "22 分钟") > strings.Index(toolObservation, "37 分钟") {
		t.Fatalf("the request shows the update before the old value: %s", toolObservation)
	}
	if !strings.Contains(toolObservation, "877") || !strings.Contains(toolObservation, "878") {
		t.Fatalf("the update's message ids are missing: %s", toolObservation)
	}
}

// The other normal entry, search_history_segments, carries the same words.
func TestTheNormalSegmentSearchEntryCarriesTheUpdate(t *testing.T) {
	segment := model.SessionSummarySegment{
		UserID: 7, SessionID: "p2c-07", Seq: 1, StartMessageID: 785, EndMessageID: 848,
		Topic: "读书习惯梳理与「芨菁」节奏定为每次22分钟",
		Content: storedSegment(t, "读书习惯梳理与「芨菁」节奏定为每次22分钟",
			[]model.SessionSummarySegmentPoint{{
				Text: histOldValue + " [message_ids=844,845]", EvidenceMessageIDs: []uint{844, 845},
			}}),
	}
	cards := &fakeThinCardRetriever{candidates: []episode.ThinCardCandidate{{
		SessionID: "p2c-07", CardID: "card-07", CardVersion: 1, Title: "Evaluation conversation",
	}}}
	registry, err := NewSearchHistorySegmentsRegistry(&fakeSegmentFinder{
		hits: []summarysegmentdao.UserSegmentHit{{
			SessionID: "p2c-07", Seq: 1, Topic: segment.Topic, Content: segment.Content,
			EndMessageID: 848, EndAt: time.Now().UTC(), Score: 9,
		}}}, &fakeSegmentReader{segments: map[string]*model.SessionSummarySegment{"p2c-07:1": &segment}}, cards)
	if err != nil {
		t.Fatal(err)
	}
	tail := &fakeSummaryTailReader{messages: histTailMessages()}
	registry.UseTailReader(tail)
	run := serviceagent.RunContext{RunID: "run-hist-search", UserID: 7, UserName: "learner",
		SessionID: "current-session", Question: "上次我们说好的每次读多久？"}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	var captured serviceagent.RunContext
	chat := &histSegmentChatModel{}
	runner, err := serviceagent.NewRunner(chat,
		&histSegmentRegistry{registry: registry, captured: &captured},
		e2eVerbatimSteps{}, histWindowBuilder{}, serviceagent.DefaultRunnerConfig())
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
	if !strings.Contains(toolObservation, "37 分钟") || !strings.Contains(toolObservation, "22 分钟") {
		t.Fatalf("the segment search request is missing the old value or the update: %s", toolObservation)
	}
	if strings.Index(toolObservation, "22 分钟") > strings.Index(toolObservation, "37 分钟") {
		t.Fatalf("the segment search shows the update before the old value: %s", toolObservation)
	}
}

type histSegmentChatModel struct {
	calls    int
	requests [][]*schema.Message
}

func (m *histSegmentChatModel) next() (*schema.Message, error) {
	m.calls++
	if m.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-segments", Type: "function",
			Function: schema.FunctionCall{Name: "search_history_segments",
				Arguments: `{"query":"每次阅读时长 多久 阅读计划"}`},
		}}), nil
	}
	return schema.AssistantMessage("上次定的是 22 分钟，后来改成了 37 分钟。", nil), nil
}

func (m *histSegmentChatModel) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.requests = append(m.requests, append([]*schema.Message{}, input...))
	return m.next()
}

func (m *histSegmentChatModel) Stream(_ context.Context, input []*schema.Message,
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

func (m *histSegmentChatModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

// histSegmentRegistry rebuilds the segment-search tools with the run the
// runner actually hands them (the effective grant set).
type histSegmentRegistry struct {
	registry *SearchHistorySegmentsRegistry
	captured *serviceagent.RunContext
}

func (r *histSegmentRegistry) Tools(ctx context.Context, run serviceagent.RunContext) ([]tool.BaseTool, error) {
	*r.captured = run
	return r.registry.Tools(ctx, run)
}

// The card watermark is the question-time ceiling: a tail message above it
// is not readable, even when it mentions the question.
func TestTheTailRespectsTheCardWatermark(t *testing.T) {
	registry, tail := histBrowseRegistry(t)
	run := serviceagent.RunContext{RunID: "run-hist-cut", UserID: 7, UserName: "learner",
		SessionID: "current-session", Question: "上次我们说好的每次读多久？"}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	var captured serviceagent.RunContext
	chat := &histChatModel{}
	runner, err := serviceagent.NewRunner(chat, &capturingRegistry{registry: registry, captured: &captured},
		e2eVerbatimSteps{}, histWindowBuilder{watermark: 850}, serviceagent.DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), run, serviceagent.DiscardEvents); err != nil {
		t.Fatal(err)
	}
	var observation string
	for _, message := range chat.requests[1] {
		if message != nil && message.Role == schema.Tool {
			observation += message.Content
		}
	}
	if strings.Contains(observation, "37 分钟") || strings.Contains(observation, "877") {
		t.Fatalf("a tail message above the card watermark reached the request: %s", observation)
	}
	_ = tail
}
