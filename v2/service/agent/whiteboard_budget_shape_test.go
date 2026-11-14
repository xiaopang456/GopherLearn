package agent

// The shape of the board in the run of 2026-11-14, rebuilt without its content.
//
// The run measured the whole product on a board that carried a refused value:
// the learner's own words carried a new number, the program could not confirm
// that the number was the value of the item it named, and the board kept the
// words instead. On that board the words never reached the window - the two
// headers were read before anything else was fitted into the budget, and the
// statements were rendered last and whole, so the budget ran out before the
// first statement was written down.
//
// What is rebuilt here is the shape, not the content: one goal, four entries,
// eight statements, and the budget the run really had. The learner's words
// are synthetic and carry no answer to anything; the names, the course and
// the numbers of that run are not here, and the statement ids are shifted so
// that nothing in this file can be read back to that session.
//
// The point of the rebuild is that the shape is what decides the outcome. A
// board with one statement has room for the statement's whole words under
// any budget, so it cannot show the defect; a board with eight statements
// and a goal and four entries cannot. So the tests below run the shape
// through the whole window - the block, the window, and the export that
// authorises reading a message back - and they say what they found.

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"GopherAI/model"
)

// anchorLabel is the message id as it is written in the window.
func anchorLabel(id uint) string {
	return strconv.FormatUint(uint64(id), 10)
}

// messageIDsInTheBlock are the ids the block names, read the way a reader
// reads them: every group of the window's own label.
func messageIDsInTheBlock(block string) map[uint]bool {
	ids := map[uint]bool{}
	for _, group := range strings.Split(block, "[message_ids=")[1:] {
		end := strings.Index(group, "]")
		if end < 0 {
			continue
		}
		for _, part := range strings.Split(group[:end], ",") {
			value, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64)
			if err != nil {
				continue
			}
			ids[uint(value)] = true
		}
	}
	return ids
}

// boardBudgetOfTheRun is the budget the board block had in the run.
//
// It is the scaled configuration's number, not a number chosen here: the
// budget is what decides how many statements fit, so a test that used a
// bigger one would not be about the run at all.
const boardBudgetOfTheRun = 360

// The value the board refused, and the statement that carries it.
//
// It is the fifth of the eight, and that position is the point: a board that
// renders the whole words first spends the budget on the first statements
// and never reaches the fifth, and the fifth is the one a reader would ask
// for.
const (
	refusedValueMessageID = 805
	refusedValueKey       = "per_round_count"
	refusedValue          = "45 个"
)

// anonymizedBoardOfTheRun rebuilds the shape of that board.
//
// The goal, the four entries and the eight statements are synthetic. Their
// sizes are the sizes of the ones the run had, because the sizes are what
// the budget is spent on.
func anonymizedBoardOfTheRun() model.SessionWhiteboardDocument {
	return model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Goal: &model.SessionWhiteboardGoal{
			Text: "想把背单词的量定下来", SourceMessageIDs: []uint{3, 5},
		},
		Constraints: []model.SessionWhiteboardEntry{
			{Key: "amount_before", Text: "之前每次背单词的量没定", Value: "三十个",
				SourceMessageIDs: []uint{12}},
			{Key: "busy_now", Text: "这阵子事情比较多", Value: "近期忙碌",
				SourceMessageIDs: []uint{77}},
			{Key: "self_check", Text: "复习计划由我自己定", Value: "自定",
				SourceMessageIDs: []uint{131}},
		},
		Plan: []model.SessionWhiteboardEntry{
			{Key: "checklist_done", Text: "错题清单整理好了", Value: "已整理",
				State: "done", SourceMessageIDs: []uint{133}},
		},
		Unverified: []model.SessionWhiteboardAnchor{
			{Key: "progress_mark", Value: "单词表上", MessageID: 801,
				Text: "那我先把进度标到背单词表上。", Complete: true,
				Reason: model.SessionWhiteboardAnchorAttributeNotInEntry},
			{Key: "next_round", Value: "下一轮", MessageID: 802,
				Text: "这一轮背完之后我要接下一轮新的。", Complete: true,
				Reason: model.SessionWhiteboardAnchorSubjectNotInEntry},
			{Key: "finish_round", Value: "先背完", MessageID: 803,
				Text: "我打算先把这轮背完。", Complete: true,
				Reason: model.SessionWhiteboardAnchorSubjectNotInQuote},
			{Key: "progress_by_word", Value: "按词表", MessageID: 804,
				Text: "那我按词表记进度。", Complete: true,
				Reason: model.SessionWhiteboardAnchorAttributeNotInQuote},
			// The refused value, and the statement about something else that
			// came from the same message.
			{Key: refusedValueKey, Value: refusedValue, MessageID: refusedValueMessageID,
				Text: "「背单词」改成每次 45 个吧。", Complete: true,
				Reason: model.SessionWhiteboardAnchorSubjectNotInQuote},
			{Key: "study_format", Value: "录音", MessageID: refusedValueMessageID,
				Text: "「背单词」改成每次 45 个吧。", Complete: true,
				Reason: model.SessionWhiteboardAnchorAttributeNotInQuote},
			{Key: "task_count", Value: "三四个", MessageID: 807,
				Text: "我现在的任务有三四个。", Complete: true,
				Reason: model.SessionWhiteboardAnchorAttributeNotInQuote},
			{Key: "progress_on_paper", Value: "写到哪", MessageID: 808,
				Text: "我会在纸上写读到哪了。", Complete: true,
				Reason: model.SessionWhiteboardAnchorAttributeNotInQuote},
		},
	}
}

