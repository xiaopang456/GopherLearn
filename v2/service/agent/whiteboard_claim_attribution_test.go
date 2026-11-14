package agent

// Which thing a number belongs to.
//
// A number is not the name of a thing. The pages a learner planned to read
// and the pages they actually read are two things, and they may carry the
// same number; the same is true of two books. An entry that carries the
// number the board already holds must not be allowed to decide on its own
// that the two are one thing, because the fact it replaces is a fact the
// learner still has.
//
// These tests walk the counterexamples through the product's own write
// path, so what they show is what the board ends up holding:
//
//   - the learner updates one book and the proposal writes another one,
//     with the same number, the same unit and the same scope;
//   - the learner reports the pages they actually read and the board holds
//     the pages of the plan, with the same number, the same unit and the
//     same scope.
//
// The last two tests are the other side of the rule: a number that is new
// still takes the place of the old one, and an entry that says the same
// thing is not a change at all.
//
// Nothing here calls a model. Every case is the product's own path over
// words that really were said.

import (
	"testing"

	"GopherAI/model"
)

// The learner's own words.
const (
	// firstBookMessage is a page count the learner settled about one book.
	firstBookMessage = "甲书以后每次读41页"
	// pagesActuallyReadMessage is a page count the learner reports about the
	// pages they read, not about the pages they planned to read.
	pagesActuallyReadMessage = "阅读计划这次实际读完了41页"
)

// planPagesBoard is a board that holds the reading plan of the learner.
//
// The plan is a page count per session, so it names how often it holds.
func planPagesBoard() model.SessionWhiteboardDocument {
	return model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("reading_plan_pages", "阅读计划每次读 41 页", "41 页", "", 51),
		},
	}
}

