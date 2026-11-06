package agent

import (
	"context"
	"testing"

	"GopherAI/model"
)

// The limit on how many segments may be selected is not a promise that many
// arrive.
//
// The count is a ceiling; the total budget is the gate. A single segment that
// costs more than the whole allowance is skipped, and the selection is empty -
// which is the right answer for a block that cannot hold it, and the reason a
// test on the count alone proves nothing about what a turn actually carries.
func TestThePreselectionBudgetCanRejectEveryCandidate(t *testing.T) {
	preselector, err := NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	segment := testSegment(t, 1, 1, 2, "每天的读量",
		[]model.SessionSummarySegmentPoint{{Text: "每天读 15 页", EvidenceMessageIDs: []uint{1}}}, nil)
	input := RunContext{UserID: 1, SessionID: "s1", Question: "我每天读多少页"}

	// Room for one segment, and a total that this segment alone exceeds.
	preselector.UseLimits(1, 300, 40)
	selected, err := preselector.Select(context.Background(), input,
		[]model.SessionSummarySegment{segment}, nil)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(selected) != 0 {
		t.Fatalf("a segment costing more than the whole allowance was selected (%d selected)", len(selected))
	}

	// The same candidate, with an allowance that fits it: now it is selected,
	// so the empty result above is the budget and not a missing lexical match.
	preselector.UseLimits(1, 300, 300)
	selected, err = preselector.Select(context.Background(), input,
		[]model.SessionSummarySegment{segment}, nil)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(selected) != 1 {
		t.Fatalf("the segment was not selected with room for it (%d selected)", len(selected))
	}
}
