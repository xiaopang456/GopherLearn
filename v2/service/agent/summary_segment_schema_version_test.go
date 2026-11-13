package agent

import (
	"context"
	"testing"

	"GopherAI/model"
)

// The documents the mode that does not record the assistant's products
// writes.
//
// The two strings below are the bytes the commit f1996a0 wrote: the same
// generator, the same model answer and the same batch, run at that commit
// and captured from it.
//
// A mode that is off has to be off in the stored documents too, and not only
// in what the model is shown. A summary that behaves the same but is stored
// in a different shape is a summary a deployment cannot roll back from: the
// code that comes back would refuse the documents the code that went away
// wrote. The version the documents carry is the whole of that difference,
// which is why it follows the mode.

// baselineSegmentContentBytes is the segment f1996a0 wrote for the batch below.
const baselineSegmentContentBytes = `{"schema_version":1,"topic":"拆包","points":[{"text":"用户要学 net 包","evidence_message_ids":[30]},{"text":"用户说基础不牢","evidence_message_ids":[32]}],"names":["net"],"numbers":["416"]}`

// baselineWhiteboardContentBytes is the board f1996a0 wrote for the same batch.
const baselineWhiteboardContentBytes = `{"schema_version":1,"goal":{"text":"学会拆包","source_message_ids":[30],"updated_at_message_id":33},"constraints":[{"key":"c1","text":"先看粘包","value":"粘包","source_message_ids":[32],"updated_at_message_id":33}],"plan":[{"key":"p1","text":"写一个 demo","value":"demo","state":"open","source_message_ids":[32],"updated_at_message_id":33}],"open_items":[{"key":"o1","text":"半包还没懂","source_message_ids":[32],"updated_at_message_id":33}]}`

// baselineModelAnswer is the answer the model gives for the batch below.
//
// It is the answer f1996a0 was given as well, so the two runs differ in
// nothing but the code.
const baselineModelAnswer = `{"schema_version":1,"segment":{"topic":"拆包","points":[{"text":"用户要学 net 包","evidence_message_ids":[30]},{"text":"用户说基础不牢","evidence_message_ids":[32]}],"names":["net"],"numbers":["416"]},"whiteboard":{"schema_version":1,"goal":{"text":"学会拆包","source_message_ids":[30]},"constraints":[{"key":"c1","text":"先看粘包","value":"粘包","source_message_ids":[32]}],"plan":[{"key":"p1","text":"写一个 demo","value":"demo","state":"open","source_message_ids":[32]}],"open_items":[{"key":"o1","text":"半包还没懂","source_message_ids":[32]}]}}`

// baselineBatch is the batch both runs summarize.
func baselineBatch() SegmentGenerationInput {
	return SegmentGenerationInput{
		Messages: []model.Message{
			{ID: 30, IsUser: true, Content: "我想学 net 包的拆包"},
			{ID: 31, IsUser: false, Content: "先分清粘包和半包，再看 io.Reader"},
			{ID: 32, IsUser: true, Content: "我的基础不牢，先从粘包讲起"},
			{ID: 33, IsUser: true, Content: "书是 416 页"},
		},
		FirstMessageID: 30,
		LastMessageID:  33,
	}
}

