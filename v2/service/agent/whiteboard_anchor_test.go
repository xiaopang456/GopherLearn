package agent

// The tests here are about a refused change and about what the board does
// with the learner's words behind it.
//
// A change the program cannot confirm is refused on purpose: the value does
// not become the current state, and it must not become one by being dressed
// up as a fact. But the refusal used to take the learner's own words with it.
// The entry was dropped, the message was cited by nothing, and the reader
// was left with a board that said the old value and no sign that the learner
// had said something else.
//
// So the two are separated here. The value stays refused. The words are kept
// as an unconfirmed statement: findable, readable back, and never a current
// value.
//
// Three rules bound the statement, and each of them is a case here:
//
//   - it does not say which item it is about, because the claim it came from
//     was refused and nothing in a refused claim may be used to place the
//     words;
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
	anchorChangeText       = "读书计划改成每次 41 页，原先那个页数不用了。"
	anchorNegatedText      = "读书计划不要改成 41 页，还是每次读 26 页。"
	anchorHypotheticalText = "读书计划如果改成 41 页会怎么样？"
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

// anchorClaim is the evidence a change rests on.
func anchorClaim(stance, update string) WhiteboardLearnerClaim {
	return learnerClaim("pages", 95, "读书计划改成每次 41 页", "读书计划", "页", "41页", stance, update)
}

// anchorParaphrasedClaim is the evidence a generator writes when it read the
// words and rewrote them.
//
// It is the commonest shape of a refusal that is not a refusal of the
// learner: the words are the learner's, the quote is not. The board keeps
// the learner's own message anyway, because the message is what the program
// can check.
func anchorParaphrasedClaim() WhiteboardLearnerClaim {
	return learnerClaim("pages", 95, "读书计划改成41页", "读书计划", "页", "41页", "decided", "yes")
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

// The change is confirmed, so it is the current value and nothing is left
// behind.
func TestAConfirmedChangeIsTheCurrentValueAndLeavesNothingBehind(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorClaim("decided", "yes")},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if got := boardValues(result.Board)["pages"]; got != "41 页/次" {
		t.Fatalf("the board holds %q, want the learner's new value", got)
	}
	if len(result.Board.Unverified) != 0 {
		t.Fatalf("the board carries %+v, want no unconfirmed statement: the change was confirmed",
			result.Board.Unverified)
	}
}

// A value the learner ruled out is not a current value.
//
// The words are still findable, because a learner who said "do not change it
// to 41 pages" is worth being able to read back - but the statement names no
// candidate value, because the learner never put one up.
func TestANegatedChangeDoesNotBecomeTheCurrentValue(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "读书计划不要改成 41 页", "读书计划", "页", "41页", "negated", "no"),
		},
	}, anchorBatch(anchorNegatedText), model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if got := boardValues(result.Board)["pages"]; got != "26 页/次" {
		t.Fatalf("the board holds %q, want the value the learner kept", got)
	}
	if len(result.Board.Unverified) != 1 {
		t.Fatalf("the board carries %+v, want the learner's own words to stay findable",
			result.Board.Unverified)
	}
	anchor := result.Board.Unverified[0]
	if anchor.MessageID != 95 {
		t.Fatalf("the statement points at message %d, want the learner's own message", anchor.MessageID)
	}
	if anchor.Reason != model.SessionWhiteboardAnchorUnsettled {
		t.Fatalf("the statement's reason = %q, want the unsettled one", anchor.Reason)
	}
	rendered := renderWhiteboardBlock(result.Board, 2000)
	if strings.Contains(rendered.Text, "candidate value") {
		t.Fatalf("the block names a candidate value for a statement the learner ruled out: %s",
			rendered.Text)
	}
}

// A question about a value is not a value.
func TestAHypotheticalChangeDoesNotBecomeTheCurrentValue(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorClaim("hypothetical", "no")},
	}, anchorBatch(anchorHypotheticalText), model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if got := boardValues(result.Board)["pages"]; got != "26 页/次" {
		t.Fatalf("the board holds %q, want the value the learner kept", got)
	}
}

// Another item's value does not overwrite this one's.
//
// The claim is about the other book, and the entry is about this one, so the
// entry is refused. The words stay findable, and the board does not point
// them at any entry: the claim they came from was refused, so the item it
// named may not be used to place them.
func TestAnotherItemsValueDoesNotOverwriteThisOne(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "另一本书每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "读书计划改成每次 41 页", "另一本书", "页", "41页", "decided", "yes"),
		},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if got := boardValues(result.Board)["pages"]; got != "26 页/次" {
		t.Fatalf("the board holds %q, want the value that was there", got)
	}
	rendered := renderWhiteboardBlock(result.Board, 2000)
	if strings.Contains(rendered.Text, "未核验原话：") {
		t.Fatalf("the block marks an entry for words the program cannot place: %s", rendered.Text)
	}
}

