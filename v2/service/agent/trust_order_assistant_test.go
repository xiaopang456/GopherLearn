package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
)

// The window that carries the assistant's products says so.
//
// The rule is a line in the system prompt, and it is written only when the
// window can carry a line marked as the assistant's proposal: a window
// without them has nothing the rule would apply to.
//
// The rule is also a prompt the surfaces that recognise prompts have to know.
// A harness that did not know this form would refuse the product's own
// request in the middle of a measurement, and the turn would end before the
// answer existed.

// trustOrderWindow builds a window whose segments are in it and returns the
// system prompt the model is called with.
func trustOrderWindow(t *testing.T, assistantContent bool) string {
	t.Helper()
	reader := &fakeSegmentReader{
		board: testBoard(t, 60, []model.SessionWhiteboardEntry{
			{Key: "duration", Text: "每次总时长 = 48 分钟", Value: "48 分钟", SourceMessageIDs: []uint{45}},
		}),
		watermark: 90,
		segments: []model.SessionSummarySegment{
			trustOrderSegment(t),
		},
	}
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 91, IsUser: true, Content: "现在改成 48 分钟"},
	}}
	config := layeredTestConfig()
	config.AssistantContent = assistantContent
	builder, err := NewContextBuilder(config, history, staticTaskState(""), nil,
		staticMemories{}, EmptySummaryProvider{})
	if err != nil {
		t.Fatalf("build context builder: %v", err)
	}
	provider, err := NewDatabaseLayeredSummaryProvider(reader, nil, 2)
	if err != nil {
		t.Fatalf("build layered provider: %v", err)
	}
	if err := builder.UseLayeredSummaryProvider(provider); err != nil {
		t.Fatalf("attach layered provider: %v", err)
	}
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 7, SessionID: "session", Question: "现在每次学习多久",
	})
	if err != nil {
		t.Fatalf("build window: %v", err)
	}
	return window.Messages[0].Content
}

// trustOrderSegment builds one stored segment that carries an assistant's
// product, the way the mode that records them writes it.
func trustOrderSegment(t *testing.T) model.SessionSummarySegment {
	t.Helper()
	content, err := model.MarshalSessionSummarySegmentContent(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2, Topic: "学习时长调整",
		Points: []model.SessionSummarySegmentPoint{
			{Text: "每次学习总时长定为 30 分钟", EvidenceMessageIDs: []uint{12}},
			{Text: "助手给出三道练习", EvidenceMessageIDs: []uint{13},
				Source: model.SessionSummarySegmentSourceAssistant},
		},
	})
	if err != nil {
		t.Fatalf("encode segment content: %v", err)
	}
	encoded, err := model.MarshalSessionSummarySegmentChanges(nil)
	if err != nil {
		t.Fatalf("encode segment changes: %v", err)
	}
	return model.SessionSummarySegment{
		UserID: 7, SessionID: "session", Seq: 1, StartMessageID: 1, EndMessageID: 30,
		StartAt: time.Unix(1790000000, 0).UTC(), EndAt: time.Unix(1790003600, 0).UTC(),
		Topic: "学习时长调整", Content: content, WhiteboardChanges: encoded, TokenEstimate: 120,
	}
}

// The rule is in the window when the mode records the assistant's products.
func TestTheWindowCarriesTheRuleAboutTheAssistantsProposals(t *testing.T) {
	prompt := trustOrderWindow(t, true)
	if !strings.Contains(prompt, "A line marked as the assistant's proposal") {
		t.Fatalf("the window does not say what an assistant's proposal is: %q", prompt)
	}
	if !strings.Contains(prompt, "not what the learner decided") {
		t.Fatalf("the window does not say that a proposal is not a decision: %q", prompt)
	}
}

// The rule is not in the window when the mode does not record them.
//
// A rule about a line the window cannot carry is a rule the model cannot
// obey, and the mode that is off must send the prompt it always sent.
func TestTheWindowWithoutAssistantProductsCarriesNoSuchRule(t *testing.T) {
	prompt := trustOrderWindow(t, false)
	if strings.Contains(prompt, "A line marked as the assistant's proposal") {
		t.Fatalf("the window carries the rule without the products: %q", prompt)
	}
}

// The list of prompts the surfaces recognise holds every form the product can
// write, including the one that carries the rule about the assistant's
// proposals.
//
// The list is what a harness classifies a model call by. A form that is
// missing from it is a request the harness refuses, and the refusal happens
// in the middle of a measurement.
func TestTheListOfTrustOrdersHoldsTheFormWithTheAssistantsProposals(t *testing.T) {
	var found bool
	for _, prompt := range TrustOrderPrompts() {
		if strings.Contains(prompt, "A line marked as the assistant's proposal") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("the list of trust orders does not hold the form that carries the assistant's rule")
	}
}
