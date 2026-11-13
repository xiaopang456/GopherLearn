package agent

import (
	"strings"
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// The points of a segment are measured together.
//
// Two points that each fit a budget do not fit it together: the space one of
// them takes is the space the other one does not have. Measuring them one at
// a time would say that both fit, and the segment would carry more than it
// was given.
func TestThePointsAreMeasuredTogether(t *testing.T) {
	points := []model.SessionSummarySegmentPoint{
		{Text: "学习者说过的话", EvidenceMessageIDs: []uint{30}},
		{Text: "助手给出的三道练习", EvidenceMessageIDs: []uint{31},
			Source: model.SessionSummarySegmentSourceAssistant},
	}
	marks := []string{"", ""}
	first := rag.EstimateTokens(renderSegmentPointLine(points[0], "")) + 1
	second := rag.EstimateTokens(renderSegmentPointLine(points[1], "")) + 1
	room := first
	if second > room {
		room = second
	}

	// Each of them fits a budget that holds it.
	if !segmentPointsFit(points[:1], marks[:1], room, 0) {
		t.Fatalf("the first point does not fit a budget of %d tokens", room)
	}
	if !segmentPointsFit(points[1:], marks[1:], room, 0) {
		t.Fatalf("the second point does not fit a budget of %d tokens", room)
	}
	// Together they do not fit a budget that holds only one of them.
	if segmentPointsFit(points, marks, room, 0) {
		t.Fatalf("two points of %d and %d tokens fit a budget of %d; they were measured one at a time",
			first, second, room)
	}
	// They do fit a budget that holds both.
	if !segmentPointsFit(points, marks, first+second, 0) {
		t.Fatalf("two points of %d and %d tokens do not fit a budget of %d",
			first, second, first+second)
	}
}

// The product nobody took on is given up before the learner's own point.
//
// The learner took the words on, so they are the least the segment is worth
// keeping - but the space they take is not the space the learner's own point
// needs.
//
// A product that another point also names is the one that goes: the message
// stays named by the other point, so nothing stops being readable.
func TestTheUnacceptedProductGoesBeforeTheLearnersPoint(t *testing.T) {
	learner := model.SessionSummarySegmentPoint{
		Text: "学习者说过的话", EvidenceMessageIDs: []uint{30},
	}
	older := model.SessionSummarySegmentPoint{
		Text: "助手先给的方案", EvidenceMessageIDs: []uint{31},
		Source: model.SessionSummarySegmentSourceAssistant,
	}
	newer := model.SessionSummarySegmentPoint{
		Text: "助手后给的方案", EvidenceMessageIDs: []uint{31},
		Source: model.SessionSummarySegmentSourceAssistant,
	}
	content, err := model.MarshalSessionSummarySegmentContent(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2, Topic: "练习",
		Points: []model.SessionSummarySegmentPoint{learner, older, newer},
	})
	if err != nil {
		t.Fatalf("encode the segment content: %v", err)
	}
	segment := model.SessionSummarySegment{
		Seq: 1, StartMessageID: 30, EndMessageID: 31, Topic: "练习", Content: content,
	}
	learnerCost := rag.EstimateTokens(renderSegmentPointLine(learner, "")) + 1
	olderCost := rag.EstimateTokens(renderSegmentPointLine(older, "")) + 1
	newerCost := rag.EstimateTokens(renderSegmentPointLine(newer, "")) + 1
	full, _, _ := renderSummarySegment(segment, model.SessionWhiteboardDocument{}, 31, 0)
	headerCost := rag.EstimateTokens(full) - learnerCost - olderCost - newerCost

	// The budget holds the header, the learner's own point and one product.
	budget := headerCost + learnerCost + olderCost
	block, evidence, trimmed := renderSummarySegment(segment, model.SessionWhiteboardDocument{}, 31, budget)
	if !strings.Contains(block, learner.Text) {
		t.Fatalf("the learner's own point was given up: %q", block)
	}
	if strings.Contains(block, older.Text) {
		t.Fatalf("the older product nobody took on was kept: %q", block)
	}
	if !trimmed {
		t.Fatal("the segment gave up a point and does not say so")
	}
	found := false
	for _, id := range evidence {
		if id == 31 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the message the surviving product points at is not readable: %v", evidence)
	}
}

// A product that is the only thing pointing at a message is not given up.
//
// Giving it up takes the assistant's words and the right to read them back
// out of the window: the learner who asks about them later is told that
// nothing was ever said. The original is in the database, and the reader
// cannot open the database.
//
// What is cut instead is the words: the point stays, it keeps its name, and
// the reader can still open the message behind it.
func TestTheProductThatPointsAtAMessageAloneIsNotGivenUp(t *testing.T) {
	learner := model.SessionSummarySegmentPoint{
		Text: "学习者说过的话", EvidenceMessageIDs: []uint{30},
	}
	product := model.SessionSummarySegmentPoint{
		Text: "助手给出的三道练习", EvidenceMessageIDs: []uint{31},
		Source: model.SessionSummarySegmentSourceAssistant,
	}
	content, err := model.MarshalSessionSummarySegmentContent(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2, Topic: "练习",
		Points: []model.SessionSummarySegmentPoint{learner, product},
	})
	if err != nil {
		t.Fatalf("encode the segment content: %v", err)
	}
	segment := model.SessionSummarySegment{
		Seq: 1, StartMessageID: 30, EndMessageID: 31, Topic: "练习", Content: content,
	}
	learnerCost := rag.EstimateTokens(renderSegmentPointLine(learner, "")) + 1
	productCost := rag.EstimateTokens(renderSegmentPointLine(product, "")) + 1
	full, _, _ := renderSummarySegment(segment, model.SessionWhiteboardDocument{}, 31, 0)
	headerCost := rag.EstimateTokens(full) - learnerCost - productCost
	if headerCost <= 0 {
		t.Fatalf("the header costs %d tokens", headerCost)
	}

	// The budget holds the names of the two points and not their words.
	budget := namedBudget(t, []model.SessionSummarySegmentPoint{learner, product})
	block, evidence, trimmed := renderSummarySegment(segment, model.SessionWhiteboardDocument{}, 31, budget)
	if !trimmed {
		t.Fatal("the segment was cut and does not say so")
	}
	if !strings.Contains(block, "[message_ids=31]") {
		t.Fatalf("the product stopped naming its message: %q", block)
	}
	found := false
	for _, id := range evidence {
		if id == 31 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the assistant's message is not readable any more: %v", evidence)
	}
}
