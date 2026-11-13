package agent

import (
	"fmt"
	"strings"
	"testing"

	"GopherAI/model"
)

// What the window read back.
//
// The read-back authorisation is drawn from what the window actually showed,
// not from what it was given. A statement the budget cut is a statement the
// model never saw, and an id it never saw is not one it may ask to read back:
// the model would be able to ask for the learner's own words on the strength
// of a citation that was not in front of it.

// windowBody is everything the window sent.
func windowBody(window ContextWindow) string {
	var body strings.Builder
	for _, message := range window.Messages {
		body.WriteString(message.Content)
		body.WriteString("\n")
	}
	return body.String()
}

// The board is cut to its budget, and the statements it did not carry
// authorise nothing.
func TestTheBoardGrantsOnlyTheStatementsItShowed(t *testing.T) {
	entries := make([]model.SessionWhiteboardEntry, 0, 20)
	for index := 0; index < 20; index++ {
		entries = append(entries, model.SessionWhiteboardEntry{
			Key: fmt.Sprintf("key-%d", index), Text: strings.Repeat("学习者定下的约束条件", 13),
			SourceMessageIDs: []uint{uint(60 - index)},
		})
	}
	reader := &fakeSegmentReader{board: testBoard(t, 60, entries), watermark: 60}
	window := buildLayeredWindow(t, reader, &fakeHistoryReader{}, nil, nil)
	body := windowBody(window)

	if len(window.SummarySegmentHistoryGrants) == 0 {
		t.Fatal("the board granted nothing")
	}
	if len(window.SummarySegmentHistoryGrants) >= len(entries) {
		t.Fatalf("the board granted %d ids for %d entries: the budget did not cut anything",
			len(window.SummarySegmentHistoryGrants), len(entries))
	}
	for _, grant := range window.SummarySegmentHistoryGrants {
		if !strings.Contains(body, fmt.Sprintf("[message_ids=%d]", grant.MessageID)) {
			t.Fatalf("the window granted %d, which it never showed", grant.MessageID)
		}
	}
}

// The segments are cut to their budget, and what they did not carry is named.
//
// A point that does not fit is left out whole, and the block says which
// messages it came from. The message is named, so it may be read back: the
// reader is told where the fact is, and the id it may open is an id it was
// shown.
func TestTheSegmentsGrantOnlyThePointsTheyShowed(t *testing.T) {
	long := strings.Repeat("学习者把每次学习总时长定为三十分钟", 12)
	segment := testSegment(t, 1, 1, 30, "学习时长调整", []model.SessionSummarySegmentPoint{
		{Text: long, EvidenceMessageIDs: []uint{12}},
		{Text: long + "第二段", EvidenceMessageIDs: []uint{13}},
		{Text: long + "第三段", EvidenceMessageIDs: []uint{14}},
	}, nil)
	reader := &fakeSegmentReader{watermark: 30, segments: []model.SessionSummarySegment{segment}}
	window := buildLayeredWindow(t, reader, &fakeHistoryReader{}, nil, nil)
	body := windowBody(window)

	if len(window.SummarySegmentHistoryGrants) == 0 {
		t.Fatal("the segment granted nothing")
	}
	for _, grant := range window.SummarySegmentHistoryGrants {
		if !strings.Contains(body, fmt.Sprintf("[message_ids=%d]", grant.MessageID)) {
			t.Fatalf("the window granted %d, which it never showed", grant.MessageID)
		}
	}
	// The budget has to have cut something, or the test above proves nothing.
	if strings.Contains(body, "第三段") {
		t.Fatalf("the last point reached the window: %s", body)
	}
	// The message the point was written from is named, so the fact is still
	// reachable.
	if !strings.Contains(body, "[message_ids=14]") {
		t.Fatalf("the block does not name the message that was left out: %s", body)
	}
}

// A value the board has since replaced keeps its marker when the point does
// not fit.
//
// The marker is what stops the model from quoting a superseded value as the
// current one. The point is left out whole - its words are not cut - and the
// marker travels with it into the note the block carries, so a reader who
// opens the message is told that the value in it is not the current one.
func TestTheSupersessionMarkerSurvivesTheCut(t *testing.T) {
	segment := testSegment(t, 1, 1, 30, "学习时长调整", []model.SessionSummarySegmentPoint{
		{Text: strings.Repeat("学习者把每次学习总时长定为三十分钟", 20), EvidenceMessageIDs: []uint{12}},
	}, []model.SessionSummarySegmentChange{{
		Key: "duration", OldValue: "30 分钟", NewValue: "30 分钟", NewMessageIDs: []uint{12},
	}})
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Constraints: []model.SessionWhiteboardEntry{
			{Key: "duration", Text: "每次总时长 = 60 分钟", Value: "60 分钟", UpdatedAtMessageID: 90},
		},
	}
	rendered := renderSummarySegmentBlock(segment, document, 90, 120)
	if !rendered.Trimmed {
		t.Fatal("the segment was not cut, so the test proves nothing")
	}
	if !strings.Contains(rendered.Text, "已改为 60 分钟") {
		t.Fatalf("the marker did not survive the cut: %s", rendered.Text)
	}
	if !strings.Contains(rendered.Text, "[message_ids=12]") {
		t.Fatalf("the ids did not survive the cut: %s", rendered.Text)
	}
	if len(rendered.EvidenceMessageIDs) != 1 || rendered.EvidenceMessageIDs[0] != 12 {
		t.Fatalf("the evidence is %v, want the id the cut point still carries", rendered.EvidenceMessageIDs)
	}
}
