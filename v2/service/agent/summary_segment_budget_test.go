package agent

import (
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// The layered summary's budgets are all stated in one unit.
//
// The segment and the board are stored as JSON and sent as rendered blocks,
// and the rendered block is a fraction of the stored body. A budget that is
// stated in one unit and spent in another drops what it should have kept: the
// two numbers below used to be read as "how big is the body" on the write side
// and "how big is the block" on the read side, and the write side refused
// segments and boards the window would have carried.
//
// The equality is the guard. A guard is worth having because the numbers live
// in three packages: the model's own limits, the context profile's block
// budgets, and the preselector's and browse tool's per-segment budgets. They
// only work if they are the same number measured the same way.
func TestTheLayeredBudgetsAreAllStatedOnTheRenderedBlock(t *testing.T) {
	if model.SessionWhiteboardTokenLimit != DefaultWhiteboardTokens {
		t.Fatalf("the board's write budget is %d and its send budget is %d",
			model.SessionWhiteboardTokenLimit, DefaultWhiteboardTokens)
	}
	// One segment carries the same number whether it is being written, rendered
	// in a block, or preselected. The preselector no longer has a budget of its
	// own: it derives from the block that renders it, so the equality to check
	// is that the derivation starts from the same per-segment number.
	segments, perSegment, total := SummarySegmentPreselectLimits(DefaultRetrievedSegmentTokens, DefaultSegmentTokens)
	if perSegment != DefaultSegmentTokens {
		t.Fatalf("one segment may carry %d tokens in a block and %d when preselected",
			DefaultSegmentTokens, perSegment)
	}
	// The block's header is part of the block's budget, and the renderer
	// charges it before it looks at a segment. The preselection therefore
	// spends what is left of the budget, not the budget itself: a preselector
	// that spent the whole budget would pick a segment the renderer then
	// drops without a word.
	if total+SummarySegmentBlockHeaderTokens() != DefaultRetrievedSegmentTokens {
		t.Fatalf("the preselection may spend %d on a block that carries %d (header %d)",
			total, DefaultRetrievedSegmentTokens, SummarySegmentBlockHeaderTokens())
	}
	if segments < 1 || segments*perSegment > total {
		t.Fatalf("the derivation spends %d x %d on a block of %d", segments, perSegment, total)
	}
}

// A stored segment's own estimate is what the block charges for it, so the
// number in the table and the number the window spends are the same number.
func TestTheStoredSegmentEstimateIsWhatTheBlockCharges(t *testing.T) {
	points := make([]model.SessionSummarySegmentPoint, 0, 12)
	for index := 0; index < 12; index++ {
		points = append(points, model.SessionSummarySegmentPoint{
			Text: "梧苑每次读十五页，后来改成三十页", EvidenceMessageIDs: []uint{21, 22, 23},
		})
	}
	segment := testSegment(t, 1, 1, 30, "梧苑读书计划的页数", points, nil)
	segment.TokenEstimate = RenderedSegmentTokens(segment)
	if segment.TokenEstimate > DefaultSegmentTokens+segmentSeparatorTokens {
		t.Fatalf("the estimate is %d, over the block's own budget", segment.TokenEstimate)
	}
	if stored := rag.EstimateTokens(segment.Content); segment.TokenEstimate >= stored {
		t.Fatalf("the estimate %d still looks like the stored body %d", segment.TokenEstimate, stored)
	}
}
