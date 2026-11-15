package model

import (
	old "GopherAI/model/internal/whiteboardv3715328"
	"testing"
)

func TestNewAnchorReasonsAreRejectedByPriorV3Decoder(t *testing.T) {
	for _, reason := range []string{
		SessionWhiteboardAnchorCitationInvalid,
		SessionWhiteboardAnchorUnplaced,
		SessionWhiteboardAnchorSameNumber,
	} {
		t.Run(reason, func(t *testing.T) {
			board := SessionWhiteboardDocument{SchemaVersion: SessionWhiteboardSchemaV3, Unverified: []SessionWhiteboardAnchor{{MessageID: 1, Reason: reason}}}
			encoded, err := MarshalSessionWhiteboardDocument(board)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeSessionWhiteboardDocument(encoded); err != nil {
				t.Fatalf("current decoder: %v", err)
			}
			if _, err := old.DecodeSessionWhiteboardDocument(encoded); err == nil {
				t.Fatal("prior decoder accepted a new reason")
			}
		})
	}
}

// A reason the program knows is a reason the board may carry.
//
// The reason of a refused entry travels with the board, so a reason the
// writer does not know is a board it cannot write: the whole batch that
// carried it fails with it. The set of reasons and the set the writer
// accepts are therefore the same set, and this is where that is kept.
func TestEveryKnownReasonIsAReasonTheBoardMayCarry(t *testing.T) {
	for _, reason := range []string{
		SessionWhiteboardAnchorStanceUnknown,
		SessionWhiteboardAnchorUpdateUnknown,
		SessionWhiteboardAnchorMessageNotLearner,
		SessionWhiteboardAnchorQuoteMissing,
		SessionWhiteboardAnchorQuoteNotInMessage,
		SessionWhiteboardAnchorValueNotInQuote,
		SessionWhiteboardAnchorValueUnitMismatch,
		SessionWhiteboardAnchorValueMismatch,
		SessionWhiteboardAnchorSubjectNotInQuote,
		SessionWhiteboardAnchorSubjectNotInEntry,
		SessionWhiteboardAnchorAttributeNotInQuote,
		SessionWhiteboardAnchorAttributeNotInEntry,
		SessionWhiteboardAnchorScopeConflict,
		SessionWhiteboardAnchorUnsettled,
		SessionWhiteboardAnchorUnbackedChange,
		SessionWhiteboardAnchorSameNumber,
		SessionWhiteboardAnchorKeyAmbiguous,
		SessionWhiteboardAnchorCitationInvalid,
		SessionWhiteboardAnchorUnplaced,
	} {
		t.Run(reason, func(t *testing.T) {
			if !ValidSessionWhiteboardAnchorReason(reason) {
				t.Fatalf("the reason %q is not one the writer accepts", reason)
			}
			board := SessionWhiteboardDocument{
				SchemaVersion: SessionWhiteboardSchemaV3,
				Unverified:    []SessionWhiteboardAnchor{{MessageID: 1, Reason: reason}},
			}
			encoded, err := MarshalSessionWhiteboardDocument(board)
			if err != nil {
				t.Fatalf("the board cannot be written: %v", err)
			}
			read, err := DecodeSessionWhiteboardDocument(encoded)
			if err != nil {
				t.Fatalf("the board cannot be read back: %v", err)
			}
			if len(read.Unverified) != 1 || read.Unverified[0].Reason != reason {
				t.Fatalf("the board read back %+v, want the statement it was written with", read.Unverified)
			}
		})
	}
}

// A reason the program does not know is a board it refuses to write.
//
// This is the other side of the same contract: the writer refuses a board
// it cannot read back, so a reason that is not in the set never reaches
// the store.
func TestAnUnknownReasonIsRefusedByTheWriter(t *testing.T) {
	board := SessionWhiteboardDocument{
		SchemaVersion: SessionWhiteboardSchemaV3,
		Unverified:    []SessionWhiteboardAnchor{{MessageID: 1, Reason: "same_number_typo"}},
	}
	if _, err := MarshalSessionWhiteboardDocument(board); err == nil {
		t.Fatal("a board with an unknown reason was written")
	}
}

func TestPriorV3DecoderAcceptsPriorReason(t *testing.T) {
	board := SessionWhiteboardDocument{SchemaVersion: SessionWhiteboardSchemaV3, Unverified: []SessionWhiteboardAnchor{{MessageID: 1, Reason: SessionWhiteboardAnchorUnsettled}}}
	encoded, err := MarshalSessionWhiteboardDocument(board)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.DecodeSessionWhiteboardDocument(encoded); err != nil {
		t.Fatalf("old reason rejected: %v", err)
	}
}
