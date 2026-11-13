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
	// whiteboardBlockHeaderUnconfirmed is the header of a board that carries
	// unconfirmed learner statements.
	//
	// It says what the plain header cannot: a value an unconfirmed
	// statement is about may not be the current one. Without it the block
	// would tell the reader that the unmarked value is the current one
	// while the board itself knows of a statement it could not confirm.
	whiteboardBlockHeaderUnconfirmed = "Current state (server state; what is true now; a value carrying 〔…已改为…〕 was changed later, so the unmarked value is the current one; a value carrying 〔…未核验原话…〕 has an unconfirmed learner statement about it, so it may not be the current one):"
	// whiteboardUnconfirmedHeader introduces the unconfirmed learner
	// statements the board carries.
	//
	// It says what they are: the learner's own words that could not be
	// confirmed, kept so that a reader can find them. They are not a
	// current value, and the header says so before a reader meets one.
	whiteboardUnconfirmedHeader = "Unconfirmed learner statements (the learner's own words, word for word; they were not confirmed as the current state, so they are not a current value):"
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
	header := whiteboardBlockHeader
	if len(document.Unverified) > 0 {
		header = whiteboardBlockHeaderUnconfirmed
	}
	body.WriteString(header)
	used := rag.EstimateTokens(header) + 4
	if document.Goal != nil {
		line := "- Goal: " + strings.TrimSpace(document.Goal.Text) + renderEvidenceIDs(document.Goal.SourceMessageIDs)
		cost := rag.EstimateTokens(line) + 1
		if used+cost <= budget {
			body.WriteString("\n" + line)
			used += cost
			out.EvidenceMessageIDs = append(out.EvidenceMessageIDs, document.Goal.SourceMessageIDs...)
		}
	}
	contested := whiteboardContestedEntries(document)
	for _, group := range [][]model.SessionWhiteboardEntry{document.Constraints, document.Plan, document.OpenItems} {
		for _, entry := range group {
			line := renderWhiteboardEntry(entry, contested[strings.TrimSpace(entry.Key)])
			cost := rag.EstimateTokens(line) + 1
			if used+cost > budget {
				continue
			}
			body.WriteString("\n" + line)
			used += cost
			out.EvidenceMessageIDs = append(out.EvidenceMessageIDs, entry.SourceMessageIDs...)
		}
	}
	// The unconfirmed statements are rendered under a header of their own.
	//
	// They are the learner's words, not a current value, and they are shown
	// as what they are: a statement that could not be confirmed, with the
	// message it came from so that a reader can ask for it. The ids are
	// collected here because a statement that was not shown authorises
	// nothing: a reader that may not see it may not read it back either.
	if len(document.Unverified) > 0 {
		headerCost := rag.EstimateTokens(whiteboardUnconfirmedHeader) + 1
		if used+headerCost <= budget {
			body.WriteString("\n" + whiteboardUnconfirmedHeader)
			used += headerCost
			for _, anchor := range document.Unverified {
				line := renderWhiteboardAnchor(anchor)
				cost := rag.EstimateTokens(line) + 1
				if used+cost > budget {
					continue
				}
				body.WriteString("\n" + line)
				used += cost
				out.EvidenceMessageIDs = append(out.EvidenceMessageIDs, anchor.MessageID)
			}
		}
	}
	out.Text = body.String()
	return out
}

// renderWhiteboardAnchor renders one unconfirmed learner statement.
//
// The quote is the learner's own wording, the value is the candidate the
// refused entry wanted to put up, and the message id is where the words came
// from. Nothing in the line says the value is current: the header above it
// says the opposite.
func renderWhiteboardAnchor(anchor model.SessionWhiteboardAnchor) string {
	var out strings.Builder
	out.WriteString("- \"")
	out.WriteString(strings.TrimSpace(anchor.Quote))
	out.WriteString("\"")
	if value := strings.TrimSpace(anchor.Value); value != "" &&
		anchor.Reason != model.SessionWhiteboardAnchorUnsettled {
		// The candidate value is what the refused entry wanted to put up.
		// A statement the learner did not settle has no candidate: the
		// learner ruled the value out or asked about it, and naming it as a
		// candidate would dress a negation up as a value.
		out.WriteString(" (candidate value: ")
		out.WriteString(value)
		out.WriteString(")")
	}
	out.WriteString(renderEvidenceIDs([]uint{anchor.MessageID}))
	return out.String()
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
	block := renderSummarySegmentBlock(segment, model.SessionWhiteboardDocument{},
		segment.EndMessageID, perSegmentTokens)
	if block.Text == "" {
		return 0
	}
	return rag.EstimateTokens(block.Text) + segmentSeparatorTokens
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
	block := renderSummarySegmentBlock(segment, model.SessionWhiteboardDocument{}, endMessageID, 0)
	if block.Text == "" {
		return 0
	}
	return rag.EstimateTokens(block.Text) + 4
}

