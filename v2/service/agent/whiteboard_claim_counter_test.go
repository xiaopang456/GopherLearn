package agent

// The counter-examples in this file are the ones a reviewer named, and each
// one is a way the claim could have been accepted without anything behind
// it.
//
// The three of them are:
//
//   - the learner's message never mentions 41 pages, and the claim and the
//     entry both say 41 pages. The two model-generated fields agree with
//     each other and there is nothing under them.
//   - the quote is invented, and everything around it looks right: the
//     entry carries the learner's own origin, it cites the learner's
//     message, and the message id is one the batch really carried.
//   - the learner's message really does say 41 pages, but it says it in a
//     way that is not a decision, and the model reported "decided" anyway.
//
// The first two are caught by the program: the value has to be in the
// quoted words, and the quoted words have to be in the message. The third
// is not fully caught by the program, and the part that is caught depends
// on the learner's own words rather than on the model's reading - which is
// why the message in that case carries the value the learner kept.
//
// Every one of them is run against an empty board and against a board that
// already holds the old value, because the two paths are different code and
// a fix that covers one of them does not cover the other.

import (
	"testing"

	"GopherAI/model"
)

// emptyPagesBoard is a board that holds nothing about the reading plan.
func emptyPagesBoard() model.SessionWhiteboardDocument {
	return model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV2}
}

// The learner's message never says 41 pages.
//
// The claim says 41 pages, the entry says 41 pages, and the two agree. The
// learner's own words say 26 pages and nothing else, so there is nothing
// behind the entry at all.
func TestAValueTheLearnerNeverWroteIsNotEvidence(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读26页", "阅读计划", "页", "41页", "decided"),
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
	if result.Diagnostics.RejectedClaims != 1 {
		t.Fatalf("rejected claims = %d, want 1", result.Diagnostics.RejectedClaims)
	}
}

// The same thing on a board that holds nothing: the invented value does not
// go on the board either.
func TestAValueTheLearnerNeverWroteIsNotEvidenceOnAnEmptyBoard(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(emptyPagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读26页", "阅读计划", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Diagnostics.RejectedClaims != 1 {
		t.Fatalf("rejected claims = %d, want 1", result.Diagnostics.RejectedClaims)
	}
}

