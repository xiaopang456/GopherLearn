package agent

// What the assistant said has to stay findable.
//
// The assistant's body is not in the window: in the mode that records the
// assistant's products it is only ever read by the model that writes the
// summary. The one place the assistant's words live afterwards is the summary
// segment, as a product labelled as the assistant's.
//
// So there are three things that have to hold at once:
//
//   - the product stays in the segment, so the learner who asks about it later
//     gets an answer instead of being told that nothing was ever said;
//   - the product is labelled as the assistant's, so it is never read back as
//     something the learner decided;
//   - the message the product points at stays readable, because a product the
//     reader cannot open is a pointer to nothing.
//
// The tests here are about those three, and about a segment that holds two
// versions of the same thing: both have to be there, and the reader has to be
// able to tell which came first.

import (
	"context"
	"strings"
	"testing"

	"GopherAI/model"
)

// The assistant gave the learner a list; the learner never took it on; the
// learner asks about it later.
//
// The answer has to be findable and it has to say whose words it is.
func TestTheAssistantsListIsStillFindableAndStillTheAssistants(t *testing.T) {
	generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"练习","points":[`+
		`{"text":"学习者要三道练习","evidence_message_ids":[30],"source":"user"},`+
		`{"text":"三道：斐波那契、动态规划、图搜索","evidence_message_ids":[31],"source":"assistant"}]}}`)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	var product *model.SessionSummarySegmentPoint
	for index := range result.Segment.Points {
		if result.Segment.Points[index].PointSource() == model.SessionSummarySegmentSourceAssistant {
			product = &result.Segment.Points[index]
		}
	}
	if product == nil {
		t.Fatalf("the assistant's list is not in the segment: %+v", result.Segment.Points)
	}
	if !strings.Contains(product.Text, "斐波那契") {
		t.Fatalf("the list itself was lost: %q", product.Text)
	}
	if len(product.EvidenceMessageIDs) != 1 || product.EvidenceMessageIDs[0] != 31 {
		t.Fatalf("the list does not point at the assistant's message: %v", product.EvidenceMessageIDs)
	}
	// It is the assistant's list, not the learner's.
	if len(result.Whiteboard.Constraints) != 0 {
		t.Fatalf("the list is on the board as the learner's: %+v", result.Whiteboard.Constraints)
	}
}

// The label that says the learner did not take it on is not a reason to refuse
// the question.
//
// The marker tells the reader whose words these are; it must not take the
// words away. The content stays, the marker stays, and the message behind it
// stays readable.
func TestTheUncertaintyLabelDoesNotTakeTheContentAway(t *testing.T) {
	content := model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2,
		Topic:         "练习",
		Points: []model.SessionSummarySegmentPoint{
			{Text: "三道：斐波那契、动态规划、图搜索", EvidenceMessageIDs: []uint{31},
				Source: model.SessionSummarySegmentSourceAssistant},
		},
	}
	raw, err := model.MarshalSessionSummarySegmentContent(content)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	segment := model.SessionSummarySegment{Seq: 1, StartMessageID: 30, EndMessageID: 33, Content: raw}
	block, evidence, _ := renderSummarySegment(segment, model.SessionWhiteboardDocument{}, 33, DefaultSegmentTokens)
	if !strings.Contains(block, "斐波那契") {
		t.Fatalf("the content is gone: %q", block)
	}
	if !strings.Contains(block, "（助手提议，用户未表示同意）") {
		t.Fatalf("the content is not marked as the assistant's: %q", block)
	}
	found := false
	for _, id := range evidence {
		if id == 31 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the assistant's message is not readable: %v", evidence)
	}
}

// The assistant gave two versions of the same thing.
//
// Asking about the first one and asking about the last one are two different
// questions, and both of them have to have an answer. The first one is not
// replaced by the second one: it is marked as the one that was changed later,
// and it keeps its own message.
func TestBothAssistantVersionsAreThereAndCanBeToldApart(t *testing.T) {
	content := model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2,
		Topic:         "练习",
		Points: []model.SessionSummarySegmentPoint{
			{Text: "最初给的三道：斐波那契、背包", EvidenceMessageIDs: []uint{31},
				Source: model.SessionSummarySegmentSourceAssistant},
			{Text: "后来给的三道：动态规划、图搜索、贪心", EvidenceMessageIDs: []uint{34},
				Source: model.SessionSummarySegmentSourceAssistant},
		},
	}
	raw, err := model.MarshalSessionSummarySegmentContent(content)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	segment := model.SessionSummarySegment{Seq: 1, StartMessageID: 30, EndMessageID: 34, Content: raw}
	block, evidence, _ := renderSummarySegment(segment, model.SessionWhiteboardDocument{}, 34, DefaultSegmentTokens)
	if !strings.Contains(block, "最初给的三道") {
		t.Fatalf("the first version is gone: %q", block)
	}
	if !strings.Contains(block, "后来给的三道") {
		t.Fatalf("the last version is gone: %q", block)
	}
	if !strings.Contains(block, "[message_ids=31]") || !strings.Contains(block, "[message_ids=34]") {
		t.Fatalf("the two versions cannot be told apart: %q", block)
	}
	readable := map[uint]bool{}
	for _, id := range evidence {
		readable[id] = true
	}
	if !readable[31] || !readable[34] {
		t.Fatalf("one of the two versions is not readable: %v", evidence)
	}
}

// The learner took the second version on.
//
// The board holds the second version now, and the first one is still in the
// segment - marked as the one that was changed later, so a reader does not
// quote it as the current one, but still there to be found.
func TestTheVersionThatWasChangedLaterIsMarkedAndNotLost(t *testing.T) {
	content := model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2,
		Topic:         "练习",
		Points: []model.SessionSummarySegmentPoint{
			{Text: "最初给的三道：斐波那契、背包", EvidenceMessageIDs: []uint{31},
				Source: model.SessionSummarySegmentSourceAssistant},
		},
	}
	raw, err := model.MarshalSessionSummarySegmentContent(content)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "plan", Text: "按后三道来", Value: "动态规划、图搜索、贪心",
			Origin: model.SessionWhiteboardOriginAssistantAccepted, SourceMessageIDs: []uint{34},
		}},
	}
	segment := model.SessionSummarySegment{
		Seq: 1, StartMessageID: 30, EndMessageID: 34, Content: raw,
		WhiteboardChanges: `[{"key":"plan","old_value":"","new_value":"斐波那契、背包",` +
			`"new_message_ids":[31]}]`,
	}
	block, _, _ := renderSummarySegment(segment, document, 34, DefaultSegmentTokens)
	if !strings.Contains(block, "最初给的三道") {
		t.Fatalf("the first version is gone: %q", block)
	}
	if !strings.Contains(block, "已改为") {
		t.Fatalf("the first version is not marked as the one that was changed later: %q", block)
	}
}

// A segment that does not fit gives up the product nobody took on - unless
// that product is the only thing pointing at the message it cites.
//
// Giving it up then takes the assistant's words and the right to read them
// back, and the learner who asks about them later is told that nothing was
// ever said.
func TestTheProductThatPointsAtAMessageAloneSurvivesATightSegment(t *testing.T) {
	generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"练习","points":[`+
		`{"text":"学习者要三道练习","evidence_message_ids":[30],"source":"user"},`+
		`{"text":"三道：斐波那契、动态规划、图搜索","evidence_message_ids":[31],"source":"assistant"}]}}`)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	kept := false
	for _, point := range result.Segment.Points {
		if point.PointSource() == model.SessionSummarySegmentSourceAssistant {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("the assistant's product is not in the segment: %+v", result.Segment.Points)
	}
	// The mode that does not record the assistant's products is untouched: the
	// assistant's words are not in its segments at all.
	off, _ := NewModelSegmentGenerator(&stubSegmentModel{content: `{"schema_version":1,"segment":{"topic":"练习","points":[` +
		`{"text":"学习者要三道练习","evidence_message_ids":[30],"source":"user"}]}}`}, 8000, 2000)
	offResult, err := off.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	for _, point := range offResult.Segment.Points {
		if point.PointSource() != model.SessionSummarySegmentSourceUser {
			t.Fatalf("the mode that is off carries an assistant's product: %+v", offResult.Segment.Points)
		}
	}
}
