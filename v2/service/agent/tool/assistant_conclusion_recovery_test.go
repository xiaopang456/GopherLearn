package agenttool

import (
	"context"
	"strconv"
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

// The drills' answer material is an assistant conclusion that sits INSIDE a
// summary segment's range but was not kept in its points:
//
//   p2-drill-03 #2  "先看语句本身和执行计划，比凭感觉加索引靠谱。"
//   p2-drill-01 #1662 "慢查询清单和事务记录能看出哪些语句被包在同一个事务里跑得久。"
//   p2-drill-04 #3640 "说明里写这份清单覆盖哪些目录，省得翻。"
//
// The question asks what the assistant said, so the product's bounded
// read-back has to deliver the original words and the message id.

type conclusionCase struct {
	name     string
	session  string
	question string
	targetID uint
	target   string
	query    string
	start    uint
	end      uint
}

var conclusionCases = []conclusionCase{
	{name: "drill-03", session: "p2c-01",
		question: "上次说到数据库这边加索引，你当时建议先看哪两样再动手？",
		targetID: 2, target: "先看语句本身和执行计划，比凭感觉加索引靠谱。", start: 1, end: 64, query: "加索引 先看 两样"},
	{name: "drill-01", session: "p2c-13",
		question: "上次你说怎么看出哪些语句被包在同一个事务里跑得久，是看哪两样东西？",
		targetID: 1662, target: "慢查询清单和事务记录能看出哪些语句被包在同一个事务里跑得久。", start: 1633, end: 1696, query: "事务 清单 看出"},
	{name: "drill-04", session: "p2c-28",
		question: "上次说写说明的时候，说明里要写清什么？",
		targetID: 3640, target: "说明里写这份清单覆盖哪些目录，省得翻。", start: 3605, end: 3668, query: "说明 目录 清单"},
}

type conclusionTailReader struct {
	messages []model.Message
}

func (f *conclusionTailReader) ListOwnedWordsAfter(_ context.Context, _ int64,
	_ string, afterID uint, limit int) ([]model.Message, error) {
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

type conclusionWindowBuilder struct {
	session string
}

func (b conclusionWindowBuilder) Build(_ context.Context, input serviceagent.RunContext) (serviceagent.ContextWindow, error) {
	window := serviceagent.ContextWindow{
		Messages: []*schema.Message{schema.SystemMessage("You are GopherAI."), schema.UserMessage(input.Question)},
		SessionCardScopes: []serviceagent.CardScope{{
			SessionID: b.session, CardID: "card-" + b.session, CardVersion: 1, CardWatermark: 4000,
		}},
	}
	window.Budget.System = 10
	window.Budget.CurrentQuestion = 10
	window.Budget.Assembled = 20
	return window, nil
}

type conclusionChatModel struct {
	session  string
	query    string
	calls    int
	requests [][]*schema.Message
}

func (m *conclusionChatModel) next() (*schema.Message, error) {
	m.calls++
	if m.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-browse", Type: "function",
			Function: schema.FunctionCall{Name: "browse_summary_segments",
				Arguments: `{"session":"` + m.session + `","query":"` + m.query + `"}`},
		}}), nil
	}
	return schema.AssistantMessage("按上次说的做。", nil), nil
}

func (m *conclusionChatModel) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.requests = append(m.requests, append([]*schema.Message{}, input...))
	return m.next()
}

func (m *conclusionChatModel) Stream(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	m.requests = append(m.requests, append([]*schema.Message{}, input...))
	return streamConclusionMessage(m)
}

