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

// unlimitedTokens is a budget large enough that the renderer shows everything.
//
// It is the width of the platform's int, not a number chosen here: a budget
// that happened to be smaller than a real board would silently hide the part
// that overflowed, and a measurement taken that way would agree with the
// renderer while disagreeing with the board.
const unlimitedTokens = int(^uint(0) >> 1)

// RenderedWhiteboardTokens is what the board costs when the window sends it.
//
// The stored body is JSON: braces, quotes, field names and message ids. What
// the model reads is the rendered block, which is a fraction of that. The
// board's budget is a budget on what is sent, so it is measured on what is
// sent; measuring the stored body instead spends the budget twice and refuses
// updates that would have fitted.
func RenderedWhiteboardTokens(board model.SessionWhiteboardDocument) int {
	return rag.EstimateTokens(renderWhiteboardBlock(board, unlimitedTokens).Text)
}

// segmentSeparatorTokens is what a block charges for the break between two
// segments. It is the same number the block's own loop adds.
const segmentSeparatorTokens = 4

// SummarySegmentBlockHeaderTokens is what a segment block spends on its own
// header, before any segment is rendered.
//
// The renderer charges this first and then walks the segments, so it comes out
// of the same budget the segments come out of. A preselector that measured its
// candidates against the whole block budget, rather than against what is left
// of it, could therefore select a segment the renderer had no room for: the
// selection would be reported and then silently dropped.
//
// The number is a function rather than a constant because the header is text,
// and the estimator is the one the block is charged in.
func SummarySegmentBlockHeaderTokens() int {
	return rag.EstimateTokens(summarySegmentsBlockHeader) + 4
}

// RenderedSegmentTokens is what one stored segment costs the block that
// carries it.
//
// It is rendered under the per-segment budget every consumer uses, so the
// number is the number the window pays rather than what the body would cost
// if nothing trimmed it. The measure is taken without the board: a
// supersession mark names a change that has not happened yet when the segment
// is written, so counting marks here would make the number depend on what the
// conversation does next, and a budget that moves after the fact cannot be
// compared with anything.
func RenderedSegmentTokens(segment model.SessionSummarySegment) int {
	return renderedSegmentCost(segment, DefaultSegmentTokens)
}

// renderedSegmentCost renders one segment the way a block will and reports
// what the block charges for it.
func renderedSegmentCost(segment model.SessionSummarySegment, perSegmentTokens int) int {
	block, _, _ := renderSummarySegment(segment, model.SessionWhiteboardDocument{},
		segment.EndMessageID, perSegmentTokens)
	if block == "" {
		return 0
	}
	return rag.EstimateTokens(block) + segmentSeparatorTokens
}

// SegmentContentTokens is what a segment body costs once it is rendered,
// measured with the header the finished segment will carry.
//
// The generator measures with this while the segment is still being built:
// the header carries the message range and the time, so a body measured on
// its own would read as fitting when the block it becomes does not.
func SegmentContentTokens(content model.SessionSummarySegmentContent,
	startMessageID, endMessageID uint) int {
	raw, err := model.MarshalSessionSummarySegmentContent(content)
	if err != nil {
		return 0
	}
	segment := model.SessionSummarySegment{
		StartMessageID: startMessageID, EndMessageID: endMessageID, Content: raw,
	}
	block, _, _ := renderSummarySegment(segment, model.SessionWhiteboardDocument{}, endMessageID, 0)
	if block == "" {
		return 0
	}
	return rag.EstimateTokens(block) + 4
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
	out.WriteString(whiteboardOriginLabel(entry))
	out.WriteString(strings.TrimSpace(entry.Text))
	if value := strings.TrimSpace(entry.Value); value != "" && !strings.Contains(entry.Text, value) {
		fmt.Fprintf(&out, " (value: %s)", value)
	}
	out.WriteString(renderEvidenceIDs(entry.SourceMessageIDs))
	return out.String()
}

// whiteboardOriginLabel is what an entry's origin reads as in the window.
//
// The origin is also a field the program checks, but a model that cannot
// tell whose words an entry holds will quote an assistant's proposal as if
// the learner had set it. The label is what stops that.
func whiteboardOriginLabel(entry model.SessionWhiteboardEntry) string {
	if entry.EntryOrigin() == model.SessionWhiteboardOriginAssistantAccepted {
		return "（助手提议，用户已同意）"
	}
	return ""
}

// segmentPointLabel is what a point's source reads as in the window.
//
// The source is also a field the program checks, and the label says the
// same thing in the text the model reads: a product the assistant produced
// and a statement the learner made are read very differently once the model
// knows which is which.
//
// The label of a product nobody took on says that nobody took it on. A
// proposal the learner did not agree to is not a decision, and a label that
// only says "the assistant gave this" leaves the model to decide what a
// proposal is worth.
func segmentPointLabel(point model.SessionSummarySegmentPoint) string {
	switch point.PointSource() {
	case model.SessionSummarySegmentSourceAssistant:
		return "（助手提议，用户未表示同意）"
	case model.SessionSummarySegmentSourceAssistantAccepted:
		return "（助手提议，用户已同意）"
	default:
		return ""
	}
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
	// DroppedSegments is how many segments the block could not carry at
	// all.
	//
	// A segment that does not fit is trimmed, and only a segment that
	// cannot be trimmed is dropped. The count says how often the window had
	// no room for a piece of the session's own memory.
	DroppedSegments int
}

