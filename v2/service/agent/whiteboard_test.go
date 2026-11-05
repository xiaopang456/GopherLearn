package agent

import (
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
	}, whiteboardBatch(21))
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
	}, whiteboardBatch(21))
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
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, whiteboardBatch(21))
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
	}, whiteboardBatch(21))
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
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{Constraints: proposed}, whiteboardBatch(21))
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
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{Plan: proposed}, whiteboardBatch(21))
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
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, whiteboardBatch(21))
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
	}, whiteboardBatch(21))
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
	}, whiteboardBatch(21))
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
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, batch)
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
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, batch)
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
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, whiteboardBatch(21))
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
		}, whiteboardBatch(21))
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Board.Constraints) != 0 {
			t.Fatalf("an entry with an unusable citation survived: %+v", result.Board.Constraints)
		}
	}
}
