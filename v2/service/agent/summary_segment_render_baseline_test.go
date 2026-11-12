package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// The rendering of the mode that does not record the assistant's products,
// frozen.
//
// The question these tests ask is the one the two arms of a measurement rest
// on: "is the window the switch is off produces still the window that was
// produced before the switch existed". The only honest way to answer it is to
// run that version's code.
//
// The code below is a copy of the renderer as it was before the assistant's
// products were recorded, with the names changed so that it can live beside
// the current one. It is a copy rather than a call on purpose: calling
// today's renderer would answer a different question - it would say what
// today's code does - and today's code knows about the products and the
// labels that carry them.
//
// The helpers the copy calls are the ones this change did not touch: the
// evidence ids, the board changes, the board's entry for a key and the token
// estimator. The parts that changed - the block's walk, the per-segment
// walk, the point's label and the supersession marks - are copied here.

// baselineRenderSummarySegments is the block renderer as it was.
func baselineRenderSummarySegments(segments []model.SessionSummarySegment, document model.SessionWhiteboardDocument,
	budget, perSegmentTokens int) baselineRenderedSummarySegments {
	out := baselineRenderedSummarySegments{}
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
	for index := len(ordered) - 1; index >= 0; index-- {
		segment := ordered[index]
		block, evidence, trimmed := baselineRenderSummarySegment(segment, document, latest, perSegmentTokens)
		if block == "" {
			continue
		}
		cost := rag.EstimateTokens(block) + 4
		if used+cost > budget {
			continue
		}
		used += cost
		out.Segments = append(out.Segments, baselineRenderedSummarySegment{
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

// baselineRenderedSummarySegment is one segment as it reached the window.
type baselineRenderedSummarySegment struct {
	Segment            model.SessionSummarySegment
	Text               string
	EvidenceMessageIDs []uint
	Trimmed            bool
}

// baselineRenderedSummarySegments is what one turn actually sent.
type baselineRenderedSummarySegments struct {
	Text     string
	Segments []baselineRenderedSummarySegment
}

// baselineRenderSummarySegment is one segment as it was rendered.
func baselineRenderSummarySegment(segment model.SessionSummarySegment, document model.SessionWhiteboardDocument,
	latestEndMessageID uint, perSegmentTokens int) (string, []uint, bool) {
	content, err := model.DecodeSessionSummarySegmentContent(segment.Content)
	if err != nil {
		return "", nil, false
	}
	pointMarks, headerMark := baselineSummarySegmentSupersession(segment, content, document, latestEndMessageID)
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
		line := "- " + baselineSegmentPointLabel(point) + text +
			renderEvidenceIDs(point.EvidenceMessageIDs) + pointMarks[index]
		cost := rag.EstimateTokens(line) + 1
		if perSegmentTokens > 0 && used+cost > perSegmentTokens {
			if len(evidence) == 0 {
				cut, ok := baselineTrimPointLine(text, baselineSegmentPointLabel(point),
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

// baselineSegmentPointLabel is the label a point carried as it was.
func baselineSegmentPointLabel(point model.SessionSummarySegmentPoint) string {
	switch point.PointSource() {
	case model.SessionSummarySegmentSourceAssistant:
		return "（助手给出）"
	case model.SessionSummarySegmentSourceAssistantAccepted:
		return "（助手提议，用户已同意）"
	default:
		return ""
	}
}

// baselineTrimPointLine shortens one point so that the whole line fits.
func baselineTrimPointLine(text, label string, ids []uint, mark string, budget int) (string, bool) {
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

// baselineSummarySegmentSupersession marks the values the board replaced.
func baselineSummarySegmentSupersession(segment model.SessionSummarySegment,
	content model.SessionSummarySegmentContent,
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
			continue
		}
		current := strings.TrimSpace(entry.Value)
		if current == "" {
			current = strings.TrimSpace(entry.Text)
		}
		if current == strings.TrimSpace(change.NewValue) {
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

// baselineDecodeSummarySegmentChanges reads the board changes a segment recorded.
func baselineDecodeSummarySegmentChanges(raw string) []model.SessionSummarySegmentChange {
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

// The mode that does not record the assistant's products renders what it
// always rendered.
//
// The comparison is over the block's text and over the segments the block
// carries, for budgets that are tight enough to make the difference show: a
// budget that holds the segments whole, one that holds one of them, and one
// that holds the header and nothing else.
//
// The segments are the ones that mode writes - the version 1 documents, with
// no source on the points - because those are the documents the mode's own
// output is made of.
func TestTheModeThatIsOffRendersWhatItAlwaysRendered(t *testing.T) {
	segments := baselineRenderSegments(t)
	document := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}
	cost := rag.EstimateTokens(renderSummarySegmentForTest(segments[0], document)) + 4
	header := rag.EstimateTokens(summarySegmentsBlockHeader) + 4

	for _, budget := range []int{
		header + cost*len(segments) + 40,
		header + cost + 2,
		header + cost - 1,
		header,
		header - 10,
		40,
	} {
		want := baselineRenderSummarySegments(segments, document, budget, DefaultSegmentTokens)
		got := renderSummarySegments(segments, document, budget, DefaultSegmentTokens, false)
		if got.Text != want.Text {
			t.Fatalf("a budget of %d tokens rendered differently:\n got %q\nwant %q",
				budget, got.Text, want.Text)
		}
		if len(got.Segments) != len(want.Segments) {
			t.Fatalf("a budget of %d tokens carried %d segments, want %d",
				budget, len(got.Segments), len(want.Segments))
		}
		for index := range got.Segments {
			if got.Segments[index].Text != want.Segments[index].Text {
				t.Fatalf("a budget of %d tokens rendered segment %d differently:\n got %q\nwant %q",
					budget, index, got.Segments[index].Text, want.Segments[index].Text)
			}
			if got.Segments[index].Trimmed != want.Segments[index].Trimmed {
				t.Fatalf("a budget of %d tokens trimmed segment %d differently", budget, index)
			}
		}
	}
}

// The mode that records the assistant's products is the one that trims.
//
// The same budgets, the other path: the segment that does not fit is cut to
// the room that is left instead of being skipped, so the window keeps a piece
// of the stretch instead of a hole in it.
func TestTheModeThatIsOnTrimsTheSegmentThatDoesNotFit(t *testing.T) {
	segments := baselineRenderSegments(t)
	document := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}
	cost := rag.EstimateTokens(renderSummarySegmentForTest(segments[0], document)) + 4
	header := rag.EstimateTokens(summarySegmentsBlockHeader) + 4

	one := segments[:1]
	off := renderSummarySegments(one, document, header+cost-1, DefaultSegmentTokens, false)
	on := renderSummarySegments(one, document, header+cost-1, DefaultSegmentTokens, true)
	// The block walks from the newest segment backwards, so with one segment
	// it is the one that does not fit.
	if len(off.Segments) != 0 {
		t.Fatalf("the mode that is off carried %d segments, want none", len(off.Segments))
	}
	if len(on.Segments) != 1 {
		t.Fatalf("the mode that is on carried %d segments, want the trimmed one", len(on.Segments))
	}
	if !on.Segments[0].Trimmed {
		t.Fatal("the segment was carried whole by a budget that could not hold it")
	}
}

// baselineRenderSegments builds the segments the comparison runs on.
//
// They are the documents the mode that does not record the assistant's
// products writes: version 1, with no source on the points, and a board
// change that the board has since replaced.
func baselineRenderSegments(t *testing.T) []model.SessionSummarySegment {
	t.Helper()
	changes, err := model.MarshalSessionSummarySegmentChanges([]model.SessionSummarySegmentChange{
		{Key: "duration", OldValue: "30 分钟", NewValue: "48 分钟",
			NewMessageIDs: []uint{91}},
	})
	if err != nil {
		t.Fatalf("encode the board changes: %v", err)
	}
	build := func(seq int, points []model.SessionSummarySegmentPoint) model.SessionSummarySegment {
		content, err := model.MarshalSessionSummarySegmentContent(model.SessionSummarySegmentContent{
			SchemaVersion: model.SessionSummarySegmentSchemaV1, Topic: "每天的读量", Points: points,
		})
		if err != nil {
			t.Fatalf("encode the segment content: %v", err)
		}
		return model.SessionSummarySegment{
			Seq: seq, StartMessageID: uint(seq * 10), EndMessageID: uint(seq*10 + 9),
			Topic: "每天的读量", Content: content, WhiteboardChanges: changes, TokenEstimate: 120,
		}
	}
	return []model.SessionSummarySegment{
		build(1, []model.SessionSummarySegmentPoint{
			{Text: "每次学习总时长定为 30 分钟", EvidenceMessageIDs: []uint{12}},
			{Text: "读量按每天十五页执行", EvidenceMessageIDs: []uint{13}},
		}),
		build(2, []model.SessionSummarySegmentPoint{
			{Text: "改成每次总时长 48 分钟", EvidenceMessageIDs: []uint{91}},
		}),
	}
}
