package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// The two blocks the layered summary puts in front of the model every turn.
//
// The board says what is true now. The segments say what happened then. The
// two are kept apart on purpose: a value that changed is still in the
// segment that recorded it, marked as superseded, so a learner who asks what
// they decided at the time still gets an answer - but the board is what the
// answer should quote.
//
// The old value is never deleted. Deleting it would make "what did I set at
// the start" unanswerable, and that question is exactly the one the marker
// exists to keep answerable.
//
// Both renderers return what they actually showed, not what they were given.
// The read-back authorisation is drawn from that: a statement the budget cut
// away is a statement the model never saw, and an id it never saw is not one
// it may ask for.

const (
	// whiteboardBlockHeader introduces the board.
	whiteboardBlockHeader = "Current state (server state; what is true now; a value carrying 〔…已改为…〕 was changed later, so the unmarked value is the current one):"
	// summarySegmentsBlockHeader introduces the segments.
	summarySegmentsBlockHeader = "Summary segments (compressed record of earlier turns in this session; the Current state block above is what is true now):"
)

// renderedWhiteboard is the board as it actually reached the window.
type renderedWhiteboard struct {
	// Text is the block, ready to be put in the window.
	Text string
	// EvidenceMessageIDs are the ids of the board statements that really
	// rendered. The goal and the entries are statements; the ones the budget
	// cut are not, and they authorise nothing.
	EvidenceMessageIDs []uint
}

// renderWhiteboardBlock renders the board under a token budget.
//
// The board is a fixed cost: it travels with every turn, so its size has to
// stay flat no matter how long the session runs. The budget is the model's
// own limit on the board's size, and the block is trimmed to it rather than
// dropped - a board that is partly shown is still the truth about the part
// that is shown.
//
// The trimming is per statement: a statement that does not fit is left out
// whole, so a half-shown constraint is never quoted as a fact.
func renderWhiteboardBlock(document model.SessionWhiteboardDocument, budget int) renderedWhiteboard {
	out := renderedWhiteboard{}
	if budget <= 0 {
		return out
	}
	var body strings.Builder
	body.WriteString(whiteboardBlockHeader)
	used := rag.EstimateTokens(whiteboardBlockHeader) + 4
	if document.Goal != nil {
		line := "- Goal: " + strings.TrimSpace(document.Goal.Text) + renderEvidenceIDs(document.Goal.SourceMessageIDs)
		cost := rag.EstimateTokens(line) + 1
		if used+cost <= budget {
			body.WriteString("\n" + line)
			used += cost
			out.EvidenceMessageIDs = append(out.EvidenceMessageIDs, document.Goal.SourceMessageIDs...)
		}
	}
	for _, group := range [][]model.SessionWhiteboardEntry{document.Constraints, document.Plan, document.OpenItems} {
		for _, entry := range group {
			line := renderWhiteboardEntry(entry)
			cost := rag.EstimateTokens(line) + 1
			if used+cost > budget {
				continue
			}
			body.WriteString("\n" + line)
			used += cost
			out.EvidenceMessageIDs = append(out.EvidenceMessageIDs, entry.SourceMessageIDs...)
		}
	}
	out.Text = body.String()
	return out
}

// renderWhiteboardEntry renders one board entry.
func renderWhiteboardEntry(entry model.SessionWhiteboardEntry) string {
	var out strings.Builder
	out.WriteString("- ")
	if key := strings.TrimSpace(entry.Key); key != "" {
		out.WriteString(key)
		if state := strings.TrimSpace(entry.State); state != "" {
			fmt.Fprintf(&out, " [%s]", state)
		}
		out.WriteString(": ")
	}
	out.WriteString(strings.TrimSpace(entry.Text))
	if value := strings.TrimSpace(entry.Value); value != "" && !strings.Contains(entry.Text, value) {
		fmt.Fprintf(&out, " (value: %s)", value)
	}
	out.WriteString(renderEvidenceIDs(entry.SourceMessageIDs))
	return out.String()
}

