package model

import (
	"fmt"
	"strings"
	"testing"
)

func TestSessionWhiteboardDocumentRejectsAnUnknownField(t *testing.T) {
	// A board that grew a field without this decoder knowing about it is a
	// board whose meaning changed underneath the code that reads it.
	if _, err := DecodeSessionWhiteboardDocument(`{"schema_version":1,"surprise":true}`); err == nil {
		t.Fatal("a document with an unknown field was accepted")
	}
}

// The two versions are read; anything else is a body this schema cannot
// describe.
func TestSessionWhiteboardDocumentRejectsAnotherSchemaVersion(t *testing.T) {
	if _, err := DecodeSessionWhiteboardDocument(`{"schema_version":3}`); err == nil {
		t.Fatal("a document with another schema version was accepted")
	}
	for _, version := range []int{SessionWhiteboardSchemaV1, SessionWhiteboardSchemaV2} {
		if _, err := DecodeSessionWhiteboardDocument(fmt.Sprintf(`{"schema_version":%d}`, version)); err != nil {
			t.Fatalf("a document with schema version %d was refused: %v", version, err)
		}
	}
}

func TestSessionWhiteboardDocumentRequiresAJSONObject(t *testing.T) {
	if _, err := DecodeSessionWhiteboardDocument("just some text"); err == nil {
		t.Fatal("a non-object document was accepted")
	}
	if _, err := DecodeSessionWhiteboardDocument(""); err == nil {
		t.Fatal("an empty document was accepted")
	}
}

func TestSessionWhiteboardDocumentRejectsTrailingJSON(t *testing.T) {
	if _, err := DecodeSessionWhiteboardDocument(`{"schema_version":1} {"schema_version":1}`); err == nil {
		t.Fatal("a document with trailing JSON was accepted")
	}
}

func TestSessionWhiteboardEntryRejectsAnEmptyText(t *testing.T) {
	entry := SessionWhiteboardEntry{Key: "duration", Text: "   "}
	if err := entry.Validate(); err == nil {
		t.Fatal("an entry with no text was accepted")
	}
}

func TestSessionWhiteboardEntryRejectsAnUnknownState(t *testing.T) {
	entry := SessionWhiteboardEntry{Text: "a plan", State: "maybe"}
	if err := entry.Validate(); err == nil {
		t.Fatal("an entry with an unknown state was accepted")
	}
}

func TestSessionWhiteboardEntryAcceptsTheKnownStates(t *testing.T) {
	for _, state := range []string{"", "open", "done"} {
		entry := SessionWhiteboardEntry{Text: "a plan", State: state}
		if err := entry.Validate(); err != nil {
			t.Fatalf("state %q was refused: %v", state, err)
		}
	}
}

func TestSessionWhiteboardEntryRejectsDuplicateAndZeroEvidenceIDs(t *testing.T) {
	if err := (SessionWhiteboardEntry{Text: "x", SourceMessageIDs: []uint{3, 3}}).Validate(); err == nil {
		t.Fatal("an entry with a duplicate evidence id was accepted")
	}
	if err := (SessionWhiteboardEntry{Text: "x", SourceMessageIDs: []uint{0}}).Validate(); err == nil {
		t.Fatal("an entry with a zero evidence id was accepted")
	}
}

func TestSessionWhiteboardEntryRejectsTooManyEvidenceIDs(t *testing.T) {
	ids := make([]uint, SessionWhiteboardEvidenceLimit+1)
	for i := range ids {
		ids[i] = uint(i + 1)
	}
	if err := (SessionWhiteboardEntry{Text: "x", SourceMessageIDs: ids}).Validate(); err == nil {
		t.Fatal("an entry citing too many messages was accepted")
	}
}

func TestSessionWhiteboardEntryRejectsAnOverlongText(t *testing.T) {
	entry := SessionWhiteboardEntry{Text: strings.Repeat("字", SessionWhiteboardTextMaxRunes+1)}
	if err := entry.Validate(); err == nil {
		t.Fatal("an overlong entry text was accepted")
	}
}