// One message about two things does not put one of them on the board.
//
// The learner's own message says both: the reading plan goes to 41 pages and
// the other book to 30. The board keeps what it held, and the statement keeps
// the message, without saying which of the two things it is about.
func TestOneMessageAboutTwoThingsDoesNotPutEitherOfThemOnTheBoard(t *testing.T) {
	message := "读书计划改成每次 41 页，另一本书改成每次 30 页。"
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorClaim("decided", "yes")},
	}, anchorBatch(message), model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if got := boardValues(result.Board)["pages"]; got != "26 页/次" {
		t.Fatalf("the board holds %q, want the value that was there", got)
	}
	if len(result.Board.Unverified) != 1 {
		t.Fatalf("the board carries %+v, want the learner's own message", result.Board.Unverified)
	}
	if !strings.Contains(result.Board.Unverified[0].Text, "另一本书改成每次 30 页") {
		t.Fatalf("the statement's text = %q, want the whole message",
			result.Board.Unverified[0].Text)
	}
}

// The refused change leaves the value alone and keeps the words findable.
//
// This is the case the whole change is about: the learner's words are the
// learner's own, they are real, and they used to go with the refused entry.
func TestARefusedChangeKeepsTheLearnersWordsFindable(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorParaphrasedClaim()},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV3)
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
	if anchor.Reason != model.SessionWhiteboardAnchorQuoteNotInMessage {
		t.Fatalf("the statement's reason = %q, want the one that checks the quote", anchor.Reason)
	}
}

// The board a reader sees says what the statement is, and does not promise
// that the value it shows is the current one.
func TestTheBoardDoesNotCallTheValueItShowsTheCurrentOne(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorParaphrasedClaim()},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	rendered := renderWhiteboardBlock(result.Board, 2000)
	if !strings.Contains(rendered.Text, "Unconfirmed learner statements") {
		t.Fatalf("the block does not introduce the statements: %s", rendered.Text)
	}
	if !strings.Contains(rendered.Text, "not a current value") {
		t.Fatalf("the block does not say the statements are not the current value: %s", rendered.Text)
	}
	if strings.Contains(rendered.Text, "the unmarked value is the current one") {
		t.Fatalf("the block still calls the value it shows the current one: %s", rendered.Text)
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
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorParaphrasedClaim()},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV3)
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
			Reason: model.SessionWhiteboardAnchorQuoteNotInMessage},
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorClaim("decided", "yes")},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Unverified) != 0 {
		t.Fatalf("the board carries %+v, want nothing: the change was confirmed", result.Board.Unverified)
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
			Reason: model.SessionWhiteboardAnchorQuoteNotInMessage},
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 26 页", "26 页/次", model.SessionWhiteboardOriginUser, 51, 95),
		},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV3)
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
			Reason:             model.SessionWhiteboardAnchorQuoteNotInMessage,
			UpdatedAtMessageID: uint(100 + i),
		})
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, anchorBatch(anchorChangeText),
		model.SessionWhiteboardSchemaV3)
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
	message := anchorLongText(400, "但是不要改成 41 页。")
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorParaphrasedClaim()},
	}, anchorBatch(message), model.SessionWhiteboardSchemaV3)
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
	if !strings.HasSuffix(anchor.Text, "但是不要改成 41 页。") {
		t.Fatalf("the statement's text ends with %q, want the negation it ends with",
			anchor.Text[len(anchor.Text)-20:])
	}
	rendered := renderWhiteboardBlock(result.Board, 4000)
	if !strings.Contains(rendered.Text, "但是不要改成 41 页。") {
		t.Fatalf("the window does not show the end of the message: %s", rendered.Text)
	}
}

// A long message that ends in a condition is kept whole, for the same reason.
func TestALongMessageThatEndsInAConditionIsKeptWhole(t *testing.T) {
	message := anchorLongText(400, "改成每次 41 页，如果太忙就改成 30 页。")
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorParaphrasedClaim()},
	}, anchorBatch(message), model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Unverified) != 1 {
		t.Fatalf("the board carries %+v, want the learner's own message", result.Board.Unverified)
	}
	if !strings.HasSuffix(result.Board.Unverified[0].Text, "如果太忙就改成 30 页。") {
		t.Fatalf("the statement's text ends with %q, want the condition it ends with",
			result.Board.Unverified[0].Text[len(result.Board.Unverified[0].Text)-20:])
	}
	rendered := renderWhiteboardBlock(result.Board, 4000)
	if !strings.Contains(rendered.Text, "如果太忙就改成 30 页。") {
		t.Fatalf("the window does not show the end of the message: %s", rendered.Text)
	}
}

