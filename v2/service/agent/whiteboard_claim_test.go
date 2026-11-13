package agent

// The tests here are about what counts as the learner saying a value.
//
// A value that appears in the learner's message is not a value the learner
// said, and the program cannot tell the difference by looking for words: a
// negation nobody thought of is read as a plain statement, and a plain
// statement that was meant as a negation is written on the board as the
// learner's current value.
//
// So the evidence is stated: the model says which message it read, quotes it
// word for word, names the subject and the attribute, and reports what kind
// of statement it was. Every part of that is checked here, and so is the
// boundary of the check - the program can prove that the quote is the
// learner's own wording, and it cannot prove what the wording means.
//
// The other half is about which entry a new value belongs to. An entry that
// renames another one may take its key over, but only when the learner's
// words settle it, when both measure the same thing, and when there is
// exactly one entry it could be replacing.

import (
	"testing"

	"GopherAI/model"
)

// learnerClaim is the evidence one entry rests on.
//
// The update answer is "yes" unless the test says otherwise: most of these
// cases are about the checks that come before it, and the ones that are
// about the answer itself say so.
func learnerClaim(key string, messageID uint, quote, subject, attribute, value, stance string, update ...string) WhiteboardLearnerClaim {
	answer := whiteboardClaimUpdateYes
	if len(update) > 0 {
		answer = update[0]
	}
	return WhiteboardLearnerClaim{
		Key: key, MessageID: messageID, Quote: quote,
		Subject: subject, Attribute: attribute, Value: value, Stance: stance,
		Update: answer,
	}
}

// pagesBoard is a board that holds the reading plan the learner has.
func pagesBoard() model.SessionWhiteboardDocument {
	return model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 26 页", "26 页/次", "", 51),
		},
	}
}

// boardValues is what the board holds, by key.
func boardValues(board model.SessionWhiteboardDocument) map[string]string {
	values := map[string]string{}
	for _, group := range [][]model.SessionWhiteboardEntry{
		board.Constraints, board.Plan, board.OpenItems,
	} {
		for _, entry := range group {
			values[entry.Key] = entry.Value
		}
	}
	return values
}

// The learner ruled the value out. The entry says it is the learner's own,
// and the learner's own words say the value is not to be used, so the entry
// does not go on the board as a current fact.
func TestTheNegatedValueIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, "阅读计划不要改成41页，仍按26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划不要改成41页", "阅读计划", "页", "41页", "negated"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Diagnostics.UnsettledClaims != 1 {
		t.Fatalf("unsettled claims = %d, want 1", result.Diagnostics.UnsettledClaims)
	}
}

// The learner asked what would happen. Nothing was decided, so the board
// keeps what it has.
func TestTheHypotheticalValueIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, "阅读计划如果改成41页会怎样，暂时保持26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划如果改成41页会怎样", "阅读计划", "页", "41页", "hypothetical"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The learner proposed the value rather than settling it. A proposal is not
// a current fact, so the board keeps what it has.
func TestTheRequestedValueIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, "阅读计划能不能改成41页，我先想想", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划能不能改成41页", "阅读计划", "页", "41页", "proposed"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Diagnostics.UnsettledClaims != 1 {
		t.Fatalf("unsettled claims = %d, want 1", result.Diagnostics.UnsettledClaims)
	}
}

// The learner was not sure about the value. Not sure is not settled, so the
// board keeps what it has.
func TestTheUncertainValueIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, "阅读计划好像要改成41页，我不确定", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划好像要改成41页", "阅读计划", "页", "41页", "uncertain"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The value of another quantity is not this value. The claim says 41
// minutes and the entry says 41 pages, and the two are not the same value
// however much they look alike.
func TestTheValueOfAnotherUnitIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, "阅读计划每次读41分钟", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划每次读41分钟", "阅读计划", "分钟", "41分钟", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Diagnostics.RejectedClaims != 1 {
		t.Fatalf("rejected claims = %d, want 1", result.Diagnostics.RejectedClaims)
	}
}

// The learner ruled the value out on a board that holds nothing. There is no
// old value to keep, and a value nobody settled is not a value either.
func TestTheNegatedStatementIsNothingOnAnEmptyBoard(t *testing.T) {
	batch := assistantBatch(95, "阅读计划不要改成41页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
	}, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划不要改成41页", "阅读计划", "页", "41页", "negated"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The learner asked what would happen on a board that holds nothing. There
// is nothing to keep and nothing that was decided.
func TestTheHypotheticalStatementIsNothingOnAnEmptyBoard(t *testing.T) {
	batch := assistantBatch(95, "阅读计划如果改成41页会怎样", 96, "好的。")
	result, err := ApplyWhiteboardProposal(model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
	}, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划如果改成41页会怎样", "阅读计划", "页", "41页", "hypothetical"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The learner said the number about another thing. The subject of the claim
// is not the subject of the entry, so the claim is not evidence for it.
func TestTheValueOfAnotherThingIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, "另一本书41页，这本仍然26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "另一本书41页", "另一本书", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Diagnostics.RejectedClaims != 1 {
		t.Fatalf("rejected claims = %d, want 1", result.Diagnostics.RejectedClaims)
	}
}

