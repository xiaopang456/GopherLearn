package agent

import (
	"fmt"
	"strings"
	"testing"

	"GopherAI/model"
)

// The board engine is the part of the structure that has to survive a wrong
// model output. Its rules are the ones the rest of the design leans on: a
// changed value replaces the old one, an entry nobody sourced is dropped, an
// entry the generator forgot is kept, and no failure rejects the whole
// proposal.

func whiteboardBatch(ids ...uint) WhiteboardBatch {
	batch := WhiteboardBatch{MessageIDs: ids}
	for _, id := range ids {
		batch.Messages = append(batch.Messages, "消息 "+strings.TrimSpace(string(rune('0'+id%10))))
	}
	batch.FirstMessageID = ids[0]
	batch.LastMessageID = ids[len(ids)-1]
	return batch
}

func whiteboardEntry(key, text, value string, ids ...uint) model.SessionWhiteboardEntry {
	return model.SessionWhiteboardEntry{
		Key: key, Text: text, Value: value,
		SourceMessageIDs: ids, UpdatedAtMessageID: 20,
	}
}

// A value that changed replaces the old one, and the change is recorded. The
// old value is not left in the board next to the new one.
func TestWhiteboardReplacesTheValueUnderTheSameKey(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Constraints:   []model.SessionWhiteboardEntry{whiteboardEntry("duration", "每天学习 30 分钟", "30 分钟", 11)},
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			whiteboardEntry("duration", "每天学习 48 分钟", "48 分钟", 21),
		},
	}, whiteboardBatch(21), model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 {
		t.Fatalf("constraints = %+v", result.Board.Constraints)
	}
	entry := result.Board.Constraints[0]
	if entry.Value != "48 分钟" || entry.Text != "每天学习 48 分钟" {
		t.Fatalf("the old value was not replaced: %+v", entry)
	}
	if len(entry.SourceMessageIDs) != 1 || entry.SourceMessageIDs[0] != 21 {
		t.Fatalf("the new entry does not cite the batch: %+v", entry)
	}
	if len(result.Changes) != 1 {
		t.Fatalf("changes = %+v", result.Changes)
	}
	change := result.Changes[0]
	if change.Key != "duration" || change.OldValue != "30 分钟" || change.NewValue != "48 分钟" {
		t.Fatalf("the change does not describe the replacement: %+v", change)
	}
	if len(change.OldMessageIDs) != 1 || change.OldMessageIDs[0] != 11 {
		t.Fatalf("the change lost the old citation: %+v", change)
	}
	if len(change.NewMessageIDs) != 1 || change.NewMessageIDs[0] != 21 {
		t.Fatalf("the change lost the new citation: %+v", change)
	}
}

// An entry the generator did not repeat keeps the citations it had. The
// generator did not see those messages this time, so it cannot repeat them,
// and the board must not lose the proof of where the value came from.
func TestWhiteboardKeepsTheOldCitationsOfAnUnchangedEntry(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Constraints: []model.SessionWhiteboardEntry{
			whiteboardEntry("duration", "每天学习 30 分钟", "30 分钟", 11),
		},
	}
	proposed := whiteboardEntry("duration", "每天学习 30 分钟", "30 分钟")
	proposed.UpdatedAtMessageID = 21
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{proposed},
	}, whiteboardBatch(21), model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	entry := result.Board.Constraints[0]
	if len(entry.SourceMessageIDs) != 1 || entry.SourceMessageIDs[0] != 11 {
		t.Fatalf("the old citations were not kept: %+v", entry)
	}
}

// An entry the proposal left out is kept. A proposal that stops repeating an
// entry has not said the entry is gone, and dropping a valid constraint
// because the generator lost interest would lose what the learner most needs.
func TestWhiteboardKeepsAnEntryTheProposalLeftOut(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Constraints: []model.SessionWhiteboardEntry{
			whiteboardEntry("duration", "每天学习 30 分钟", "30 分钟", 11),
		},
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, whiteboardBatch(21), model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 || result.Board.Constraints[0].Key != "duration" {
		t.Fatalf("the entry was dropped: %+v", result.Board.Constraints)
	}
	if result.Diagnostics.SuspiciousDeletes != 1 {
		t.Fatalf("the omission was not reported: %+v", result.Diagnostics)
	}
}

