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
func TestTheUnacceptedProductGoesBeforeTheLearnersPoint(t *testing.T) {
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
	full := renderSummarySegment(segment, model.SessionWhiteboardDocument{}, 31, 0)
	headerCost := rag.EstimateTokens(full.Text) - learnerCost - productCost

	// The budget holds the header and the learner's own point, and not the
	// product.
	budget := headerCost + learnerCost + 1
	rendered := renderSummarySegment(segment, model.SessionWhiteboardDocument{}, 31, budget)
	if !strings.Contains(rendered.Text, learner.Text) {
		t.Fatalf("the learner's own point was given up: %q", rendered.Text)
	}
	if strings.Contains(rendered.Text, product.Text) {
		t.Fatalf("the product nobody took on was kept: %q", rendered.Text)
	}
	if !rendered.Trimmed {
		t.Fatal("the segment gave up a point and does not say so")
	}
	for _, id := range rendered.EvidenceMessageIDs {
		if id == 31 {
			t.Fatalf("the given-up product still authorises a read: %v", rendered.EvidenceMessageIDs)
		}
	}
}
