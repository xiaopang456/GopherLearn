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

// Turning the mode off keeps the version the board already has, and keeps
// the statements in it.
//
// The board's version never goes backwards. A session whose board was written
// in version 3 keeps writing version 3 after the mode that records the
// assistant's products is turned off, and the learner's own words it carries
// stay in it: they are the learner's words, not the mode's property to throw
// away. Writing a version 1 document over it would drop them without trace,
// and a board that loses a turn of the conversation silently is the failure
// this whole change is about.
func TestTurningTheModeOffKeepsTheVersionAndTheStatements(t *testing.T) {
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
	if result.Board.SchemaVersion != model.SessionWhiteboardSchemaV3 {
		t.Fatalf("the board is version %d, want the version it already had",
			result.Board.SchemaVersion)
	}
	if len(result.Board.Unverified) != 1 {
		t.Fatalf("the board carries %+v, want the learner's own words to stay",
			result.Board.Unverified)
	}
	if result.Board.Unverified[0].MessageID != 95 {
		t.Fatalf("the statement points at message %d, want the learner's own message",
			result.Board.Unverified[0].MessageID)
	}
	if result.Diagnostics.ResolvedUnconfirmedStatements != 0 {
		t.Fatalf("resolved statements = %d, want 0: the board still holds the old value",
			result.Diagnostics.ResolvedUnconfirmedStatements)
	}
	if result.Diagnostics.ExpiredUnconfirmedStatements != 0 {
		t.Fatalf("expired statements = %d, want 0: nothing was crowded out",
			result.Diagnostics.ExpiredUnconfirmedStatements)
	}
	// The mode is off, so the claim checks do not run: the entry the
	// proposal brought is judged the way that mode has always judged it.
	if result.Diagnostics.RejectedClaims != 0 {
		t.Fatalf("rejected claims = %d, want 0: the mode does not run the claim checks",
			result.Diagnostics.RejectedClaims)
	}
	// And the document that is written is a valid version 3 one.
	content, err := model.MarshalSessionWhiteboardDocument(result.Board)
	if err != nil {
		t.Fatalf("the board that would be stored is not a valid document: %v", err)
	}
	stored, err := model.DecodeSessionWhiteboardDocument(content)
	if err != nil {
		t.Fatalf("the board that would be stored cannot be read back: %v", err)
	}
	if len(stored.Unverified) != 1 {
		t.Fatalf("the stored board carries %+v, want the learner's own words", stored.Unverified)
	}
}

// A deployment that never turned the mode on keeps the path it always had.
//
// There is no version 3 board to keep, so the board is written in version 1
// exactly as it was before this change, and it carries no statements.
func TestADeploymentThatNeverTurnedTheModeOnKeepsItsOwnPath(t *testing.T) {
	result, err := ApplyWhiteboardProposal(model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 26 页", "26 页/次", "", 51),
		},
	}, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 26 页", "26 页/次", "", 51),
		},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Board.SchemaVersion != model.SessionWhiteboardSchemaV1 {
		t.Fatalf("the board is version %d, want the version this mode has always written",
			result.Board.SchemaVersion)
	}
	if len(result.Board.Unverified) != 0 {
		t.Fatalf("the board carries %+v, want none", result.Board.Unverified)
	}
	rendered := renderWhiteboardBlock(result.Board, 2000)
	if !strings.HasPrefix(rendered.Text, whiteboardBlockHeader) {
		t.Fatalf("the block header changed: %s", rendered.Text)
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