// A new entry that cites a message this batch never contained is dropped on
// its own. The rest of the proposal still lands.
func TestWhiteboardDropsAnEntryThatCitesAMessageItNeverSaw(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Constraints: []model.SessionWhiteboardEntry{
			whiteboardEntry("duration", "每天学习 30 分钟", "30 分钟", 11),
		},
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			whiteboardEntry("invented", "学员喜欢手写笔记", "手写", 99),
			whiteboardEntry("deadline", "考试在 9 月", "9 月", 21),
		},
	}, whiteboardBatch(21), model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 2 {
		t.Fatalf("constraints = %+v", result.Board.Constraints)
	}
	for _, entry := range result.Board.Constraints {
		if entry.Key == "invented" {
			t.Fatalf("the unsourced entry survived: %+v", entry)
		}
	}
	if result.Diagnostics.FabricatedEntries != 1 {
		t.Fatalf("the unsourced entry was not counted: %+v", result.Diagnostics)
	}
	if result.Board.Constraints[1].Key != "deadline" {
		t.Fatalf("the sourced entry was lost: %+v", result.Board.Constraints)
	}
}

// Too many entries is a trimming problem, not a rejection. A board that
// refuses a whole update stops updating, which is the failure this structure
// exists to remove.
func TestWhiteboardTrimsInsteadOfRejectingWhenThereAreTooManyEntries(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}
	proposed := make([]model.SessionWhiteboardEntry, 0, 50)
	for index := 0; index < 50; index++ {
		entry := whiteboardEntry("key-"+strings.TrimSpace(string(rune('a'+index%26)))+strings.TrimSpace(string(rune('a'+index/26))),
			"条目", "", 21)
		entry.UpdatedAtMessageID = uint(21 + index)
		proposed = append(proposed, entry)
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{Constraints: proposed}, whiteboardBatch(21), model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatalf("an oversized proposal must not fail: %v", err)
	}
	if len(result.Board.Constraints) != model.SessionWhiteboardItemLimit {
		t.Fatalf("constraints = %d, want %d", len(result.Board.Constraints), model.SessionWhiteboardItemLimit)
	}
	if len(result.Changes) == 0 {
		t.Fatalf("the dropped entries were not recorded: %+v", result)
	}
	// The newest entries are the ones that survive.
	newest := result.Board.Constraints[len(result.Board.Constraints)-1]
	if newest.UpdatedAtMessageID != uint(21+49) {
		t.Fatalf("the newest entry did not survive: %+v", newest)
	}
}

// A finished step gives up its place before an unfinished one.
func TestWhiteboardDropsFinishedPlanEntriesFirst(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}
	proposed := make([]model.SessionWhiteboardEntry, 0, 25)
	for index := 0; index < 25; index++ {
		entry := whiteboardEntry("step-"+strings.TrimSpace(string(rune('a'+index))),
			"步骤", "", 21)
		entry.UpdatedAtMessageID = uint(21 + index)
		if index == 24 {
			entry.State = "done"
			entry.UpdatedAtMessageID = uint(21 + 100)
		}
		proposed = append(proposed, entry)
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{Plan: proposed}, whiteboardBatch(21), model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range result.Board.Plan {
		if entry.State == "done" {
			t.Fatalf("a finished step kept its place: %+v", entry)
		}
	}
	if result.Diagnostics.DroppedDonePlan != 1 {
		t.Fatalf("the finished step was not counted: %+v", result.Diagnostics)
	}
}

// The goal is one entry and it is never trimmed away.
func TestWhiteboardKeepsTheGoalWhenTheProposalHasNone(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Goal: &model.SessionWhiteboardGoal{
			Text: "学完 RPC 粘包", SourceMessageIDs: []uint{11}, UpdatedAtMessageID: 11,
		},
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, whiteboardBatch(21), model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Board.Goal == nil || result.Board.Goal.Text != "学完 RPC 粘包" {
		t.Fatalf("the goal was lost: %+v", result.Board.Goal)
	}
}

