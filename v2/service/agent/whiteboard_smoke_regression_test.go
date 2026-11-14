package agent

// The offline regression of the six-question smoke.
//
// The proposals below are the shapes a run of 2026-11-14 produced, with the
// questions' own subjects, numbers and ids taken out. What is left is the
// shape of each case: which check refused an entry, which citation was
// illegal, which words the learner said, and what a reader could still find
// afterwards.
//
// Nothing here calls a model. The engine is a function on a proposal, so a
// whole run can be replayed against it offline and the replay costs nothing.
//
// The cases are the ones the smoke produced:
//
//   - the learner's verified new value with the assistant's confirmation
//     beside it;
//   - the same entry with the assistant's label on it;
//   - an entry with no claim at all that cites another batch's message;
//   - a citation of a message that does not exist;
//   - the learner ruling the value out;
//   - the learner only asking what would happen;
//   - a value that belongs to another thing.
//
// The first two are the ones that were broken: the learner's own words
// settled the value, the entry was dropped anyway, and the words went with
// it. The last five are the ones that have to keep working: a value nobody
// settled, a value that was ruled out and a value that belongs to another
// thing must not become the current one, and the learner's words must stay
// findable all the same.

import (
	"testing"

	"GopherAI/model"
)

// smokeBoard is the board before the batch.
//
// It is empty, and that is what the smoke found: the batch before it had
// every one of its entries refused, so the board carried nothing and the
// learner's new value had nowhere to go.
func smokeBoard() model.SessionWhiteboardDocument {
	return model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3}
}

// smokeEntryOf is the entry the smoke's proposals carried.
//
// It is the learner's new value, written the way a model writes it: the
// subject, the value and the messages the model says it came from.
func smokeEntryOf(origin string, ids ...uint) model.SessionWhiteboardEntry {
	return ownEntry("words_per_day", "背单词每次 45 个", "每次 45 个", origin, ids...)
}

// smokeEntry is the one entry of the diagnostics.
func smokeEntry(t *testing.T, result WhiteboardResult) WhiteboardEntryOutcome {
	t.Helper()
	if len(result.Diagnostics.Entries) != 1 {
		t.Fatalf("the diagnostics carry %d entries, want 1: %+v",
			len(result.Diagnostics.Entries), result.Diagnostics.Entries)
	}
	return result.Diagnostics.Entries[0]
}

// The learner's own words settled the value and the model wrote the
// assistant's confirmation beside them.
//
// The confirmation is a real message, so the entry is not wrong. But the
// assistant confirming something is not the learner saying it, so the
// confirmation is not evidence of the learner's fact, and the entry's
// evidence is the learner's own message alone.
//
// Before the fix the whole entry was dropped: the citation of the
// assistant's message was read as a citation of something the batch may not
// hold, and the learner's verified value went with it.
func TestTheAssistantConfirmationBesideTheVerifiedValueCostsNothing(t *testing.T) {
	batch := assistantBatch(93, "背单词改成每次45个吧。", 94, "好，之后按45个来。")
	result, err := ApplyWhiteboardProposal(smokeBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{smokeEntryOf(model.SessionWhiteboardOriginUser, 93, 94)},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("words_per_day", 93, "背单词改成每次45个吧。", "背单词", "每次", "45 个", "decided", "yes"),
		},
	}, batch, model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["words_per_day"] != "每次 45 个" {
		t.Fatalf("the board = %+v, want the learner's new value", result.Board.Constraints)
	}
	// The evidence is the learner's own message. The assistant's message is
	// not in it: it is not the learner's evidence.
	for _, entry := range result.Board.Constraints {
		if entry.Key != "words_per_day" {
			continue
		}
		if len(entry.SourceMessageIDs) != 1 || entry.SourceMessageIDs[0] != 93 {
			t.Fatalf("the entry's evidence = %v, want the learner's own message alone",
				entry.SourceMessageIDs)
		}
	}
	entry := smokeEntry(t, result)
	if entry.ClaimVerdict != whiteboardClaimVerdictLabelSettled {
		t.Fatalf("the claim's verdict = %q, want settled", entry.ClaimVerdict)
	}
	if entry.Outcome != whiteboardEntryOutcomeApplied {
		t.Fatalf("the entry's outcome = %q, want applied", entry.Outcome)
	}
	if entry.AnchorMessageID != 0 {
		t.Fatalf("the entry carries an anchor although it landed: %+v", entry)
	}
}

