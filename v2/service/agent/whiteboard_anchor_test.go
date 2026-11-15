package agent

// A refused entry, and what the board does with the learner's words behind it.
//
// An entry the program cannot place is refused on purpose: the value does not
// become the current state, and it must not become one by being dressed up as
// a fact. But the refusal used to take the learner's own words with it. The
// entry was dropped, the message was cited by nothing, and the reader was
// left with a board that said the old value and no sign that the learner had
// said something else.
//
// So the two are separated here. The value stays refused. The words are kept
// as an unconfirmed statement: findable, readable back, and never a current
// value.
//
// Three rules bound the statement, and each of them is a case here:
//
//   - it does not say which item it is about, because a refused entry may not
//     be used to place the words;
//   - the words are whole or not shown, because a message that is cut in half
//     hides the condition or the negation at its end;
//   - it is cleared only when the board demonstrably holds what it pointed
//     at, and being crowded out is not that.

import (
	"context"
	"strings"
	"testing"

	"GopherAI/model"
)

// The learner's own words, as a run produced them.
const (
	anchorChangeText  = "读书计划改成每次 41 页，原先那个页数不用了。"
	anchorLongEnding  = "但是不要改成 41 页。"
	anchorPointerText = "这次就按 41 页走。"
)

// anchorBoard is the board the changes arrive at: the reading plan with the
// value the learner gave it first.
func anchorBoard() model.SessionWhiteboardDocument {
	return model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 26 页", "26 页/次", "", 51),
		},
	}
}

// anchorBatch is the batch the changes arrive in.
func anchorBatch(text string) WhiteboardBatch {
	return assistantBatch(95, text, 96, "往后按新的页数走。")
}

// anchorLongText builds a message of the given length that ends with the
// given words.
//
// The end is the part that matters: a condition or a negation at the end of
// a long sentence is what a cut would hide.
func anchorLongText(runes int, ending string) string {
	filler := strings.Repeat("读书的进度我记在本子上 ", runes/12+1)
	text := []rune(filler)
	if len(text) > runes {
		text = text[:runes]
	}
	return string(text) + ending
}

// The entry names an origin the program does not know.
//
// The label is the model's own word about where the entry came from, and a
// label the program cannot read is not a reason to lose the learner's words.
// The value is refused; the words stay findable.
func TestARefusedEntryKeepsTheLearnersWordsFindable(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", "assistant_accepted_by_the_learner", 95),
		},
	}, anchorBatch(anchorChangeText), assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	// The value is refused: the board still holds what it held.
	if got := boardValues(result.Board)["pages"]; got != "26 页/次" {
		t.Fatalf("the board holds %q, want the value that was there", got)
	}
	if len(result.Board.Unverified) != 1 {
		t.Fatalf("the board carries %+v, want the learner's own words", result.Board.Unverified)
	}
	anchor := result.Board.Unverified[0]
	if anchor.MessageID != 95 {
		t.Fatalf("the statement points at message %d, want the learner's own message", anchor.MessageID)
	}
	if !anchor.Complete {
		t.Fatal("the statement does not carry the whole message")
	}
	if anchor.Text != anchorChangeText {
		t.Fatalf("the statement's text = %q, want the learner's own message", anchor.Text)
	}
	if anchor.Reason != model.SessionWhiteboardAnchorUnplaced {
		t.Fatalf("the statement's reason = %q, want the one that says it was not placed",
			anchor.Reason)
	}
}