func streamConclusionMessage(m *conclusionChatModel) (*schema.StreamReader[*schema.Message], error) {
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

func (m *conclusionChatModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

func TestTheDrillConclusionsAreDeliveredByTheBoundedReadback(t *testing.T) {
	for _, test := range conclusionCases {
		t.Run(test.name, func(t *testing.T) {
			segment := model.SessionSummarySegment{
				UserID: 7, SessionID: test.session, Seq: 1, StartMessageID: test.start, EndMessageID: test.end,
				Topic: test.query,
				Content: storedSegment(t, test.query, []model.SessionSummarySegmentPoint{{
					Text:               "学习者的话 [message_ids=" + strconv.FormatUint(uint64(test.start), 10) + "]",
					EvidenceMessageIDs: []uint{test.start},
				}}),
			}
			registry, err := NewBrowseSummarySegmentsRegistry(&fakeSegmentBrowser{
				segments: []model.SessionSummarySegment{segment}})
			if err != nil {
				t.Fatal(err)
			}
			registry.UseSessionParameter(true)
			tail := &conclusionTailReader{}
			for id := test.start; id <= test.end; id++ {
				content := "这一段和问题无关。"
				isUser := true
				if id == test.targetID {
					content = test.target
					isUser = false
				}
				tail.messages = append(tail.messages, model.Message{ID: id, SessionID: test.session,
					IsUser: isUser, Content: content, CreatedAt: time.Now().UTC()})
			}
			registry.UseTailReader(tail)
			run := serviceagent.RunContext{RunID: "run-" + test.name, UserID: 7, UserName: "learner",
				SessionID: "current-session", Question: test.question}
			run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
			var captured serviceagent.RunContext
			chat := &conclusionChatModel{session: test.session, query: test.query}
			runner, err := serviceagent.NewRunner(chat, &conclusionRegistry{registry: registry,
				captured: &captured, session: test.session}, e2eVerbatimSteps{},
				conclusionWindowBuilder{session: test.session}, serviceagent.DefaultRunnerConfig())
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
			if !strings.Contains(observation, test.target) {
				t.Fatalf("the conclusion did not reach the request: %s", observation)
			}
			if !strings.Contains(observation, strconv.FormatUint(uint64(test.targetID), 10)) {
				t.Fatalf("the conclusion's message id did not reach the request: %s", observation)
			}
		})
	}
}

// conclusionRegistry builds the browse tool with the effective run.
type conclusionRegistry struct {
	registry *BrowseSummarySegmentsRegistry
	captured *serviceagent.RunContext
	session  string
}

func (r *conclusionRegistry) Tools(ctx context.Context, run serviceagent.RunContext) ([]tool.BaseTool, error) {
	*r.captured = run
	return r.registry.Tools(ctx, run)
}

// A segment-search variant for the same question shape.
func TestTheDrillConclusionIsDeliveredByTheSegmentSearch(t *testing.T) {
	test := conclusionCases[0]
	content := storedSegment(t, "本轮话题", []model.SessionSummarySegmentPoint{{
		Text: "学习者的话 [message_ids=1]", EvidenceMessageIDs: []uint{1},
	}})
	segment := model.SessionSummarySegment{UserID: 7, SessionID: test.session, Seq: 1,
		StartMessageID: test.start, EndMessageID: test.end, Topic: "本轮话题", Content: content}
	hits := []summarysegmentdao.UserSegmentHit{{SessionID: test.session, Seq: 1, Topic: "本轮话题",
		Content: content, EndMessageID: test.end, EndAt: time.Now().UTC(), Score: 9}}
	cards := &fakeThinCardRetriever{candidates: []episode.ThinCardCandidate{{
		SessionID: test.session, CardID: "card", CardVersion: 1,
	}}}
	registry, err := NewSearchHistorySegmentsRegistry(&fakeSegmentFinder{hits: hits},
		&fakeSegmentReader{segments: map[string]*model.SessionSummarySegment{"p2c-01:1": &segment}}, cards)
	if err != nil {
		t.Fatal(err)
	}
	tail := &conclusionTailReader{}
	for id := test.start; id <= test.end; id++ {
		content := "这一段和问题无关。"
		isUser := true
		if id == test.targetID {
			content = test.target
			isUser = false
		}
		tail.messages = append(tail.messages, model.Message{ID: id, SessionID: test.session,
			IsUser: isUser, Content: content, CreatedAt: time.Now().UTC()})
	}
	registry.UseTailReader(tail)
	run := serviceagent.RunContext{RunID: "run-seg-conclusion", UserID: 7, UserName: "learner",
		SessionID: "current-session", Question: test.question}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	var captured serviceagent.RunContext
	chat := &conclusionSegmentChatModel{}
	runner, err := serviceagent.NewRunner(chat, &histSegmentRegistry{registry: registry, captured: &captured},
		e2eVerbatimSteps{}, conclusionWindowBuilder{session: test.session}, serviceagent.DefaultRunnerConfig())
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
	if !strings.Contains(observation, test.target) {
		t.Fatalf("the segment-search conclusion did not reach the request: %s", observation)
	}
}

type conclusionSegmentChatModel struct {
	calls    int
	requests [][]*schema.Message
}

func (m *conclusionSegmentChatModel) next() (*schema.Message, error) {
	m.calls++
	if m.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-segments", Type: "function",
			Function: schema.FunctionCall{Name: "search_history_segments",
				Arguments: `{"query":"上次 你说的 建议 哪两样"}`},
		}}), nil
	}
	return schema.AssistantMessage("按上次说的做。", nil), nil
}

func (m *conclusionSegmentChatModel) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.requests = append(m.requests, append([]*schema.Message{}, input...))
	return m.next()
}

func (m *conclusionSegmentChatModel) Stream(_ context.Context, input []*schema.Message,
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

func (m *conclusionSegmentChatModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}
