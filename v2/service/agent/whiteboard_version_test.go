package agent

// The switch, the versions, and what happens to the learner's own words when
// the switch is turned off.
//
// The statements are a version 3 field. A session that had them written
// keeps them until something else writes over the board, and the write that
// cannot hold them says so in a count rather than dropping them in silence.
//
// This is not a conversion and it is not a way back: turning the switch off
// does not turn a version 3 document into a version 1 one, and a deployment
// that has written a version 3 document cannot be rolled back to a release
// that does not know the version.

import (
	"strings"
	"testing"

	"GopherAI/model"
)

// A board that carries the statements is read whatever the switch says.
//
// The reader reads the document that is stored. A version 3 document is a
// document the program knows how to read, and the statements in it are the
// learner's own words.
func TestABoardThatCarriesTheStatementsIsReadWhateverTheSwitchSays(t *testing.T) {
	stored := `{"schema_version":3,"constraints":[{"key":"pages","text":"26 pages","value":"26"}],` +
		`"unverified":[{"key":"pages","value":"41","message_id":95,"text":"41 pages",` +
		`"complete":true,"reason":"quote_not_in_message"}]}`
	document, err := decodeCurrentWhiteboard(stored, model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatalf("the stored board was refused: %v", err)
	}
	if len(document.Unverified) != 1 {
		t.Fatalf("the document carries %+v, want the learner's own words", document.Unverified)
	}
	rendered := renderWhiteboardBlock(document, 2000)
	if !strings.Contains(rendered.Text, "Unconfirmed learner statements") {
		t.Fatalf("the block does not carry the statements: %s", rendered.Text)
	}
}

// The write that cannot hold them says how many went.
//
// This is the whole of the "off" story: the next write in the mode that does
// not record the assistant's products produces a version 1 document, the
// statements are not in it, and the count says how many of the learner's own
// words left the board with that write.
func TestTheWriteThatCannotHoldTheStatementsCountsThem(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 26 页", "26 页/次", "", 51),
		},
		Unverified: []model.SessionWhiteboardAnchor{
			{Key: "pages", Value: "41 页/次", MessageID: 95, Text: anchorChangeText,
				Complete: true, Reason: model.SessionWhiteboardAnchorQuoteNotInMessage},
		},
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 26 页", "26 页/次", "", 51),
		},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Board.SchemaVersion != model.SessionWhiteboardSchemaV1 {
		t.Fatalf("the board is version %d, want the version the mode writes",
			result.Board.SchemaVersion)
	}
	if len(result.Board.Unverified) != 0 {
		t.Fatalf("the board carries %+v, want none: this version cannot hold them",
			result.Board.Unverified)
	}
	if result.Diagnostics.DroppedUnconfirmedStatements != 1 {
		t.Fatalf("dropped statements = %d, want 1", result.Diagnostics.DroppedUnconfirmedStatements)
	}
	if result.Diagnostics.ResolvedUnconfirmedStatements != 0 {
		t.Fatalf("resolved statements = %d, want 0: they were dropped, not resolved",
			result.Diagnostics.ResolvedUnconfirmedStatements)
	}
	if result.Diagnostics.ExpiredUnconfirmedStatements != 0 {
		t.Fatalf("expired statements = %d, want 0: they were dropped, not crowded out",
			result.Diagnostics.ExpiredUnconfirmedStatements)
	}
}

// The version that carries the statements writes them, and the version that
// does not run the claim checks at all.
//
// A version 2 board is the format that existed before the statements: it is
// read, it is written, and it never carries them.
func TestTheVersionThatExistedBeforeTheStatementsNeverCarriesThem(t *testing.T) {
	result, err := ApplyWhiteboardProposal(model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 26 页", "26 页/次", "", 51),
		},
	}, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorParaphrasedClaim()},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Unverified) != 0 {
		t.Fatalf("the board carries %+v, want none: this version cannot hold them",
			result.Board.Unverified)
	}
	// The claim is still checked in this version: the entry was refused.
	if result.Diagnostics.RejectedClaims != 1 {
		t.Fatalf("rejected claims = %d, want 1", result.Diagnostics.RejectedClaims)
	}
}

// The generator writes the version that holds the statements, and the mode
// that does not record the assistant's products writes the version it always
// wrote.
func TestTheGeneratorWritesTheVersionThatHoldsTheStatements(t *testing.T) {
	on := &ModelSegmentGenerator{assistantContent: true}
	if got := on.boardSchemaVersion(); got != model.SessionWhiteboardSchemaV3 {
		t.Fatalf("the board version = %d, want %d", got, model.SessionWhiteboardSchemaV3)
	}
	off := &ModelSegmentGenerator{assistantContent: false}
	if got := off.boardSchemaVersion(); got != model.SessionWhiteboardSchemaV1 {
		t.Fatalf("the board version = %d, want %d", got, model.SessionWhiteboardSchemaV1)
	}
}