// The same entry with the assistant's label on it takes the same path.
//
// The label is the model's guess about which side of the conversation the
// entry belongs to, and the claim is the evidence. A guess the evidence
// contradicts is not a reason to lose the value.
func TestTheEntryLabelledTheAssistantsTakesTheSamePath(t *testing.T) {
	batch := assistantBatch(93, "背单词改成每次45个吧。", 94, "好，之后按45个来。")
	result, err := ApplyWhiteboardProposal(smokeBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			smokeEntryOf(model.SessionWhiteboardOriginAssistantAccepted, 93, 94),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("words_per_day", 93, "背单词改成每次45个吧。", "背单词", "每次", "45 个", "decided", "yes"),
		},
	}, batch, model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["words_per_day"] != "每次 45 个" {
		t.Fatalf("the board = %+v, want the learner's new value", result.Board.Constraints)
	}
	for _, entry := range result.Board.Constraints {
		if entry.Key != "words_per_day" {
			continue
		}
		if len(entry.SourceMessageIDs) != 1 || entry.SourceMessageIDs[0] != 93 {
			t.Fatalf("the entry's evidence = %v, want the learner's own message alone",
				entry.SourceMessageIDs)
		}
	}
	if result.Diagnostics.MislabeledOrigins != 1 {
		t.Fatalf("the mislabelled origins = %d, want 1", result.Diagnostics.MislabeledOrigins)
	}
	entry := smokeEntry(t, result)
	if entry.ClaimVerdict != whiteboardClaimVerdictLabelSettled {
		t.Fatalf("the claim's verdict = %q, want settled", entry.ClaimVerdict)
	}
	if entry.Outcome != whiteboardEntryOutcomeApplied {
		t.Fatalf("the entry's outcome = %q, want applied", entry.Outcome)
	}
}

// The entry carries no claim and cites another batch's message.
//
// Two facts are in play. The citation of the other batch's message is not a
// citation this batch may hold, so the entry does not go on the board: a
// proposal that cites what it cannot check is refused.
//
// And the learner's own words are not lost with it. They are kept as an
// unconfirmed statement under the message of this batch that really carries
// the value, so a reader can read them back. The statement is navigation:
// the board still holds nothing that is confirmed.
func TestAnEntryWithoutAClaimKeepsTheLearnersWordsFindable(t *testing.T) {
	batch := assistantBatch(95, "背单词改成每次45个吧。", 96, "好，之后按45个来。")
	result, err := ApplyWhiteboardProposal(smokeBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_day", "背单词每次读的个数原为 30 个，现改为 45 个", "45 个",
				model.SessionWhiteboardOriginUser, 51, 95),
		},
	}, batch, model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the board = %+v, want no entry: nothing confirmed the value",
			result.Board.Constraints)
	}
	if result.Diagnostics.FabricatedEntries != 1 {
		t.Fatalf("the fabricated entries = %d, want 1", result.Diagnostics.FabricatedEntries)
	}
	// The learner's words are findable, and they are findable under the
	// message of this batch.
	if len(result.Board.Unverified) != 1 {
		t.Fatalf("the board carries %+v, want the learner's own words",
			result.Board.Unverified)
	}
	anchor := result.Board.Unverified[0]
	if anchor.MessageID != 95 {
		t.Fatalf("the statement names message %d, want the learner's own message of this batch",
			anchor.MessageID)
	}
	if anchor.Value != "45 个" {
		t.Fatalf("the statement carries %q, want the value the learner said", anchor.Value)
	}
	if anchor.Reason != model.SessionWhiteboardAnchorCitationInvalid {
		t.Fatalf("the statement's reason = %q, want the one that names the citation",
			anchor.Reason)
	}
	entry := smokeEntry(t, result)
	if entry.ClaimVerdict != whiteboardClaimVerdictLabelAbsent {
		t.Fatalf("the claim's verdict = %q, want absent: the entry carried no claim",
			entry.ClaimVerdict)
	}
	if entry.Outcome != whiteboardEntryOutcomeRefused {
		t.Fatalf("the entry's outcome = %q, want refused", entry.Outcome)
	}
	if entry.AnchorMessageID != 95 {
		t.Fatalf("the entry's anchor = %d, want the learner's own message", entry.AnchorMessageID)
	}
}

