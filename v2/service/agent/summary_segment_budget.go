package agent

// What a summary segment gives up when the window has no room for it.
//
// A point is two things at once: what was said, and the name of the message
// it was said in. The name is what lets the reader read that message back,
// and the reader can only read what the segment names.
//
// So the segment never gives up a name and never gives up half a fact. A
// point that does not fit is left out whole, and the block names the
// messages the left-out points came from: the fact is not in the window any
// more, but the way back to it is, and the count says how often that
// happened.
//
// The words of a point are never cut. A cut point keeps its name and loses
// its meaning: "不要改成 41 页" cut short says something else, and a summary
// that says something else is worse than one that says less. A summary that
// does not fit is a question for the model that wrote it - write it again,
// shorter - and that is a generation question, not a rendering one.
//
// Two points that say the same thing are not one point. The same words can
// be the learner's and the assistant's, or the old value and the new one,
// and the reader is told apart by who said it and by when. Merging them
// would throw that away, so nothing here merges anything.

import (
	"strconv"
	"strings"

	"GopherAI/common/rag"
	"GopherAI/model"
)

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
//
// It is a statement about the names and not about the facts: a message may
// hold two facts and one of them may be left out while the message stays
// readable. What is left out is then a fact the window does not carry, and
// the count of the omitted points says so.
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
// thing naming a message is kept, exactly as a product of the assistant's
// is.
//
// When every point is the last name of one of its messages, nothing is given
// up. A segment that does not fit is a segment the renderer leaves out, and
// the renderer says which messages it left out.
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
	// segment that does not fit is a segment the renderer leaves out, and
	// the renderer says which messages it left out.
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

// segmentOmittedIDs is the messages the points that were left out named.
//
// It is the way back to what did not fit: the fact is not in the window, but
// the message it was written from is named, so the reader can open it.
//
// The ids are the ones of the points themselves and not the ones of the
// messages no point names: a message that two points name is named by the
// point that was kept, and naming it again would say that it is unreadable
// when it is not.
func segmentOmittedIDs(points []model.SessionSummarySegmentPoint, kept int) []uint {
	ids := make([]uint, 0, 4)
	seen := make(map[uint]struct{}, 4)
	for index := kept; index < len(points); index++ {
		for _, id := range points[index].EvidenceMessageIDs {
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

// uniqueSegmentIDs is a list of message ids without the repeats.
func uniqueSegmentIDs(ids []uint) []uint {
	unique := make([]uint, 0, len(ids))
	seen := make(map[uint]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}

// segmentOmittedMarks are the marks the points that were left out carried.
//
// A mark says that the value in that message is not the current one, and it
// travels with the point it belongs to. A point that is left out takes its
// mark with it, and the note in the block carries it, so a reader who opens
// the message is told that the value in it is not the current one.
func segmentOmittedMarks(marks []string, kept int) []string {
	if kept >= len(marks) {
		return nil
	}
	return marks[kept:]
}

// segmentPointCount is how many points a segment holds.
//
// It is what a segment that never reached the window costs in points: the
// facts that are not in the window, and that the count has to say.
func segmentPointCount(segment model.SessionSummarySegment) int {
	content, err := model.DecodeSessionSummarySegmentContent(segment.Content)
	if err != nil {
		return 0
	}
	return len(content.Points)
}

// segmentCitedIDs is every message a segment's points name.
//
// It is what a segment that never reached the window takes with it: the
// messages that were readable through it and are not readable through
// anything now.
func segmentCitedIDs(segment model.SessionSummarySegment) []uint {
	content, err := model.DecodeSessionSummarySegmentContent(segment.Content)
	if err != nil {
		return nil
	}
	ids := make([]uint, 0, 8)
	for _, point := range content.Points {
		ids = append(ids, point.EvidenceMessageIDs...)
	}
	return uniqueSegmentIDs(ids)
}

// lostSegmentEntrances are the messages that nothing in the final window
// names any more.
//
// The candidates are the messages the points that were left out named. A
// candidate has lost its entrance only when the window does not name it
// anywhere: not in a point that was kept, not in the note that says what
// was left out, and not in the authorisation that lets the model read it
// back. A message another point names has an entrance, and a message that
// the note names has one too, so neither is lost.
//
// The order is the order the candidates were found in, so the count reads
// the way the window does.
func lostSegmentEntrances(candidates, shown []uint) []uint {
	kept := make(map[uint]struct{}, len(shown))
	for _, id := range shown {
		if id == 0 {
			continue
		}
		kept[id] = struct{}{}
	}
	lost := make([]uint, 0, len(candidates))
	seen := make(map[uint]struct{}, len(candidates))
	for _, id := range candidates {
		if id == 0 {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		if _, hasEntrance := kept[id]; hasEntrance {
			continue
		}
		lost = append(lost, id)
	}
	return lost
}

// segmentOmissionNote is what the block says about the points it left out.
//
// The note is the navigation. It says how many points did not fit and which
// messages they were written from, so a reader who wants the fact knows
// where it is instead of being told that it is gone.
//
// A mark the omitted points carried comes with them: the mark says that the
// value in that message is not the current one, and a reader who opens the
// message needs to be told that as much as the reader who reads the point.
func segmentOmissionNote(omitted int, ids []uint, marks []string) string {
	if omitted <= 0 || len(ids) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		// The ids are written the way the points write them, so that the
		// rule "an id that is not shown is not one the model may read
		// back" holds for the note as well.
		parts = append(parts, renderEvidenceIDs([]uint{id}))
	}
	if len(parts) == 0 {
		return ""
	}
	note := "（未展开 " + strconv.Itoa(omitted) + " 条 " + strings.Join(parts, " ")
	carried := make([]string, 0, len(marks))
	for _, mark := range marks {
		if strings.TrimSpace(mark) == "" {
			continue
		}
		carried = append(carried, mark)
	}
	if len(carried) > 0 {
		note += " " + strings.Join(carried, " ")
	}
	return note + "）"
}