// The learner updated one book and the proposal wrote the other one.
//
// The number is the same, the unit is the same and the scope is the same,
// and the two books are two things. The board keeps the book the learner
// spoke about, and the other one does not appear.
func TestTheNumberOfAnotherBookIsNotTheNumberOfThisOne(t *testing.T) {
	batch := assistantBatch(95, firstBookMessage, 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("book_one_pages", "甲书每次读 26 页", "26 页", "", 51),
		},
	}, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("book_two_pages", "乙书每次读 41 页", "41 页", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("book_two_pages", 95, firstBookMessage, "甲书", "每次读多少页", "41 页",
				whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["book_one_pages"] != "26 页" {
		t.Fatalf("the board = %+v, want the book the learner spoke about", result.Board.Constraints)
	}
	if _, found := values["book_two_pages"]; found {
		t.Fatalf("the board carries the other book: %+v", result.Board.Constraints)
	}
	if result.Diagnostics.VerifiedClaims != 0 {
		t.Fatalf("verified claims = %d, want 0", result.Diagnostics.VerifiedClaims)
	}
	if result.Diagnostics.RejectedClaims != 1 {
		t.Fatalf("rejected claims = %d, want 1", result.Diagnostics.RejectedClaims)
	}
}

// The pages the learner read are not the pages of the plan.
//
// The plan is a page count per session and the learner reports the pages
// they read. The number is the same, the unit is the same and the scope is
// the same, and the two are two things: the plan is a fact the learner
// still has. So the entry may not take the plan's place, and the learner's
// own words are kept so that a reader can find them.
func TestThePagesActuallyReadAreNotThePagesOfThePlan(t *testing.T) {
	batch := assistantBatch(95, pagesActuallyReadMessage, 96, "好的。")
	result, err := ApplyWhiteboardProposal(planPagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("reading_plan_pages", "阅读计划实际完成 41 页", "41 页", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("reading_plan_pages", 95, pagesActuallyReadMessage, "阅读计划", "实际完成页数",
				"41 页", whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := boardEntry(result.Board, "reading_plan_pages"); value.Value != "41 页" ||
		value.Text != "阅读计划每次读 41 页" {
		t.Fatalf("the board = %+v, want the plan the learner has", result.Board.Constraints)
	}
	if result.Diagnostics.VerifiedClaims != 1 {
		t.Fatalf("verified claims = %d, want 1", result.Diagnostics.VerifiedClaims)
	}
	if result.Diagnostics.DroppedEntries != 1 {
		t.Fatalf("dropped entries = %d, want 1", result.Diagnostics.DroppedEntries)
	}
	// The entry was refused, and the refusal says why: the number it
	// carries is the number the board already holds.
	if len(result.Diagnostics.Entries) != 1 {
		t.Fatalf("the run recorded %d entries, want 1", len(result.Diagnostics.Entries))
	}
	entry := result.Diagnostics.Entries[0]
	if entry.Outcome != whiteboardEntryOutcomeRefused {
		t.Fatalf("the entry's outcome = %q, want %q", entry.Outcome, whiteboardEntryOutcomeRefused)
	}
	if entry.OutcomeReason != model.SessionWhiteboardAnchorSameNumber {
		t.Fatalf("the entry's reason = %q, want %q", entry.OutcomeReason,
			model.SessionWhiteboardAnchorSameNumber)
	}
}

// The same two things, and the proposal gave the new entry a name of its
// own. The new entry may not take the plan's key either: the number is not
// what makes them one thing.
func TestThePagesActuallyReadDoNotTakeOverThePlan(t *testing.T) {
	batch := assistantBatch(95, pagesActuallyReadMessage, 96, "好的。")
	result, err := ApplyWhiteboardProposal(planPagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages_actually_read", "阅读计划实际完成 41 页", "41 页", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages_actually_read", 95, pagesActuallyReadMessage, "阅读计划", "实际完成页数",
				"41 页", whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["reading_plan_pages"] != "41 页" {
		t.Fatalf("the plan was taken over: %+v", result.Board.Constraints)
	}
	if values["pages_actually_read"] != "41 页" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// A number that is new takes the place of the old one.
//
// The rule is about a number that says nothing new. A learner who changes
// the number of their reading plan gets the new number on the board.
func TestANewNumberStillTakesThePlaceOfTheOldOne(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读55页", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(planPagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("reading_plan_pages", "阅读计划每次读 55 页", "55 页", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("reading_plan_pages", 95, "阅读计划以后每次读55页", "阅读计划", "每次读多少页",
				"55 页", whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	value, found := boardEntry(result.Board, "reading_plan_pages")
	if !found {
		t.Fatalf("the board does not carry the reading plan: %+v", result.Board.Constraints)
	}
	if value.Value != "55 页" {
		t.Fatalf("the board carries %q, want the learner's new value", value.Value)
	}
	if result.Diagnostics.VerifiedClaims != 1 {
		t.Fatalf("verified claims = %d, want 1", result.Diagnostics.VerifiedClaims)
	}
}

// A step that only moves along is not a change of what it is about.
//
// The board holds the value and the wording, and the entry says the same
// thing and only marks the step as done. A number that says nothing new
// is not the reason to refuse that: what the step is about has not
// changed, only where it is.
func TestTheStepThatOnlyMovesAlongIsNotRefused(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Plan: []model.SessionWhiteboardEntry{
			{Key: "reading_plan_pages", Text: "阅读计划每次读 41 页", Value: "41 页",
				State: "open", SourceMessageIDs: []uint{51}},
		},
	}
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Plan: []model.SessionWhiteboardEntry{
			{Key: "reading_plan_pages", Text: "阅读计划每次读 41 页", Value: "41 页",
				State: "done", SourceMessageIDs: []uint{95}},
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("reading_plan_pages", 95, "阅读计划以后每次读41页", "阅读计划", "每次读多少页",
				"41 页", whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	entry, found := boardEntry(result.Board, "reading_plan_pages")
	if !found {
		t.Fatalf("the board does not carry the plan: %+v", result.Board.Plan)
	}
	if entry.State != "done" {
		t.Fatalf("the step's state = %q, want %q", entry.State, "done")
	}
	if result.Diagnostics.DroppedEntries != 0 {
		t.Fatalf("dropped entries = %d, want 0", result.Diagnostics.DroppedEntries)
	}
}

// An entry that says the same thing is not a change, and a rule about
// changes does not refuse it.
//
// The board already holds this value and this wording, so the proposal
// changes nothing and the entry stays where it is.
func TestTheSameNumberInTheSameWordsIsNotRefused(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(planPagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("reading_plan_pages", "阅读计划每次读 41 页", "41 页", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("reading_plan_pages", 95, "阅读计划以后每次读41页", "阅读计划", "每次读多少页",
				"41 页", whiteboardClaimStanceDecided),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	value, found := boardEntry(result.Board, "reading_plan_pages")
	if !found {
		t.Fatalf("the board does not carry the reading plan: %+v", result.Board.Constraints)
	}
	if value.Value != "41 页" {
		t.Fatalf("the board carries %q, want the value the learner has", value.Value)
	}
	if result.Diagnostics.DroppedEntries != 0 {
		t.Fatalf("dropped entries = %d, want 0", result.Diagnostics.DroppedEntries)
	}
}