// A message too long to be carried whole becomes a pointer.
//
// The reader is told to read it back and keeps the message id. Half of the
// message is not shown, and the candidate value is not shown either: a value
// named next to a fragment reads as a decision.
func TestAMessageTooLongToCarryBecomesAPointerThatCanBeReadBack(t *testing.T) {
	message := anchorLongText(model.SessionWhiteboardAnchorMessageMaxRunes+120, "这次就按 41 页走。")
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorParaphrasedClaim()},
	}, anchorBatch(message), model.SessionWhiteboardSchemaV3)
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
	if strings.Contains(rendered.Text, "candidate value") {
		t.Fatalf("the block names a candidate value for a statement it did not show: %s", rendered.Text)
	}
	if !strings.Contains(rendered.Text, "read it back to see what was said") {
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

// The reason the entry was refused is recorded, so a refusal is a place and
// not a count.
func TestTheReasonOfARefusalIsRecorded(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "另一本书每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "读书计划改成每次 41 页", "读书计划", "页", "41页", "decided", "yes"),
		},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics.Entries) != 1 {
		t.Fatalf("the diagnostics carry %+v, want the refused entry",
			result.Diagnostics.Entries)
	}
	entry := result.Diagnostics.Entries[0]
	if entry.ClaimReason != model.SessionWhiteboardAnchorSubjectNotInEntry {
		t.Fatalf("the refusal's reason = %q, want the one that checks the subject", entry.ClaimReason)
	}
	if entry.ClaimVerdict != whiteboardClaimVerdictLabelRejected {
		t.Fatalf("the refusal's verdict = %q, want rejected", entry.ClaimVerdict)
	}
	if entry.ClaimMessageID != 95 {
		t.Fatalf("the refusal names message %d, want the learner's own message", entry.ClaimMessageID)
	}
	if entry.Outcome != whiteboardEntryOutcomeRefused {
		t.Fatalf("the entry's outcome = %q, want refused", entry.Outcome)
	}
	if entry.OutcomeReason != model.SessionWhiteboardAnchorSubjectNotInEntry {
		t.Fatalf("the entry's reason = %q, want the one that checks the subject", entry.OutcomeReason)
	}
}

// The mode that does not record what the assistant produced is untouched.
//
// It does not run the claim checks, so it has no refusals and no statements,
// and the block it sends is the block it always sent.
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
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV1)
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
// The model's quote is not the learner's wording, so the value does not
// reach the board. The learner's message does, and the window authorises
// reading it back.
func TestTheWholePathFromTheAnswerToTheReadBack(t *testing.T) {
	answer := `{"schema_version":1,"segment":{"topic":"读书计划的页数","points":[` +
		`{"text":"学习者说了新的页数","evidence_message_ids":[95],"source":"user"}]},` +
		`"whiteboard":{"schema_version":1,"constraints":[` +
		`{"key":"pages","text":"读书计划每次读 41 页","value":"41 页/次",` +
		`"source_message_ids":[95],` +
		`"learner_claim":{"message_id":95,"quote":"读书计划改成41页","subject":"读书计划",` +
		`"attribute":"页","value":"41页","stance":"decided","update":"yes"}}]}}`
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
	// The value did not reach the board: the claim was refused.
	if got := boardValues(result.Whiteboard)["pages"]; got != "" {
		t.Fatalf("the board holds %q, want no value for the refused entry", got)
	}
	// The learner's words did.
	if len(result.Whiteboard.Unverified) != 1 {
		t.Fatalf("the board carries %+v, want the learner's own words", result.Whiteboard.Unverified)
	}
	// The window shows them and authorises reading them back.
	rendered := renderWhiteboardBlock(result.Whiteboard, 2000)
	if !strings.Contains(rendered.Text, anchorChangeText) {
		t.Fatalf("the window does not show the learner's words: %s", rendered.Text)
	}
	layered := LayeredSummarySnapshot{
		Board: &model.SessionWhiteboard{CoveredMessageID: 96, Version: 1},
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
	// And the refusal says which check refused it.
	if len(result.Diagnostics.Entries) != 1 {
		t.Fatalf("the diagnostics carry %+v, want the refused entry",
			result.Diagnostics.Entries)
	}
	if result.Diagnostics.Entries[0].ClaimReason != model.SessionWhiteboardAnchorQuoteNotInMessage {
		t.Fatalf("the refusal's reason = %q, want the one that checks the quote",
			result.Diagnostics.Entries[0].ClaimReason)
	}
	// The claim failed, the entry was refused, and the learner's words were
	// kept findable: the three layers say three different things, and a
	// reader has to be able to see all three.
	entry := result.Diagnostics.Entries[0]
	if entry.ClaimVerdict != whiteboardClaimVerdictLabelRejected {
		t.Fatalf("the claim's verdict = %q, want rejected", entry.ClaimVerdict)
	}
	if entry.Outcome != whiteboardEntryOutcomeRefused {
		t.Fatalf("the entry's outcome = %q, want refused", entry.Outcome)
	}
	if entry.AnchorMessageID == 0 {
		t.Fatalf("the entry carries no anchor: %+v", entry)
	}
	if entry.AnchorReason != model.SessionWhiteboardAnchorQuoteNotInMessage {
		t.Fatalf("the anchor's reason = %q, want the one that checks the quote",
			entry.AnchorReason)
	}
}
