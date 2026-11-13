package agent

// What a summary segment gives up when it does not fit its budget.
//
// A point is two things at once: what was said, and the name of the message
// it was said in. The name is what lets the reader read that message back,
// and the reader can only read what the segment names. The original is in
// the database, and a model cannot open the database.
//
// So the order in which a segment spends a budget it does not have is:
//
//  1. the words that say the same thing twice go first: two points that
//     carry the same text are one point written twice, and the one that is
//     left names every message both of them named;
//  2. then the words go: a point that does not fit is cut short rather than
//     removed, and a cut point keeps its names, its label and its mark;
//  3. and only then is a point given up, and only when every message it
//     names is named by a point that is kept.
//
// The third rule is the same for the learner's own points and for the
// assistant's products. A point that is the last name of a message is kept,
// whichever side wrote it: a rule that protected the assistant's products by
// giving up the learner's own points would move the same loss onto the
// other side, and the learner's point is as often the last name of a message
// as the assistant's product is.
//
// What is left when even that is not enough is the point that carries the
// least: the one that names the fewest messages, and among those the
// shortest text. That is the last thing the engine gives up, and it gives up
// the point whose loss costs the least.

import (
	"strings"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// segmentPointFloorRunes is how much of a point's text is kept when the
// point is cut short.
//
// The floor is small on purpose: a cut point is still a point, and it still
// names its messages. What it loses is the detail, and the detail is
// readable in the message behind it.
const segmentPointFloorRunes = 8

// segmentPointNameOnlyRunes is the smallest a point's text may become.
//
// A point that is cut down to this is almost a name: it says that something
// happened in that message and lets the reader open it. It is the form a
// point takes when the segment has room for the names and not for the
// words.
const segmentPointNameOnlyRunes = 4

// cutPointText shortens a text to a number of runes, keeping its beginning
// and its end.
//
// The end of a summary is often the value - "改成每次 41 页" - and the
// beginning is what it is about, so a cut that keeps only the beginning
// throws away the part that matters most. The end therefore gets the larger
// half of what is left. The cut is marked, because a reader has to be able
// to see that the text is short.
func cutPointText(text string, keep int) string {
	runes := []rune(text)
	if keep >= len(runes) {
		return text
	}
	if keep <= 1 {
		return string(runes[:1])
	}
	head := keep / 3
	tail := keep - head - 1
	if tail < 0 {
		tail = 0
	}
	if head+tail+1 >= len(runes) {
		return string(runes[:keep])
	}
	return string(runes[:head]) + "…" + string(runes[len(runes)-tail:])
}

// mergeSegmentDuplicates merges the points that say the same thing.
//
// Two points that carry the same text are one point written twice, and the
// segment only has room for one of them. The one that is left names every
// message both of them named, so the merge loses no message and no name.
//
// The order of the points is kept: the first of the two is the one that
// stays, because the segment is read in the order it happened.
func mergeSegmentDuplicates(points []model.SessionSummarySegmentPoint) []model.SessionSummarySegmentPoint {
	merged := make([]model.SessionSummarySegmentPoint, 0, len(points))
	at := make(map[string]int, len(points))
	for _, point := range points {
		key := strings.TrimSpace(point.Text)
		if key == "" {
			merged = append(merged, point)
			continue
		}
		position, exists := at[key]
		if !exists {
			at[key] = len(merged)
			merged = append(merged, point)
			continue
		}
		merged[position].EvidenceMessageIDs = mergeSegmentMessageIDs(
			merged[position].EvidenceMessageIDs, point.EvidenceMessageIDs)
	}
	return merged
}

// mergeSegmentMessageIDs is the union of two lists of message ids.
func mergeSegmentMessageIDs(left, right []uint) []uint {
	ids := make([]uint, 0, len(left)+len(right))
	seen := make(map[uint]struct{}, len(left)+len(right))
	for _, list := range [][]uint{left, right} {
		for _, id := range list {
			if id == 0 {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	return ids
}

// shortenSegmentPoints cuts the texts of the points until they fit a budget.
//
// The longest text goes first: the learner's points can be hundreds of runes
// long and the assistant's products are short by construction, so the words
// that cost the most are the words that are cut. A cut point keeps its
// message ids, its label and its mark - what it says about the message
// stays, only the words are fewer.
//
// The cost is measured by the caller, because the two places that fit a
// segment measure it differently: one measures the stored body, the other
// measures the block that is written into the window.
func shortenSegmentPoints(points []model.SessionSummarySegmentPoint,
	cost func([]model.SessionSummarySegmentPoint) int, budget int) ([]model.SessionSummarySegmentPoint, bool) {
	if len(points) == 0 {
		return points, false
	}
	shortened := make([]model.SessionSummarySegmentPoint, len(points))
	copy(shortened, points)
	changed := false
	for _, floor := range []int{segmentPointFloorRunes, segmentPointNameOnlyRunes} {
		for {
			size := cost(shortened)
			if size <= budget {
				return shortened, changed
			}
			victim := -1
			longest := 0
			for index, point := range shortened {
				runes := len([]rune(point.Text))
				if runes <= floor {
					continue
				}
				if victim < 0 || runes > longest {
					victim, longest = index, runes
				}
			}
			if victim < 0 {
				// Every text is at the floor. The next floor is smaller, and
				// if that one is reached too there is nothing left to cut:
				// what is left to give up is a point, and that is a different
				// decision.
				break
			}
			runes := len([]rune(shortened[victim].Text))
			// The cut is what this point can give and no more, so that the
			// excess is spread over the points instead of being taken out
			// of one of them.
			cut := size - budget
			if room := runes - floor; cut > room {
				cut = room
			}
			if cut < 1 {
				cut = 1
			}
			keep := runes - cut
			if keep < floor {
				keep = floor
			}
			shortened[victim].Text = cutPointText(shortened[victim].Text, keep)
			changed = true
		}
	}
	return shortened, changed
}

// segmentHasAssistantPoint reports whether the segment holds a point of the
// assistant's.
//
// It is how the renderer tells the two modes apart: the mode that records
// the assistant's products writes segments that hold them, and the mode that
// does not writes segments that never do.
func segmentHasAssistantPoint(points []model.SessionSummarySegmentPoint) bool {
	for _, point := range points {
		if point.IsAssistantPoint() {
			return true
		}
	}
	return false
}

// segmentPointIsLastName reports whether the point at that place is the only
// thing in the set that names one of the messages it cites.
//
// A point that is the last name of a message carries something the rest of
// the segment does not: whatever was said in that message, and the right to
// read it back. A point whose messages are all named elsewhere says nothing
// the segment would lose.
func segmentPointIsLastName(points []model.SessionSummarySegmentPoint, index int) bool {
	if index < 0 || index >= len(points) {
		return false
	}
	for _, id := range points[index].EvidenceMessageIDs {
		if id == 0 {
			continue
		}
		shared := false
		for other, candidate := range points {
			if other == index {
				continue
			}
			if segmentPointCitesID(candidate, id) {
				shared = true
				break
			}
		}
		if !shared {
			return true
		}
	}
	return false
}

// segmentPointCitesID reports whether a point names a message.
func segmentPointCitesID(point model.SessionSummarySegmentPoint, id uint) bool {
	for _, cited := range point.EvidenceMessageIDs {
		if cited == id {
			return true
		}
	}
	return false
}

// segmentPointToGiveUp finds the point to give up when a segment does not
// fit.
//
// The assistant's products nobody took on go first: nobody agreed to them,
// so they are the least the segment is worth keeping. The oldest goes first
// among them, for the same reason the tail goes in the mode that does not
// record the assistant's products: a segment describes what happened last,
// and the product closest to now is the one the learner is most likely to
// ask about.
//
// A point that is the last name of one of its messages is never chosen. The
// message would stop being readable, and the reader can only read what the
// segment names. That is the same rule for the learner's own points and for
// the assistant's products: the point of the learner's that is the only
// thing naming a message is kept, exactly as the product of the assistant's
// is.
//
// When every point is the last name of one of its messages, nothing is given
// up. A segment that does not fit is a segment the renderer cuts, and the
// renderer cuts the words and not the names.
func segmentPointToGiveUp(points []model.SessionSummarySegmentPoint) (int, bool) {
	victim := -1
	for index, point := range points {
		if point.PointSource() != model.SessionSummarySegmentSourceAssistant {
			continue
		}
		if segmentPointIsLastName(points, index) {
			continue
		}
		if victim < 0 || segmentPointLastID(point) < segmentPointLastID(points[victim]) {
			victim = index
		}
	}
	if victim >= 0 {
		return victim, true
	}
	for index, point := range points {
		if segmentPointIsLastName(points, index) {
			continue
		}
		if victim < 0 || segmentPointLastID(point) < segmentPointLastID(points[victim]) {
			victim = index
		}
	}
	if victim >= 0 {
		return victim, true
	}
	// Nothing can go without taking a name with it, so nothing goes. A
	// segment that does not fit is a segment the renderer cuts, and the
	// renderer cuts the words and not the names.
	return 0, false
}

// segmentPointsCost is what the points of a segment cost together.
//
// The points are measured together: the space one of them takes is the space
// the ones after it do not have. Measuring them one at a time would say that
// two points fit a budget that only one of them fits.
func segmentPointsCost(points []model.SessionSummarySegmentPoint, marks []string, used int) int {
	total := used
	for index, point := range points {
		total += rag.EstimateTokens(renderSegmentPointLine(point, segmentPointMark(marks, index))) + 1
	}
	return total
}
