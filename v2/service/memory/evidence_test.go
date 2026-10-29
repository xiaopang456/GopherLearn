package memory

import (
	"GopherAI/model"
	"errors"
	"strings"
	"testing"
)

func TestAgentRunAuthorityNeverTreatsKnowledgeOrAssistantTextAsVerified(t *testing.T) {
	tests := []struct {
		name  string
		steps []model.AgentStep
		want  EvidenceAuthority
	}{
		{name: "knowledge search", steps: []model.AgentStep{{Type: model.AgentStepTool, ToolName: "knowledge_search"}}, want: EvidenceAuthorityKnowledgeOnly},
		{name: "search and read", steps: []model.AgentStep{{Type: model.AgentStepTool, ToolName: "knowledge_search"}, {Type: model.AgentStepTool, ToolName: "document_read"}}, want: EvidenceAuthorityKnowledgeOnly},
		{name: "answer only", steps: []model.AgentStep{{Type: model.AgentStepAnswer}}, want: EvidenceAuthorityAssistantInference},
		{name: "unknown tool", steps: []model.AgentStep{{Type: model.AgentStepTool, ToolName: "future_tool"}}, want: EvidenceAuthorityAssistantInference},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentRunAuthority(tc.steps); got != tc.want {
				t.Fatalf("authority=%s want=%s", got, tc.want)
			}
		})
	}
}

// The summarizer's `current_goal` describes the conversation in progress, and
// handing it to the extractor turned three questions about serialization into a
// permanent global fact about what the learner wants. Everything the summary
// says about the learner survives; only the session's own state is removed.
func TestSummaryEvidenceDropsSessionScopedFields(t *testing.T) {
	payload := `{"current_goal":"clarify prior decisions about serialization",` +
		`"open_loops":["How PacketBuffer handles sticky packets"],` +
		`"completed_items":["read day3 notes"],` +
		`"key_errors":["误以为 TCP 保证消息边界"],` +
		`"user_corrections":["不是 JSON，是 Protobuf"],` +
		`"confirmed_decisions":["Body 采用 Protobuf"]}`

	stripped, err := durableSummaryEvidence(payload)
	if err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{"current_goal", "open_loops", "completed_items"} {
		if strings.Contains(stripped, gone) {
			t.Fatalf("%q describes the session, not the learner: %s", gone, stripped)
		}
	}
	// A mistake the learner made is a fact about their understanding, which is
	// what topic memory exists to hold. Only progress fields are removed.
	for _, kept := range []string{"user_corrections", "Protobuf", "confirmed_decisions", "key_errors"} {
		if !strings.Contains(stripped, kept) {
			t.Fatalf("%q was removed along with the goal: %s", kept, stripped)
		}
	}
}

func TestSummaryEvidenceKeepsOnlyDurableAllowlistedFields(t *testing.T) {
	payload := `{"user_corrections":["不是 JSON，是 Protobuf"]}`
	if got, err := durableSummaryEvidence(payload); err != nil || got != payload {
		t.Fatalf("payload was rewritten:\n got %s\nwant %s", got, payload)
	}
	for _, payload := range []string{
		`{"current_goal":"finish this session"}`,
		`{"open_loops":["temporary"],"completed_items":["temporary"],"references":["doc:1"]}`,
		`   `,
	} {
		if got, err := durableSummaryEvidence(payload); got != "" || !errors.Is(err, ErrNoDurableMemory) {
			t.Fatalf("session-only payload got=%q err=%v", got, err)
		}
	}
}

func TestSummaryEvidenceAcceptsV2AndKeepsTextNotInternalIDs(t *testing.T) {
	payload := `{"schema_version":2,"confirmed_decisions":[{"text":"Body 使用 Protobuf","evidence_message_ids":[7]}],"user_corrections":[{"text":"不是 JSON","evidence_message_ids":[9]}]}`
	got, err := durableSummaryEvidence(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Body 使用 Protobuf") || !strings.Contains(got, "不是 JSON") || strings.Contains(got, "evidence_message_ids") {
		t.Fatalf("durable evidence=%s", got)
	}
}

func TestSummaryEvidenceRejectsMalformedProducerContract(t *testing.T) {
	for _, payload := range []string{
		"not json at all",
		`null`,
		`[]`,
		`{"current_goal":1,"user_corrections":["x"]}`,
		`{"user_corrections":"x"}`,
		`{"user_corrections":["x"],"unknown":true}`,
		`{"user_corrections":["x"]} {}`,
	} {
		got, err := durableSummaryEvidence(payload)
		if got != "" || !errors.Is(err, ErrInvalidSummaryEvidence) {
			t.Fatalf("payload=%q got=%q err=%v", payload, got, err)
		}
	}
}
