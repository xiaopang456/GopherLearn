package memory

import (
	"GopherAI/model"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type memoryChat struct {
	response *schema.Message
	tools    []*schema.ToolInfo
	messages []*schema.Message
	calls    int
}

func (m *memoryChat) Generate(_ context.Context, messages []*schema.Message, _ ...modelcomponent.Option) (*schema.Message, error) {
	m.calls++
	m.messages = append([]*schema.Message(nil), messages...)
	return m.response, nil
}
func (m *memoryChat) Stream(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("unused")
}
func (m *memoryChat) WithTools(tools []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	m.tools = append([]*schema.ToolInfo(nil), tools...)
	return m, nil
}

func toolMessage(name, arguments string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{ID: "call-1", Type: "function", Function: schema.FunctionCall{Name: name, Arguments: arguments}}})
}

func TestModelExtractorUsesForcedTypedToolAndDerivesTags(t *testing.T) {
	chat := &memoryChat{response: toolMessage(toolRememberTopic, `{"subject":"etcd Watch revision 的理解","proposed_content":"用户以为 Watch 断线等于实例下线，已纠正","durability":"stable","topic":"etcd Watch","confidence":0.9,"importance":7,"sensitive":false}`)}
	extractor, _ := NewModelExtractor(chat)
	candidate, err := extractor.Extract(context.Background(), Evidence{
		EventType: model.MemoryEventAgentRun, Authority: EvidenceAuthoritySystemVerified,
		UserID: 1, SourceID: "r", SourceRefs: []string{"agent_run:r"}, Content: "etcd watch revision 断线恢复",
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Scope != model.MemoryScopeTopic || !containsTag(candidate.Tags, "etcd") {
		t.Fatalf("candidate=%#v", candidate)
	}
	// The topic is normalized server-side; the model supplies prose, not a key.
	if candidate.TopicKey != model.NormalizeLearningTopicKey("etcd Watch") {
		t.Fatalf("topic key=%q", candidate.TopicKey)
	}
	if candidate.Durability != model.MemoryDurabilityStable {
		t.Errorf("topic memory durability=%q", candidate.Durability)
	}
	if len(chat.tools) != 2 || chat.tools[0].Name != toolRememberTopic || chat.tools[1].Name != toolRejectMemory {
		t.Fatalf("tools=%#v", chat.tools)
	}
	if len(chat.messages) != 2 || !strings.Contains(chat.messages[1].Content, `"authority":"system_verified"`) {
		t.Fatalf("messages=%#v", chat.messages)
	}
}

func TestModelExtractorGlobalRequiresDurabilityAndNoTopic(t *testing.T) {
	chat := &memoryChat{response: toolMessage(toolRememberGlobal, `{"subject":"讲解顺序偏好","proposed_content":"用户希望先讲整体链路再看代码锚点","durability":"stable","confidence":0.9,"importance":8,"sensitive":false}`)}
	extractor, _ := NewModelExtractor(chat)
	candidate, err := extractor.Extract(context.Background(), Evidence{
		EventType: model.MemoryEventExplicitRemember, Authority: EvidenceAuthorityUserConfirmed,
		UserID: 1, SourceID: "r", SourceRefs: []string{"agent_run:r"}, Content: "请记住：先讲整体链路",
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Scope != model.MemoryScopeGlobal || candidate.Durability != model.MemoryDurabilityStable {
		t.Fatalf("candidate=%#v", candidate)
	}
	if candidate.TopicKey != "" {
		t.Errorf("a profile must not be bound to a technical topic: %q", candidate.TopicKey)
	}

	// A legal tool with the wrong body must still fail closed rather than
	// produce a memory whose type cannot be decided from its shape.
	missing := &memoryChat{response: toolMessage(toolRememberGlobal, `{"subject":"x","proposed_content":"y","confidence":0.8,"importance":5,"sensitive":false}`)}
	missingExtractor, _ := NewModelExtractor(missing)
	if _, err := missingExtractor.Extract(context.Background(), Evidence{
		EventType: model.MemoryEventExplicitRemember, Authority: EvidenceAuthorityUserConfirmed,
		UserID: 1, SourceID: "r", SourceRefs: []string{"agent_run:r"}, Content: "请记住",
	}); !errors.Is(err, ErrCandidateBusinessValidation) {
		t.Fatalf("profile without durability error=%v", err)
	}

	noTopic := &memoryChat{response: toolMessage(toolRememberTopic, `{"subject":"x","proposed_content":"y","durability":"stable","topic":"   ","confidence":0.8,"importance":5,"sensitive":false}`)}
	noTopicExtractor, _ := NewModelExtractor(noTopic)
	if _, err := noTopicExtractor.Extract(context.Background(), Evidence{
		EventType: model.MemoryEventAgentRun, Authority: EvidenceAuthoritySystemVerified,
		UserID: 1, SourceID: "r", SourceRefs: []string{"agent_run:r"}, Content: "verified",
	}); !errors.Is(err, ErrCandidateBusinessValidation) {
		t.Fatalf("topic note without a topic error=%v", err)
	}
}

func TestExtractionToolsExplicitlyRejectAdditionalProperties(t *testing.T) {
	for _, authority := range []EvidenceAuthority{
		EvidenceAuthorityUserConfirmed,
		EvidenceAuthoritySystemVerified,
		EvidenceAuthorityLearningAssessment,
	} {
		for _, tool := range extractionTools(authority) {
			params, err := tool.ParamsOneOf.ToJSONSchema()
			if err != nil {
				t.Fatalf("authority=%s tool=%s schema error=%v", authority, tool.Name, err)
			}
			encoded, err := json.Marshal(params)
			if err != nil {
				t.Fatalf("authority=%s tool=%s marshal error=%v", authority, tool.Name, err)
			}
			if !strings.Contains(string(encoded), `"additionalProperties":false`) {
				t.Fatalf("authority=%s tool=%s schema=%s", authority, tool.Name, encoded)
			}
		}
	}
}

// An unverified run still must not reach the model: nothing in it carries
// authority over anything.
func TestModelExtractorHardRejectsAssistantInferenceBeforeModel(t *testing.T) {
	chat := &memoryChat{response: toolMessage(toolRememberTopic, `{}`)}
	extractor, _ := NewModelExtractor(chat)
	_, err := extractor.Extract(context.Background(), Evidence{
		EventType: model.MemoryEventAgentRun, Authority: EvidenceAuthorityAssistantInference, UserID: 1,
		SourceID: "r", SourceRefs: []string{"agent_run:r"}, Content: "RAG says the team uses PostgreSQL",
	})
	if !errors.Is(err, ErrNoDurableMemory) || chat.calls != 0 {
		t.Fatalf("calls=%d err=%v", chat.calls, err)
	}
}

// A document-grounded answer remains untrusted for autobiographical writes.
// Even when the learner's sentence sounds like a confusion, this authority
// envelope mixes it with retrieved/model text and cannot grant write access.
func TestKnowledgeOnlyEvidenceHardRejectsBeforeModel(t *testing.T) {
	chat := &memoryChat{response: toolMessage(toolRememberTopic, `{}`)}
	extractor, _ := NewModelExtractor(chat)
	_, err := extractor.Extract(context.Background(), Evidence{
		EventType: model.MemoryEventAgentRun, Authority: EvidenceAuthorityKnowledgeOnly, UserID: 1,
		SourceID: "r", SourceRefs: []string{"agent_run:r"}, Content: "用户说他以前理解错了 TCP 的 read",
	})
	if !errors.Is(err, ErrNoDurableMemory) || chat.calls != 0 || len(chat.tools) != 0 {
		t.Fatalf("calls=%d tools=%v err=%v", chat.calls, chat.tools, err)
	}
}

func TestModelExtractorAuthorityRestrictsMemoryTypes(t *testing.T) {
	chat := &memoryChat{response: toolMessage(toolRememberGlobal, `{"subject":"x","proposed_content":"y","confidence":0.8,"importance":5,"sensitive":false}`)}
	extractor, _ := NewModelExtractor(chat)
	_, err := extractor.Extract(context.Background(), Evidence{
		EventType: model.MemoryEventAgentRun, Authority: EvidenceAuthoritySystemVerified,
		UserID: 1, SourceID: "r", SourceRefs: []string{"agent_run:r"}, Content: "verified result",
	})
	if !errors.Is(err, ErrCandidateToolProtocol) {
		t.Fatalf("error=%v", err)
	}
	for _, tool := range chat.tools {
		if tool.Name == toolRememberGlobal {
			t.Fatal("a system-verified run carries no statement by the user and must not expose remember_profile")
		}
	}
	if len(chat.tools) != 2 || chat.tools[0].Name != toolRememberTopic {
		t.Fatalf("system_verified should expose only the topic note tool: %#v", chat.tools)
	}
}

func TestModelExtractorValidatesContentLimitAndSecrets(t *testing.T) {
	// A memory is one conclusion. Anything longer belongs in several, because
	// five memories at the old 2000-rune ceiling could never fit the budget.
	long := strings.Repeat("长", model.MemoryContentMaxRunes+1)
	chat := &memoryChat{response: toolMessage(toolRememberTopic, `{"subject":"deploy","proposed_content":"`+long+`","durability":"stable","topic":"docker","confidence":0.8,"importance":5,"sensitive":false}`)}
	extractor, _ := NewModelExtractor(chat)
	_, err := extractor.Extract(context.Background(), Evidence{
		EventType: model.MemoryEventAgentRun, Authority: EvidenceAuthoritySystemVerified,
		UserID: 1, SourceID: "r", SourceRefs: []string{"agent_run:r"}, Content: "verified deploy",
	})
	if !errors.Is(err, ErrCandidateBusinessValidation) {
		t.Fatalf("over-long content error=%v", err)
	}

	secretChat := &memoryChat{response: toolMessage(toolRejectMemory, `{"reject_reason":"secret","sensitive":true}`)}
	secretExtractor, _ := NewModelExtractor(secretChat)
	_, err = secretExtractor.Extract(context.Background(), Evidence{
		EventType: model.MemoryEventExplicitRemember, Authority: EvidenceAuthorityUserConfirmed,
		UserID: 1, SourceID: "r", SourceRefs: []string{"agent_run:r"}, Content: "请记住 password=do-not-store-this",
	})
	if !errors.Is(err, ErrSensitiveMemory) || secretChat.calls != 0 {
		t.Fatalf("secret calls=%d error=%v", secretChat.calls, err)
	}
}

func TestModelExtractorClassifiesToolContractFailures(t *testing.T) {
	base := `{"subject":"scope","proposed_content":"project scope","confidence":0.8,"importance":5,"sensitive":false}`
	tests := []struct {
		name     string
		response *schema.Message
		want     error
		code     string
	}{
		{name: "nil", response: nil, want: ErrCandidateEmptyResponse, code: "empty_response"},
		{name: "no tool", response: schema.AssistantMessage(base, nil), want: ErrCandidateToolProtocol, code: "tool_protocol"},
		{name: "syntax", response: toolMessage(toolRememberTopic, `{`), want: ErrCandidateJSONSyntax, code: "json_syntax"},
		{name: "importance type", response: toolMessage(toolRememberTopic, strings.Replace(base, `"importance":5`, `"importance":0.8`, 1)), want: ErrCandidateFieldType, code: "field_type"},
		{name: "unknown field", response: toolMessage(toolRememberTopic, strings.Replace(base, `}`, `,"extra":true}`, 1)), want: ErrCandidateUnknownField, code: "unknown_field"},
		{name: "business", response: toolMessage(toolRememberTopic, strings.Replace(base, `"importance":5`, `"importance":0`, 1)), want: ErrCandidateBusinessValidation, code: "business_validation"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chat := &memoryChat{response: tc.response}
			extractor, _ := NewModelExtractor(chat)
			_, err := extractor.Extract(context.Background(), Evidence{
				EventType: model.MemoryEventExplicitRemember, Authority: EvidenceAuthorityUserConfirmed,
				UserID: 1, SourceID: "r", SourceRefs: []string{"agent_run:r"}, Content: "remember project scope",
			})
			if !errors.Is(err, tc.want) || ExtractionErrorCode(err) != tc.code {
				t.Fatalf("error=%v code=%s want=%v/%s", err, ExtractionErrorCode(err), tc.want, tc.code)
			}
		})
	}
}

func TestModelExtractorWellTypedRejectIsNoDurableMemory(t *testing.T) {
	chat := &memoryChat{response: toolMessage(toolRejectMemory, `{"reject_reason":"temporary","sensitive":false}`)}
	extractor, _ := NewModelExtractor(chat)
	_, err := extractor.Extract(context.Background(), Evidence{
		EventType: model.MemoryEventExplicitRemember, Authority: EvidenceAuthorityUserConfirmed,
		UserID: 1, SourceID: "r", SourceRefs: []string{"agent_run:r"}, Content: "temporary task",
	})
	if !errors.Is(err, ErrNoDurableMemory) || ExtractionErrorCode(err) != "no_durable_memory" {
		t.Fatalf("error=%v code=%s", err, ExtractionErrorCode(err))
	}
}

func containsTag(tags []string, target string) bool {
	for _, tag := range tags {
		if tag == target {
			return true
		}
	}
	return false
}
