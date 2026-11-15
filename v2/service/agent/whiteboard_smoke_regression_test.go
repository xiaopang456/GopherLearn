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
//   - the learner's own new value, which is the one the old check refused;
//   - the same entry with the assistant's label on it;
//   - an entry that cites another batch's message;
//   - a citation of a message that does not exist;
//   - a value the learner ruled out by saying the same number in other words;
//   - a proposal of the assistant's nobody took on;
//   - a proposal of the assistant's the learner took on.
//
// The first two are the ones that were broken: the learner's own words
// settled the value, the entry was dropped anyway, and the words went with
// it. The last five are the ones that have to keep working: a value nobody
// settled, a citation the program cannot check and a proposal nobody agreed
// to must not become the current one, and the learner's words must stay
// findable all the same.
//
// What the program does not do is read what a sentence means. It checks that
// a citation names a message of this session, that the message has the role
// the entry needs, and that a proposal of the assistant's the learner took on
// cites the assistant's message and a learner message after it. Everything
// else - whether the learner meant what the model wrote down, whether they
// only asked about it, whether they ruled it out - is the model's judgement,
// and the engine leaves it to the model. The two cases at the end of this
// file say what that costs.

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
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
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
// entry belongs to, and the citations are the facts. A guess the citations
// contradict is not a reason to lose the value.
func TestTheEntryLabelledTheAssistantsTakesTheSamePath(t *testing.T) {
	batch := assistantBatch(93, "背单词改成每次45个吧。", 94, "好，之后按45个来。")
	result, err := ApplyWhiteboardProposal(smokeBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			smokeEntryOf(model.SessionWhiteboardOriginAssistantAccepted, 93, 94),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
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
		if entry.EntryOrigin() != model.SessionWhiteboardOriginUser {
			t.Fatalf("the entry's origin = %q, want the learner's: the words were the learner's",
				entry.EntryOrigin())
		}
	}
	if result.Diagnostics.MislabeledOrigins != 1 {
		t.Fatalf("the mislabelled origins = %d, want 1", result.Diagnostics.MislabeledOrigins)
	}
	entry := smokeEntry(t, result)
	if entry.Outcome != whiteboardEntryOutcomeApplied {
		t.Fatalf("the entry's outcome = %q, want applied", entry.Outcome)
	}
}