// The reason the entry was refused is recorded, so a refusal is a place and
// not a count.
func TestTheReasonOfARefusalIsRecorded(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", "assistant_accepted_by_the_learner", 95),
		},
	}, anchorBatch(anchorChangeText), assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics.Entries) != 1 {
		t.Fatalf("the diagnostics carry %+v, want the refused entry",
			result.Diagnostics.Entries)
	}
	entry := result.Diagnostics.Entries[0]
	if entry.Outcome != whiteboardEntryOutcomeRefused {
		t.Fatalf("the entry's outcome = %q, want refused", entry.Outcome)
	}
	if entry.OutcomeReason != whiteboardEntryReasonOriginUnknown {
		t.Fatalf("the entry's reason = %q, want the one about the unknown origin",
			entry.OutcomeReason)
	}
	if result.Diagnostics.UnknownOrigins != 1 {
		t.Fatalf("unknown origins = %d, want 1", result.Diagnostics.UnknownOrigins)
	}
}

// The board a reader sees says what the statement is, and does not promise
// that the value it shows is the current one.
func TestTheBoardDoesNotCallTheValueItShowsTheCurrentOne(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", "assistant_accepted_by_the_learner", 95),
		},
	}, anchorBatch(anchorChangeText), assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	rendered := renderWhiteboardBlock(result.Board, 2000)
	if !strings.Contains(rendered.Text, "Unconfirmed learner statements") {
		t.Fatalf("the block does not introduce the statements: %s", rendered.Text)
	}
	if !strings.Contains(rendered.Text, "not the current value") {
		t.Fatalf("the block does not say the statements are not the current value: %s", rendered.Text)
	}
	if !strings.Contains(rendered.Text, "may not be the current one") {
		t.Fatalf("the block does not say a value shown may not be the current one: %s", rendered.Text)
	}
}

// The message the words came from is authorised for reading back.
//
// The grant is what the window gives the model, and it is drawn from what
// really reached the window: a statement that was not shown authorises
// nothing.
func TestTheRefusedChangeAuthorisesReadingTheLearnersWordsBack(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", "assistant_accepted_by_the_learner", 95),
		},
	}, anchorBatch(anchorChangeText), assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	rendered := renderWhiteboardBlock(result.Board, 2000)
	granted := false
	for _, id := range rendered.EvidenceMessageIDs {
		if id == 95 {
			granted = true
		}
	}
	if !granted {
		t.Fatalf("the window shows %v, want the learner's own message among them",
			rendered.EvidenceMessageIDs)
	}
	layered := LayeredSummarySnapshot{
		Board: &model.SessionWhiteboard{CoveredMessageID: 95, Version: 3},
	}
	grants := whiteboardHistoryGrants(rendered, layered, "session-1")
	found := false
	for _, grant := range grants {
		if grant.MessageID == 95 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the read-back grants = %+v, want the learner's own message", grants)
	}
}

// A statement the board has caught up with is cleared, and counted as
// resolved.
func TestAStatementTheBoardCaughtUpWithIsCleared(t *testing.T) {
	current := anchorBoard()
	current.Unverified = []model.SessionWhiteboardAnchor{
		{Key: "pages", Value: "41 页/次", MessageID: 95,
			Text: anchorChangeText, Complete: true,
			Reason: model.SessionWhiteboardAnchorUnplaced},
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
	}, anchorBatch(anchorChangeText), assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Unverified) != 0 {
		t.Fatalf("the board carries %+v, want nothing: the board caught up with the statement",
			result.Board.Unverified)
	}
	if result.Diagnostics.ResolvedUnconfirmedStatements != 1 {
		t.Fatalf("resolved statements = %d, want 1", result.Diagnostics.ResolvedUnconfirmedStatements)
	}
	if result.Diagnostics.ExpiredUnconfirmedStatements != 0 {
		t.Fatalf("expired statements = %d, want 0: the statement was resolved, not crowded out",
			result.Diagnostics.ExpiredUnconfirmedStatements)
	}
}