// renderWhiteboardEntry renders one board entry.
func renderWhiteboardEntry(entry model.SessionWhiteboardEntry, contested string) string {
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
	if strings.TrimSpace(contested) != "" {
		// The value is contested: the learner's own words say another
		// value of it, and the program could not confirm them. The entry
		// stays what it was, and the marker says that it may not be the
		// current one.
		out.WriteString("〔未核验原话：")
		out.WriteString(strings.TrimSpace(contested))
		out.WriteString("〕")
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

// renderedSegmentBlock is what one segment contributed to the window.
type renderedSegmentBlock struct {
	// Text is the block the segment contributed.
	Text string
	// EvidenceMessageIDs are the ids of the points that really rendered. A
	// point the budget cut is not one the model saw.
	EvidenceMessageIDs []uint
	// Trimmed says the budget cut this segment.
	Trimmed bool
	// OmittedPoints is how many points did not fit into the segment's
	// room.
	OmittedPoints int
	// OmittedMessageIDs are the messages the points that were left out
	// were written from.
	//
	// They are the candidates for a lost entrance. Whether they actually
	// lost one is decided when the window is assembled, because a message
	// another point names, or the note itself, still gives it a way back.
	OmittedMessageIDs []uint
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
	// OmittedPoints is how many points of the segments the block carries
	// did not fit into the window.
	//
	// The point is left out whole - its words are not cut - and the block
	// names the messages it came from. The count is what says how much of
	// the summary the window did not carry. It is counted from the window
	// that was actually assembled: a segment that is rendered twice and
	// sent the second time contributes the second render's omissions, not
	// both.
	OmittedPoints int
	// OmittedMessageIDs are the messages that lost their way back.
	//
	// They are the messages the points that were left out named, and the
	// ones nothing in the final window names: not a point that was kept,
	// not the note that says what was left out, and not the authorisation
	// that lets the model read it back. A message another point names has
	// an entrance and is not counted here.
	//
	// A lost entrance is a count of what the reader cannot get back to. It
	// does not say the fact is gone - the summary still holds it - and it
	// does not say that another point naming the same message loses
	// anything: the message is still readable through that other point.
	OmittedMessageIDs []uint
}

// renderSummarySegments renders the segments under a token budget.
//
// The budget is split in two: a whole budget for the block and a smaller one
// for each segment. A block that is one long segment would spend everything
// on the oldest stretch and leave the newest one - the part the learner is
// actually talking about - out of the window.
//
// A segment that does not fit is handled in the way the deployment's mode
// says it is handled. The mode that does not record the assistant's products
// keeps the path it always had: the segment does not fit the room that is
// left, so the block does not carry it. The mode that does record them trims
// the segment to the room that is left instead, because a segment the
// learner can still read is worth more than a hole in the window.
//
// A segment that cannot be carried at all is dropped, and the count says so:
// a dropped segment is a hole in what the model can be told about the session
// it is in, and the hole is invisible from the outside - the block simply
// does not mention the stretch.
func renderSummarySegments(segments []model.SessionSummarySegment, document model.SessionWhiteboardDocument,
	budget, perSegmentTokens int, assistantContent bool) renderedSummarySegments {
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
	// What the final window carries, and what it does not.
	//
	// The counts are worked out at the end, from the window that was
	// actually assembled, rather than added up as the segments are walked.
	// A segment that does not fit is rendered twice - once in full and once
	// in the room that is left - and the second render replaces the first.
	// Adding the two sets of omissions counts a point twice that was left
	// out once.
	shown := make([]uint, 0, 8)
	candidates := make([]uint, 0, 8)
	omittedPoints := 0
	// The walk goes from the newest segment backwards: the newest one covers
	// the turns the learner is talking about, and it is the one that must not
	// be squeezed out by an older and larger stretch. What is collected is
	// put back in chronological order before it is sent.
	for index := len(ordered) - 1; index >= 0; index-- {
		segment := ordered[index]
		rendered := renderSummarySegmentBlock(segment, document, latest, perSegmentTokens)
		if rendered.Text == "" {
			continue
		}
		cost := rag.EstimateTokens(rendered.Text) + 4
		if used+cost > budget {
			if !assistantContent {
				// The mode that does not record the assistant's products
				// keeps the path it always had: the segment does not fit
				// the room that is left, so the block does not carry it.
				out.DroppedSegments++
				omittedPoints += segmentPointCount(segment)
				candidates = append(candidates, segmentCitedIDs(segment)...)
				continue
			}
			// The mode that does record them trims the segment to the room
			// that is left rather than dropping it.
			room := budget - used - 4
			trimmed := renderSummarySegmentBlock(segment, document, latest, room)
			if trimmed.Text != "" {
				trimmedCost := rag.EstimateTokens(trimmed.Text) + 4
				if used+trimmedCost <= budget {
					used += trimmedCost
					out.Segments = append(out.Segments, renderedSummarySegment{
						Segment: segment, Text: trimmed.Text,
						EvidenceMessageIDs: trimmed.EvidenceMessageIDs,
						Trimmed:            rendered.Trimmed || trimmed.Trimmed,
					})
					// The trimmed render is the one that is sent, so its
					// omissions are the ones that happened. The first
					// render's omissions are not added to them: they are
					// the same omissions, counted twice.
					omittedPoints += trimmed.OmittedPoints
					candidates = append(candidates, trimmed.OmittedMessageIDs...)
					shown = append(shown, trimmed.EvidenceMessageIDs...)
					continue
				}
			}
			// There is no room for even the smallest form of this
			// segment. It is dropped, and the count says so.
			out.DroppedSegments++
			omittedPoints += segmentPointCount(segment)
			candidates = append(candidates, segmentCitedIDs(segment)...)
			continue
		}
		used += cost
		out.Segments = append(out.Segments, renderedSummarySegment{
			Segment: segment, Text: rendered.Text,
			EvidenceMessageIDs: rendered.EvidenceMessageIDs, Trimmed: rendered.Trimmed,
		})
		omittedPoints += rendered.OmittedPoints
		candidates = append(candidates, rendered.OmittedMessageIDs...)
		shown = append(shown, rendered.EvidenceMessageIDs...)
	}
	out.OmittedPoints = omittedPoints
	// The messages that lost their way back.
	//
	// A message named by a point that was left out is a candidate. It has
	// lost its entrance only when nothing in the final window names it: not
	// a point that was kept, not the note that says what was left out, and
	// not the authorisation that lets the model read it back. A message that
	// another point names has an entrance and is not lost.
	out.OmittedMessageIDs = lostSegmentEntrances(candidates, shown)
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

// renderSummarySegment renders one segment and reports what it carried.
//
// It is the form of the renderer that was always here: the text, the
// messages the points really rendered, and whether the budget cut it. The
// block form carries more - what was left out and what was lost - and this
// is the one the callers that do not need the rest use.
//
// The two are one render. The block form is the whole answer, and this
// reads the three fields of it that have always been read.
func renderSummarySegment(segment model.SessionSummarySegment, document model.SessionWhiteboardDocument,
	latestEndMessageID uint, perSegmentTokens int) (string, []uint, bool) {
	block := renderSummarySegmentBlock(segment, document, latestEndMessageID, perSegmentTokens)
	return block.Text, block.EvidenceMessageIDs, block.Trimmed
}

// renderSummarySegmentBlock renders one segment.
//
// The points are rendered one at a time and a point is never cut in half: a
// line that says the old value without saying it was replaced afterwards is
// worse than no line at all, because it invites the model to quote a value
// that is not current any more.
//
// When the points do not fit, the assistant's products nobody took on are
// given up first, oldest first: nobody agreed to them, so they are the least
// the segment is worth keeping. A point that is the last name of one of its
// messages is not given up at all, and that is the same rule for the
// learner's own points and for the assistant's products.
//
// What does not fit after that is left out whole, and the block says which
// messages the left-out points came from. The words of a point are never cut:
// a cut point keeps its name and loses its meaning, and the meaning is
// usually the part that says what the learner ruled out.
func renderSummarySegmentBlock(segment model.SessionSummarySegment, document model.SessionWhiteboardDocument,
	latestEndMessageID uint, perSegmentTokens int) renderedSegmentBlock {
	content, err := model.DecodeSessionSummarySegmentContent(segment.Content)
	if err != nil {
		// A segment this schema cannot read carries nothing the model could
		// use, and guessing at its body would put text the learner never
		// said into their mouth.
		return renderedSegmentBlock{}
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
	dropped := 0
	droppedIDs := make([]uint, 0, 4)
	if perSegmentTokens > 0 {
		// The points that the document can give up are the ones the
		// document carries as the assistant's: a product nobody took on is
		// the least the segment is worth keeping. A point that is the last
		// name of one of its messages is kept, because the message would
		// stop being readable. A document that carries no such point gives
		// nothing up, and the mode that does not record the assistant's
		// products writes exactly such documents.
		points, pointMarks, dropped, droppedIDs = dropUnacceptedProducts(points, pointMarks, perSegmentTokens, used)
		trimmed = trimmed || dropped > 0
	}
	// The points are rendered one at a time, and a point that does not fit
	// is left out whole: its words are not cut, because a cut point says
	// something else.
	lines := make([]string, 0, len(points))
	costs := make([]int, 0, len(points))
	kept := 0
	for index, point := range points {
		line := renderSegmentPointLine(point, segmentPointMark(pointMarks, index))
		cost := rag.EstimateTokens(line) + 1
		if perSegmentTokens > 0 && used+cost > perSegmentTokens {
			trimmed = true
			break
		}
		lines = append(lines, line)
		costs = append(costs, cost)
		used += cost
		kept = index + 1
	}
	omitted := dropped + len(points) - kept
	omittedIDs := uniqueSegmentIDs(append(droppedIDs, segmentOmittedIDs(points, kept)...))
	var evidence []uint
	for index, point := range points {
		if index >= kept {
			break
		}
		evidence = append(evidence, point.EvidenceMessageIDs...)
	}
	// The note is the navigation: it names the messages the left-out points
	// were written from, so the facts that did not fit are still one read
	// away.
	//
	// The note is written in the room that is left, and it never takes the
	// room of a point that did fit: a fact the learner can read is worth
	// more than a way back to a fact they cannot. When the room is not
	// there, the omission is counted and the count is what says so.
	if omitted > 0 && len(omittedIDs) > 0 {
		note := segmentOmissionNote(omitted, omittedIDs, segmentOmittedMarks(pointMarks, kept))
		noteCost := rag.EstimateTokens(note) + 1
		if note != "" && (perSegmentTokens <= 0 || used+noteCost <= perSegmentTokens) {
			out.WriteString("\n" + note)
			used += noteCost
			// The ids the note names are shown, so they may be read back.
			// An id that is not shown is not one the model may read.
			evidence = append(evidence, omittedIDs...)
		}
	}
	for _, line := range lines {
		out.WriteString("\n" + line)
	}
	return renderedSegmentBlock{
		Text: out.String(), EvidenceMessageIDs: evidence, Trimmed: trimmed,
		OmittedPoints: omitted, OmittedMessageIDs: omittedIDs,
	}
}

// dropUnacceptedProducts gives up the assistant's products nobody took on
// when the segment does not fit.
//
// The oldest goes first, for the same reason the tail does in the mode that
// does not record the assistant's products: a segment describes what
// happened last, and the product closest to now is the one the learner is
// most likely to ask about.
//
// A point that is the last name of one of its messages is not given up. The
// message would stop being readable, and the reader can only read what the
// segment names - the original is in the database, and a model cannot open
// the database. The rule is the same for the learner's own points and for
// the assistant's products: a point of the learner's that is the only thing
// naming a message is kept, exactly as a product of the assistant's is.
//
// The learner's own points and the products the learner took on are not
// given up here.
//
// The ids of the points that were given up come back with the rest: they are
// the entrances that did not enter the window, and the block names them.
func dropUnacceptedProducts(points []model.SessionSummarySegmentPoint, marks []string,
	budget, used int) ([]model.SessionSummarySegmentPoint, []string, int, []uint) {
	kept := points
	keptMarks := marks
	dropped := 0
	droppedIDs := make([]uint, 0, 4)
	for {
		if segmentPointsFit(kept, keptMarks, budget, used) {
			return kept, keptMarks, dropped, droppedIDs
		}
		victim, found := segmentPointToGiveUp(kept)
		if !found {
			return kept, keptMarks, dropped, droppedIDs
		}
		droppedIDs = append(droppedIDs, kept[victim].EvidenceMessageIDs...)
		kept = append(kept[:victim], kept[victim+1:]...)
		if victim < len(keptMarks) {
			keptMarks = append(keptMarks[:victim], keptMarks[victim+1:]...)
		}
		dropped++
	}
}

// segmentPointsFit reports whether the points fit the segment's budget.
//
// The points are measured together: the space one of them takes is the space
// the ones after it do not have. Measuring them one at a time would say that
// two points fit a budget that only one of them fits.
func segmentPointsFit(points []model.SessionSummarySegmentPoint, marks []string, budget, used int) bool {
	if budget <= 0 {
		return true
	}
	return segmentPointsCost(points, marks, used) <= budget
}

// renderSegmentPointLine is one point as it is written into the block.
//
// The label, the ids and the supersession mark are part of the line, so the
// line is built in one place: what the budget is measured against and what
// the window shows have to be the same text.
func renderSegmentPointLine(point model.SessionSummarySegmentPoint, mark string) string {
	return "- " + segmentPointLabel(point) + strings.TrimSpace(point.Text) +
		renderEvidenceIDs(point.EvidenceMessageIDs) + mark
}

// segmentPointMark reads the mark of the point at that index.
//
// A list can be shorter than the points it belongs to - a point that was
// given up takes its mark with it - and a missing mark is an empty one.
func segmentPointMark(marks []string, index int) string {
	if index < 0 || index >= len(marks) {
		return ""
	}
	return marks[index]
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
