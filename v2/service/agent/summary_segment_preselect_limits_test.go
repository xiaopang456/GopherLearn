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

// The resident set is sized by its own block, with the preselector's rule.
//
// It used to be a constant of two, and a constant is a second answer to a
// question the budget already answers: the scaled configuration gives the
// resident block 310 tokens, which holds one 300-token segment, so the fixed
// two asked the renderer for a segment it could never send. Both blocks now
// divide by the same segment size.
//
// The block's own header comes out of its budget first, so a block that
// carries four 300-token segments needs room for the header as well. The
// shipped 1200-token block therefore holds three, not four.
func TestTheResidentSetIsSizedByItsOwnBlock(t *testing.T) {
	header := SummarySegmentBlockHeaderTokens()
	for _, c := range []struct {
		name                     string
		resident, segment, count int
	}{
		{"shipped", DefaultResidentSegmentTokens, DefaultSegmentTokens, 3},
		{"scaled", 310, 300, 1},
		{"below one segment", 120, 300, 1},
		{"no resident budget", 0, 300, 1},
	} {
		if got := ResidentSegmentCount(c.resident, c.segment); got != c.count {
			t.Fatalf("%s: a %d-token block of %d-token segments holds %d residents, want %d",
				c.name, c.resident, c.segment, got, c.count)
		}
	}
	// The floor is a floor, not a clamp that hides a configuration mistake the
	// other way round: a block wide enough for five asks for five.
	if ResidentSegmentCount(1600, 300) != 5 {
		t.Fatal("a block of five segments asked for fewer")
	}
	// The count the rule derives is a count that fits: the segments it asks
	// for and the block's own header together stay inside the block.
	residents := ResidentSegmentCount(DefaultResidentSegmentTokens, DefaultSegmentTokens)
	if residents*DefaultSegmentTokens+header > DefaultResidentSegmentTokens {
		t.Fatalf("the shipped block asks for %d segments of %d tokens, which does not fit together with the %d-token header",
			residents, DefaultSegmentTokens, header)
	}
}

// A reader built without a resident count is refused rather than defaulted.
//
// The default is what made the constant load-bearing: every caller could omit
// it and still get a number that had nothing to do with its configuration.
func TestTheReaderRefusesToInventAResidentCount(t *testing.T) {
	if _, err := NewDatabaseLayeredSummaryProvider(&fakeSegmentReader{}, nil, 0); err == nil {
		t.Fatal("a reader with no resident count was built anyway")
	}
}
