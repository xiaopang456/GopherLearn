package agent

// The refused value's own words have to reach the window.
//
// The board holds two kinds of things: the values it confirmed, and the
// learner's own words it could not confirm. The second kind is the only way
// back to a value the board refused - it says which message carries the
// value, and its message id is what lets a reader ask for that message.
//
// It used to reach the window only by accident. The board's budget is a fixed
// number, the two headers were read before anything else was fitted into it,
// and the statements were rendered last and whole. So on the boards of
// 2026-11-14 the learner's own words were left out of the window entirely:
// a value the board refused was not readable, and an id the model never saw
// is not one it may read back.
//
// The tests below are about the path the words travel: from the refused
// entry, through the statement, into the window, and from there into the
// authorisation that lets a reader ask for the message.
//
// They are about the whole path on purpose. A test that only renders a
// statement would pass while the window still dropped it.

import (
	"strings"
	"testing"

	"GopherAI/model"
)

// budgetOfTheDay is the board's budget in the run of 2026-11-14.
const boardBudgetOfTheDay = 360

// A board that confirmed a few things and refused the one that matters.
func boardWithARefusedValue() model.SessionWhiteboardDocument {
	return model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			{Key: "base_unfinished", Text: "基础还没学完", Value: "基础的都还没学完",
				Origin: model.SessionWhiteboardOriginUser, SourceMessageIDs: []uint{15}},
			{Key: "time_busy", Text: "这阵子事情多", Value: "这阵子事情多",
				Origin: model.SessionWhiteboardOriginUser, SourceMessageIDs: []uint{77}},
			{Key: "acceptance_self", Text: "验收标准自己定", Value: "我自己定",
				Origin: model.SessionWhiteboardOriginUser, SourceMessageIDs: []uint{131}},
			{Key: "acceptance_checklist", Text: "验收清单已经写下来了", Value: "写下来了",
				Origin: model.SessionWhiteboardOriginUser, SourceMessageIDs: []uint{133}},
		},
		Unverified: []model.SessionWhiteboardAnchor{
			// The refused value: the learner's own words carry it, and the
			// program could not confirm that it is the value of that item.
			{Key: "pages_per_session", Value: "23 页", MessageID: 93,
				Text: "背单词改成每次45个吧。", Complete: true,
				Reason: model.SessionWhiteboardAnchorSubjectNotInQuote},
			// A statement about something else, from the same batch.
			{Key: "delivery_mode", Value: "录播", MessageID: 93,
				Text: "背单词改成每次45个吧。", Complete: true,
				Reason: model.SessionWhiteboardAnchorAttributeNotInQuote},
		},
	}
}

// The learner's own words about the refused value reach the window.
//
// The board of 2026-11-14 rendered the statements last and whole, and the
// budget ran out before the first one. The statement that carries the value
// a reader would ask for was the fifth of eight, so it never got there.
func TestTheRefusedValueReachesTheWindow(t *testing.T) {
	rendered := renderWhiteboardBlock(boardWithARefusedValue(), boardBudgetOfTheDay)
	found := false
	for _, id := range rendered.AnchorMessageIDs {
		if id == 93 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the window shows no statement of message 93: %v\n%s",
			rendered.AnchorMessageIDs, rendered.Text)
	}
	if !strings.Contains(rendered.Text, "23 页") {
		t.Fatalf("the window does not carry the refused value:\n%s", rendered.Text)
	}
}

// What is shown is reachable: the message id in the window is the one the
// statement points at.
//
// A statement the budget cut is a statement the model never saw, and an id
// it never saw is not one it may read back. So the ids the window reports
// are the ids it really showed.
func TestOnlyTheStatementsThatWereShownAreAuthorised(t *testing.T) {
	document := boardWithARefusedValue()
	rendered := renderWhiteboardBlock(document, boardBudgetOfTheDay)
	shown := map[uint]bool{}
	for _, id := range rendered.AnchorMessageIDs {
		shown[id] = true
	}
	for _, anchor := range document.Unverified {
		inWindow := strings.Contains(rendered.Text, renderEvidenceIDs([]uint{anchor.MessageID}))
		if inWindow && !shown[anchor.MessageID] {
			t.Fatalf("the window shows message %d but does not authorise it", anchor.MessageID)
		}
		if !inWindow && shown[anchor.MessageID] {
			t.Fatalf("the window authorises message %d but does not show it", anchor.MessageID)
		}
	}
}

