package agent

// The tests here are about what counts as the learner saying a value.
//
// A value that appears in the learner's message is not a value the learner
// said. The learner may have negated it, asked about it, made it a
// hypothesis, asked the assistant for it, or said it about something else.
// The engine reads the learner's message as a list of statements and takes
// only the plain ones, and a plain statement that carries another value of
// the same quantity cancels the one before it.
//
// The other half is about which entry a new value belongs to. An entry that
// renames another one may take its key over, but only when it says which
// entry it replaces, when both measure the same thing, and when there is
// exactly one entry it could be replacing.

import (
	"testing"

	"GopherAI/model"
)

// pagesBoard is a board that holds the reading plan the learner has.
func pagesBoard() model.SessionWhiteboardDocument {
	return model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 26 页", "26 页/次", "", 51),
		},
	}
}

// The learner says what not to do. The value is in the message and the
// message says the opposite, so the board keeps what it has.
func TestTheNegatedValueIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, "不要改成41页，仍按26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 || result.Board.Constraints[0].Value != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The learner asks what would happen. Nothing was decided, so the board
// keeps what it has.
func TestTheHypotheticalValueIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, "如果改成41页会怎样，暂时保持26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 || result.Board.Constraints[0].Value != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The learner says the number about another thing. It is not this value.
func TestTheValueOfAnotherThingIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, "另一本书41页，这本书仍然26页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 || result.Board.Constraints[0].Value != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The learner asks the assistant for something. The count in the request is
// not a current value of the learner's.
func TestTheRequestedValueIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, "给我41页的阅读计划", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 || result.Board.Constraints[0].Value != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// A number of another unit is another value. "41 minutes" is not "41 pages",
// however alike the two numbers are.
func TestTheValueOfAnotherUnitIsNotTheLearnersValue(t *testing.T) {
	batch := assistantBatch(95, "每次读41页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(pagesBoard(), WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("minutes", "每次读 41 分钟", "41 分钟", model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 || result.Board.Constraints[0].Value != "26 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The board is empty - the last batch failed to generate - and the learner
// says a value. The learner's own words are the only thing there is, and
// they are enough: the entry is the learner's, whatever the model labelled
// it, and it does not have to replace a value the board already held.
func TestTheLearnersStatementIsTheValueOnAnEmptyBoard(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV2}
	batch := assistantBatch(95, "每次读41页", 96, "好的，往后按41页来。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", model.SessionWhiteboardOriginAssistantAccepted, 95, 96),
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

// The board is empty and the learner negates the value. There is nothing to
// put on the board, and nothing is put there.
func TestTheNegatedStatementIsNothingOnAnEmptyBoard(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV2}
	batch := assistantBatch(95, "不要改成41页", 96, "好的。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", model.SessionWhiteboardOriginAssistantAccepted, 95, 96),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The board is empty and the learner asks a question. Nothing was decided.
func TestTheHypotheticalStatementIsNothingOnAnEmptyBoard(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV2}
	batch := assistantBatch(95, "如果改成41页会怎样", 96, "好的。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", model.SessionWhiteboardOriginAssistantAccepted, 95, 96),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
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
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 2 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	pages := ""
	for _, entry := range result.Board.Constraints {
		if entry.Key == "pages_per_session" {
			pages = entry.Value
		}
	}
	if pages != "栗矶，41 页/次" {
		t.Fatalf("the pages entry = %q, want the new value", pages)
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
	batch := assistantBatch(95, "阅读时长改成45分钟", 96, "往后按新的时长来。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("reading_time", "阅读时长改为 45 分钟（原 30 分钟不再用）", "45 分钟",
				model.SessionWhiteboardOriginAssistantAccepted, 53, 95, 96),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, entry := range result.Board.Constraints {
		values[entry.Key] = entry.Value
	}
	if values["read_pages"] != "26 页" {
		t.Fatalf("the pages entry was taken over: %+v", result.Board.Constraints)
	}
	if values["read_minutes"] != "45 分钟" {
		t.Fatalf("the minutes entry was not taken over: %+v", result.Board.Constraints)
	}
}

// The same unit and the same words are not enough: the new entry is about
// another book, and it does not say which entry it replaces.
func TestTheNewEntryDoesNotTakeOverAnotherThingOfTheSameUnit(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("book_a_pages", "甲书每次读 26 页", "26 页", "", 51),
		},
	}
	batch := assistantBatch(95, "乙书改成每次41页", 96, "往后按新的页数来。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("book_b_pages", "乙书每次读 41 页", "41 页",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 2 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	values := map[string]string{}
	for _, entry := range result.Board.Constraints {
		values[entry.Key] = entry.Value
	}
	if values["book_a_pages"] != "26 页" {
		t.Fatalf("the other book's entry was taken over: %+v", result.Board.Constraints)
	}
	if values["book_b_pages"] != "41 页" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
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
	batch := assistantBatch(95, "改成每次读41页", 96, "往后按新的页数来。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("plan_pages", "改为每次读 41 页（原 26 页不用了）", "41 页",
				model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 3 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	values := map[string]string{}
	for _, entry := range result.Board.Constraints {
		values[entry.Key] = entry.Value
	}
	if values["plan_a_pages"] != "26 页" || values["plan_b_pages"] != "26 页" {
		t.Fatalf("one of the two entries was taken over: %+v", result.Board.Constraints)
	}
	if values["plan_pages"] != "41 页" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}