// Another entry citing the same message does not clear the statement.
//
// It says the message was used for something. It does not say the words
// behind the statement were confirmed, so the statement stays.
func TestAnotherEntryCitingTheSameMessageDoesNotClearTheStatement(t *testing.T) {
	current := anchorBoard()
	current.Unverified = []model.SessionWhiteboardAnchor{
		{Key: "pages", Value: "41 页/次", MessageID: 95,
			Text: anchorChangeText, Complete: true,
			Reason: model.SessionWhiteboardAnchorUnplaced},
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 26 页", "26 页/次", model.SessionWhiteboardOriginUser, 51, 95),
		},
	}, anchorBatch(anchorChangeText), assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Unverified) != 1 {
		t.Fatalf("the board carries %+v, want the statement to stay", result.Board.Unverified)
	}
	if result.Diagnostics.ResolvedUnconfirmedStatements != 0 {
		t.Fatalf("resolved statements = %d, want 0: another entry citing the message is not a resolution",
			result.Diagnostics.ResolvedUnconfirmedStatements)
	}
}

// A statement that is crowded out is expired, not resolved.
//
// The two are counted apart, because the difference is what a reader of the
// counters needs: one was dealt with, the other was forgotten.
func TestAStatementThatIsCrowdedOutIsExpiredNotResolved(t *testing.T) {
	current := anchorBoard()
	for i := 0; i < model.SessionWhiteboardAnchorLimit+2; i++ {
		current.Unverified = append(current.Unverified, model.SessionWhiteboardAnchor{
			Key: "pages", Value: "41 页/次", MessageID: uint(100 + i),
			Text: anchorChangeText, Complete: true,
			Reason:             model.SessionWhiteboardAnchorUnplaced,
			UpdatedAtMessageID: uint(100 + i),
		})
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, anchorBatch(anchorChangeText),
		assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Unverified) != model.SessionWhiteboardAnchorLimit {
		t.Fatalf("the board carries %d statements, want %d", len(result.Board.Unverified),
			model.SessionWhiteboardAnchorLimit)
	}
	if result.Diagnostics.ExpiredUnconfirmedStatements != 2 {
		t.Fatalf("expired statements = %d, want 2", result.Diagnostics.ExpiredUnconfirmedStatements)
	}
	if result.Diagnostics.ResolvedUnconfirmedStatements != 0 {
		t.Fatalf("resolved statements = %d, want 0: crowding out is not a resolution",
			result.Diagnostics.ResolvedUnconfirmedStatements)
	}
}

// A long message that ends in a negation is kept whole.
//
// A cut would have shown the value and hidden the negation, which reads as a
// decision the learner never made.
func TestALongMessageThatEndsInANegationIsKeptWhole(t *testing.T) {
	message := anchorLongText(400, anchorLongEnding)
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", "assistant_accepted_by_the_learner", 95),
		},
	}, anchorBatch(message), assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Unverified) != 1 {
		t.Fatalf("the board carries %+v, want the learner's own message", result.Board.Unverified)
	}
	anchor := result.Board.Unverified[0]
	if !anchor.Complete {
		t.Fatal("the statement does not carry the whole message")
	}
	if !strings.HasSuffix(anchor.Text, anchorLongEnding) {
		t.Fatalf("the statement's text ends with %q, want the negation it ends with",
			anchor.Text[len(anchor.Text)-20:])
	}
	rendered := renderWhiteboardBlock(result.Board, 4000)
	if !strings.Contains(rendered.Text, anchorLongEnding) {
		t.Fatalf("the window does not show the end of the message: %s", rendered.Text)
	}
}