// A goal the learner never said is dropped like any other unsourced entry.
func TestWhiteboardDropsAnUnsourcedGoal(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Goal: &model.SessionWhiteboardGoal{
			Text: "学完 RPC 粘包", SourceMessageIDs: []uint{11}, UpdatedAtMessageID: 11,
		},
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Goal: &model.SessionWhiteboardGoal{Text: "改成别的目标", SourceMessageIDs: []uint{99}},
	}, whiteboardBatch(21), model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Board.Goal == nil || result.Board.Goal.Text != "学完 RPC 粘包" {
		t.Fatalf("an unsourced goal replaced the old one: %+v", result.Board.Goal)
	}
	if result.Diagnostics.FabricatedEntries != 1 {
		t.Fatalf("the unsourced goal was not counted: %+v", result.Diagnostics)
	}
}

// An entry that is too long is cut, not refused.
func TestWhiteboardTruncatesAnEntryThatIsTooLong(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}
	long := strings.Repeat("很长的文字", 80)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{whiteboardEntry("note", long, "", 21)},
	}, whiteboardBatch(21), model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 {
		t.Fatalf("constraints = %+v", result.Board.Constraints)
	}
	entry := result.Board.Constraints[0]
	if len([]rune(entry.Text)) != model.SessionWhiteboardTextMaxRunes {
		t.Fatalf("text length = %d, want %d", len([]rune(entry.Text)), model.SessionWhiteboardTextMaxRunes)
	}
	if result.Diagnostics.TruncatedTexts != 1 {
		t.Fatalf("the truncation was not counted: %+v", result.Diagnostics)
	}
}

// A value the learner's own words look like they replaced, while the board
// still carries the old one, is reported as a suspicion. The board is not
// changed: the engine does not know the correct value.
func TestWhiteboardReportsAValueTheBatchLooksLikeItReplaced(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Constraints: []model.SessionWhiteboardEntry{
			whiteboardEntry("duration", "每天学习 30 分钟", "30 分钟", 11),
		},
	}
	batch := WhiteboardBatch{
		MessageIDs: []uint{21}, Messages: []string{"改成 48 分钟吧"},
		FirstMessageID: 21, LastMessageID: 21,
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, batch, model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SuspectedMisses) != 1 || result.SuspectedMisses[0] != "duration" {
		t.Fatalf("the suspected update was not reported: %+v", result.SuspectedMisses)
	}
	if result.Board.Constraints[0].Value != "30 分钟" {
		t.Fatalf("the board was changed by a suspicion: %+v", result.Board.Constraints[0])
	}
	if len(result.Changes) != 1 || !result.Changes[0].Suspected {
		t.Fatalf("the suspicion was not recorded as a change: %+v", result.Changes)
	}
}

// A number of a different unit is not evidence that the value changed.
func TestWhiteboardDoesNotReportAValueOfAnotherUnit(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Constraints: []model.SessionWhiteboardEntry{
			whiteboardEntry("duration", "每天学习 30 分钟", "30 分钟", 11),
		},
	}
	batch := WhiteboardBatch{
		MessageIDs: []uint{21}, Messages: []string{"另外还有 2 小时的阅读时间"},
		FirstMessageID: 21, LastMessageID: 21,
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, batch, model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SuspectedMisses) != 0 {
		t.Fatalf("a value of another unit was reported: %+v", result.SuspectedMisses)
	}
}

// A proposal that says nothing leaves the board alone.
func TestWhiteboardLeavesTheBoardAloneWhenNothingWasProposed(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Constraints: []model.SessionWhiteboardEntry{
			whiteboardEntry("duration", "每天学习 30 分钟", "30 分钟", 11),
		},
	}
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, whiteboardBatch(21), model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 0 {
		t.Fatalf("changes = %+v", result.Changes)
	}
	if result.Diagnostics.ChangedEntries != 0 {
		t.Fatalf("the board was reported as changed: %+v", result.Diagnostics)
	}
}