// The learner settled the value and the model wrote the entry down as the
// assistant's. The learner's own words are the evidence, and the entry is
// the learner's current value.
func TestTheSettledValueIsTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读41页", "阅读计划", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	entry := result.Board.Constraints[0]
	if entry.Value != "41 页/次" {
		t.Fatalf("the board still carries %q, want the learner's new value", entry.Value)
	}
	if entry.EntryOrigin() != model.SessionWhiteboardOriginUser {
		t.Fatalf("the entry's origin = %q, want the learner's", entry.EntryOrigin())
	}
	if len(result.Changes) != 1 || result.Changes[0].OldValue != "26 页/次" {
		t.Fatalf("the change does not record what it replaced: %+v", result.Changes)
	}
	if result.Diagnostics.VerifiedClaims != 1 {
		t.Fatalf("verified claims = %d, want 1", result.Diagnostics.VerifiedClaims)
	}
}

// The quote has to be the learner's own wording. A quote that is not in the
// message is not evidence, whatever else it says.
func TestAQuoteThatIsNotInTheMessageIsNotEvidence(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读55页", "阅读计划", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Diagnostics.VerifiedClaims != 0 {
		t.Fatalf("verified claims = %d, want 0", result.Diagnostics.VerifiedClaims)
	}
}

// The claim has to rest on the learner's own message. The assistant
// confirming something is not the learner accepting it.
func TestTheAssistantsMessageIsNotTheLearnersEvidence(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 96, "往后按新的页数走", "阅读计划", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Diagnostics.VerifiedClaims != 0 {
		t.Fatalf("verified claims = %d, want 0", result.Diagnostics.VerifiedClaims)
	}
}

// The value of the claim has to be the value of the entry. A claim about
// another value of the same quantity proves nothing about this one.
func TestAClaimAboutAnotherValueIsNotEvidence(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读41页", "阅读计划", "页", "55页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The board is empty - the last batch failed to generate - and the learner
// says a value. The learner's own words are the only thing there is, and
// they are enough: the entry is the learner's, whatever the model labelled
// it, and it does not have to replace a value the board already held.
func TestTheLearnersStatementIsTheValueOnAnEmptyBoard(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV2}
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "好的，往后按41页来。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", model.SessionWhiteboardOriginAssistantAccepted, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读41页", "阅读计划", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	entry := result.Board.Constraints[0]
	if entry.Value != "41 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if entry.EntryOrigin() != model.SessionWhiteboardOriginUser {
		t.Fatalf("the entry's origin = %q, want the learner's", entry.EntryOrigin())
	}
	if len(entry.SourceMessageIDs) != 1 || entry.SourceMessageIDs[0] != 95 {
		t.Fatalf("the entry does not point at the learner's own words: %+v", entry.SourceMessageIDs)
	}
}