// buildWindowWithTheBoard assembles the whole window with this board.
//
// It is the product's own path: the board is stored, the read side reads it,
// the window is built, and the block is put in front of the conversation. A
// test that only called the renderer would pass while the window still
// dropped the block.
func buildWindowWithTheBoard(t *testing.T, document model.SessionWhiteboardDocument, boardTokens int) ContextWindow {
	t.Helper()
	encoded, err := model.MarshalSessionWhiteboardDocument(document)
	if err != nil {
		t.Fatalf("encode board: %v", err)
	}
	reader := &fakeSegmentReader{
		board: &model.SessionWhiteboard{
			UserID: 7, SessionID: "session", Content: encoded,
			CoveredMessageID: 9100, TokenEstimate: 60, Version: 3,
		},
		watermark: 9100,
	}
	config := layeredTestConfig()
	config.MaxWhiteboardTokens = boardTokens
	builder, err := NewContextBuilder(config, &fakeHistoryReader{}, staticTaskState(""), nil,
		staticMemories{}, EmptySummaryProvider{})
	if err != nil {
		t.Fatalf("build context builder: %v", err)
	}
	preselector, err := NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		t.Fatalf("build preselector: %v", err)
	}
	provider, err := NewDatabaseLayeredSummaryProvider(reader, preselector, 2)
	if err != nil {
		t.Fatalf("build layered provider: %v", err)
	}
	if err := builder.UseLayeredSummaryProvider(provider); err != nil {
		t.Fatalf("attach layered provider: %v", err)
	}
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 7, SessionID: "session", Question: "我一开始定的背单词量是多少",
	})
	if err != nil {
		t.Fatalf("build window: %v", err)
	}
	return window
}

// boardBlockOfTheWindow is the board block the window really carries.
func boardBlockOfTheWindow(t *testing.T, window ContextWindow) string {
	t.Helper()
	for _, message := range window.Messages {
		if strings.Contains(message.Content, "Current state (") {
			return message.Content
		}
	}
	t.Fatalf("the window carries no board block: %d messages", len(window.Messages))
	return ""
}

// The refused value's own words reach the window.
//
// This is the failure the rebuild is about. The shape of 2026-11-14 fitted
// no statement at all into the window, and the statement that carried the
// value a reader would ask for was the fifth of eight. So the value was in
// the store, and it was not in the window, and an id the model never saw is
// not one it may read back.
func TestTheBoardOfTheRunCarriesTheRefusedValueToTheWindow(t *testing.T) {
	window := buildWindowWithTheBoard(t, anonymizedBoardOfTheRun(), boardBudgetOfTheRun)
	block := boardBlockOfTheWindow(t, window)
	if !strings.Contains(block, "message_ids="+anchorLabel(refusedValueMessageID)) {
		t.Fatalf("the window does not point at the refused value's own words:\n%s", block)
	}
	found := false
	for _, id := range window.WhiteboardAnchorMessageIDs {
		if id == refusedValueMessageID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the window does not report the refused value's message: %v\n%s",
			window.WhiteboardAnchorMessageIDs, block)
	}
}

