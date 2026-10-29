package episode

import (
	"GopherAI/model"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type episodeChat struct {
	responses []*schema.Message
	tools     []*schema.ToolInfo
	calls     int
	// sent records what was actually put on the wire, so a test can assert on
	// what a component did NOT send as well as what it did.
	sent []*schema.Message
}

func (m *episodeChat) Generate(_ context.Context, messages []*schema.Message, _ ...modelcomponent.Option) (*schema.Message, error) {
	m.sent = append(m.sent, messages...)
	if m.calls >= len(m.responses) {
		return nil, errors.New("unexpected model call")
	}
	response := m.responses[m.calls]
	m.calls++
	return response, nil
}

func (m *episodeChat) Stream(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("unused")
}

func (m *episodeChat) WithTools(tools []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	m.tools = append([]*schema.ToolInfo(nil), tools...)
	return m, nil
}

func episodeToolResponse(arguments string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{ID: "episode-call", Type: "function", Function: schema.FunctionCall{Name: episodeToolName, Arguments: arguments}}})
}

func projectionEvidence(messages ...model.Message) ProjectionEvidence {
	target := messages[len(messages)-1]
	return ProjectionEvidence{
		UserID: 7, Session: model.Session{ID: "session-1", UserID: 7, Title: "RPC 粘包"},
		Messages: messages, TargetMessageID: target.ID, TargetMessageAt: target.CreatedAt,
	}
}

func TestModelGeneratorValidatesSourcesAndNormalizesTopics(t *testing.T) {
	now := time.Now()
	chat := &episodeChat{responses: []*schema.Message{episodeToolResponse(`{
		"synopsis":"用户讨论了 RPC 粘包并决定使用长度前缀。",
		"topics":[{"key":"model-owned","label":"RPC 粘包"}],
		"decisions":[{"text":"使用长度前缀协议","evidence_message_ids":[1]}],
		"open_loops":[],"user_corrections":[],"source_handles":["message:1"]}`)}}
	generator, err := NewModelGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := generator.Generate(context.Background(), projectionEvidence(
		model.Message{ID: 1, SessionID: "session-1", IsUser: true, Content: "我们决定使用长度前缀", CreatedAt: now},
	))
	if err != nil {
		t.Fatal(err)
	}
	document := outcome.Document
	if got, want := document.Topics[0].Key, model.NormalizeLearningTopicKey("RPC 粘包"); got != want {
		t.Fatalf("topic key=%q want=%q", got, want)
	}
	if chat.calls != 1 || len(chat.tools) != 1 || chat.tools[0].Name != episodeToolName {
		t.Fatalf("calls=%d tools=%v", chat.calls, chat.tools)
	}
}

func TestModelGeneratorCanPromoteAuditedSummaryDecision(t *testing.T) {
	now := time.Now()
	evidence := ProjectionEvidence{
		UserID: 7, Session: model.Session{ID: "session-1", UserID: 7, Title: "RPC 粘包"},
		TargetMessageID: 1, TargetMessageAt: now,
		Summary: &model.SessionSummary{
			UserID: 7, SessionID: "session-1", Version: 2, CoveredMessageID: 1,
			Content: `{"schema_version":2,"confirmed_decisions":[{"text":"使用长度前缀","evidence_message_ids":[1]}]}`,
		},
		SummaryMessages: []model.Message{{ID: 1, SessionID: "session-1", IsUser: true, Content: "决定使用长度前缀", CreatedAt: now}},
	}
	chat := &episodeChat{responses: []*schema.Message{episodeToolResponse(`{"synopsis":"用户此前确认长度前缀方案。","topics":[],"decisions":[{"text":"使用长度前缀","evidence_message_ids":[1]}],"open_loops":[],"user_corrections":[],"source_handles":["summary:session-1:v2"]}`)}}
	generator, _ := NewModelGenerator(chat, 4096, 1200)
	outcome, err := generator.Generate(context.Background(), evidence)
	if err != nil {
		t.Fatal(err)
	}
	document := outcome.Document
	if len(document.Decisions) != 1 || document.Decisions[0].EvidenceMessageIDs[0] != 1 {
		t.Fatalf("document=%#v", document)
	}
}

