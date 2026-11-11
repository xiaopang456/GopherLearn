package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
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

// baselineSegmentContent is the segment f1996a0 wrote for the batch below.
const baselineSegmentContent = `{"schema_version":1,"topic":"拆包","points":[{"text":"用户要学 net 包","evidence_message_ids":[30]},{"text":"用户说基础不牢","evidence_message_ids":[32]}],"names":["net"],"numbers":["416"]}`

// baselineWhiteboardContent is the board f1996a0 wrote for the same batch.
const baselineWhiteboardContent = `{"schema_version":1,"goal":{"text":"学会拆包","source_message_ids":[30],"updated_at_message_id":33},"constraints":[{"key":"c1","text":"先看粘包","value":"粘包","source_message_ids":[32],"updated_at_message_id":33}],"plan":[{"key":"p1","text":"写一个 demo","value":"demo","state":"open","source_message_ids":[32],"updated_at_message_id":33}],"open_items":[{"key":"o1","text":"半包还没懂","source_message_ids":[32],"updated_at_message_id":33}]}`

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
	if segment != baselineSegmentContent {
		t.Fatalf("the segment is not the one the baseline wrote:\n got %s\nwant %s", segment, baselineSegmentContent)
	}
	if board != baselineWhiteboardContent {
		t.Fatalf("the board is not the one the baseline wrote:\n got %s\nwant %s", board, baselineWhiteboardContent)
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
	if boardDocument.SchemaVersion != model.SessionWhiteboardSchemaV2 {
		t.Fatalf("the board is version %d, want %d", boardDocument.SchemaVersion, model.SessionWhiteboardSchemaV2)
	}
}

// The decoder of the version before this change reads what the mode that is
// off writes.
//
// The decoder below is the one from f1996a0, copied: it refuses a schema
// version it does not know, it refuses a field it does not know, and it
// refuses trailing JSON. A deployment that rolls back to that version has to
// be able to read the documents this one wrote, and this is that claim made
// executable.
func TestTheBaselineDecoderReadsWhatTheModeThatIsOffWrites(t *testing.T) {
	segment, board := baselineDocuments(t, false)
	decodedSegment, err := decodeSegmentContentAsOfBaseline(segment)
	if err != nil {
		t.Fatalf("the baseline decoder refused the segment: %v", err)
	}
	if decodedSegment.Topic != "拆包" || len(decodedSegment.Points) != 2 {
		t.Fatalf("the baseline decoder read the segment as %+v", decodedSegment)
	}
	decodedBoard, err := decodeBoardAsOfBaseline(board)
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
	if _, err := decodeSegmentContentAsOfBaseline(segment); err == nil {
		t.Fatal("the baseline decoder read a version 2 segment")
	}
	if _, err := decodeBoardAsOfBaseline(board); err == nil {
		t.Fatal("the baseline decoder read a version 2 board")
	}
}

// decodeSegmentContentAsOfBaseline is the segment decoder of f1996a0.
//
// It is a copy, not a call: the point of the test is what that version's
// code did, and a call into today's code would only say what today's code
// does.
func decodeSegmentContentAsOfBaseline(content string) (model.SessionSummarySegmentContent, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return model.SessionSummarySegmentContent{}, fmt.Errorf("summary segment content is empty")
	}
	if !strings.HasPrefix(content, "{") {
		return model.SessionSummarySegmentContent{}, fmt.Errorf("summary segment must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var document model.SessionSummarySegmentContent
	if err := decoder.Decode(&document); err != nil {
		return model.SessionSummarySegmentContent{}, fmt.Errorf("decode summary segment: %w", err)
	}
	if err := ensureSegmentJSONEOFAsOfBaseline(decoder); err != nil {
		return model.SessionSummarySegmentContent{}, err
	}
	if document.SchemaVersion != model.SessionSummarySegmentSchemaV1 {
		return model.SessionSummarySegmentContent{}, fmt.Errorf("unsupported summary segment schema version %d", document.SchemaVersion)
	}
	if err := document.Validate(); err != nil {
		return model.SessionSummarySegmentContent{}, err
	}
	return document, nil
}

// decodeBoardAsOfBaseline is the board decoder of f1996a0.
func decodeBoardAsOfBaseline(content string) (model.SessionWhiteboardDocument, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return model.SessionWhiteboardDocument{}, fmt.Errorf("session whiteboard is empty")
	}
	if !strings.HasPrefix(content, "{") {
		return model.SessionWhiteboardDocument{}, fmt.Errorf("session whiteboard must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var document model.SessionWhiteboardDocument
	if err := decoder.Decode(&document); err != nil {
		return model.SessionWhiteboardDocument{}, fmt.Errorf("decode session whiteboard: %w", err)
	}
	if err := ensureWhiteboardJSONEOFAsOfBaseline(decoder); err != nil {
		return model.SessionWhiteboardDocument{}, err
	}
	if document.SchemaVersion != model.SessionWhiteboardSchemaV1 {
		return model.SessionWhiteboardDocument{}, fmt.Errorf("unsupported session whiteboard schema version %d", document.SchemaVersion)
	}
	if err := document.Validate(); err != nil {
		return model.SessionWhiteboardDocument{}, err
	}
	return document, nil
}

// ensureSegmentJSONEOFAsOfBaseline is the trailing-JSON check of f1996a0.
func ensureSegmentJSONEOFAsOfBaseline(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing summary segment: %w", err)
	}
	return fmt.Errorf("summary segment contains trailing JSON")
}

// ensureWhiteboardJSONEOFAsOfBaseline is the trailing-JSON check of f1996a0.
func ensureWhiteboardJSONEOFAsOfBaseline(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing session whiteboard: %w", err)
	}
	return fmt.Errorf("session whiteboard contains trailing JSON")
}