// The export authorises exactly what the window showed.
//
// The export is what a later read-back is checked against, so a statement
// the window showed and the export does not carry is a message the learner
// cannot get back, and a statement the export carries and the window did
// not show is a message the model was never shown.
func TestTheExportAuthorisesExactlyWhatTheWindowShowed(t *testing.T) {
	window := buildWindowWithTheBoard(t, anonymizedBoardOfTheRun(), boardBudgetOfTheRun)
	block := boardBlockOfTheWindow(t, window)
	granted := map[uint]bool{}
	for _, grant := range window.SummarySegmentHistoryGrants {
		if grant.SourceKind != HistorySourceWhiteboard {
			continue
		}
		granted[grant.MessageID] = true
	}
	for _, id := range window.WhiteboardAnchorMessageIDs {
		if !granted[id] {
			t.Fatalf("the window shows message %d and the export does not authorise it", id)
		}
	}
	shown := messageIDsInTheBlock(block)
	for id := range granted {
		if !shown[id] {
			t.Fatalf("the export authorises message %d and the window does not show it", id)
		}
	}
	for id := range shown {
		if !granted[id] {
			t.Fatalf("the window shows message %d and the export does not authorise it", id)
		}
	}
	found := false
	for id := range granted {
		if id == refusedValueMessageID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the export does not authorise the refused value's message: %v", granted)
	}
}

// The board of that shape still fits its budget.
//
// The fix gave the room back to the statements by saying less in the
// headers, not by asking for more. A board that renders more than its
// budget would make every other block's budget a lie.
func TestTheBoardOfTheRunFitsItsBudget(t *testing.T) {
	document := anonymizedBoardOfTheRun()
	rendered := renderWhiteboardBlock(document, boardBudgetOfTheRun)
	if cost := messageTokenCost(rendered.Text); cost > boardBudgetOfTheRun {
		t.Fatalf("the board rendered %d tokens on a budget of %d", cost, boardBudgetOfTheRun)
	}
}

// The statements the budget could not hold are left out, and they authorise
// nothing.
//
// This is the boundary the fix does not move: the budget is a fixed number,
// and a board that carries a goal, four entries and eight statements cannot
// carry all of them. The statements that do not fit are left out whole, and
// a statement that is left out is not one the model may read back.
//
// So the claim is not that every statement is reachable. The claim is that
// the ones that are reachable are the ones the budget paid for, and that
// the ones it did not pay for are gone rather than half shown.
func TestTheStatementsTheBudgetCouldNotHoldAreLeftOutAndAuthoriseNothing(t *testing.T) {
	document := anonymizedBoardOfTheRun()
	window := buildWindowWithTheBoard(t, document, boardBudgetOfTheRun)
	block := boardBlockOfTheWindow(t, window)
	shown := map[uint]bool{}
	for _, id := range window.WhiteboardAnchorMessageIDs {
		shown[id] = true
	}
	omitted := 0
	for _, anchor := range document.Unverified {
		if shown[anchor.MessageID] {
			continue
		}
		omitted++
		if strings.Contains(block, "message_ids="+anchorLabel(anchor.MessageID)) {
			t.Fatalf("the window names message %d and does not show it", anchor.MessageID)
		}
	}
	if omitted == 0 {
		t.Fatalf("the board of that shape fitted every statement; the shape is not the one the run had")
	}
	if len(window.WhiteboardAnchorMessageIDs) == len(document.Unverified) {
		t.Fatalf("every statement is shown: the test does not describe a board under pressure")
	}
}

// A pointer carries no value of its own.
//
// The learner's own words are what carry the value, and a pointer is not
// those words: it is the way back to them. A value named next to a pointer
// reads as a decision the learner made, and the sentence that may refuse it
// is not on the line.
func TestAPointerOfTheRunCarriesNoValueOfItsOwn(t *testing.T) {
	document := anonymizedBoardOfTheRun()
	rendered := renderWhiteboardBlock(document, boardBudgetOfTheRun)
	for _, line := range strings.Split(rendered.Text, "\n") {
		if !strings.HasPrefix(line, "- learner message") {
			continue
		}
		if strings.Contains(line, "candidate value") {
			t.Fatalf("a pointer carries a value:\n%s", line)
		}
	}
}

