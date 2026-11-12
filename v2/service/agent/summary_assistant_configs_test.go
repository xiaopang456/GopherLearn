package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"GopherAI/config"
	"GopherAI/model"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// The two configurations a measurement runs under.
//
// The scaled-down one has a small window on purpose: a segment that does not
// fit shows up there instead of hiding. The other one is what the product
// starts from. A fix that holds in one of them and not in the other is not a
// fix, so both are run.
func measurementConfigurations() []struct {
	name  string
	agent config.AgentConfig
} {
	return []struct {
		name  string
		agent config.AgentConfig
	}{
		{
			name: "scaled",
			agent: config.AgentConfig{
				SummaryAssistantContent:          "on",
				ContextInputTargetTokens:         9200,
				KnowledgeMaxRunTokens:            6500,
				ContextMaxWhiteboardTokens:       360,
				ContextMaxResidentSegmentTokens:  370,
				ContextMaxRetrievedSegmentTokens: 370,
				ContextMaxSummaryTokens:          128,
				ContextMaxSegmentTokens:          300,
				SummarySegmentMaxInputTokens:     8000,
				SummarySegmentMaxOutputTokens:    2000,
			},
		},
		{
			name: "product default",
			agent: config.AgentConfig{
				SummaryAssistantContent:          "on",
				ContextInputTargetTokens:         28000,
				KnowledgeMaxRunTokens:            6500,
				ContextMaxWhiteboardTokens:       1200,
				ContextMaxResidentSegmentTokens:  1200,
				ContextMaxRetrievedSegmentTokens: 1000,
				ContextMaxSummaryTokens:          2000,
				ContextMaxSegmentTokens:          300,
				SummarySegmentMaxInputTokens:     8000,
				SummarySegmentMaxOutputTokens:    2000,
			},
		},
	}
}

// A long conversation is generated under both configurations, in one call,
// and the segment it produced reaches the window.
//
// The four assertions are the ones the measurement needs: the batch was
// generated (no `input_too_large`), it was not split, the segment was not
// dropped, and it is in the block.
func TestTheLongConversationIsGeneratedAndKeptUnderBothConfigurations(t *testing.T) {
	for _, testCase := range measurementConfigurations() {
		t.Run(testCase.name, func(t *testing.T) {
			messages := longConversation(65)
			stub := &longConversationModel{}
			generator, err := NewModelSegmentGenerator(stub,
				testCase.agent.EffectiveSummarySegmentMaxInputTokens(),
				testCase.agent.SummarySegmentMaxOutputTokens)
			if err != nil {
				t.Fatal(err)
			}
			generator.UseAssistantContent(true)
			generator.UseSegmentBudget(testCase.agent.EffectiveSegmentTokens())
			result, err := generator.GenerateSegment(context.Background(), SegmentGenerationInput{
				Messages: messages, FirstMessageID: 1, LastMessageID: messages[len(messages)-1].ID,
			})
			if err != nil {
				t.Fatalf("the batch was not generated: %v", err)
			}
			if result.Calls != 1 {
				t.Fatalf("the batch took %d model calls, want 1", result.Calls)
			}
			if result.Degraded != 0 {
				t.Fatalf("%d of the assistant's messages were given up to make the batch fit", result.Degraded)
			}
			segment := model.SessionSummarySegment{
				StartMessageID: 1, EndMessageID: messages[len(messages)-1].ID, Content: result.SegmentContent,
			}
			rendered := renderSummarySegments([]model.SessionSummarySegment{segment}, result.Whiteboard,
				testCase.agent.EffectiveResidentSegmentTokens(), testCase.agent.EffectiveSegmentTokens(), true)
			if rendered.DroppedSegments != 0 {
				t.Fatalf("%d segments were dropped from the window", rendered.DroppedSegments)
			}
			if len(rendered.Segments) != 1 {
				t.Fatalf("segments in the block = %d, want 1", len(rendered.Segments))
			}
			if rendered.Text == "" {
				t.Fatal("the segment did not reach the window")
			}
		})
	}
}

// The scaled configuration is the one that could not carry a segment at all.
//
// The block budget is smaller than the segment the mode produces, and the
// block is raised to what one whole segment costs - so the segment fits
// instead of being dropped with nothing said about it.
func TestTheScaledConfigurationRaisesTheBlockToWhatASegmentCosts(t *testing.T) {
	for _, testCase := range measurementConfigurations() {
		t.Run(testCase.name, func(t *testing.T) {
			needed := testCase.agent.EffectiveSegmentTokens() + SummarySegmentBlockHeaderTokens()
			if got := testCase.agent.EffectiveResidentSegmentTokens(); got < needed {
				t.Fatalf("the resident block carries %d tokens, a segment needs %d", got, needed)
			}
			if got := testCase.agent.EffectiveRetrievedSegmentTokens(); got < needed {
				t.Fatalf("the retrieved block carries %d tokens, a segment needs %d", got, needed)
			}
		})
	}
}