// The quote is invented.
//
// Everything around the quote is right: the entry says the learner's own
// origin, it cites the learner's message, and the message is one the batch
// really carried. Only the words are not the learner's.
func TestAnInventedQuoteIsNotEvidence(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读41页", "阅读计划", "页", "41页", "decided"),
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

// The same invented quote on an empty board.
func TestAnInventedQuoteIsNotEvidenceOnAnEmptyBoard(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(emptyPagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读41页", "阅读计划", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The learner really did write 41 pages, but not as a decision.
//
// The message says "do not change it to 41 pages, keep 26". The model
// reported "decided" and "yes" - both wrong - and the entry wants 41 pages.
// The claim's words are the learner's own and the 41 is really in them, so
// the checks that compare the claim with the entry cannot tell this from a
// decision. What tells it is the learner's own words: 26 is the value of
// the same quantity that stands after the 41.
//
// This is the boundary of what the program can prove. It cannot read the
// sentence, so it cannot tell a negation that names the value it keeps
// from one that does not. What it can do is refuse to raise the value when
// the learner's own words leave a different value standing.
func TestAValueTheLearnerMovedOnFromIsNotADecision(t *testing.T) {
	batch := assistantBatch(95, "阅读计划不要改成41页，仍按26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划不要改成41页", "阅读计划", "页", "41页", "decided"),
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

// The same on a board that holds nothing.
func TestAValueTheLearnerMovedOnFromIsNotADecisionOnAnEmptyBoard(t *testing.T) {
	batch := assistantBatch(95, "阅读计划不要改成41页，仍按26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(emptyPagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划不要改成41页", "阅读计划", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// A rejected claim cannot come back through the old path.
//
// Before the claim existed, an entry that the model labelled as the
// learner's own and that cited the learner's message was raised to the
// learner's own even without any evidence - the citation was taken as
// proof. A claim that does not hold must not be a way back to that.
func TestARejectedClaimDoesNotComeBackThroughTheOldPath(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读41页", "阅读计划", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Diagnostics.MislabeledOrigins != 0 {
		t.Fatalf("mislabeled origins = %d, want 0", result.Diagnostics.MislabeledOrigins)
	}
	if result.Diagnostics.RejectedClaims != 1 {
		t.Fatalf("rejected claims = %d, want 1", result.Diagnostics.RejectedClaims)
	}
}

// A rejected claim does not become the learner's own on an empty board
// either. The old path did that too, and it was the way a value nobody said
// reached an empty board.
func TestARejectedClaimDoesNotReachAnEmptyBoard(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(emptyPagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读41页", "阅读计划", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// A rejected claim does not take over another entry's key.
//
// The key is what makes an entry replace another one, and an entry whose
// evidence does not hold is not one to give another entry's place.
func TestARejectedClaimDoesNotTakeOverAnotherKey(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages_new", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages_new", 95, "阅读计划以后每次读41页", "阅读计划", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["pages"] != "26 页/次" {
		t.Fatalf("the old entry = %+v", result.Board.Constraints)
	}
	if _, taken := values["pages_new"]; taken {
		t.Fatalf("the entry took over a key it had no evidence for: %+v", result.Board.Constraints)
	}
}

// A rejected claim does not replace the value the board already holds.
func TestARejectedClaimDoesNotReplaceTheOldValue(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读41页", "阅读计划", "页", "41页", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Diagnostics.UnbackedChanges != 0 {
		// The change was refused as a change with nothing behind it, which
		// is a different count from the claim being refused.
		t.Fatalf("unbacked changes = %d, want 0", result.Diagnostics.UnbackedChanges)
	}
}

// One bad entry does not take the others with it.
//
// The claim is refused on its own, and the entries beside it are still
// processed: a proposal is not a batch of one mistake.
func TestARejectedClaimDoesNotTakeTheOtherEntriesWithIt(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读26页，每天练30分钟", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次", "", 95),
			ownEntry("minutes", "每天练 30 分钟", "30 分钟", "", 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读41页", "阅读计划", "页", "41页", "decided"),
			learnerClaim("minutes", 95, "每天练30分钟", "每天", "分钟", "30分钟", "decided"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if values["minutes"] != "30 分钟" {
		t.Fatalf("the good entry was taken with the bad one: %+v", result.Board.Constraints)
	}
}

// The model is asked two questions about the same words, and the two
// answers have to agree.
//
// A model that reads a negation as a plain statement usually still answers
// "no" to the second question, because the second question is about what
// the words do to the item rather than about what kind of sentence they
// are. This is what stops one wrong reading from reaching the board.
func TestTheTwoAnswersHaveToAgree(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{
			// The model calls it a decision and then says the words do
			// not update the item. It has contradicted itself.
			learnerClaim("pages", 95, "阅读计划以后每次读41页", "阅读计划", "页", "41页",
				"decided", whiteboardClaimUpdateNo),
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
	if result.Diagnostics.UnsettledClaims != 1 {
		t.Fatalf("unsettled claims = %d, want 1", result.Diagnostics.UnsettledClaims)
	}
}

// The other way round: the model says the words do not update the item and
// then calls them a decision. The two disagree, and the disagreement is
// what the program acts on.
func TestTheTwoAnswersHaveToAgreeTheOtherWayRound(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划以后每次读41页", "阅读计划", "页", "41页",
				"proposed", whiteboardClaimUpdateYes),
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

// An answer the program does not know is not an answer that says yes.
//
// A model that leaves the second question out has said one thing where two
// are needed, and what it did not say is not consent.
func TestAnUnansweredSecondQuestionIsNotConsent(t *testing.T) {
	batch := assistantBatch(95, "阅读计划以后每次读41页", 96, "好的。")
	claim := learnerClaim("pages", 95, "阅读计划以后每次读41页", "阅读计划", "页", "41页", "decided")
	claim.Update = ""
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
		Claims: []WhiteboardLearnerClaim{claim},
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

// An unsettled claim does not become the learner's own through the old
// path either.
//
// The entry is labelled as a proposal the learner took on, it cites the
// learner's message and nothing of the assistant's, and the old rule read
// those citations as proof that the label was wrong and the words were the
// learner's. The claim says the learner did not settle the value, and that
// is the only account of the learner's words there is.
func TestAnUnsettledClaimDoesNotBecomeTheLearnersThroughTheOldPath(t *testing.T) {
	batch := assistantBatch(95, "阅读计划每次读41页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划每次读41页", "阅读计划", "页", "41页", "proposed"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Diagnostics.MislabeledOrigins != 0 {
		t.Fatalf("mislabeled origins = %d, want 0", result.Diagnostics.MislabeledOrigins)
	}
}

// An unsettled claim does not replace the value the board already holds,
// whatever label the entry carries.
func TestAnUnsettledClaimDoesNotReplaceTheOldValue(t *testing.T) {
	batch := assistantBatch(95, "阅读计划每次读41页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "阅读计划每次读 41 页", "41 页/次",
				model.SessionWhiteboardOriginAssistantAccepted, 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("pages", 95, "阅读计划每次读41页", "阅读计划", "页", "41页", "proposed"),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if boardValues(result.Board)["pages"] != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The learner's own words that do settle the value still work.
//
// This is the case the whole mechanism is for: the learner says the value
// changed, the assistant only confirms it, and the model wrote the entry
// down as the assistant's.
func TestTheLearnersOwnWordsStillSettleTheValue(t *testing.T) {
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
	entry := result.Board.Constraints[0]
	if entry.Value != "41 页/次" {
		t.Fatalf("the board still carries %q", entry.Value)
	}
	if entry.EntryOrigin() != model.SessionWhiteboardOriginUser {
		t.Fatalf("the entry's origin = %q, want the learner's", entry.EntryOrigin())
	}
	if result.Diagnostics.VerifiedClaims != 1 {
		t.Fatalf("verified claims = %d, want 1", result.Diagnostics.VerifiedClaims)
	}
}