// baselineDocuments runs the generator in the given mode and returns the two
// documents it wrote.
func baselineDocuments(t *testing.T, assistantContent bool) (string, string) {
	t.Helper()
	stub := &stubSegmentModel{content: baselineModelAnswer}
	generator, err := NewModelSegmentGenerator(stub, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(assistantContent)
	result, err := generator.GenerateSegment(context.Background(), baselineBatch())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	return result.SegmentContent, result.WhiteboardContent
}

// The mode that is off writes the documents the version before this change
// wrote, byte for byte.
//
// The comparison is against the bytes captured at f1996a0 and not against
// this code's own output: a comparison against the code would only say that
// the code is the code.
func TestTheModeThatIsOffWritesTheDocumentsTheBaselineWrote(t *testing.T) {
	segment, board := baselineDocuments(t, false)
	if segment != baselineSegmentContentBytes {
		t.Fatalf("the segment is not the one the baseline wrote:\n got %s\nwant %s", segment, baselineSegmentContentBytes)
	}
	if board != baselineWhiteboardContentBytes {
		t.Fatalf("the board is not the one the baseline wrote:\n got %s\nwant %s", board, baselineWhiteboardContentBytes)
	}
}

// The mode that is on writes the version that can hold the assistant's
// products.
//
// The two documents differ in the version and in the fields the version
// carries, and that is the whole difference the mode makes on disk.
func TestTheModeThatIsOnWritesTheVersionThatHoldsTheAssistantsProducts(t *testing.T) {
	segment, board := baselineDocuments(t, true)
	decoded, err := model.DecodeSessionSummarySegmentContent(segment)
	if err != nil {
		t.Fatalf("decode the segment: %v", err)
	}
	if decoded.SchemaVersion != model.SessionSummarySegmentSchemaV2 {
		t.Fatalf("the segment is version %d, want %d", decoded.SchemaVersion, model.SessionSummarySegmentSchemaV2)
	}
	boardDocument, err := model.DecodeSessionWhiteboardDocument(board)
	if err != nil {
		t.Fatalf("decode the board: %v", err)
	}
	if boardDocument.SchemaVersion != model.SessionWhiteboardSchemaV3 {
		t.Fatalf("the board is version %d, want %d", boardDocument.SchemaVersion, model.SessionWhiteboardSchemaV2)
	}
}

// The decoder of the version before this change reads what the mode that is
// off writes.
//
// The decoder is the one from f1996a0, copied into the test with its
// structures and its checks: it refuses a schema version it does not know, it
// refuses a field it does not know, and it refuses trailing JSON. A
// deployment that rolls back to that version has to be able to read the
// documents this one writes, and this is that claim made executable.
func TestTheBaselineDecoderReadsWhatTheModeThatIsOffWrites(t *testing.T) {
	segment, board := baselineDocuments(t, false)
	decodedSegment, err := decodeBaselineSegmentContent(segment)
	if err != nil {
		t.Fatalf("the baseline decoder refused the segment: %v", err)
	}
	if decodedSegment.Topic != "拆包" || len(decodedSegment.Points) != 2 {
		t.Fatalf("the baseline decoder read the segment as %+v", decodedSegment)
	}
	decodedBoard, err := decodeBaselineBoardDocument(board)
	if err != nil {
		t.Fatalf("the baseline decoder refused the board: %v", err)
	}
	if decodedBoard.Goal == nil || decodedBoard.Goal.Text != "学会拆包" {
		t.Fatalf("the baseline decoder read the board as %+v", decodedBoard)
	}
}

// The same decoder refuses what the mode that is on writes.
//
// This is the other half of the rollback: a deployment that turns the mode
// on writes documents the version before this change cannot read. That is
// what turning the mode on means, and it is why the mode is off by default.
func TestTheBaselineDecoderRefusesWhatTheModeThatIsOnWrites(t *testing.T) {
	segment, board := baselineDocuments(t, true)
	if _, err := decodeBaselineSegmentContent(segment); err == nil {
		t.Fatal("the baseline decoder read a version 2 segment")
	}
	if _, err := decodeBaselineBoardDocument(board); err == nil {
		t.Fatal("the baseline decoder read a version 2 board")
	}
}

// A version 1 document that carries one of the new fields is refused by the
// version that was in the field last week.
//
// The two fields are written only when they say something other than the
// default, and this is the check that proves it. A document that carried
// "source":"user" would be read by today's code - the field exists today -
// and refused by that one, because the structure it decodes into has no such
// field. A decoder that accepted it would be today's decoder with a
// different name, and the test would say nothing.
func TestTheBaselineDecoderRefusesTheNewFieldsInAVersion1Document(t *testing.T) {
	segment := `{"schema_version":1,"topic":"拆包","points":[{"text":"用户要学 net 包","evidence_message_ids":[30],"source":"user"}]}`
	board := `{"schema_version":1,"goal":{"text":"学会拆包","source_message_ids":[30],"updated_at_message_id":33},` +
		`"constraints":[{"key":"c1","text":"先看粘包","value":"粘包","source_message_ids":[32],"origin":"user","updated_at_message_id":33}]}`

	if _, err := decodeBaselineSegmentContent(segment); err == nil {
		t.Fatal("the baseline decoder read a version 1 segment with a source field")
	}
	if _, err := decodeBaselineBoardDocument(board); err == nil {
		t.Fatal("the baseline decoder read a version 1 board with an origin field")
	}

	// Today's decoder reads both, because the fields exist today. The
	// difference between the two decoders is the frozen structures, and not
	// a rule that happens to refuse everything.
	if _, err := model.DecodeSessionSummarySegmentContent(segment); err != nil {
		t.Fatalf("today's decoder refused a segment it knows: %v", err)
	}
	if _, err := model.DecodeSessionWhiteboardDocument(board); err != nil {
		t.Fatalf("today's decoder refused a board it knows: %v", err)
	}
}
