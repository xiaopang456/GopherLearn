package agent

// What a summary segment gives up when the window has no room for it.
//
// A point is two things at once: what was said, and the name of the message
// it was said in. The name is what lets the reader read that message back,
// and the reader can only read what the segment names.
//
// So the segment never gives up a name and never gives up half a fact. A
// point that does not fit is left out whole, and the block names the
// messages the left-out points came from.
//
// The tests here are about the three things that must not happen: two points
// that say the same thing must not be merged, two points about one message
// must not be taken for one fact, and the words of a point must not be cut.
// The last one is a negation: "不要改成 41 页" cut short says something else,
// and what it says is often the opposite of what was said.

import (
	"strings"
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// budgetSegment is a segment with the points it is given.
func budgetSegment(points ...model.SessionSummarySegmentPoint) model.SessionSummarySegmentContent {
	return model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2,
		Topic:         "练习",
		Points:        points,
	}
}

// renderTestSegment renders one segment under a per-segment budget.
func renderTestSegment(content model.SessionSummarySegmentContent,
	startMessageID, endMessageID uint, perSegmentTokens int) renderedSummarySegments {
	raw, err := model.MarshalSessionSummarySegmentContent(content)
	if err != nil {
		return renderedSummarySegments{}
	}
	segment := model.SessionSummarySegment{
		Seq: 1, StartMessageID: startMessageID, EndMessageID: endMessageID, Content: raw,
	}
	return renderSummarySegments([]model.SessionSummarySegment{segment}, model.SessionWhiteboardDocument{},
		4000, perSegmentTokens, true)
}

// segmentBudgetFor is the per-segment budget that holds the header and the
// first points and not the one after them.
//
// The budget is measured on the rendered block, so it is worked out from the
// block itself rather than guessed.
func segmentBudgetFor(t *testing.T, content model.SessionSummarySegmentContent,
	startMessageID, endMessageID uint, keep int) int {
	t.Helper()
	header := content
	header.Points = nil
	raw, err := model.MarshalSessionSummarySegmentContent(header)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	segment := model.SessionSummarySegment{
		Seq: 1, StartMessageID: startMessageID, EndMessageID: endMessageID, Content: raw,
	}
	budget := rag.EstimateTokens(renderSummarySegment(segment, model.SessionWhiteboardDocument{},
		endMessageID, 0).Text)
	for index, point := range content.Points {
		if index >= keep {
			break
		}
		budget += rag.EstimateTokens(renderSegmentPointLine(point, "")) + 1
	}
	return budget
}

// The same words from two sides are two points.
//
// The learner's own point and the assistant's product can say the same
// thing, and so can the product nobody took on and the one the learner took
// on. Merging them would throw away who said it and whether the learner
// agreed, and the reader is told apart by exactly those two things.
func TestTheSameWordsFromTwoSourcesAreNotMerged(t *testing.T) {
	content := budgetSegment(
		model.SessionSummarySegmentPoint{
			Text: "改成每次 41 页", EvidenceMessageIDs: []uint{30},
		},
		model.SessionSummarySegmentPoint{
			Text: "改成每次 41 页", EvidenceMessageIDs: []uint{31},
			Source: model.SessionSummarySegmentSourceAssistant,
		},
	)
	rendered := renderTestSegment(content, 30, 31, 400)
	if rendered.OmittedPoints != 0 {
		t.Fatalf("omitted points = %d, want 0: %q", rendered.OmittedPoints, rendered.Text)
	}
	if strings.Count(rendered.Text, "改成每次 41 页") != 2 {
		t.Fatalf("the two points were merged: %q", rendered.Text)
	}
	if !strings.Contains(rendered.Text, "（助手提议，用户未表示同意）改成每次 41 页") {
		t.Fatalf("the reader cannot tell whose words these are: %q", rendered.Text)
	}
}

// The same words from two versions are two points.
//
// The old value and the new one are told apart by when they were said, and
// a summary that keeps only one of them says the wrong one is current.
func TestTheSameWordsFromTwoVersionsAreNotMerged(t *testing.T) {
	content := budgetSegment(
		model.SessionSummarySegmentPoint{
			Text: "每次 41 页", EvidenceMessageIDs: []uint{30},
		},
		model.SessionSummarySegmentPoint{
			Text: "每次 41 页", EvidenceMessageIDs: []uint{31},
		},
	)
	rendered := renderTestSegment(content, 30, 31, 400)
	if strings.Count(rendered.Text, "每次 41 页") != 2 {
		t.Fatalf("the two versions were merged: %q", rendered.Text)
	}
}