func TestSessionWhiteboardDocumentRejectsTooManyEntries(t *testing.T) {
	document := SessionWhiteboardDocument{SchemaVersion: SessionWhiteboardSchemaV1}
	for i := 0; i <= SessionWhiteboardItemLimit; i++ {
		document.Constraints = append(document.Constraints, SessionWhiteboardEntry{
			Key:  "key",
			Text: "an entry",
		})
	}
	if err := document.Validate(); err == nil {
		t.Fatal("a board with too many entries was accepted")
	}
}

func TestSessionWhiteboardEvidenceMessageIDsAreSortedAndDeduplicated(t *testing.T) {
	document := SessionWhiteboardDocument{
		SchemaVersion: SessionWhiteboardSchemaV1,
		Goal:          &SessionWhiteboardGoal{Text: "goal", SourceMessageIDs: []uint{9, 3}},
		Constraints:   []SessionWhiteboardEntry{{Text: "c", SourceMessageIDs: []uint{3, 7}}},
	}
	got := document.EvidenceMessageIDs()
	want := []uint{3, 7, 9}
	if len(got) != len(want) {
		t.Fatalf("evidence ids=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("evidence ids=%v want %v", got, want)
		}
	}
}

func TestMarshalSessionWhiteboardDocumentIsCanonical(t *testing.T) {
	document := SessionWhiteboardDocument{
		SchemaVersion: SessionWhiteboardSchemaV1,
		Goal:          &SessionWhiteboardGoal{Text: "goal"},
	}
	first, err := MarshalSessionWhiteboardDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalSessionWhiteboardDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the same board encoded differently:\n%s\n%s", first, second)
	}
}

func TestSessionWhiteboardRowRefusesAnEmptyContent(t *testing.T) {
	if err := (&SessionWhiteboard{UserID: 1, SessionID: "s", Content: "  "}).Validate(); err == nil {
		t.Fatal("a board row with no content was accepted")
	}
}

func TestSessionSummarySegmentContentRejectsAnUnknownField(t *testing.T) {
	if _, err := DecodeSessionSummarySegmentContent(`{"schema_version":1,"surprise":true}`); err == nil {
		t.Fatal("a segment with an unknown field was accepted")
	}
}

// The two versions are read; anything else is a body this schema cannot
// describe.
func TestSessionSummarySegmentContentRejectsAnotherSchemaVersion(t *testing.T) {
	if _, err := DecodeSessionSummarySegmentContent(`{"schema_version":3}`); err == nil {
		t.Fatal("a segment with another schema version was accepted")
	}
	for _, version := range []int{SessionSummarySegmentSchemaV1, SessionSummarySegmentSchemaV2} {
		if _, err := DecodeSessionSummarySegmentContent(fmt.Sprintf(`{"schema_version":%d}`, version)); err != nil {
			t.Fatalf("a segment with schema version %d was refused: %v", version, err)
		}
	}
}

func TestSessionSummarySegmentContentRejectsTooManyPoints(t *testing.T) {
	content := SessionSummarySegmentContent{SchemaVersion: SessionSummarySegmentSchemaV1}
	for i := 0; i <= SessionSummarySegmentPointLimit; i++ {
		content.Points = append(content.Points, SessionSummarySegmentPoint{Text: "a point"})
	}
	if err := content.Validate(); err == nil {
		t.Fatal("a segment with too many points was accepted")
	}
}

func TestSessionSummarySegmentPointRejectsAnEmptyText(t *testing.T) {
	if err := (SessionSummarySegmentPoint{Text: " "}).Validate(); err == nil {
		t.Fatal("a point with no text was accepted")
	}
}

func TestSessionSummarySegmentPointRejectsDuplicateEvidenceIDs(t *testing.T) {
	point := SessionSummarySegmentPoint{Text: "x", EvidenceMessageIDs: []uint{4, 4}}
	if err := point.Validate(); err == nil {
		t.Fatal("a point with a duplicate evidence id was accepted")
	}
}