// The entry cites another batch's message.
//
// The citation of the other batch's message is not a citation this batch may
// hold, so the entry does not go on the board: a proposal that cites what it
// cannot check is refused.
//
// And the learner's own words are not lost with it. They are kept as an
// unconfirmed statement under the message of this batch that really carries
// the value, so a reader can read them back. The statement is navigation:
// the board still holds nothing that is confirmed.
func TestAnEntryThatCitesAnotherBatchIsRefusedAndTheWordsStayFindable(t *testing.T) {
	batch := assistantBatch(95, "背单词改成每次45个吧。", 96, "好，之后按45个来。")
	result, err := ApplyWhiteboardProposal(smokeBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("words_per_day", "背单词每次读的个数原为 30 个，现改为 45 个", "45 个",
				model.SessionWhiteboardOriginUser, 51, 95),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
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
// proposal that invented its sources has to lose the entry. The learner's own
// words are still kept, so the reader can still find them - but the value
// does not land.
func TestACitationOfAMessageThatIsNotThereRefusesTheEntry(t *testing.T) {
	batch := assistantBatch(93, "背单词改成每次45个吧。", 94, "好，之后按45个来。")
	result, err := ApplyWhiteboardProposal(smokeBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{smokeEntryOf(model.SessionWhiteboardOriginUser, 93, 9999)},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
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
//
// What is checked here is not the negation: it is that the entry carries the
// number the board already holds, says something else about it, and is not
// about another frequency. An entry like that would replace a fact the
// learner still has with a fact about something else, and the reader would
// never learn that it happened - the number on the board would not even
// change.
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
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["words_per_day"] != "每次 30 个" {
		t.Fatalf("the board = %+v, want the old value: the learner ruled the new one out",
			result.Board.Constraints)
	}
	entry := smokeEntry(t, result)
	if entry.Outcome != whiteboardEntryOutcomeRefused {
		t.Fatalf("the entry's outcome = %q, want refused", entry.Outcome)
	}
	if entry.OutcomeReason != model.SessionWhiteboardAnchorSameNumber {
		t.Fatalf("the entry's reason = %q, want the one that names the number",
			entry.OutcomeReason)
	}
	if entry.AnchorMessageID == 0 {
		t.Fatalf("the entry carries no anchor: the learner's words are lost: %+v", entry)
	}
}

// The learner's own new value replaces the old one.
//
// This is the case the smoke was about. The learner says "改成每轮9道题吧",
// and the model writes the value down as "每轮 9 道题" - words the learner
// never used, about a plan the learner did not name. The board takes it: what
// the learner said is what the learner said, and the program does not ask
// the model to say it the same way twice.
func TestTheLearnersOwnNewValueReplacesTheOldOne(t *testing.T) {
	board := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("round_question_count", "每轮先做 14 道题。", "14 道", "", 59),
		},
	}
	batch := assistantBatch(93, "改成每轮9道题吧。", 94, "好，之后按9道来。")
	result, err := ApplyWhiteboardProposal(board, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("round_question_count", "每轮 9 道题", "9 道题",
				model.SessionWhiteboardOriginUser, 93),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["round_question_count"] != "9 道题" {
		t.Fatalf("the board = %+v, want the learner's new value", result.Board.Constraints)
	}
	entry := smokeEntry(t, result)
	if entry.Outcome != whiteboardEntryOutcomeApplied {
		t.Fatalf("the entry's outcome = %q, want applied", entry.Outcome)
	}
}

// A proposal of the assistant's nobody took on does not become the learner's
// state.
//
// The three exercises the assistant offered are a product of the assistant's,
// and they stay one: they are in the summary segment, labelled as the
// assistant's, and they are not on the board. The board is what the learner
// decided, and a proposal nobody agreed to is not a decision.
func TestAProposalNobodyTookOnIsNotTheLearnersState(t *testing.T) {
	batch := assistantBatch(93, "给我三道动态规划的练习", 94, "1. 最长递增子序列 2. 0-1 背包 3. 区间 DP")
	result, err := ApplyWhiteboardProposal(smokeBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("practice_set", "三道动态规划的练习", "三道",
				model.SessionWhiteboardOriginAssistant, 94),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the proposal is on the board: %+v", result.Board.Constraints)
	}
	if result.Diagnostics.AssistantProposals != 1 {
		t.Fatalf("the assistant's proposals = %d, want 1", result.Diagnostics.AssistantProposals)
	}
	entry := smokeEntry(t, result)
	if entry.Outcome != whiteboardEntryOutcomeRefused {
		t.Fatalf("the entry's outcome = %q, want refused", entry.Outcome)
	}
	if entry.OutcomeReason != whiteboardEntryReasonAssistantProduct {
		t.Fatalf("the entry's reason = %q, want the one that names the product",
			entry.OutcomeReason)
	}
}

// The learner takes the proposal on.
//
// The assistant offered three exercises in its own message, and the learner
// said "好，就这样" after it. The two messages together are what makes it an
// acceptance, so the board keeps the content that was accepted and cites
// both sides.
func TestAProposalTheLearnerTookOnBecomesTheBoardsState(t *testing.T) {
	batch := withLearnerMessage(
		assistantBatch(93, "给我三道动态规划的练习", 94, "1. 最长递增子序列 2. 0-1 背包 3. 区间 DP"),
		95, "好，就这样")
	result, err := ApplyWhiteboardProposal(smokeBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("practice_set", "三道动态规划的练习", "三道",
				model.SessionWhiteboardOriginAssistantAccepted, 94, 95),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 {
		t.Fatalf("the board = %+v, want the accepted content", result.Board.Constraints)
	}
	entry := result.Board.Constraints[0]
	if entry.Value != "三道" {
		t.Fatalf("the board holds %q, want the content that was accepted", entry.Value)
	}
	if entry.EntryOrigin() != model.SessionWhiteboardOriginAssistantAccepted {
		t.Fatalf("the entry's origin = %q, want the one that says the learner took it on",
			entry.EntryOrigin())
	}
	if !whiteboardCitesID(entry.SourceMessageIDs, 94) || !whiteboardCitesID(entry.SourceMessageIDs, 95) {
		t.Fatalf("the entry's evidence = %v, want the assistant's message and the learner's answer",
			entry.SourceMessageIDs)
	}
}

// A proposal the learner did not take on is not an acceptance.
//
// The model says the learner took the proposal on, and the citations do not
// show it: the learner's message came before the assistant's, so there is no
// acceptance to point at, and the value is not in the learner's own words
// either.
//
// The program cannot tell what a sentence means, so it goes by what it can
// check. An acceptance that is not shown is not an acceptance, and the entry
// stays what it is - a product of the assistant's.
func TestAProposalTheLearnerDidNotTakeOnIsNotAnAcceptance(t *testing.T) {
	batch := assistantBatch(94, "我想把数学补一补", 95, "我给你排了三周的复习计划")
	result, err := ApplyWhiteboardProposal(smokeBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("review_plan", "三周的复习计划", "三周计划",
				model.SessionWhiteboardOriginAssistantAccepted, 94, 95),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the board = %+v, want no entry: the acceptance is not shown", result.Board.Constraints)
	}
	if result.Diagnostics.UnprovenAcceptances != 1 {
		t.Fatalf("the unproven acceptances = %d, want 1", result.Diagnostics.UnprovenAcceptances)
	}
	entry := smokeEntry(t, result)
	if entry.Outcome != whiteboardEntryOutcomeRefused {
		t.Fatalf("the entry's outcome = %q, want refused", entry.Outcome)
	}
	if entry.OutcomeReason != whiteboardEntryReasonUnprovenAcceptance {
		t.Fatalf("the entry's reason = %q, want the one that names the acceptance",
			entry.OutcomeReason)
	}
}

// The program does not read a question out of the learner's words.
//
// The learner asked what would happen and said not to change anything yet,
// and the model wrote the value down anyway. The program cannot tell that
// apart from a decision: telling "改成 45 个" from "改成 45 个会怎么样" is
// reading the sentence, and the engine does not read sentences.
//
// This is the known limit of the design, and it is why the prompt asks the
// model not to write down a value the learner only asked about. The test is
// here to say what the limit is, not to say that it is a good idea.
func TestTheProgramDoesNotReadAQuestionOutOfTheLearnersWords(t *testing.T) {
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
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	// The engine took the value the model wrote down: it has no way of
	// knowing that the learner only asked about it.
	values := boardValues(result.Board)
	if values["words_per_day"] != "每次 45 个" {
		t.Fatalf("the board = %+v, want the value the model wrote down", result.Board.Constraints)
	}
}

// The program does not read a negation out of the learner's words either.
//
// The learner ruled the value out and the model wrote the ruled-out value
// down. The engine cannot tell that apart from a decision, for the same
// reason as the question above.
//
// What it can do is not write two values of the same thing on top of each
// other: the entry carries a number the board does not hold, so it is the
// learner's current value as far as the program is concerned.
func TestTheProgramDoesNotReadANegationOutOfTheLearnersWords(t *testing.T) {
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
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["words_per_day"] != "每次 45 个" {
		t.Fatalf("the board = %+v, want the value the model wrote down", result.Board.Constraints)
	}
}