// The board keeps the learner's own entries when the assistant's proposal is
// refused.
//
// The refusal of one entry is the refusal of that entry: the entries of the
// learner around it are not collateral.
func TestTheLearnersEntriesSurviveARefusedProposalUnderBothConfigurations(t *testing.T) {
	for _, testCase := range measurementConfigurations() {
		t.Run(testCase.name, func(t *testing.T) {
			messages := longConversation(65)
			stub := &mixedProposalModel{}
			generator, err := NewModelSegmentGenerator(stub,
				testCase.agent.EffectiveSummarySegmentMaxInputTokens(),
				testCase.agent.SummarySegmentMaxOutputTokens)
			if err != nil {
				t.Fatal(err)
			}
			generator.UseAssistantContent(true)
			generator.UseSegmentBudget(testCase.agent.EffectiveSegmentTokens())
			result, err := generator.GenerateSegment(context.Background(), SegmentGenerationInput{
				Messages: messages, FirstMessageID: 1, LastMessageID: messages[len(messages)-1].ID,
			})
			if err != nil {
				t.Fatalf("the batch was not generated: %v", err)
			}
			if len(result.Whiteboard.Constraints) != 1 {
				t.Fatalf("the board holds %d entries, want the learner's one", len(result.Whiteboard.Constraints))
			}
			if result.Whiteboard.Constraints[0].Key != "learner" {
				t.Fatalf("the wrong entry survived: %+v", result.Whiteboard.Constraints)
			}
			if got := result.Whiteboard.Constraints[0].EntryOrigin(); got != model.SessionWhiteboardOriginUser {
				t.Fatalf("the entry's origin = %q, want the learner's", got)
			}
			if result.Diagnostics.AssistantProposals != 1 {
				t.Fatalf("assistant proposals = %d, want 1", result.Diagnostics.AssistantProposals)
			}
			found := false
			for _, point := range result.Segment.Points {
				if point.PointSource() == model.SessionSummarySegmentSourceAssistant &&
					strings.Contains(point.Text, "助手提议的安排") {
					found = true
				}
			}
			if !found {
				t.Fatalf("the proposal was lost instead of becoming a product: %+v", result.Segment.Points)
			}
		})
	}
}

// The mode that does not record the assistant's products behaves as it did.
//
// Its batch is not split: a batch that does not fit is a batch that failed,
// exactly as it failed before. The two arms of a measurement have to differ
// in the switch and nowhere else.
func TestTheOffModeStillFailsTheBatchThatDoesNotFit(t *testing.T) {
	messages := longConversation(65)
	stub := &longConversationModel{}
	generator, err := NewModelSegmentGenerator(stub, 1200, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if generator.AssistantContentEnabled() {
		t.Fatal("the generator records the assistant's products without being asked to")
	}
	if _, err := generator.GenerateSegment(context.Background(), SegmentGenerationInput{
		Messages: messages, FirstMessageID: 1, LastMessageID: messages[len(messages)-1].ID,
	}); err == nil {
		t.Fatal("a batch that does not fit must fail the way it always did")
	} else if !strings.Contains(err.Error(), "exceeds token limit") {
		t.Fatalf("the failure does not say why: %v", err)
	}
	if stub.calls != 0 {
		t.Fatalf("the model was asked %d times; the batch never fit", stub.calls)
	}
}

// mixedProposalModel answers with a board that holds one entry of the
// learner's and one proposal of the assistant's that nobody took on.
type mixedProposalModel struct {
	calls int
}

func (m *mixedProposalModel) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.calls++
	payload := ""
	if len(input) > 1 && input[1] != nil {
		payload = input[1].Content
	}
	user, assistant := firstMessageIDs(payload)
	content := fmt.Sprintf(`{"schema_version":1,"segment":{"topic":"长对话","points":[`+
		`{"text":"学习者提出问题","evidence_message_ids":[%d],"source":"user"}]},`+
		`"whiteboard":{"schema_version":1,"constraints":[`+
		`{"key":"learner","text":"学习者自己的约束","source_message_ids":[%d]},`+
		`{"key":"proposal","text":"助手提议的安排","source_message_ids":[%d],"origin":"assistant"}]}}`,
		user, user, assistant)
	return schema.AssistantMessage(content, nil), nil
}

func (m *mixedProposalModel) Stream(context.Context, []*schema.Message,
	...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	reader, writer := schema.Pipe[*schema.Message](1)
	writer.Send(schema.AssistantMessage("", nil), nil)
	writer.Close()
	return reader, nil
}

func (m *mixedProposalModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}