// renderEvidenceIDs renders the ids a compressed statement was drawn from.
//
// They are shown so the model can ask to read the learner's own words back;
// an id that is not shown is an id the model cannot ask for.
func renderEvidenceIDs(ids []uint) string {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d", id))
	}
	if len(parts) == 0 {
		return ""
	}
	return " [message_ids=" + strings.Join(parts, ",") + "]"
}

// renderedSummarySegment is one segment as it actually reached the window.
type renderedSummarySegment struct {
	// Segment is the stored segment.
	Segment model.SessionSummarySegment
	// Text is the block this segment contributed.
	Text string
	// EvidenceMessageIDs are the ids of the points that really rendered. A
	// point the budget cut is not one the model saw.
	EvidenceMessageIDs []uint
	// Trimmed says the budget cut this segment.
	Trimmed bool
}

// renderedSummarySegments is what one turn actually sent.
type renderedSummarySegments struct {
	// Text is the block, ready to be put in the window.
	Text string
	// Segments are the segments the block really carries, oldest first. The
	// grants are drawn from these and not from the ones that were offered:
	// a segment the budget trimmed away is a segment the model never saw.
	Segments []renderedSummarySegment
}

// renderSummarySegments renders the segments under a token budget.
//
// The budget is split in two: a whole budget for the block and a smaller one
// for each segment. A block that is one long segment would spend everything
// on the oldest stretch and leave the newest one - the part the learner is
// actually talking about - out of the window.
func renderSummarySegments(segments []model.SessionSummarySegment, document model.SessionWhiteboardDocument,
	budget, perSegmentTokens int) renderedSummarySegments {
	out := renderedSummarySegments{}
	if budget <= 0 || len(segments) == 0 {
		return out
	}
	ordered := make([]model.SessionSummarySegment, len(segments))
	copy(ordered, segments)
	sort.SliceStable(ordered, func(left, right int) bool { return ordered[left].Seq < ordered[right].Seq })
	latest := uint(0)
	for _, segment := range ordered {
		if segment.EndMessageID > latest {
			latest = segment.EndMessageID
		}
	}
	header := summarySegmentsBlockHeader
	used := rag.EstimateTokens(header) + 4
	// The walk goes from the newest segment backwards: the newest one covers
	// the turns the learner is talking about, and it is the one that must not
	// be squeezed out by an older and larger stretch. What is collected is
	// put back in chronological order before it is sent.
	for index := len(ordered) - 1; index >= 0; index-- {
		segment := ordered[index]
		block, evidence, trimmed := renderSummarySegment(segment, document, latest, perSegmentTokens)
		if block == "" {
			continue
		}
		cost := rag.EstimateTokens(block) + 4
		if used+cost > budget {
			continue
		}
		used += cost
		out.Segments = append(out.Segments, renderedSummarySegment{
			Segment: segment, Text: block, EvidenceMessageIDs: evidence, Trimmed: trimmed,
		})
	}
	if len(out.Segments) == 0 {
		return out
	}
	sort.SliceStable(out.Segments, func(left, right int) bool {
		return out.Segments[left].Segment.Seq < out.Segments[right].Segment.Seq
	})
	var body strings.Builder
	for _, rendered := range out.Segments {
		body.WriteString("\n")
		body.WriteString(rendered.Text)
	}
	out.Text = header + body.String()
	return out
}