// A citation of a message that does not exist refuses the entry.
//
// The check may not be got past by filtering the bad citation out: a
// proposal that invented its sources has to lose the entry, even when the
// claim itself holds. The learner's own words are still kept, so the reader
// can still find them - but the value does not land.
func TestACitationOfAMessageThatIsNotThereRefusesTheEntry(t *testing.T) {
	batch := assistantBatch(93, "背单词改成每次45个吧。", 94, "好，之后按45个来。")
	result, err := ApplyWhiteboardProposal(smokeBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{smokeEntryOf(model.SessionWhiteboardOriginUser, 93, 9999)},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("words_per_day", 93, "背单词改成每次45个吧。", "背单词", "每次", "45 个", "decided", "yes"),
		},
	}, batch, model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the board = %+v, want no entry: a citation of nothing cannot be filtered away",
			result.Board.Constraints)
	}
	if result.Diagnostics.FabricatedEntries != 1 {
		t.Fatalf("the fabricated entries = %d, want 1", result.Diagnostics.FabricatedEntries)
	}
	entry := smokeEntry(t, result)
	if entry.ClaimVerdict != whiteboardClaimVerdictLabelSettled {
		t.Fatalf("the claim's verdict = %q, want settled", entry.ClaimVerdict)
	}
	if entry.Outcome != whiteboardEntryOutcomeRefused {
		t.Fatalf("the entry's outcome = %q, want refused", entry.Outcome)
	}
	if entry.OutcomeReason != model.SessionWhiteboardAnchorCitationInvalid {
		t.Fatalf("the entry's reason = %q, want the one that names the citation",
			entry.OutcomeReason)
	}
	// The words are still findable: the entry was refused, not the turn.
	if entry.AnchorMessageID != 93 {
		t.Fatalf("the entry's anchor = %d, want the learner's own message", entry.AnchorMessageID)
	}
}

// The learner ruled the value out.
//
// The new value is in the learner's words, and the words say it is not to be
// used. The board keeps the value that holds now, and the learner's words
// stay findable so a reader can see what was said.
func TestTheRuledOutValueIsNotTheLearnersValue(t *testing.T) {
	board := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_day", "背单词每次 30 个", "每次 30 个", "", 51),
		},
	}
	batch := assistantBatch(93, "还是每次看30个吧，别改成45个", 94, "好的。")
	result, err := ApplyWhiteboardProposal(board, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_day", "背单词每次看 30 个，不改 45 个", "每次 30 个",
				model.SessionWhiteboardOriginUser, 93),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("words_per_day", 93, "还是每次看30个吧，别改成45个", "背单词", "每次看多少个", "30 个", "decided", "yes"),
		},
	}, batch, model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["words_per_day"] != "每次 30 个" {
		t.Fatalf("the board = %+v, want the old value: the learner ruled the new one out",
			result.Board.Constraints)
	}
	entry := smokeEntry(t, result)
	if entry.ClaimVerdict != whiteboardClaimVerdictLabelRejected {
		t.Fatalf("the claim's verdict = %q, want rejected", entry.ClaimVerdict)
	}
	if entry.Outcome != whiteboardEntryOutcomeRefused {
		t.Fatalf("the entry's outcome = %q, want refused", entry.Outcome)
	}
	if entry.AnchorMessageID == 0 {
		t.Fatalf("the entry carries no anchor: the learner's words are lost: %+v", entry)
	}
}

// The learner ruled the value out and the entry named the value itself.
//
// This is the case the check exists for: the new value is the entry's value,
// so nothing but the claim stands between the learner's words and the
// board. The claim says the value was ruled out, and the entry does not go
// on the board.
func TestTheRuledOutValueDoesNotLandEvenWhenTheEntryNamesIt(t *testing.T) {
	board := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_day", "背单词每次 30 个", "每次 30 个", "", 51),
		},
	}
	batch := assistantBatch(93, "背单词还是每次30个，别改成45个", 94, "好的。")
	result, err := ApplyWhiteboardProposal(board, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_day", "背单词每次 45 个", "每次 45 个",
				model.SessionWhiteboardOriginUser, 93),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("words_per_day", 93, "背单词还是每次30个，别改成45个", "背单词", "每次", "45 个", "negated", "yes"),
		},
	}, batch, model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["words_per_day"] != "每次 30 个" {
		t.Fatalf("the board = %+v, want the old value: the learner ruled the new one out",
			result.Board.Constraints)
	}
	entry := smokeEntry(t, result)
	if entry.ClaimVerdict != whiteboardClaimVerdictLabelUnsettled {
		t.Fatalf("the claim's verdict = %q, want unsettled", entry.ClaimVerdict)
	}
	if entry.Outcome != whiteboardEntryOutcomeRefused {
		t.Fatalf("the entry's outcome = %q, want refused", entry.Outcome)
	}
}