// The words of a statement are the whole message.
//
// A reader that is shown the beginning of a sentence whose end is missing
// reads a decision the learner may not have made: the end of the sentence
// is where the condition or the negation lives.
func TestTheWordsOfAStatementAreNeverCut(t *testing.T) {
	document := anonymizedBoardOfTheRun()
	rendered := renderWhiteboardBlock(document, boardBudgetOfTheRun)
	for _, anchor := range document.Unverified {
		text := strings.TrimSpace(anchor.Text)
		if text == "" {
			continue
		}
		found := strings.Contains(rendered.Text, text)
		if found {
			continue
		}
		// The statement is not in the window as words. It may still be
		// there as a pointer, and a pointer carries no part of the words.
		if strings.Contains(rendered.Text, "message_ids="+anchorLabel(anchor.MessageID)) {
			for _, prefix := range strings.Split(text, "") {
				if len([]rune(prefix)) < 6 {
					continue
				}
				if strings.Contains(rendered.Text, prefix) {
					t.Fatalf("the window shows part of message %d: %q", anchor.MessageID, prefix)
				}
			}
		}
	}
}

// The two phases do not take an entrance away from each other.
//
// The first phase gives every statement a pointer and decides which
// statements reach the window; the second phase spends what is left on the
// whole words. The second phase may only replace a pointer with the words
// of the same statement - it may never take a statement out of the window,
// and it may never bring one in that the first phase left out.
//
// So the board that can show the words and the board that cannot have to
// carry the same statements. The board that cannot is the same board with
// the words taken away, so the difference between the two renderings is
// exactly what the second phase did.
func TestTheTwoPhasesAgreeOnWhichStatementsTheWindowCarries(t *testing.T) {
	document := anonymizedBoardOfTheRun()
	withoutWords := anonymizedBoardOfTheRun()
	for index := range withoutWords.Unverified {
		withoutWords.Unverified[index].Complete = false
	}
	for _, budget := range []int{140, 180, 220, 260, 300, 360, 480, 720} {
		withWords := renderWhiteboardBlock(document, budget)
		pointersOnly := renderWhiteboardBlock(withoutWords, budget)
		if len(withWords.AnchorMessageIDs) != len(pointersOnly.AnchorMessageIDs) {
			t.Fatalf("budget %d: the window carries %d statements and the pointers alone carry %d",
				budget, len(withWords.AnchorMessageIDs), len(pointersOnly.AnchorMessageIDs))
		}
		for index, id := range withWords.AnchorMessageIDs {
			if id != pointersOnly.AnchorMessageIDs[index] {
				t.Fatalf("budget %d: the window carries message %d where the pointers alone carry %d",
					budget, id, pointersOnly.AnchorMessageIDs[index])
			}
		}
		for _, id := range withWords.AnchorMessageIDs {
			carried := 0
			for _, carriedID := range withWords.AnchorMessageIDs {
				if carriedID == id {
					carried++
				}
			}
			if count := strings.Count(withWords.Text, "message_ids="+anchorLabel(id)); count != carried {
				t.Fatalf("budget %d: message %d is named %d times and %d statements carry it",
					budget, id, count, carried)
			}
		}
	}
}

// The statements the window carries are the first ones the board keeps.
//
// The order is the board's own, and the first phase walks it in that order:
// so a window under pressure carries the newest statements and leaves the
// rest out, rather than carrying an arbitrary half.
func TestTheStatementsTheWindowCarriesAreTheFirstOnesTheBoardKeeps(t *testing.T) {
	document := anonymizedBoardOfTheRun()
	for _, budget := range []int{140, 180, 220, 260, 300, 360, 480, 720} {
		rendered := renderWhiteboardBlock(document, budget)
		if len(rendered.AnchorMessageIDs) > len(document.Unverified) {
			t.Fatalf("budget %d: the window carries %d statements and the board holds %d",
				budget, len(rendered.AnchorMessageIDs), len(document.Unverified))
		}
		for index, id := range rendered.AnchorMessageIDs {
			if id != document.Unverified[index].MessageID {
				t.Fatalf("budget %d: the window carries message %d where the board's statement %d is",
					budget, id, index)
			}
		}
	}
}

// A bigger budget never carries fewer statements.
//
// The entrance a board paid for is never taken away by what it shows later:
// a statement that is in the window on a tight budget is in the window on a
// looser one, and the loose board is a superset of the tight one.
func TestALargerBudgetNeverCarriesFewerStatements(t *testing.T) {
	document := anonymizedBoardOfTheRun()
	previous := map[uint]bool{}
	for _, budget := range []int{160, 200, 240, 280, 320, 360, 420, 520, 720, 1200} {
		rendered := renderWhiteboardBlock(document, budget)
		current := map[uint]bool{}
		for _, id := range rendered.AnchorMessageIDs {
			current[id] = true
		}
		for id := range previous {
			if !current[id] {
				t.Fatalf("budget %d: message %d was carried before and is gone now", budget, id)
			}
		}
		previous = current
	}
}
