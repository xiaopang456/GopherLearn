package model

// The versions of the board, and what may be done with each of them.
//
// The statements are a new field in a stored document, so they are a new
// format. A decoder that was written before the field existed refuses a
// document that carries it - the field is one it does not know - and a
// deployment that rolls back meets documents it cannot read.
//
// These tests are about that, and they are about the other side of it: the
// new code reads everything the old code wrote, and turning the switch off
// is not a way back.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// decodeSessionWhiteboardAsOfVersion2 is the decoder as it was before the
// statements existed.
//
// It is written out here rather than reached for, because the point of the
// test is what the old decoder does with a document the new one writes: it
// refuses it. A copy that drifted from the original would prove nothing, so
// this one keeps the old rules exactly - unknown fields are refused, only
// versions 1 and 2 are known, and a version 2 document cannot carry the
// statements.
func decodeSessionWhiteboardAsOfVersion2(content string) (SessionWhiteboardDocument, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return SessionWhiteboardDocument{}, fmt.Errorf("session whiteboard is empty")
	}
	if !strings.HasPrefix(content, "{") {
		return SessionWhiteboardDocument{}, fmt.Errorf("session whiteboard must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var document SessionWhiteboardDocument
	if err := decoder.Decode(&document); err != nil {
		return SessionWhiteboardDocument{}, fmt.Errorf("decode session whiteboard: %w", err)
	}
	if document.SchemaVersion != SessionWhiteboardSchemaV1 &&
		document.SchemaVersion != SessionWhiteboardSchemaV2 {
		return SessionWhiteboardDocument{}, fmt.Errorf("unsupported session whiteboard schema version %d",
			document.SchemaVersion)
	}
	if len(document.Unverified) > 0 {
		return SessionWhiteboardDocument{}, fmt.Errorf(
			"session whiteboard version %d cannot carry an unconfirmed statement", document.SchemaVersion)
	}
	return document, nil
}

// The new code reads what the old code wrote.
//
// A deployment is upgraded while it has boards on disk, and those boards are
// in the old formats. Both of them are read, unchanged.
func TestTheNewDecoderReadsTheDocumentsTheOldOneWrote(t *testing.T) {
	version1 := `{"schema_version":1,"constraints":[{"key":"pages","text":"26 pages","value":"26"}]}`
	version2 := `{"schema_version":2,"constraints":[{"key":"pages","text":"26 pages","value":"26",` +
		`"origin":"user","source_message_ids":[51]}]}`
	for _, content := range []string{version1, version2} {
		document, err := DecodeSessionWhiteboardDocument(content)
		if err != nil {
			t.Fatalf("a document the old code wrote was refused: %v", err)
		}
		if len(document.Constraints) != 1 {
			t.Fatalf("the document = %+v, want its entry", document.Constraints)
		}
		if len(document.Unverified) != 0 {
			t.Fatalf("the document carries %+v, want nothing", document.Unverified)
		}
	}
}

// The old decoder refuses what the new one writes.
//
// It fails loudly rather than reading the document without the statements:
// a board that silently lost the learner's own words would be a board that
// said the old value and no sign that the learner had said something else.
func TestTheOldDecoderRefusesTheDocumentThatCarriesTheStatements(t *testing.T) {
	document := SessionWhiteboardDocument{
		SchemaVersion: SessionWhiteboardSchemaV3,
		Constraints: []SessionWhiteboardEntry{
			{Key: "pages", Text: "26 pages", Value: "26"},
		},
		Unverified: []SessionWhiteboardAnchor{
			{Key: "pages", Value: "41", MessageID: 95, Text: "41 pages", Complete: true,
				Reason: SessionWhiteboardAnchorQuoteNotInMessage},
		},
	}
	content, err := MarshalSessionWhiteboardDocument(document)
	if err != nil {
		t.Fatalf("marshal the document: %v", err)
	}
	if _, err = DecodeSessionWhiteboardDocument(content); err != nil {
		t.Fatalf("the new decoder refused its own document: %v", err)
	}
	if _, err = decodeSessionWhiteboardAsOfVersion2(content); err == nil {
		t.Fatal("the old decoder accepted a document it cannot describe")
	}
}

// A document of an old version cannot carry the statements.
//
// The field belongs to version 3. A version 2 document that carries it is
// refused rather than read with part of itself dropped.
func TestAnOldVersionCannotCarryTheStatements(t *testing.T) {
	content := `{"schema_version":2,"unverified":[{"message_id":95,"reason":"quote_not_in_message"}]}`
	if _, err := DecodeSessionWhiteboardDocument(content); err == nil {
		t.Fatal("a version 2 document carrying the statements was accepted")
	}
}

// Turning the switch off is not a way back.
//
// The stored document is what it is: a version 3 document stays a version 3
// document until something else writes over it. Nothing in this change
// converts it, and nothing in this change claims that turning a switch off
// would.
func TestTheStatementsAreOnlyAVersion3Field(t *testing.T) {
	document := SessionWhiteboardDocument{
		SchemaVersion: SessionWhiteboardSchemaV3,
		Unverified: []SessionWhiteboardAnchor{
			{Key: "pages", MessageID: 95, Reason: SessionWhiteboardAnchorQuoteNotInMessage},
		},
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("a version 3 document with the statements was refused: %v", err)
	}
	document.SchemaVersion = SessionWhiteboardSchemaV2
	if err := document.Validate(); err == nil {
		t.Fatal("a version 2 document with the statements was accepted")
	}
	document.SchemaVersion = SessionWhiteboardSchemaV1
	if err := document.Validate(); err == nil {
		t.Fatal("a version 1 document with the statements was accepted")
	}
}