// renderSummarySegment renders one segment.
//
// The points are rendered one at a time and a point is never cut in half: a
// line that says the old value without saying it was replaced afterwards is
// worse than no line at all, because it invites the model to quote a value
// that is not current any more.
func renderSummarySegment(segment model.SessionSummarySegment, document model.SessionWhiteboardDocument,
	latestEndMessageID uint, perSegmentTokens int) (string, []uint, bool) {
	content, err := model.DecodeSessionSummarySegmentContent(segment.Content)
	if err != nil {
		// A segment this schema cannot read carries nothing the model could
		// use, and guessing at its body would put text the learner never
		// said into their mouth.
		return "", nil, false
	}
	pointMarks, headerMark := summarySegmentSupersession(segment, content, document, latestEndMessageID)
	var out strings.Builder
	fmt.Fprintf(&out, "[第 %d 段 · 消息 %d-%d · %s]", segment.Seq, segment.StartMessageID,
		segment.EndMessageID, segment.EndAt.UTC().Format("2006-01-02 15:04"))
	if headerMark != "" {
		out.WriteString(headerMark)
	}
	if topic := strings.TrimSpace(content.Topic); topic != "" {
		fmt.Fprintf(&out, "\n话题：%s", topic)
	}
	used := rag.EstimateTokens(out.String())
	var evidence []uint
	trimmed := false
	for index, point := range content.Points {
		text := strings.TrimSpace(point.Text)
		line := "- " + text + renderEvidenceIDs(point.EvidenceMessageIDs) + pointMarks[index]
		cost := rag.EstimateTokens(line) + 1
		if perSegmentTokens > 0 && used+cost > perSegmentTokens {
			if len(evidence) == 0 {
				// The first point does not fit on its own. Its text is cut,
				// but its ids and its marker stay: the marker is what says
				// the value is not current any more.
				cut, ok := trimPointLine(text, point.EvidenceMessageIDs, pointMarks[index], perSegmentTokens-used)
				if ok {
					out.WriteString("\n" + cut)
					evidence = append(evidence, point.EvidenceMessageIDs...)
				}
			}
			trimmed = true
			break
		}
		out.WriteString("\n" + line)
		evidence = append(evidence, point.EvidenceMessageIDs...)
		used += cost
	}
	return out.String(), evidence, trimmed
}

// trimPointLine shortens one point so that the whole line fits a budget.
//
// The ids and the supersession marker are the last things to go. A line that
// lost its marker would show a superseded value as if it were current, and
// the point of the marker is to prevent exactly that.
func trimPointLine(text string, ids []uint, mark string, budget int) (string, bool) {
	suffix := renderEvidenceIDs(ids) + mark
	room := budget - rag.EstimateTokens("- "+suffix) - 1
	if room <= 0 {
		return "", false
	}
	cut := prefixWithinTokens(text, room)
	if cut == "" {
		return "", false
	}
	return "- " + cut + suffix, true
}

// summarySegmentSupersession marks the values of a segment that the board has
// since replaced.
//
// It returns one mark per point and one for the segment as a whole. The mark
// names the value the board holds now, because that is the one the answer has
// to use, and it names the message that changed it, because "it changed" and
// "it changed when" are different questions.
//
// A mark is only written when the board disagrees with what the segment set.
// A value the board still holds is not marked: a mark on every value would be
// noise, and the model's rule is "an unmarked value is the current one".
func summarySegmentSupersession(segment model.SessionSummarySegment, content model.SessionSummarySegmentContent,
	document model.SessionWhiteboardDocument, latestEndMessageID uint) (map[int]string, string) {
	changes := decodeSummarySegmentChanges(segment.WhiteboardChanges)
	if len(changes) == 0 {
		return nil, ""
	}
	pointMarks := make(map[int]string)
	var headerMark string
	for _, change := range changes {
		key := strings.TrimSpace(change.Key)
		if key == "" {
			continue
		}
		entry, found := boardEntryForKey(document, key)
		if !found {
			// The board does not carry this key any more. A value the board
			// dropped is not a value the board replaced, so there is nothing
			// to point the learner at.
			continue
		}
		current := strings.TrimSpace(entry.Value)
		if current == "" {
			current = strings.TrimSpace(entry.Text)
		}
		if current == strings.TrimSpace(change.NewValue) {
			// The value this segment set is still in force.
			continue
		}
		at := entry.UpdatedAtMessageID
		if at == 0 {
			at = latestEndMessageID
		}
		mark := fmt.Sprintf("〔第 %d 轮已改为 %s，以当前状态为准〕", at, current)
		placed := false
		for index, point := range content.Points {
			if !sharesEvidence(point.EvidenceMessageIDs, change.NewMessageIDs) {
				continue
			}
			pointMarks[index] += mark
			placed = true
		}
		if !placed {
			headerMark += mark
		}
	}
	return pointMarks, headerMark
}