// A citation of zero, or a repeated one, is not a citation.
func TestWhiteboardDropsAnEntryWithAnUnusableCitation(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}
	for _, ids := range [][]uint{{0}, {21, 21}} {
		result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
			Constraints: []model.SessionWhiteboardEntry{whiteboardEntry("note", "条目", "", ids...)},
		}, whiteboardBatch(21), model.SessionWhiteboardSchemaV1)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Board.Constraints) != 0 {
			t.Fatalf("an entry with an unusable citation survived: %+v", result.Board.Constraints)
		}
	}
}

// The board is measured on the block the window sends, not on the body it is
// stored in.
//
// The stored body is JSON: braces, quotes, field names and message ids. On a
// real board it runs to about a fifth more than the block the model reads, so
// a budget spent on the body dropped entries the window would have carried
// and still let the block overrun the budget it was supposed to fit.
func TestTheBoardIsMeasuredOnTheBlockTheWindowSends(t *testing.T) {
	board := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}
	for index := 0; index < model.SessionWhiteboardItemLimit; index++ {
		entry := whiteboardEntry("constraint-"+string(rune('a'+index)),
			"每天学习三十分钟，周末不学", strings.Repeat("值", 8))
		entry.SourceMessageIDs = []uint{21, 22}
		entry.UpdatedAtMessageID = uint(21 + index)
		board.Constraints = append(board.Constraints, entry)
	}
	rendered := RenderedWhiteboardTokens(board)
	stored := storedWhiteboardTokens(board)
	if stored <= rendered {
		t.Fatalf("the stored body costs %d and the block %d; the fixture does not show the difference", stored, rendered)
	}
	// Trimming is decided on the block, so a board whose block already fits
	// keeps every entry no matter what its stored body costs.
	if rendered > model.SessionWhiteboardTokenLimit {
		t.Fatalf("the fixture does not fit the block: %d > %d", rendered, model.SessionWhiteboardTokenLimit)
	}
	trimmed := board
	if changes, _ := trimWhiteboardToTokenLimit(&trimmed); len(changes) != 0 {
		t.Fatalf("a board that fits the block lost %d entries", len(changes))
	}
	if len(trimmed.Constraints) != len(board.Constraints) {
		t.Fatalf("entries = %d, want %d", len(trimmed.Constraints), len(board.Constraints))
	}
}

// Whatever the trim takes away, the block that is left has to fit.
func TestTheTrimmedBoardFitsTheBlockItIsSentIn(t *testing.T) {
	board := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}
	for index := 0; index < model.SessionWhiteboardItemLimit; index++ {
		entry := whiteboardEntry("constraint-"+string(rune('a'+index)),
			strings.Repeat("考试前两周改成每天一小时", 6), "")
		entry.UpdatedAtMessageID = uint(21 + index)
		board.Constraints = append(board.Constraints, entry)
	}
	changes, _ := trimWhiteboardToTokenLimit(&board)
	if len(changes) == 0 {
		t.Fatal("the fixture did not need trimming, so it proves nothing")
	}
	if cost := RenderedWhiteboardTokens(board); cost > model.SessionWhiteboardTokenLimit {
		t.Fatalf("the trimmed block costs %d, the budget is %d", cost, model.SessionWhiteboardTokenLimit)
	}
	if len(changes) != model.SessionWhiteboardItemLimit-len(board.Constraints) {
		t.Fatalf("%d entries were dropped but %d changes were recorded", model.SessionWhiteboardItemLimit-len(board.Constraints), len(changes))
	}
}