// A statement that does not fit is shown as a pointer, and the pointer
// still says which message it is.
//
// The words are never cut. A reader that is shown the beginning of a
// sentence whose end is missing reads a decision that the learner may not
// have made: the end of the sentence is where the condition or the negation
// lives.
func TestAStatementThatDoesNotFitIsAPointerAndNotAHalfSentence(t *testing.T) {
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Unverified: []model.SessionWhiteboardAnchor{
			{Key: "long", Value: "45 个", MessageID: 95,
				Text:   strings.Repeat("学习者说了很长的一段话，", 40),
				Reason: model.SessionWhiteboardAnchorCitationInvalid},
		},
	}
	rendered := renderWhiteboardBlock(document, 200)
	if !strings.Contains(rendered.Text, "message_ids=95") {
		t.Fatalf("the pointer does not carry the message id:\n%s", rendered.Text)
	}
	if strings.Contains(rendered.Text, "学习者说了很长的一段话") {
		t.Fatalf("the window shows half of a message:\n%s", rendered.Text)
	}
	// The value is not named either: it is shown with the whole words only.
	if strings.Contains(rendered.Text, "45 个") {
		t.Fatalf("the window names a value whose words are not there:\n%s", rendered.Text)
	}
}

// Every statement the board holds gets a pointer, so every one of them is
// reachable.
//
// This is the property the fix is about. The board's budget is a fixed
// number, the entries take what they need, and the statements used to be
// rendered last and whole - so on a board with several statements the one
// that carried the value a reader would ask for was the one that never
// reached the window.
func TestEveryStatementReachesTheWindowWhenTheBoardIsFull(t *testing.T) {
	document := boardWithARefusedValue()
	for _, anchor := range []model.SessionWhiteboardAnchor{
		{Key: "a", Value: "1 个", MessageID: 91, Text: "一", Complete: true},
		{Key: "b", Value: "2 个", MessageID: 92, Text: "二", Complete: true},
		{Key: "c", Value: "3 个", MessageID: 93, Text: "三", Complete: true},
		{Key: "d", Value: "4 个", MessageID: 94, Text: "四", Complete: true},
		{Key: "e", Value: "5 个", MessageID: 95, Text: "五", Complete: true},
		{Key: "f", Value: "6 个", MessageID: 96, Text: "六", Complete: true},
	} {
		document.Unverified = append(document.Unverified, anchor)
	}
	rendered := renderWhiteboardBlock(document, boardBudgetOfTheDay)
	for _, want := range []uint{91, 92, 93, 94, 95, 96} {
		found := false
		for _, id := range rendered.AnchorMessageIDs {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("the window does not reach message %d: %v\n%s",
				want, rendered.AnchorMessageIDs, rendered.Text)
		}
	}
}

// A value the learner only asked about is not shown as a value.
//
// The learner asked what would happen if the value were 23 pages. The board
// refused the entry, so the words are kept - but the value is not shown
// next to them, because a value named next to a question reads as a
// decision the learner made, and the whole point of the question was that
// they had not made one.
func TestAValueNobodySettledIsNotShownAsAValue(t *testing.T) {
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Unverified: []model.SessionWhiteboardAnchor{
			{Key: "pages_change_asked", Value: "23 页", MessageID: 93,
				Text: "背单词要是改成每次看45个会怎么样？先别改，我就问问。", Complete: true,
				Reason: model.SessionWhiteboardAnchorUnsettled},
		},
	}
	rendered := renderWhiteboardBlock(document, boardBudgetOfTheDay)
	if strings.Contains(rendered.Text, "23 页") {
		t.Fatalf("the window shows the value of a question that was not a decision:\n%s", rendered.Text)
	}
	if !strings.Contains(rendered.Text, "message_ids=93") {
		t.Fatalf("the window does not point at the learner's own words:\n%s", rendered.Text)
	}
}

// The value the learner ruled out is not shown without the learner's own
// words.
//
// The learner said "还是每次看30个吧，别改成45个". The board refused the
// entry, and the words are kept - but a pointer never carries the value, so
// the value the learner ruled out is never named without the sentence that
// refuses it. When the whole sentence fits, the value is shown next to it
// and the reader sees the refusal; when it does not fit, the value is not
// shown at all.
func TestAValueNobodyWroteIsNeverNamedWithoutTheWords(t *testing.T) {
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Unverified: []model.SessionWhiteboardAnchor{
			{Key: "reeding_amount", Value: "45 个", MessageID: 93,
				Text: "还是每次看30个吧，别改成45个", Complete: true,
				Reason: model.SessionWhiteboardAnchorSubjectNotInQuote},
		},
	}
	// With the whole sentence there, the value is shown - beside the words
	// that refuse it.
	rendered := renderWhiteboardBlock(document, boardBudgetOfTheDay)
	if !strings.Contains(rendered.Text, "45 个") {
		t.Fatalf("the window does not carry the value at all:\n%s", rendered.Text)
	}
	if !strings.Contains(rendered.Text, "别改成45个") {
		t.Fatalf("the window shows the value without the words that refuse it:\n%s", rendered.Text)
	}
	// With a budget too small for the words, the value is not named at all.
	small := renderWhiteboardBlock(document, 140)
	if strings.Contains(small.Text, "45 个") {
		t.Fatalf("the window names a value whose words are not there:\n%s", small.Text)
	}
	if !strings.Contains(small.Text, "message_ids=93") {
		t.Fatalf("the window does not point at the learner's own words:\n%s", small.Text)
	}
}