// A message too long to be carried whole becomes a pointer.
//
// The reader is told to read it back and keeps the message id. Half of the
// message is not shown, and the candidate value is not shown either: a value
// named next to a fragment reads as a decision.
func TestAMessageTooLongToCarryBecomesAPointerThatCanBeReadBack(t *testing.T) {
	message := anchorLongText(model.SessionWhiteboardAnchorMessageMaxRunes+120, anchorPointerText)
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", "assistant_accepted_by_the_learner", 95),
		},
	}, anchorBatch(message), assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Unverified) != 1 {
		t.Fatalf("the board carries %+v, want the learner's own message", result.Board.Unverified)
	}
	anchor := result.Board.Unverified[0]
	if anchor.Complete || anchor.Text != "" {
		t.Fatalf("the statement carries %q, want no text at all", anchor.Text)
	}
	if anchor.MessageID != 95 {
		t.Fatalf("the statement points at message %d, want the learner's own message", anchor.MessageID)
	}
	rendered := renderWhiteboardBlock(result.Board, 4000)
	if !strings.Contains(rendered.Text, "read them back") {
		t.Fatalf("the block does not tell the reader to read the message back: %s", rendered.Text)
	}
	if !strings.Contains(rendered.Text, "[message_ids=95]") {
		t.Fatalf("the block does not carry the message id: %s", rendered.Text)
	}
	granted := false
	for _, id := range rendered.EvidenceMessageIDs {
		if id == 95 {
			granted = true
		}
	}
	if !granted {
		t.Fatalf("the window shows %v, want the learner's own message among them",
			rendered.EvidenceMessageIDs)
	}
}

// The mode that does not record what the assistant produced is untouched.
//
// It does not record the products of the assistant, so it has no statements
// to write, and the block it sends is the block it always sent.
func TestTheModeThatDoesNotRecordTheAssistantIsUnchanged(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", "", 95),
		},
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", "", 95),
		},
	}, anchorBatch(anchorChangeText), learnerOnlyPolicy(model.SessionWhiteboardSchemaV1))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Unverified) != 0 {
		t.Fatalf("the board carries %+v, want nothing", result.Board.Unverified)
	}
	rendered := renderWhiteboardBlock(result.Board, 2000)
	if strings.Contains(rendered.Text, "Unconfirmed learner statements") {
		t.Fatalf("the block carries statements the mode never writes: %s", rendered.Text)
	}
	if !strings.HasPrefix(rendered.Text, whiteboardBlockHeader) {
		t.Fatalf("the block header changed: %s", rendered.Text)
	}
}

// The whole path: the model's answer, the board it produced, the window the
// board reaches, and the read-back the window authorises.
//
// The model's answer names an origin the program cannot read, so the value
// does not reach the board. The learner's message does, and the window
// authorises reading it back.
func TestTheWholePathFromTheAnswerToTheReadBack(t *testing.T) {
	answer := `{"schema_version":1,"segment":{"topic":"读书计划的页数","points":[` +
		`{"text":"学习者说了新的页数","evidence_message_ids":[95],"source":"user"}]},` +
		`"whiteboard":{"schema_version":1,"constraints":[` +
		`{"key":"pages","text":"读书计划每次读 41 页","value":"41 页/次",` +
		`"source_message_ids":[95],"origin":"assistant_accepted_by_the_learner"}]}}`
	stub := &recordingSegmentModel{answers: []string{answer}}
	generator := rewritingGenerator(t, stub)
	result, err := generator.GenerateSegment(context.Background(), SegmentGenerationInput{
		Messages: []model.Message{
			{ID: 95, IsUser: true, Content: anchorChangeText},
			{ID: 96, IsUser: false, Content: "往后按新的页数走。"},
		},
		FirstMessageID: 95,
		LastMessageID:  96,
	})
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	// The value did not reach the board: the origin could not be read.
	if got := boardValues(result.Whiteboard)["pages"]; got != "" {
		t.Fatalf("the board holds %q, want no value", got)
	}
	// The learner's own message did: it is kept as a statement.
	if len(result.Whiteboard.Unverified) != 1 {
		t.Fatalf("the board carries %+v, want the learner's own words", result.Whiteboard.Unverified)
	}
	rendered := renderWhiteboardBlock(result.Whiteboard, 2000)
	found := false
	for _, id := range rendered.EvidenceMessageIDs {
		if id == 95 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the window shows %v, want the learner's own message among them",
			rendered.EvidenceMessageIDs)
	}
}