func TestModelGeneratorDropsAssistantAttributionAndRejectsInvention(t *testing.T) {
	now := time.Now()
	messages := []model.Message{
		{ID: 1, SessionID: "session-1", IsUser: true, Content: "问题", CreatedAt: now},
		{ID: 2, SessionID: "session-1", IsUser: false, Content: "建议采用方案", CreatedAt: now.Add(time.Second)},
	}

	// What must never happen is that a decision the assistant proposed is stored
	// as one the user made. Dropping that entry achieves it while keeping the
	// rest of the projection, so the assertion is on the stored result rather
	// than on whether the whole document was thrown away.
	t.Run("assistant attribution is dropped", func(t *testing.T) {
		chat := &episodeChat{responses: []*schema.Message{episodeToolResponse(`{"synopsis":"讨论","topics":[],"decisions":[{"text":"采用方案","evidence_message_ids":[2]}],"open_loops":[{"text":"用户尚未回应","evidence_message_ids":[2]}],"user_corrections":[],"source_handles":["message:2"]}`)}}
		generator, _ := NewModelGenerator(chat, 4096, 1200)
		outcome, err := generator.Generate(context.Background(), projectionEvidence(messages...))
		if err != nil {
			t.Fatalf("error=%v want=nil", err)
		}
		// Both entries cited the assistant turn, so both are dropped. With
		// nothing left, the projection abstains rather than storing a
		// synopsis-only Episode - that shell was the over-generation this
		// round removes, and a synopsis is not a reason to remember a session.
		if outcome.Kind != OutcomeAbstained {
			t.Fatalf("kind=%q want abstained; document=%#v", outcome.Kind, outcome.Document)
		}
		if outcome.Abstain.Reason != model.AbstainNoDurableAssertion {
			t.Fatalf("reason=%q", outcome.Abstain.Reason)
		}
	})

	// A cited id that is not in the input is not over-attribution but invention,
	// and it says nothing else in the document can be trusted.
	t.Run("invented evidence id is rejected", func(t *testing.T) {
		chat := &episodeChat{responses: []*schema.Message{episodeToolResponse(`{"synopsis":"讨论","topics":[],"decisions":[{"text":"采用方案","evidence_message_ids":[99]}],"open_loops":[],"user_corrections":[],"source_handles":["message:2"]}`)}}
		generator, _ := NewModelGenerator(chat, 4096, 1200)
		if _, err := generator.Generate(context.Background(), projectionEvidence(messages...)); !errors.Is(err, ErrEpisodeSourceBoundary) {
			t.Fatalf("error=%v want=%v", err, ErrEpisodeSourceBoundary)
		}
	})

	t.Run("unknown field is rejected", func(t *testing.T) {
		chat := &episodeChat{responses: []*schema.Message{episodeToolResponse(`{"synopsis":"讨论","topics":[],"decisions":[],"open_loops":[],"user_corrections":[],"source_handles":["message:2"],"invented":true}`)}}
		generator, _ := NewModelGenerator(chat, 4096, 1200)
		if _, err := generator.Generate(context.Background(), projectionEvidence(messages...)); !errors.Is(err, ErrEpisodeToolProtocol) {
			t.Fatalf("error=%v want=%v", err, ErrEpisodeToolProtocol)
		}
	})
}

func TestModelGeneratorRejectsInventedHandleAndSecret(t *testing.T) {
	now := time.Now()
	tests := []struct {
		args string
		want error
	}{
		{`{"synopsis":"讨论","topics":[],"decisions":[],"open_loops":[],"user_corrections":[],"source_handles":["message:999"]}`, ErrEpisodeSourceBoundary},
		{`{"synopsis":"用户保存 password=do-not-store","topics":[],"decisions":[],"open_loops":[],"user_corrections":[],"source_handles":["message:1"]}`, ErrEpisodeSensitive},
	}
	for _, test := range tests {
		chat := &episodeChat{responses: []*schema.Message{episodeToolResponse(test.args)}}
		generator, _ := NewModelGenerator(chat, 4096, 1200)
		_, err := generator.Generate(context.Background(), projectionEvidence(model.Message{ID: 1, SessionID: "session-1", IsUser: true, Content: "讨论", CreatedAt: now}))
		if !errors.Is(err, test.want) {
			t.Fatalf("error=%v want=%v", err, test.want)
		}
	}
}

func TestModelGeneratorNeverTruncatesAnOversizedMessage(t *testing.T) {
	now := time.Now()
	chat := &episodeChat{}
	generator, _ := NewModelGenerator(chat, 1024, 512)
	_, err := generator.Generate(context.Background(), projectionEvidence(model.Message{
		ID: 1, SessionID: "session-1", IsUser: true, Content: strings.Repeat("完整消息", 4000), CreatedAt: now,
	}))
	if !errors.Is(err, ErrEpisodeInputTooLarge) || chat.calls != 0 {
		t.Fatalf("error=%v calls=%d", err, chat.calls)
	}
}

func TestModelGeneratorCompressesOnlyAtCompleteMessageBoundaries(t *testing.T) {
	now := time.Now()
	first := `{"synopsis":"第一段","topics":[],"decisions":[],"open_loops":[],"user_corrections":[],"source_handles":["message:1"]}`
	// The final response carries one decision. Without it the projection would
	// now abstain - correctly, since a synopsis alone is no longer a reason to
	// remember a session - and this test is about batch boundaries, not about
	// eligibility.
	second := `{"synopsis":"两段消息均已处理","topics":[],"decisions":[{"text":"采用长度前缀","evidence_message_ids":[1]}],"open_loops":[],"user_corrections":[],"source_handles":["message:1","message:2"]}`
	chat := &episodeChat{responses: []*schema.Message{episodeToolResponse(first), episodeToolResponse(second)}}
	generator, _ := NewModelGenerator(chat, 1024, 512)
	messages := []model.Message{
		{ID: 1, SessionID: "session-1", IsUser: true, Content: strings.Repeat("a", 2500), CreatedAt: now},
		{ID: 2, SessionID: "session-1", IsUser: false, Content: strings.Repeat("b", 2500), CreatedAt: now.Add(time.Second)},
	}
	if got := largestCompleteBatch(projectionEvidence(messages...), nil, messages, 1024); got != 1 {
		t.Fatalf("first batch size=%d want=1", got)
	}
	outcome, err := generator.Generate(context.Background(), projectionEvidence(messages...))
	if err != nil {
		t.Fatal(err)
	}
	document := outcome.Document
	if chat.calls != 2 || document.Synopsis != "两段消息均已处理" {
		t.Fatalf("calls=%d document=%#v", chat.calls, document)
	}
}