func TestSessionSummarySegmentEvidenceMessageIDsAreSortedAndDeduplicated(t *testing.T) {
	content := SessionSummarySegmentContent{
		SchemaVersion: SessionSummarySegmentSchemaV1,
		Points: []SessionSummarySegmentPoint{
			{Text: "a", EvidenceMessageIDs: []uint{8, 2}},
			{Text: "b", EvidenceMessageIDs: []uint{2, 5}},
		},
	}
	got := content.EvidenceMessageIDs()
	want := []uint{2, 5, 8}
	if len(got) != len(want) {
		t.Fatalf("evidence ids=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("evidence ids=%v want %v", got, want)
		}
	}
}

func TestSessionSummarySegmentRowRefusesAnInvertedRange(t *testing.T) {
	segment := &SessionSummarySegment{UserID: 1, SessionID: "s", Seq: 1, StartMessageID: 20, EndMessageID: 10, Content: "{}"}
	if err := segment.Validate(); err == nil {
		t.Fatal("a segment with an inverted range was accepted")
	}
}

func TestSessionSummarySegmentRowRefusesANonPositiveSeq(t *testing.T) {
	segment := &SessionSummarySegment{UserID: 1, SessionID: "s", Seq: 0, StartMessageID: 1, EndMessageID: 10, Content: "{}"}
	if err := segment.Validate(); err == nil {
		t.Fatal("a segment with no sequence number was accepted")
	}
}

func TestSessionSummarySegmentRowAcceptsTheKnownIndexStates(t *testing.T) {
	for _, status := range []string{"", SessionSummarySegmentIndexPending, SessionSummarySegmentIndexIndexed, SessionSummarySegmentIndexFailed} {
		segment := &SessionSummarySegment{UserID: 1, SessionID: "s", Seq: 1, StartMessageID: 1, EndMessageID: 10, Content: "{}", IndexStatus: status}
		if err := segment.Validate(); err != nil {
			t.Fatalf("index status %q was refused: %v", status, err)
		}
	}
	segment := &SessionSummarySegment{UserID: 1, SessionID: "s", Seq: 1, StartMessageID: 1, EndMessageID: 10, Content: "{}", IndexStatus: "elsewhere"}
	if err := segment.Validate(); err == nil {
		t.Fatal("a segment with an unknown index status was accepted")
	}
}

func TestSegmentChangesAcceptAnAbsentList(t *testing.T) {
	var changes SegmentChanges
	if err := changes.UnmarshalJSON([]byte("null")); err != nil {
		t.Fatal(err)
	}
	if changes != nil {
		t.Fatalf("changes=%v want nil", changes)
	}
}

func TestSessionSummarySegmentJobRefusesAnInvertedRange(t *testing.T) {
	job := &SessionSummarySegmentJob{ID: "j", UserID: 1, SessionID: "s", DesiredMessageID: 5, StartMessageID: 9, Status: SessionSummarySegmentJobPending}
	if err := job.Validate(); err == nil {
		t.Fatal("a job with an inverted range was accepted")
	}
}

func TestSessionSummarySegmentJobRefusesAnUnknownStatus(t *testing.T) {
	job := &SessionSummarySegmentJob{ID: "j", UserID: 1, SessionID: "s", DesiredMessageID: 9, StartMessageID: 1, Status: "waiting"}
	if err := job.Validate(); err == nil {
		t.Fatal("a job with an unknown status was accepted")
	}
}

func TestSessionSummarySegmentJobAcceptsTheKnownStatuses(t *testing.T) {
	for _, status := range []string{
		SessionSummarySegmentJobPending, SessionSummarySegmentJobProcessing,
		SessionSummarySegmentJobCompleted, SessionSummarySegmentJobFailed,
	} {
		job := &SessionSummarySegmentJob{ID: "j", UserID: 1, SessionID: "s", DesiredMessageID: 9, StartMessageID: 1, Status: status}
		if err := job.Validate(); err != nil {
			t.Fatalf("status %q was refused: %v", status, err)
		}
	}
}