// A finished step gives up its place before anything that still stands, the
// values and plan steps nobody mentioned since go next, and a constraint is
// the last thing the board gives up.
func TestTheBoardGivesUpItsEntriesInOrder(t *testing.T) {
	board := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}
	constraint := whiteboardEntry("约束", "每天学习三十分钟", "三十分钟", 21)
	// The constraint is the oldest entry on the board, so the previous rule -
	// oldest first - would have taken it before anything else.
	constraint.UpdatedAtMessageID = 5
	done := whiteboardEntry("计划-已完成", "先读第一章", "已完成", 21)
	done.State = "done"
	done.UpdatedAtMessageID = 90
	open := whiteboardEntry("计划-未完成", "再读第二章", "进行中", 21)
	open.UpdatedAtMessageID = 40
	worry := whiteboardEntry("担心", "怕读不完", "", 21)
	worry.UpdatedAtMessageID = 60
	board.Constraints = []model.SessionWhiteboardEntry{constraint}
	board.Plan = []model.SessionWhiteboardEntry{done, open}
	board.OpenItems = []model.SessionWhiteboardEntry{worry}

	want := []string{"计划-已完成", "计划-未完成", "担心", "约束"}
	for index, key := range want {
		victim, found := nextBoardEntryToDrop(board)
		if !found {
			t.Fatalf("step %d: the board stopped giving entries up", index)
		}
		if strings.TrimSpace(victim.entry.Key) != key {
			t.Fatalf("step %d: the board gave up %q, want %q", index, victim.entry.Key, key)
		}
		switch victim.group {
		case whiteboardGroupConstraints:
			board.Constraints = removeWhiteboardEntry(board.Constraints, victim.entry)
		case whiteboardGroupPlan:
			board.Plan = removeWhiteboardEntry(board.Plan, victim.entry)
		case whiteboardGroupOpenItems:
			board.OpenItems = removeWhiteboardEntry(board.OpenItems, victim.entry)
		}
	}
	if _, found := nextBoardEntryToDrop(board); found {
		t.Fatal("an empty board still offered an entry")
	}
}

func TestEntryRetentionReportsCapacityAndOutputTrimming(t *testing.T) {
	entries := make([]model.SessionWhiteboardEntry, 21)
	for i := range entries {
		entries[i] = whiteboardEntry(fmt.Sprintf("key%d", i), fmt.Sprintf("entry %d", i), fmt.Sprintf("value %d", i), 21)
	}
	result, err := ApplyWhiteboardProposal(model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}, WhiteboardProposal{Constraints: entries}, whiteboardBatch(21), model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	var removed *WhiteboardEntryOutcome
	for i := range result.Diagnostics.Entries {
		if result.Diagnostics.Entries[i].Key == "key20" {
			removed = &result.Diagnostics.Entries[i]
		}
	}
	if removed == nil || removed.Outcome != "applied" || removed.EntryRetention != "removed" || removed.EntryRetentionReason != "item_capacity" {
		t.Fatalf("diagnostic=%+v", removed)
	}
	changes := trimWhiteboardToOutputLimit(&result.Board, 1)
	markRemovedEntries(&result.Diagnostics, changes, "output_budget")
	for _, change := range changes {
		for _, d := range result.Diagnostics.Entries {
			if d.Key == change.Key && d.EntryRetentionReason != "output_budget" {
				t.Fatalf("output trim=%+v", d)
			}
		}
	}
}

func TestAnchorRetentionDistinguishesCapacityAndRenderBudget(t *testing.T) {
	board := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3}
	d := WhiteboardDiagnostics{Entries: []WhiteboardEntryOutcome{{AnchorMessageID: 10, candidateValue: "v"}}}
	reconcileAnchorRetention(&d, board, "anchor_capacity")
	if d.Entries[0].AnchorRetention != "removed" || d.Entries[0].AnchorRetentionReason != "anchor_capacity" {
		t.Fatal(d)
	}
	d.Entries[0].AnchorRetention = ""
	board.Unverified = []model.SessionWhiteboardAnchor{{MessageID: 10, Value: "v", Reason: model.SessionWhiteboardAnchorNoClaim}}
	reconcileAnchorRetention(&d, board, "anchor_capacity")
	if d.Entries[0].AnchorRetention != "retained" {
		t.Fatal(d)
	}
	board.Unverified = nil
	reconcileAnchorRetention(&d, board, "render_budget")
	if d.Entries[0].AnchorRetentionReason != "render_budget" {
		t.Fatal(d)
	}
}