// decodeSummarySegmentChanges reads the board changes a segment recorded.
func decodeSummarySegmentChanges(raw string) []model.SessionSummarySegmentChange {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var changes model.SegmentChanges
	if err := json.Unmarshal([]byte(raw), &changes); err != nil {
		return nil
	}
	return changes
}

// boardEntryForKey finds the board's current entry for one key.
func boardEntryForKey(document model.SessionWhiteboardDocument, key string) (model.SessionWhiteboardEntry, bool) {
	for _, group := range [][]model.SessionWhiteboardEntry{document.Constraints, document.Plan, document.OpenItems} {
		for _, entry := range group {
			if strings.TrimSpace(entry.Key) == key {
				return entry, true
			}
		}
	}
	return model.SessionWhiteboardEntry{}, false
}

// sharesEvidence reports whether two id lists have an id in common.
func sharesEvidence(left, right []uint) bool {
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	for _, candidate := range left {
		for _, other := range right {
			if candidate == other {
				return true
			}
		}
	}
	return false
}

// RankSummarySegmentsForQuestion returns the segments that match a question,
// best first.
//
// It is the lexical channel on its own, exported because the browse tool
// needs the same judgement the server-side preselection makes: a segment the
// tool shows has to be one the question is about, and the two answers must
// not disagree.
func RankSummarySegmentsForQuestion(question string, segments []model.SessionSummarySegment) []model.SessionSummarySegment {
	terms := summarySegmentQueryTerms(strings.TrimSpace(question))
	if len(terms) == 0 {
		return nil
	}
	type scored struct {
		segment model.SessionSummarySegment
		count   int
	}
	matches := make([]scored, 0, len(segments))
	for _, segment := range segments {
		count := summarySegmentTermHits(terms, segment)
		if count == 0 {
			continue
		}
		matches = append(matches, scored{segment: segment, count: count})
	}
	sort.SliceStable(matches, func(left, right int) bool {
		if matches[left].count != matches[right].count {
			return matches[left].count > matches[right].count
		}
		return matches[left].segment.Seq < matches[right].segment.Seq
	})
	out := make([]model.SessionSummarySegment, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.segment)
	}
	return out
}

// SummarySegmentSupersessionNote is one value of a segment that the board has
// since replaced.
type SummarySegmentSupersessionNote struct {
	// Key is the board key the value was written under.
	Key string
	// OldValue is what the segment set.
	OldValue string
	// CurrentValue is what the board holds now.
	CurrentValue string
	// ChangedAtMessageID is the message the change was recorded at.
	ChangedAtMessageID uint
}

// SummarySegmentSupersessionNotes returns the values of a segment the board
// has since replaced.
//
// The browse tool shows this to the model, so that a segment read on its own
// carries the same warning the window's own rendering carries: a value the
// board no longer holds must not be quoted as the current one.
func SummarySegmentSupersessionNotes(segment model.SessionSummarySegment,
	document model.SessionWhiteboardDocument) []SummarySegmentSupersessionNote {
	changes := decodeSummarySegmentChanges(segment.WhiteboardChanges)
	if len(changes) == 0 {
		return nil
	}
	var notes []SummarySegmentSupersessionNote
	for _, change := range changes {
		key := strings.TrimSpace(change.Key)
		if key == "" {
			continue
		}
		entry, found := boardEntryForKey(document, key)
		if !found {
			continue
		}
		current := strings.TrimSpace(entry.Value)
		if current == "" {
			current = strings.TrimSpace(entry.Text)
		}
		if current == strings.TrimSpace(change.NewValue) {
			continue
		}
		notes = append(notes, SummarySegmentSupersessionNote{
			Key: key, OldValue: strings.TrimSpace(change.OldValue), CurrentValue: current,
			ChangedAtMessageID: entry.UpdatedAtMessageID,
		})
	}
	return notes
}

// RenderSummarySegmentPoint renders one point of a segment as the model reads
// it.
//
// The evidence ids travel with the text: they are what the model needs in
// order to ask for the learner's own words back, and a point without them
// is a claim the model cannot check.
func RenderSummarySegmentPoint(point model.SessionSummarySegmentPoint) string {
	return strings.TrimSpace(point.Text) + renderEvidenceIDs(point.EvidenceMessageIDs)
}
