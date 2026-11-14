package model

import (
	old "GopherAI/model/internal/whiteboardv3715328"
	"testing"
)

func TestNewAnchorReasonsAreRejectedByPriorV3Decoder(t *testing.T) {
	for _, reason := range []string{SessionWhiteboardAnchorCitationInvalid, SessionWhiteboardAnchorNoClaim} {
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
