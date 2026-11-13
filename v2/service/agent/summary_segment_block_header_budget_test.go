package agent

import (
	"fmt"
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// A block budget has to hold the header as well as the segment.
//
// The renderer charges the block header first and then walks the segments
// from the newest backwards, skipping any that does not fit. A budget that
// holds exactly one segment's worth therefore holds nothing at all: the header
// has already spent part of it, and the segment is skipped as too large.
//
// This is the second appearance of the same mistake - a cap stated in one
// unit and measured in another. The cap is in tokens, the segment is in
// tokens, and the header is in tokens too; what went wrong was that the
// arithmetic forgot one of the three.
//
// The numbers here are the ones the scaled configuration runs on, so a change
// to either side of the equation shows up as a failure rather than as an empty
// block in a report.
func TestABlockBudgetThatHoldsOneSegmentHoldsTheHeaderToo(t *testing.T) {
	header := rag.EstimateTokens(summarySegmentsBlockHeader) + 4
	// A segment at the per-segment cap: many points, so the renderer keeps
	// them until the 300-token line is full. A small segment would fit in
	// almost any budget and would prove nothing.
	points := make([]model.SessionSummarySegmentPoint, 0, model.SessionSummarySegmentPointLimit)
	for index := 0; index < model.SessionSummarySegmentPointLimit; index++ {
		points = append(points, model.SessionSummarySegmentPoint{
			Text:               fmt.Sprintf("第 %d 条决定：每天的读量按 %d 页执行，并在当前状态里记下这次改动的来源与时间", index, 15+index),
			EvidenceMessageIDs: []uint{uint(index + 1)},
		})
	}
	segment := model.SessionSummarySegment{
		Seq: 1, StartMessageID: 1, EndMessageID: 30,
		Topic: "每天的读量",
		Content: mustSegmentContent(t, model.SessionSummarySegmentContent{
			SchemaVersion: model.SessionSummarySegmentSchemaV1,
			Topic:         "每天的读量",
			Points:        points,
		}),
	}
	document := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}

	if header <= 0 {
		t.Fatal("the block header costs nothing, so the arithmetic below is meaningless")
	}
	// The segment's own cost, measured the way the renderer measures it.
	cost := rag.EstimateTokens(renderSummarySegmentForTest(segment, document)) + 4
	t.Logf("block header=%d segment=%d together=%d", header, cost, header+cost)

	// The budget that holds the segment and the header together.
	needed := header + cost
	rendered := renderSummarySegments([]model.SessionSummarySegment{segment}, document,
		needed, DefaultSegmentTokens, false)
	if len(rendered.Segments) != 1 {
		t.Fatalf("a budget of %d tokens held %d segments; the header costs %d and the segment %d",
			needed, len(rendered.Segments), header, cost)
	}

	// And the budget that does not hold the header must not carry the
	// segment whole.
	//
	// The two modes handle it differently, and the difference is the point:
	// the mode that does not record the assistant's products keeps the path
	// it always had and skips the segment, and the mode that does record
	// them trims it to the room the header left - a block that showed it
	// whole would have spent more than it was given, and a segment that can
	// be trimmed is not a hole in what the model can be told.
	without := renderSummarySegments([]model.SessionSummarySegment{segment}, document,
		cost, DefaultSegmentTokens, false)
	if len(without.Segments) != 0 {
		t.Fatalf("a budget of %d tokens carried %d segments; the header is %d and it does not fit",
			cost, len(without.Segments), header)
	}
	if without.DroppedSegments != 1 {
		t.Fatalf("dropped segments = %d, want 1: the mode that is off skips the segment", without.DroppedSegments)
	}
	trimmed := renderSummarySegments([]model.SessionSummarySegment{segment}, document,
		cost, DefaultSegmentTokens, true)
	if len(trimmed.Segments) != 1 {
		t.Fatalf("a budget of %d tokens held %d segments; the header is %d and it does not fit",
			cost, len(trimmed.Segments), header)
	}
	if !trimmed.Segments[0].Trimmed {
		t.Fatal("the segment was carried whole by a budget that could not hold it")
	}
	if len(trimmed.Segments[0].Text) >= len(renderSummarySegmentForTest(segment, document)) {
		t.Fatal("the trimmed segment is not shorter than the whole one")
	}
	if trimmed.DroppedSegments != 0 {
		t.Fatalf("dropped segments = %d, want 0: a segment that can be trimmed is not dropped",
			trimmed.DroppedSegments)
	}

	// The scaled configuration's numbers have to clear the bar: a 370-token
	// block must hold one segment at the per-segment cap, header included.
	// 370 is the number the run configuration uses, and it is the smallest
	// round number above 59 + 304.
	full := renderSummarySegments([]model.SessionSummarySegment{segment}, document,
		370, DefaultSegmentTokens, false)
	if len(full.Segments) != 1 {
		t.Fatalf("a 370-token block held %d segments; one full segment needs %d (header %d + segment %d)",
			len(full.Segments), needed, header, cost)
	}
	if needed > 370 {
		t.Fatalf("a full segment plus the header costs %d, over the 370-token block", needed)
	}
}

// renderSummarySegmentForTest renders one segment the way the block does.
func renderSummarySegmentForTest(segment model.SessionSummarySegment,
	document model.SessionWhiteboardDocument) string {
	block := renderSummarySegmentBlock(segment, document, segment.EndMessageID, DefaultSegmentTokens)
	return block.Text
}

// mustSegmentContent encodes a segment body for a test.
func mustSegmentContent(t *testing.T, content model.SessionSummarySegmentContent) string {
	t.Helper()
	encoded, err := model.MarshalSessionSummarySegmentContent(content)
	if err != nil {
		t.Fatalf("encode the segment: %v", err)
	}
	return encoded
}