// A new entry that names the entry it replaces takes that entry's key, so
// the board carries one value for one subject.
func TestTheRenamedEntryTakesOverTheEntryItNames(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("reading_plan_name", "读书计划定名“栗矶”", "栗矶", "", 51),
			ownEntry("pages_per_session", "每次读 26 页", "26 页", "", 51),
		},
	}
	batch := assistantBatch(95, "栗矶改成每次41页", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("plan_name_pages", "读书计划定名“栗矶”，每次读 41 页（原 26 页不再用）",
				"栗矶，41 页/次", model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("plan_name_pages", 95, "栗矶改成每次41页", "栗矶", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 2 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	values := boardValues(result.Board)
	if values["pages_per_session"] != "栗矶，41 页/次" {
		t.Fatalf("the pages entry = %q, want the new value", values["pages_per_session"])
	}
	if len(result.Changes) != 1 || result.Changes[0].OldValue != "26 页" {
		t.Fatalf("the change does not record what it replaced: %+v", result.Changes)
	}
}

// Two entries about the same subject, one of them measured in pages and the
// other in minutes, are two entries. The new one takes over the entry of its
// own kind and leaves the other alone.
func TestTheNewEntryDoesNotTakeOverAnotherAttribute(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("read_pages", "每次读 26 页", "26 页", "", 51),
			ownEntry("read_minutes", "每次读 30 分钟", "30 分钟", "", 53),
		},
	}
	batch := assistantBatch(95, "阅读时长以后改成每次45分钟", 96, "往后按新的时长来。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("reading_time", "阅读时长改为 45 分钟（原 30 分钟不再用）", "45 分钟",
				model.SessionWhiteboardOriginAssistantAccepted, 53, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("reading_time", 95, "阅读时长以后改成每次45分钟", "阅读时长", "分钟", "45分钟", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["read_pages"] != "26 页" {
		t.Fatalf("the pages entry was taken over: %+v", result.Board.Constraints)
	}
	if values["read_minutes"] != "45 分钟" {
		t.Fatalf("the minutes entry was not taken over: %+v", result.Board.Constraints)
	}
}

// The same unit and the same number are not enough: the new entry is about
// another book, and it does not say which entry it replaces.
func TestTheNewEntryDoesNotTakeOverAnotherThingOfTheSameUnit(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("book_a_pages", "甲书每次读 26 页", "26 页", "", 51),
		},
	}
	batch := assistantBatch(95, "乙书以后每次读41页", 96, "往后按新的页数来。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("book_b_pages", "乙书每次读 41 页", "41 页",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("book_b_pages", 95, "乙书以后每次读41页", "乙书", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 2 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	values := boardValues(result.Board)
	if values["book_a_pages"] != "26 页" {
		t.Fatalf("the other book's entry was taken over: %+v", result.Board.Constraints)
	}
	if values["book_b_pages"] != "41 页" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The same number about another thing may not raise the source: the value
// matches, the number matches, and the subject does not, so the entry keeps
// the label the model gave it.
func TestTheSameNumberAboutAnotherThingDoesNotRaiseTheSource(t *testing.T) {
	batch := assistantBatch(95, "乙书以后每次读41页", 96, "往后按新的页数来。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "乙书以后每次读41页", "乙书", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Diagnostics.VerifiedClaims != 0 {
		t.Fatalf("verified claims = %d, want 0", result.Diagnostics.VerifiedClaims)
	}
}

// Two entries the new entry could be replacing: the engine cannot tell which
// one it replaces, so it replaces neither.
func TestTheNewEntryTakesOverNothingWhenTwoEntriesMatch(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("plan_a_pages", "甲方案每次读 26 页", "26 页", "", 51),
			ownEntry("plan_b_pages", "乙方案每次读 26 页", "26 页", "", 53),
		},
	}
	batch := assistantBatch(95, "阅读计划改成每次读41页", 96, "往后按新的页数来。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("plan_pages", "阅读计划改为每次读 41 页（原 26 页不用了）", "41 页",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("plan_pages", 95, "阅读计划改成每次读41页", "阅读计划", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 3 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	values := boardValues(result.Board)
	if values["plan_a_pages"] != "26 页" || values["plan_b_pages"] != "26 页" {
		t.Fatalf("one of the two entries was taken over: %+v", result.Board.Constraints)
	}
	if values["plan_pages"] != "41 页" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// A claim that cannot be placed is not used. Two entries of one proposal
// carry the same key, and the claim cannot be told apart from one to the
// other.
func TestAClaimThatCannotBePlacedIsNotUsed(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 26 页", "26 页/次", "", 51),
		},
	}
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读41页", "阅读计划", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if result.Diagnostics.VerifiedClaims != 0 {
		t.Fatalf("verified claims = %d, want 0", result.Diagnostics.VerifiedClaims)
	}
}

// An entry that changes a value the board already holds has to be backed by
// the learner's own words. The model wrote the entry as the learner's own
// and did not show the words, so the value on the board stays what it was.
func TestAValueIsNotReplacedWithoutTheLearnersWords(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Diagnostics.UnbackedChanges != 1 {
		t.Fatalf("unbacked changes = %d, want 1", result.Diagnostics.UnbackedChanges)
	}
}

// An entry without evidence is not raised to the learner's. The engine does
// not read a decision out of a sentence the model did not back up.
func TestAnEntryWithoutEvidenceIsNotRaisedToTheLearners(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if result.Diagnostics.VerifiedClaims != 0 {
		t.Fatalf("verified claims = %d, want 0", result.Diagnostics.VerifiedClaims)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The claim is evidence and evidence is optional, so a claim that arrives with
// a field the program does not read must not take the whole batch with it.
// The document around it stays strict: an unknown field there still fails.
func TestAClaimWithAnExtraFieldDoesNotFailTheBatch(t *testing.T) {
	document := `{"schema_version":1,"segment":{"topic":"阅读","points":[
	{"text":"阅读计划每次读 41 页","evidence_message_ids":[95],"source":"user"}]},
	"whiteboard":{"schema_version":1,"constraints":[
	{"key":"pages","text":"阅读计划每次读 41 页","value":"41 页/次","source_message_ids":[95],
	 "learner_claim":{"message_id":95,"quote":"阅读计划以后每次读41页","subject":"阅读计划",
	 "attribute":"每次读","value":"41页","stance":"decided","confidence":"high"}}]}}`
	decoded, err := decodeSegmentGenerationDocument(document)
	if err != nil {
		t.Fatalf("decode the document: %v", err)
	}
	if len(decoded.Whiteboard.Constraints) != 1 {
		t.Fatalf("constraints = %d, want 1", len(decoded.Whiteboard.Constraints))
	}
	claim := decoded.Whiteboard.Constraints[0].Claim
	if claim == nil {
		t.Fatal("the claim did not come through")
	}
	if claim.MessageID != 95 || claim.Stance != "decided" || claim.Quote == "" {
		t.Fatalf("the claim = %+v", claim)
	}
	// The rest of the document is still read strictly.
	strict := `{"schema_version":1,"segment":{"topic":"阅读","points":[
	{"text":"阅读计划每次读 41 页","evidence_message_ids":[95],"source":"user","tone":"plain"}]}}`
	if _, err := decodeSegmentGenerationDocument(strict); err == nil {
		t.Fatal("an unknown field in the document was ignored")
	}
}