// One message can hold two facts, and they are two points.
//
// The points are not duplicates: they name the same message and they say
// different things. Treating them as one would lose one of the facts, and
// the fact that both name the same message proves nothing about that.
func TestTwoFactsInOneMessageStayTwoFacts(t *testing.T) {
	content := budgetSegment(
		model.SessionSummarySegmentPoint{
			Text: "甲书改成每次 41 页", EvidenceMessageIDs: []uint{30},
		},
		model.SessionSummarySegmentPoint{
			Text: "乙书改成每次 30 页", EvidenceMessageIDs: []uint{30},
		},
	)
	rendered := renderTestSegment(content, 30, 30, 400)
	if rendered.OmittedPoints != 0 {
		t.Fatalf("omitted points = %d, want 0: %q", rendered.OmittedPoints, rendered.Text)
	}
	if !strings.Contains(rendered.Text, "甲书改成每次 41 页") ||
		!strings.Contains(rendered.Text, "乙书改成每次 30 页") {
		t.Fatalf("a fact was lost: %q", rendered.Text)
	}
}

// A fact that did not fit is not in the window, and the block says so.
//
// The message the fact came from is still named, and the reader may open
// it. That is the way back to the fact - and it is only the way back: the
// fact itself is not in the window, and the point that is left proves that
// the message is readable, not that the fact is still there.
func TestAFactThatDidNotFitIsNamedAndNotThere(t *testing.T) {
	content := budgetSegment(
		model.SessionSummarySegmentPoint{
			Text: "甲书改成每次 41 页", EvidenceMessageIDs: []uint{30},
		},
		model.SessionSummarySegmentPoint{
			Text: "乙书改成每次 30 页", EvidenceMessageIDs: []uint{30},
		},
	)
	rendered := renderTestSegment(content, 30, 30, segmentBudgetFor(t, content, 30, 30, 1))
	if rendered.OmittedPoints != 1 {
		t.Fatalf("omitted points = %d, want 1: %q", rendered.OmittedPoints, rendered.Text)
	}
	// One of the two facts is gone, and the other one is still there.
	if strings.Contains(rendered.Text, "甲书改成每次 41 页") {
		t.Fatalf("the fact that did not fit is in the window: %q", rendered.Text)
	}
	if !strings.Contains(rendered.Text, "乙书改成每次 30 页") {
		t.Fatalf("the fact that fitted is not in the window: %q", rendered.Text)
	}
	// The message is still named: the entrance is there even though the
	// fact is not.
	if !strings.Contains(rendered.Text, "[message_ids=30]") {
		t.Fatalf("the block does not name the message: %q", rendered.Text)
	}
	found := false
	for _, id := range rendered.OmittedMessageIDs {
		if id == 30 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the message the fact came from was not counted: %v", rendered.OmittedMessageIDs)
	}
}

// The words of a point are never cut.
//
// A cut point keeps its name and loses its meaning, and the meaning is
// usually the part that says what the learner ruled out. A point that does
// not fit is left out whole.
func TestTheWordsOfAPointAreNotCut(t *testing.T) {
	content := budgetSegment(
		model.SessionSummarySegmentPoint{
			Text: "不要改成 41 页", EvidenceMessageIDs: []uint{30},
		},
		model.SessionSummarySegmentPoint{
			Text: strings.Repeat("很长的说明", 60), EvidenceMessageIDs: []uint{31},
		},
	)
	rendered := renderTestSegment(content, 30, 31, segmentBudgetFor(t, content, 30, 31, 1))
	if strings.Contains(rendered.Text, "不要改成") && !strings.Contains(rendered.Text, "不要改成 41 页") {
		t.Fatalf("the point was cut: %q", rendered.Text)
	}
	if strings.Contains(rendered.Text, "…") {
		t.Fatalf("a point was cut short: %q", rendered.Text)
	}
}

// What did not fit is counted, and the count is what says how much of the
// summary the window did not carry.
func TestTheOmittedPointsAreCounted(t *testing.T) {
	content := budgetSegment(
		model.SessionSummarySegmentPoint{
			Text: "甲书改成每次 41 页", EvidenceMessageIDs: []uint{30},
		},
		model.SessionSummarySegmentPoint{
			Text: strings.Repeat("很长的说明", 60), EvidenceMessageIDs: []uint{31},
		},
	)
	rendered := renderTestSegment(content, 30, 31, segmentBudgetFor(t, content, 30, 31, 1))
	if rendered.OmittedPoints != 1 {
		t.Fatalf("omitted points = %d, want 1: %q", rendered.OmittedPoints, rendered.Text)
	}
	if len(rendered.OmittedMessageIDs) != 1 || rendered.OmittedMessageIDs[0] != 31 {
		t.Fatalf("omitted messages = %v, want [31]", rendered.OmittedMessageIDs)
	}
}
