package agenttool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	summarysegmentdao "GopherAI/dao/summarysegment"
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// The segment search under a lock.
//
// The learner picked a conversation; the index still returns its best matches
// across the learner's whole history, including a higher-scoring hit from
// another conversation. The lock decides the reach of BOTH channels: the other
// conversation's body must not be assembled into the tool's output, and must
// not enter the model's request.

const (
	lockedSegmentMarker  = "锁内命中：动作、提问、验收"
	outsideSegmentMarker = "锁外命中：HTTP 连接复用"
)

// fakeVectorFinder is the semantic half of the segment index.
type fakeVectorFinder struct {
	hits []summarysegmentdao.UserSegmentHit
}

func (f *fakeVectorFinder) SearchUserSegmentsByVector(_ context.Context, _ int64,
	_ []float32, _ int) ([]summarysegmentdao.UserSegmentHit, error) {
	return f.hits, nil
}

// fakeEmbedder turns the query into a vector. Any vector works for the fake.
type fakeEmbedder struct{}

func (fakeEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0}, nil
}

func lockScopeSegmentRun(locked bool) serviceagent.RunContext {
	run := serviceagent.RunContext{RunID: "run-lock-scope", UserID: 7, UserName: "learner",
		SessionID: "current-session", Question: "上次那份练习单子，按哪几样东西分开列的？"}
	if locked {
		run.LockedSessionID = "p2c-01"
	}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	return run
}

func lockScopeHits() []summarysegmentdao.UserSegmentHit {
	return []summarysegmentdao.UserSegmentHit{
		{SessionID: "p2c-02", Seq: 1, Topic: "别的会话", Content: "",
			EndMessageID: 20, EndAt: time.Now().UTC(), Score: 9.5},
		{SessionID: "p2c-01", Seq: 1, Topic: "锁内会话", Content: "",
			EndMessageID: 10, EndAt: time.Now().UTC(), Score: 1.0},
	}
}

func mustLockScopeRegistry(t *testing.T) *SearchHistorySegmentsRegistry {
	t.Helper()
	segments := map[string]*model.SessionSummarySegment{
		"p2c-01:1": storedSummarySegment(t, "p2c-01", "锁内会话", lockedSegmentMarker),
		"p2c-02:1": storedSummarySegment(t, "p2c-02", "别的会话", outsideSegmentMarker),
	}
	registry, err := NewSearchHistorySegmentsRegistry(&fakeSegmentFinder{hits: lockScopeHits()},
		&fakeSegmentReader{segments: segments}, &fakeThinCardRetriever{})
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSemanticChannel(&fakeVectorFinder{hits: lockScopeHits()}, fakeEmbedder{})
	return registry
}

func storedSummarySegment(t *testing.T, sessionID, topic, point string) *model.SessionSummarySegment {
	t.Helper()
	body := storedSegment(t, topic, []model.SessionSummarySegmentPoint{{Text: point}})
	return &model.SessionSummarySegment{UserID: 7, SessionID: sessionID, Seq: 1,
		StartMessageID: 1, EndMessageID: 10, Topic: topic, Content: body}
}

func callLockScopeSearch(t *testing.T, registry *SearchHistorySegmentsRegistry,
	run serviceagent.RunContext) *SearchHistorySegmentsOutput {
	t.Helper()
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatal("the segment search tool is not invokable")
	}
	raw, err := invokable.InvokableRun(context.Background(), `{"query":"练习单子 分开列"}`)
	if err != nil {
		t.Fatal(err)
	}
	out := &SearchHistorySegmentsOutput{}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Under the lock only the locked conversation's body is assembled, even though
// the index returned a higher-scoring hit from another conversation in both
// channels.
func TestTheLockedSegmentSearchDropsOtherSessionsFromBothChannels(t *testing.T) {
	run := lockScopeSegmentRun(true)
	out := callLockScopeSearch(t, mustLockScopeRegistry(t), run)
	if len(out.Segments) != 1 {
		t.Fatalf("the locked search returned %d segments: %+v", len(out.Segments), out.Segments)
	}
	if out.Segments[0].SessionID != "p2c-01" {
		t.Fatalf("the locked search returned %q", out.Segments[0].SessionID)
	}
	if !strings.Contains(out.Segments[0].Text, "锁内命中") {
		t.Fatalf("the locked segment body is %q", out.Segments[0].Text)
	}
	for _, segment := range out.Segments {
		if strings.Contains(segment.Text, "锁外命中") {
			t.Fatal("another conversation's body reached the output")
		}
	}
	if !out.VectorChannel {
		t.Fatal("the semantic channel did not run, so its scope was not covered")
	}
}

// With no lock the search keeps every hit it found.
func TestTheUnlockedSegmentSearchKeepsEveryHit(t *testing.T) {
	run := lockScopeSegmentRun(false)
	out := callLockScopeSearch(t, mustLockScopeRegistry(t), run)
	if len(out.Segments) != 2 {
		t.Fatalf("the unlocked search returned %d segments, want both", len(out.Segments))
	}
	seen := map[string]bool{}
	for _, segment := range out.Segments {
		seen[segment.SessionID] = true
	}
	if !seen["p2c-01"] || !seen["p2c-02"] {
		t.Fatalf("the unlocked search returned %v", seen)
	}
}

// The model's actual request carries the locked body and not the other
// conversation's, even though the other one ranks higher.
type segmentLockChatModel struct {
	calls    int
	requests [][]*schema.Message
}

func (m *segmentLockChatModel) next() (*schema.Message, error) {
	m.calls++
	if m.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-segments", Type: "function",
			Function: schema.FunctionCall{Name: "search_history_segments",
				Arguments: `{"query":"练习单子 分开列"}`},
		}}), nil
	}
	return schema.AssistantMessage("清单是按动作、提问、验收分开列的。", nil), nil
}

func (m *segmentLockChatModel) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.requests = append(m.requests, append([]*schema.Message{}, input...))
	return m.next()
}

func (m *segmentLockChatModel) Stream(_ context.Context, input []*schema.Message,
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

func (m *segmentLockChatModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

func TestTheLockedSegmentBodiesInTheModelRequestAreFromTheLockedSession(t *testing.T) {
	run := lockScopeSegmentRun(true)
	tools, err := mustLockScopeRegistry(t).Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	chat := &segmentLockChatModel{}
	runner, err := serviceagent.NewRunner(chat, serviceagent.StaticToolRegistry{Registered: tools},
		e2eVerbatimSteps{}, e2eVerbatimWindowBuilder{locked: &serviceagent.LockedSession{SessionID: "p2c-01"}},
		serviceagent.DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), run, serviceagent.DiscardEvents); err != nil {
		t.Fatal(err)
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
	if strings.Contains(request.String(), "锁外命中") {
		t.Fatalf("another conversation's body entered the locked request: %s", request.String())
	}
	if !strings.Contains(request.String(), "锁内命中") {
		t.Fatalf("the locked body did not enter the request: %s", request.String())
	}
}
