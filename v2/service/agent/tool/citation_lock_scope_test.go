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

// The p2-art-04-r2 rejection: a locked turn cited ids the browse tool had just
// granted for the locked conversation, and the draft was refused with
// "no verifiable citation" because the knowledge pre-retrieval had also run
// and found nothing.
//
// These tests pin the three-way split the record could not show by itself:
// the granted, locked citation must pass; an id nothing granted must still be
// refused; and an id granted for ANOTHER conversation must be refused under
// the lock.

type citationChatModel struct {
	calls int
	draft string
}

func (m *citationChatModel) next() (*schema.Message, error) {
	m.calls++
	if m.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-browse", Type: "function",
			Function: schema.FunctionCall{Name: "browse_summary_segments",
				Arguments: `{"session":"p2c-04","query":"三道练习 第二道"}`},
		}}), nil
	}
	return schema.AssistantMessage(m.draft, nil), nil
}

func (m *citationChatModel) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	return m.next()
}

func (m *citationChatModel) Stream(_ context.Context, _ []*schema.Message,
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

func (m *citationChatModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

// citationWindowBuilder carries the card scope the window established: the
// browse tool is allowed to open p2c-04 because a session card did.
type citationWindowBuilder struct{}

func (citationWindowBuilder) Build(_ context.Context, input serviceagent.RunContext) (serviceagent.ContextWindow, error) {
	window := serviceagent.ContextWindow{
		Messages:    []*schema.Message{schema.SystemMessage("You are GopherAI."), schema.UserMessage(input.Question)},
		SessionLock: &serviceagent.LockedSession{SessionID: "p2c-04"},
		SessionCardScopes: []serviceagent.CardScope{{
			SessionID: "p2c-04", CardID: "card-04", CardVersion: 2, CardWatermark: 1000,
		}},
	}
	window.Budget.System = 10
	window.Budget.CurrentQuestion = 10
	window.Budget.Assembled = 20
	return window, nil
}

// citationProbingRegistry is the capturing registry plus the pre-retrieval
// the runner asks for: it reports "ran, found nothing", which is the state the
// record's turn was in when the valid citation was refused.
type citationProbingRegistry struct {
	*capturingRegistry
}

func (r *citationProbingRegistry) ProbeRelevance(context.Context, serviceagent.RunContext,
	string, float64) (serviceagent.RelevanceOutcome, error) {
	return serviceagent.RelevanceOutcome{Status: serviceagent.RelevanceBelowThreshold}, nil
}

type capturingRegistry struct {
	registry *BrowseSummarySegmentsRegistry
	captured *serviceagent.RunContext
}

func (r *capturingRegistry) Tools(ctx context.Context, run serviceagent.RunContext) ([]tool.BaseTool, error) {
	*r.captured = run
	return r.registry.Tools(ctx, run)
}

func citationRun() serviceagent.RunContext {
	run := serviceagent.RunContext{RunID: "run-citation", UserID: 7, UserName: "learner",
		SessionID: "current-session", LockedSessionID: "p2c-04",
		Question: "上次你给我排的那三道题里，第二道练的是什么？"}
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	run.GrantSet.ExtendCardScope(run, serviceagent.CardScope{
		SessionID: "p2c-04", CardID: "card-04", CardVersion: 2, CardWatermark: 1000,
	})
	// The knowledge pre-retrieval ran and found nothing. That used to be
	// enough to refuse a locked conversation citation.
	status := serviceagent.RelevanceBelowThreshold
	run.Relevance = &serviceagent.RelevanceOutcome{Status: status}
	return run
}

func browseRegistryWithPoint(t *testing.T, point string) *BrowseSummarySegmentsRegistry {
	t.Helper()
	// The point's evidence ids are what the browse tool draws the read-back
	// grants from; the text carries them the way the stored record does.
	segment := model.SessionSummarySegment{
		UserID: 7, SessionID: "p2c-04", Seq: 1, StartMessageID: 1, EndMessageID: 500,
		Topic: "三道练习题做完后的复盘与心得",
		Content: storedSegment(t, "三道练习题做完后的复盘与心得", []model.SessionSummarySegmentPoint{{
			Text: point, EvidenceMessageIDs: []uint{405},
		}}),
	}
	registry, err := NewBrowseSummarySegmentsRegistry(&fakeSegmentBrowser{
		segments: []model.SessionSummarySegment{segment}})
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSessionParameter(true)
	return registry
}

func runCitationTurn(t *testing.T, registry *BrowseSummarySegmentsRegistry,
	run serviceagent.RunContext, draft string) serviceagent.RunnerResult {
	t.Helper()
	var captured serviceagent.RunContext
	runner, err := serviceagent.NewRunner(&citationChatModel{draft: draft},
		&citationProbingRegistry{capturingRegistry: &capturingRegistry{registry: registry, captured: &captured}},
		e2eVerbatimSteps{}, citationWindowBuilder{}, serviceagent.DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), run, serviceagent.DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	// The tool's extensions live on the context the runner handed it; attach
	// that set back to the returned result's run for the assertions below.
	run.GrantSet = captured.GrantSet
	lastCitationRun = run
	return result
}

// lastCitationRun is the effective RunContext of the last citation test turn.
var lastCitationRun serviceagent.RunContext

// The cited id was granted by the browse tool, for the locked conversation:
// it is a valid citation and must pass.
func TestALockedGrantedCitationIsNotRefused(t *testing.T) {
	run := citationRun()
	draft := "第二道练的是性能与并发用例 [405]。"
	result := runCitationTurn(t, browseRegistryWithPoint(t, "第二道：性能与并发用例 [message_ids=405]"), run, draft)
	if result.GroundingRefusal != nil {
		t.Fatalf("a granted locked citation was refused: rule=%s citations=%v draft=%q toolCalls=%d modelCalls=%d refs=%v",
			result.GroundingRefusal.Rule, result.GroundingRefusal.Citations, result.GroundingRefusal.Draft,
			result.ToolCalls, result.ModelCalls, effectiveReferences())
	}
	if result.Answer != draft {
		t.Fatalf("answer=%q, want the draft", result.Answer)
	}
	if result.AnswerBasis != "conversation" {
		t.Fatalf("answer basis=%q, want conversation", result.AnswerBasis)
	}
	// The authorisation really was issued, for the locked conversation.
	effective := lastCitationRun.GrantSet
	if !containsReference(effective.References(), 405) {
		t.Fatalf("405 was not in the run's references: %v", effective.References())
	}
	inLocked := false
	for _, grant := range effective.Grants(405) {
		if grant.SourceSessionID == "p2c-04" {
			inLocked = true
		}
	}
	if !inLocked {
		t.Fatal("405 was not granted for the locked conversation")
	}
}

// An id nothing granted is a fabricated citation and is still refused.
func TestAnUngrantedCitationIsStillRefused(t *testing.T) {
	run := citationRun()
	result := runCitationTurn(t, browseRegistryWithPoint(t, "第二道：性能与并发用例 [message_ids=405]"),
		run, "第二道练的是性能与并发用例 [999]。")
	if result.GroundingRefusal == nil {
		t.Fatalf("an ungranted citation passed: %q", result.Answer)
	}
	if result.GroundingRefusal.Rule != serviceagent.GroundingRuleInvalidCitation {
		t.Fatalf("refusal rule=%q", result.GroundingRefusal.Rule)
	}
	if len(result.GroundingRefusal.Citations) != 1 || result.GroundingRefusal.Citations[0] != 999 {
		t.Fatalf("refused citations=%v", result.GroundingRefusal.Citations)
	}
	if result.Answer != serviceagent.UnsupportedGroundingAnswer {
		t.Fatalf("answer=%q", result.Answer)
	}
}

// An id granted for ANOTHER conversation does not become a citation of the
// locked one.
func TestACitationFromOutsideTheLockedSessionIsStillRefused(t *testing.T) {
	run := citationRun()
	// 407 exists and is authorised, but for p2c-02, not the locked p2c-04.
	run.GrantSet.Extend(run, []serviceagent.HistoryMessageGrant{{
		MessageID: 407, SourceKind: serviceagent.HistorySourceSessionSearch,
		SourceSessionID: "p2c-02",
	}})
	result := runCitationTurn(t, browseRegistryWithPoint(t, "第二道：性能与并发用例 [message_ids=405]"),
		run, "第二道练的是性能与并发用例 [407]。")
	if result.GroundingRefusal == nil {
		t.Fatalf("a citation from outside the locked conversation passed: %q", result.Answer)
	}
	if result.GroundingRefusal.Rule != serviceagent.GroundingRuleInvalidCitation {
		t.Fatalf("refusal rule=%q", result.GroundingRefusal.Rule)
	}
	if !strings.Contains(result.Answer, "未能形成可验证的引用") {
		t.Fatalf("answer=%q", result.Answer)
	}
}

func effectiveReferences() []int {
	if lastCitationRun.GrantSet == nil {
		return nil
	}
	return lastCitationRun.GrantSet.References()
}

func containsReference(references []int, wanted int) bool {
	for _, reference := range references {
		if reference == wanted {
			return true
		}
	}
	return false
}