// A statement whose value the learner's own words do not carry is not built
// at all.
//
// A statement that points at a message that does not mention the value
// would send a reader looking for words that are not there.
func TestAStatementWhoseValueIsNotInTheWordsIsNotBuilt(t *testing.T) {
	board := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3}
	batch := WhiteboardBatch{
		MessageIDs: []uint{93}, Messages: []string{"还是每次看30个吧"},
		AssistantMessageIDs: []uint{94},
		KnownMessages: map[uint]WhiteboardMessageRole{
			93: WhiteboardMessageRoleUser, 94: WhiteboardMessageRoleAssistant,
		},
		FirstMessageID: 93, LastMessageID: 94,
	}
	result, err := ApplyWhiteboardProposal(board, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "pages_per_session", Text: "背单词每次读的个数原为 30 个，现改为 45 个",
			Value: "45 个", Origin: model.SessionWhiteboardOriginUser,
			SourceMessageIDs: []uint{93},
		}},
		Claims: []WhiteboardLearnerClaim{{
			Key: "pages_per_session", MessageID: 93, Quote: "还是每次看30个吧",
			Subject: "背单词", Attribute: "每次", Value: "45 个",
			Stance: "decided", Update: "yes",
		}},
	}, batch, model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Unverified) != 0 {
		t.Fatalf("the board carries %+v, want no statement: the words do not carry the value",
			result.Board.Unverified)
	}
}

// The whole path: a refused entry leaves a statement, the statement reaches
// the window, and the window authorises the message it shows.
//
// The three layers are kept apart on purpose. The claim holds or does not,
// the entry lands or does not, and the statement that keeps the learner's
// words findable is a third thing - and it is the third thing that has to
// survive all the way into the authorisation.
func TestTheRefusedValueIsReadableBackThroughTheWholePath(t *testing.T) {
	board := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3}
	batch := WhiteboardBatch{
		MessageIDs: []uint{93}, Messages: []string{"背单词改成每次45个吧。"},
		AssistantMessageIDs: []uint{94},
		KnownMessages: map[uint]WhiteboardMessageRole{
			93: WhiteboardMessageRoleUser, 94: WhiteboardMessageRoleAssistant,
		},
		FirstMessageID: 93, LastMessageID: 94,
	}
	result, err := ApplyWhiteboardProposal(board, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "pages_per_session", Text: "背单词每次读的个数原为 30 个，现改为 45 个",
			Value: "45 个", Origin: model.SessionWhiteboardOriginUser,
			SourceMessageIDs: []uint{93, 94},
		}},
		Claims: []WhiteboardLearnerClaim{{
			Key: "pages_per_session", MessageID: 93, Quote: "背单词改成每次45个吧。",
			Subject: "背单词", Attribute: "每次", Value: "45 个",
			Stance: "decided", Update: "yes",
		}},
	}, batch, model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	// The claim holds and the entry lands: the learner's own words decided
	// it, and the assistant's confirmation beside it does not stop it.
	if len(result.Board.Constraints) != 1 {
		t.Fatalf("the board = %+v, want the learner's new value", result.Board.Constraints)
	}
	if len(result.Diagnostics.Entries) != 1 ||
		result.Diagnostics.Entries[0].ClaimVerdict != whiteboardClaimVerdictLabelSettled {
		t.Fatalf("the diagnostics = %+v, want one settled claim", result.Diagnostics.Entries)
	}

	// Now the other side of the board: an entry that cites a message this
	// batch never held, so it is refused, and the statement that keeps the
	// learner's own words findable.
	refused, err := ApplyWhiteboardProposal(model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
	}, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{{
			Key: "pages_per_session", Text: "背单词每次读的个数原为 30 个，现改为 45 个",
			Value: "45 个", Origin: model.SessionWhiteboardOriginUser,
			SourceMessageIDs: []uint{51, 93},
		}},
	}, batch, model.SessionWhiteboardSchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if len(refused.Board.Constraints) != 0 {
		t.Fatalf("the board = %+v, want no entry: the entry cites what it cannot check",
			refused.Board.Constraints)
	}
	if len(refused.Board.Unverified) != 1 {
		t.Fatalf("the refused entry left %d statements, want 1: %+v",
			len(refused.Board.Unverified), refused.Board.Unverified)
	}
	statement := refused.Board.Unverified[0]
	if statement.MessageID != 93 {
		t.Fatalf("the statement names message %d, want the learner's own", statement.MessageID)
	}

	rendered := renderWhiteboardBlock(refused.Board, boardBudgetOfTheDay)
	authorised := false
	for _, id := range rendered.AnchorMessageIDs {
		if id == 93 {
			authorised = true
		}
	}
	if !authorised {
		t.Fatalf("the window does not authorise the message the statement points at:\n%s", rendered.Text)
	}
	if !strings.Contains(rendered.Text, "45 个") {
		t.Fatalf("the window does not carry the refused value:\n%s", rendered.Text)
	}
}