// The learner only asked what would happen.
//
// A question is not a decision. The value stays what it was, and the words
// stay findable - a learner who asked is worth being able to read back.
func TestTheValueTheLearnerOnlyAskedAboutDoesNotLand(t *testing.T) {
	board := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_day", "背单词每次 30 个", "每次 30 个", "", 51),
		},
	}
	batch := assistantBatch(93, "背单词要是改成每次看45个会怎么样？先别改，我就问问。", 94, "可以先试试。")
	result, err := ApplyWhiteboardProposal(board, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_day", "背单词每次看 45 个", "每次 45 个",
				model.SessionWhiteboardOriginUser, 93),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("words_per_day", 93, "背单词要是改成每次看45个会怎么样？先别改，我就问问。",
				"背单词", "每次看", "45 个", "hypothetical", "no"),
		},
	}, batch, model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["words_per_day"] != "每次 30 个" {
		t.Fatalf("the board = %+v, want the old value: a question is not a decision",
			result.Board.Constraints)
	}
	entry := smokeEntry(t, result)
	if entry.ClaimVerdict != whiteboardClaimVerdictLabelUnsettled {
		t.Fatalf("the claim's verdict = %q, want unsettled", entry.ClaimVerdict)
	}
	if entry.Outcome != whiteboardEntryOutcomeRefused {
		t.Fatalf("the entry's outcome = %q, want refused", entry.Outcome)
	}
	if entry.AnchorMessageID == 0 {
		t.Fatalf("the entry carries no anchor: the learner's words are lost: %+v", entry)
	}
}

// The value belongs to another thing.
//
// The learner spoke about something else, so the claim about this entry does
// not hold. The entry does not go on the board, and the board keeps what it
// had.
func TestTheValueOfAnotherThingIsNotThisEntry(t *testing.T) {
	board := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_day", "背单词每次 30 个", "每次 30 个", "", 51),
		},
	}
	batch := assistantBatch(95, "跑步改成每次45个", 96, "好。")
	result, err := ApplyWhiteboardProposal(board, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_day", "背单词每次 45 个", "每次 45 个",
				model.SessionWhiteboardOriginUser, 95),
		},
		Claims: []WhiteboardLearnerClaim{
			learnerClaim("words_per_day", 95, "跑步改成每次45个", "跑步", "每次", "45 个", "decided", "yes"),
		},
	}, batch, model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["words_per_day"] != "每次 30 个" {
		t.Fatalf("the board = %+v, want the old value: the learner spoke about another thing",
			result.Board.Constraints)
	}
	entry := smokeEntry(t, result)
	if entry.ClaimVerdict != whiteboardClaimVerdictLabelRejected {
		t.Fatalf("the claim's verdict = %q, want rejected", entry.ClaimVerdict)
	}
	if entry.Outcome != whiteboardEntryOutcomeRefused {
		t.Fatalf("the entry's outcome = %q, want refused", entry.Outcome)
	}
	if entry.ClaimReason != model.SessionWhiteboardAnchorSubjectNotInEntry {
		t.Fatalf("the claim's reason = %q, want the one that checks the subject",
			entry.ClaimReason)
	}
}

// Every verdict has its own name.
//
// A claim that holds and a claim that was never made are two different
// facts, and a diagnostic that calls both of them "ambiguous" cannot tell a
// reader which one happened.
func TestEveryVerdictHasItsOwnName(t *testing.T) {
	labels := map[string]whiteboardClaimVerdict{}
	for _, verdict := range []whiteboardClaimVerdict{
		whiteboardClaimRejected, whiteboardClaimUnsettled,
		whiteboardClaimSettled, whiteboardClaimAbsent,
	} {
		label := whiteboardClaimVerdictLabel(verdict)
		if label == "" {
			t.Fatalf("verdict %d has no label", verdict)
		}
		if other, taken := labels[label]; taken {
			t.Fatalf("verdicts %d and %d share the label %q", other, verdict, label)
		}
		labels[label] = verdict
	}
	if whiteboardClaimVerdictLabel(whiteboardClaimSettled) == whiteboardClaimVerdictLabelAmbiguous {
		t.Fatal("a claim that holds is written under the ambiguous name")
	}
	if whiteboardClaimVerdictLabel(whiteboardClaimAbsent) == whiteboardClaimVerdictLabelAmbiguous {
		t.Fatal("a claim that was never made is written under the ambiguous name")
	}
}
