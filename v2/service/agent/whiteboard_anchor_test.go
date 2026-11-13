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
// The wording of the changes is the wording of a real evaluation item, so
// that the cases here are the cases a run produced: "读书计划改成每次 41 页" is
// what a learner wrote, and "不要改成 41 页" and "如果改成 41 页呢" are what
// they wrote when they did not mean it.

import (
	"context"
	"strings"
	"testing"

	"GopherAI/model"
)

// The learner's own words, as the frozen item wrote them.
const (
	anchorChangeText       = "手上这本书的内容变简单了，读书计划改成每次 41 页，原先那个页数不用了。"
	anchorNegatedText      = "读书计划不要改成 41 页，还是每次读 26 页。"
	anchorHypotheticalText = "读书计划如果改成 41 页会怎么样？"
)

// anchorBoard is the board the changes arrive at: the reading plan under the
// name the learner gave it, with the value they gave it first.
func anchorBoard() model.SessionWhiteboardDocument {
	return model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
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
// the learner's own sentence anyway, because the sentence is what the
// program can check.
func anchorParaphrasedClaim() WhiteboardLearnerClaim {
	return learnerClaim("pages", 95, "读书计划改成41页", "读书计划", "页", "41页", "decided", "yes")
}

// The change is confirmed, so it is the current value and nothing is left
// behind.
func TestAConfirmedChangeIsTheCurrentValueAndLeavesNothingBehind(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorClaim("decided", "yes")},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV2)
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
	}, anchorBatch(anchorNegatedText), model.SessionWhiteboardSchemaV2)
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
	}, anchorBatch(anchorHypotheticalText), model.SessionWhiteboardSchemaV2)
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
// entry is refused. The words stay findable, and they do not contest this
// entry: the program cannot place them on it.
func TestAnotherItemsValueDoesNotOverwriteThisOne(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "乙书每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "读书计划改成每次 41 页", "乙书", "页", "41页", "decided", "yes"),
		},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if got := boardValues(result.Board)["pages"]; got != "26 页/次" {
		t.Fatalf("the board holds %q, want the value that was there", got)
	}
	rendered := renderWhiteboardBlock(result.Board, 2000)
	if strings.Contains(rendered.Text, "未核验原话：") {
		t.Fatalf("the block contests the entry for words about another item: %s", rendered.Text)
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
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV2)
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
	if !strings.Contains(anchor.Quote, "读书计划改成每次 41 页") {
		t.Fatalf("the statement's quote = %q, want the learner's own words", anchor.Quote)
	}
	if anchor.Reason != model.SessionWhiteboardAnchorQuoteNotInMessage {
		t.Fatalf("the statement's reason = %q, want the one that checks the quote", anchor.Reason)
	}
}

// The board a reader sees says what the statement is, and the value next to
// it is not presented as the one true value.
func TestTheBoardSaysTheStatementIsNotTheCurrentValue(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorParaphrasedClaim()},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV2)
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
	if !strings.Contains(rendered.Text, "未核验原话：41 页/次") {
		t.Fatalf("the block does not mark the value as contested: %s", rendered.Text)
	}
	if !strings.Contains(rendered.Text, "may not be the current one") {
		t.Fatalf("the block does not say the marked value may not be the current one: %s", rendered.Text)
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
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV2)
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

// A statement the board has caught up with is dropped.
//
// The words are on the board now, so the statement has nothing left to say,
// and the board does not carry it forever.
func TestAStatementTheBoardCaughtUpWithIsDropped(t *testing.T) {
	current := anchorBoard()
	current.Unverified = []model.SessionWhiteboardAnchor{
		{Key: "pages", Subject: "读书计划", Value: "41 页/次", MessageID: 95,
			Quote: "读书计划改成每次 41 页", Reason: model.SessionWhiteboardAnchorQuoteNotInMessage},
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "读书计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{anchorClaim("decided", "yes")},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Unverified) != 0 {
		t.Fatalf("the board carries %+v, want nothing: the change was confirmed", result.Board.Unverified)
	}
}

// The reason the entry was refused is recorded, so a refusal is a place and
// not a count.
func TestTheReasonOfARefusalIsRecorded(t *testing.T) {
	result, err := ApplyWhiteboardProposal(anchorBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "乙书每次读 41 页", "41 页/次", model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "读书计划改成每次 41 页", "读书计划", "页", "41页", "decided", "yes"),
		},
	}, anchorBatch(anchorChangeText), model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics.ClaimCandidates) != 1 {
		t.Fatalf("the diagnostics carry %+v, want the refused candidate",
			result.Diagnostics.ClaimCandidates)
	}
	candidate := result.Diagnostics.ClaimCandidates[0]
	if candidate.Reason != model.SessionWhiteboardAnchorSubjectNotInEntry {
		t.Fatalf("the refusal's reason = %q, want the one that checks the subject", candidate.Reason)
	}
	if candidate.Verdict != whiteboardClaimVerdictLabelRejected {
		t.Fatalf("the refusal's verdict = %q, want rejected", candidate.Verdict)
	}
	if candidate.MessageID != 95 {
		t.Fatalf("the refusal names message %d, want the learner's own message", candidate.MessageID)
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
// reach the board. The learner's words do, and the window authorises reading
// them back.
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
	if !strings.Contains(rendered.Text, "读书计划改成每次 41 页") {
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
	if len(result.Diagnostics.ClaimCandidates) != 1 {
		t.Fatalf("the diagnostics carry %+v, want the refused candidate",
			result.Diagnostics.ClaimCandidates)
	}
	if result.Diagnostics.ClaimCandidates[0].Reason != model.SessionWhiteboardAnchorQuoteNotInMessage {
		t.Fatalf("the refusal's reason = %q, want the one that checks the quote",
			result.Diagnostics.ClaimCandidates[0].Reason)
	}
}