// The board's budget is a fixed number, and the statements are inside it.
//
// The fix gave the room back to the statements by saying less in the
// headers, not by asking for more. A board that renders more than its
// budget would make every other block's budget a lie.
func TestTheBoardStillFitsItsBudget(t *testing.T) {
	for _, budget := range []int{120, 180, 240, 360} {
		rendered := renderWhiteboardBlock(boardWithARefusedValue(), budget)
		if cost := messageTokenCost(rendered.Text); cost > budget {
			t.Fatalf("the board rendered %d tokens on a budget of %d", cost, budget)
		}
	}
}

// The statements are the last thing in the block, and they are under a
// header that says what they are.
//
// A reader meets a value before a statement, so the header has to say the
// board carries statements that are not a current value - and it has to say
// it before a reader meets one.
func TestTheStatementsAreIntroducedBeforeTheyAppear(t *testing.T) {
	rendered := renderWhiteboardBlock(boardWithARefusedValue(), boardBudgetOfTheDay)
	header := strings.Index(rendered.Text, whiteboardUnconfirmedHeader)
	statement := strings.Index(rendered.Text, "23 页")
	if header < 0 {
		t.Fatalf("the window does not introduce the statements:\n%s", rendered.Text)
	}
	if statement < 0 {
		t.Fatalf("the window does not carry the statement:\n%s", rendered.Text)
	}
	if header > statement {
		t.Fatalf("the statement comes before its introduction:\n%s", rendered.Text)
	}
}

// The window that carries a statement says the value in it may not be the
// current one.
//
// The block's own header drops its promise when the board carries
// statements, because the board cannot promise that a value it shows is the
// current one when it also carries words it could not confirm.
func TestTheBlockSaysAValueMayNotBeTheCurrentOneWhenItCarriesStatements(t *testing.T) {
	rendered := renderWhiteboardBlock(boardWithARefusedValue(), boardBudgetOfTheDay)
	if !strings.Contains(rendered.Text, "may not be the current one") {
		t.Fatalf("the window does not warn about the statements:\n%s", rendered.Text)
	}
}

// The headers are shorter, and they still say everything they said.
//
// The room the statements needed came out of the two headers, so the headers
// have to keep every promise they made: where a statement comes from, that
// it was not confirmed, that the value beside it is not the current one, and
// that the message can be read back.
//
// A shorter header that dropped one of these would buy the statements their
// room by misleading the reader, which is the trade this fix must not make.
func TestTheShortenedHeadersStillSayWhatTheySaid(t *testing.T) {
	rendered := renderWhiteboardBlock(boardWithARefusedValue(), boardBudgetOfTheDay)
	for _, want := range []string{
		"the learner's own words",
		"not confirmed",
		"not the current value",
		"read them back",
		"may not be the current one",
	} {
		if !strings.Contains(rendered.Text, want) {
			t.Fatalf("the window does not say %q:\n%s", want, rendered.Text)
		}
	}
}

// The build of the product is what the fix is measured against: the board
// renders under the budget the run really had, and the statements are
// reachable.
func TestTheBoardTheRunMeasuredRendersTheRefusedValue(t *testing.T) {
	rendered := renderWhiteboardBlock(boardWithARefusedValue(), boardBudgetOfTheDay)
	found := false
	for _, id := range rendered.AnchorMessageIDs {
		if id == 93 {
			found = true
		}
	}
	if !found {
		t.Fatalf("on the budget the run had (%d) the window shows no statement of message 93: %v",
			boardBudgetOfTheDay, rendered.AnchorMessageIDs)
	}
}