// renderSummarySegments renders the segments under a token budget.
//
// The budget is split in two: a whole budget for the block and a smaller one
// for each segment. A block that is one long segment would spend everything
// on the oldest stretch and leave the newest one - the part the learner is
// actually talking about - out of the window.
//
// A segment that does not fit is trimmed to the room that is left rather
// than dropped. A dropped segment is a hole in what the model can be told
// about the session it is in, and the hole is invisible from the outside:
// the block simply does not mention the stretch.
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
			// The segment does not fit whole. It is trimmed to the room
			// that is left rather than dropped.
			room := budget - used - 4
			trimmedBlock, trimmedEvidence, wasTrimmed := renderSummarySegment(segment, document, latest, room)
			if trimmedBlock != "" {
				trimmedCost := rag.EstimateTokens(trimmedBlock) + 4
				if used+trimmedCost <= budget {
					used += trimmedCost
					out.Segments = append(out.Segments, renderedSummarySegment{
						Segment: segment, Text: trimmedBlock,
						EvidenceMessageIDs: trimmedEvidence, Trimmed: trimmed || wasTrimmed,
					})
					continue
				}
			}
			// There is no room for even the smallest form of this
			// segment. It is dropped, and the count says so.
			out.DroppedSegments++
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
//
// When the points do not fit, the assistant's products nobody took on are
// given up first, oldest first: nobody agreed to them, so they are the least
// the segment is worth keeping. The learner's own points and the products
// the learner took on are not given up here - they are trimmed for display,
// which is a different thing from the segment giving them up.
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
	points := content.Points
	trimmed := false
	if perSegmentTokens > 0 {
		points, pointMarks = dropUnacceptedProducts(points, pointMarks, perSegmentTokens, used)
	}
	var evidence []uint
	for index, point := range points {
		text := strings.TrimSpace(point.Text)
		line := "- " + segmentPointLabel(point) + text +
			renderEvidenceIDs(point.EvidenceMessageIDs) + pointMarks[index]
		cost := rag.EstimateTokens(line) + 1
		if perSegmentTokens > 0 && used+cost > perSegmentTokens {
			if len(evidence) == 0 {
				// The first point does not fit on its own. Its text is cut,
				// but its ids and its marker stay: the marker is what says
				// the value is not current any more.
				cut, ok := trimPointLine(text, segmentPointLabel(point),
					point.EvidenceMessageIDs, pointMarks[index], perSegmentTokens-used)
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

// dropUnacceptedProducts gives up the assistant's products nobody took on
// when the segment does not fit.
//
// The oldest goes first, for the same reason the tail does in the mode that
// does not record the assistant's products: a segment describes what
// happened last, and the product closest to now is the one the learner is
// most likely to ask about.
//
// The learner's own points and the products the learner took on are not
// given up here.
func dropUnacceptedProducts(points []model.SessionSummarySegmentPoint, marks []string,
	budget, used int) ([]model.SessionSummarySegmentPoint, []string) {
	kept := points
	keptMarks := marks
	for {
		if segmentPointsFit(kept, keptMarks, budget, used) {
			return kept, keptMarks
		}
		victim, found := oldestUnacceptedAssistantPoint(kept)
		if !found {
			return kept, keptMarks
		}
		kept = append(kept[:victim], kept[victim+1:]...)
		if victim < len(keptMarks) {
			keptMarks = append(keptMarks[:victim], keptMarks[victim+1:]...)
		}
	}
}

// segmentPointsFit reports whether the points fit the segment's budget.
func segmentPointsFit(points []model.SessionSummarySegmentPoint, marks []string, budget, used int) bool {
	if budget <= 0 {
		return true
	}
	for index, point := range points {
		mark := ""
		if index < len(marks) {
			mark = marks[index]
		}
		line := "- " + segmentPointLabel(point) + strings.TrimSpace(point.Text) +
			renderEvidenceIDs(point.EvidenceMessageIDs) + mark
		if used+rag.EstimateTokens(line)+1 > budget {
			return false
		}
	}
	return true
}

// trimPointLine shortens one point so that the whole line fits a budget.
//
// The ids and the supersession marker are the last things to go. A line that
// lost its marker would show a superseded value as if it were current, and
// the point of the marker is to prevent exactly that. The source label goes
// with them for the same reason: a line that lost it would show the
// assistant's words as if they were the learner's.
func trimPointLine(text, label string, ids []uint, mark string, budget int) (string, bool) {
	suffix := label + renderEvidenceIDs(ids) + mark
	room := budget - rag.EstimateTokens("- "+suffix) - 1
	if room <= 0 {
		return "", false
	}
	cut := prefixWithinTokens(text, room)
	if cut == "" {
		return "", false
	}
	return "- " + label + cut + renderEvidenceIDs(ids) + mark, true
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
//
// The marks are a list parallel to the points rather than a map of the point
// numbers: a point that is given up takes its mark with it, and a list cannot
// be left pointing at the wrong point.
func summarySegmentSupersession(segment model.SessionSummarySegment, content model.SessionSummarySegmentContent,
	document model.SessionWhiteboardDocument, latestEndMessageID uint) ([]string, string) {
	changes := decodeSummarySegmentChanges(segment.WhiteboardChanges)
	pointMarks := make([]string, len(content.Points))
	if len(changes) == 0 {
		return pointMarks, ""
	}
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
//
// The source label travels with them too. A point the assistant produced is
// not a thing the learner said, and a model that cannot tell the two apart
// will answer with the assistant's words as if they were the learner's.
func RenderSummarySegmentPoint(point model.SessionSummarySegmentPoint) string {
	return segmentPointLabel(point) + strings.TrimSpace(point.Text) + renderEvidenceIDs(point.EvidenceMessageIDs)
}
