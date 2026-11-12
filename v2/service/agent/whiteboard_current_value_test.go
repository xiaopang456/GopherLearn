package agent

// The tests here are about whose value a board carries and about what may be
// read back.
//
// Two things went wrong in the same place. A learner who changed a value had
// the change dropped, because the model labelled the learner's own new value
// as the assistant's and the engine believed the label. And an assistant's
// product was sometimes lost, because the model cited the learner's message
// for the assistant's own words.
//
// The board has one job here: the learner's current state. What the assistant
// produced is a product, and a product is recallable - labelled as the
// assistant's - without ever becoming the learner's state.

import (
	"context"
	"strings"
	"testing"

	"GopherAI/model"
)

// learnerBatch is a batch in which the learner says what the value is and
// the assistant answers afterwards.
func learnerBatch(texts map[uint]string, ids ...uint) WhiteboardBatch {
	batch := WhiteboardBatch{MessageIDs: ids}
	for _, id := range ids {
		batch.Messages = append(batch.Messages, texts[id])
	}
	batch.KnownMessages = map[uint]WhiteboardMessageRole{}
	for _, id := range ids {
		batch.KnownMessages[id] = WhiteboardMessageRoleUser
	}
	batch.FirstMessageID = ids[0]
	batch.LastMessageID = ids[len(ids)-1]
	return batch
}

// assistantBatch is a batch of the learner's message and the assistant's
// answer to it.
func assistantBatch(learnerID uint, learnerText string, assistantID uint, assistantText string) WhiteboardBatch {
	batch := WhiteboardBatch{
		MessageIDs:          []uint{learnerID},
		Messages:            []string{learnerText},
		AssistantMessageIDs: []uint{assistantID},
		KnownMessages: map[uint]WhiteboardMessageRole{
			learnerID:   WhiteboardMessageRoleUser,
			assistantID: WhiteboardMessageRoleAssistant,
		},
		FirstMessageID: learnerID,
		LastMessageID:  assistantID,
	}
	return batch
}

// withLearnerMessage adds a message the learner says after the assistant's.
func withLearnerMessage(batch WhiteboardBatch, id uint, text string) WhiteboardBatch {
	batch.MessageIDs = append(batch.MessageIDs, id)
	batch.Messages = append(batch.Messages, text)
	batch.KnownMessages[id] = WhiteboardMessageRoleUser
	batch.LastMessageID = id
	return batch
}

func ownEntry(key, text, value string, origin string, ids ...uint) model.SessionWhiteboardEntry {
	return model.SessionWhiteboardEntry{
		Key: key, Text: text, Value: value, Origin: origin,
		SourceMessageIDs: ids, UpdatedAtMessageID: 21,
	}
}

// The learner changed a value and the model wrote the learner's own new value
// down as the assistant's.
//
// The label is wrong and the words are the learner's, so the new value is the
// learner's current value and the old one is what it replaced.
func TestTheLearnersChangedValueIsTheCurrentValue(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 26 页", "26 页/次", "", 51),
		},
	}
	batch := assistantBatch(95, "这本书变简单了，栗矶改成每次 41 页，原先那个页数不用了。", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
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
}

// The old value may stay, but it has to be marked as the one that was changed
// later, so a reader does not take it for the current one.
func TestTheReplacedValueIsMarkedAsChangedLater(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 26 页", "26 页/次", "", 51),
		},
	}
	batch := assistantBatch(95, "栗矶改成每次 41 页，原先那个页数不用了。", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 1 {
		t.Fatalf("changes = %+v", result.Changes)
	}
	change := result.Changes[0]
	if change.Key != "pages" || change.OldValue != "26 页/次" || change.NewValue != "41 页/次" {
		t.Fatalf("the change = %+v", change)
	}
	if len(change.OldMessageIDs) != 1 || change.OldMessageIDs[0] != 51 {
		t.Fatalf("the change lost the old citation: %+v", change)
	}
}

// The assistant repeats the old value after the learner changed it. The old
// value is not the current one any more, and a later batch that mentions it
// again does not put it back.
func TestTheAssistantRepeatingTheOldValueDoesNotPutItBack(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", "", 95),
		},
	}
	batch := learnerBatch(map[uint]string{129: "我担心复述做不完。", 131: "还是 26 页吧"}, 129, 131)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 26 页", "26 页/次", model.SessionWhiteboardOriginAssistantAccepted, 51),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 || result.Board.Constraints[0].Value != "41 页/次" {
		t.Fatalf("the old value came back: %+v", result.Board.Constraints)
	}
}

// The learner took a proposal on: the assistant proposed it, and the learner
// said so afterwards. That is the one case in which an accepted proposal is
// on the board, and it keeps its origin.
func TestAnAcceptedProposalKeepsItsOrigin(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV2}
	batch := withLearnerMessage(
		assistantBatch(31, "给我三道动态规划的练习", 32, "1. 最长递增子序列\n2. 0-1 背包"),
		33, "好，就按这三道来")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("plan", "按三道练习来", "三道", model.SessionWhiteboardOriginAssistantAccepted, 32, 33),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 {
		t.Fatalf("the accepted proposal is not on the board: %+v", result.Board.Constraints)
	}
	entry := result.Board.Constraints[0]
	if entry.EntryOrigin() != model.SessionWhiteboardOriginAssistantAccepted {
		t.Fatalf("the entry's origin = %q, want the accepted proposal's", entry.EntryOrigin())
	}
}

// The assistant proposed it and the learner only asked about it. The proposal
// is not a current fact, so it does not go on the board - and it does not
// become the learner's decision either.
func TestAProposalTheLearnerOnlyAskedAboutIsNotTheLearnersDecision(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV2}
	batch := withLearnerMessage(
		assistantBatch(31, "给我三道动态规划的练习", 32, "1. 最长递增子序列\n2. 0-1 背包"),
		33, "第三题有点难")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("plan", "按三道练习来", "三道", model.SessionWhiteboardOriginAssistantAccepted, 31, 32),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the proposal is on the board: %+v", result.Board.Constraints)
	}
	if result.Diagnostics.UnprovenAcceptances != 1 {
		t.Fatalf("unproven acceptances = %d, want 1", result.Diagnostics.UnprovenAcceptances)
	}
}

// The learner's own change was written down with the assistant's label and
// the assistant's message id, and the value the learner said is the value the
// board carries.
func TestTheLearnersOwnChangeLabelledAsTheAssistantsIsTheLearners(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("deadline", "提前 3 天交", "3 天", "", 53),
		},
	}
	batch := assistantBatch(111, "楠濑改成提前 5 天交，之前那个天数不作数。", 112, "往后统一按新的天数执行。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("deadline", "提前 5 天交", "5 天", model.SessionWhiteboardOriginAssistantAccepted, 53, 111, 112),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 || result.Board.Constraints[0].Value != "5 天" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Board.Constraints[0].EntryOrigin() != model.SessionWhiteboardOriginUser {
		t.Fatalf("the entry's origin = %q, want the learner's",
			result.Board.Constraints[0].EntryOrigin())
	}
}

// The board is tight: the entry limit is reached and the board has to give
// something up. The learner's current value is not what goes.
func TestTheCurrentValueSurvivesATightBoard(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 26 页", "26 页/次", "", 51),
		},
	}
	for index := 0; index < model.SessionWhiteboardItemLimit; index++ {
		current.Plan = append(current.Plan, model.SessionWhiteboardEntry{
			Key: "filler-" + string(rune('a'+index)), Text: "旧条目", Value: "旧条目",
			SourceMessageIDs: []uint{uint(10 + index)}, UpdatedAtMessageID: uint(10 + index),
		})
	}
	batch := assistantBatch(95, "栗矶改成每次 41 页，原先那个页数不用了。", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", model.SessionWhiteboardOriginAssistantAccepted, 51, 95, 96),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range result.Board.Constraints {
		if entry.Key == "pages" && entry.Value == "41 页/次" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the learner's current value is not on the board: %+v", result.Board.Constraints)
	}
}

// The assistant's product nobody took on stays recallable, and it is labelled
// as the assistant's.
func TestTheAssistantsProductIsCarriedIntoTheSegment(t *testing.T) {
	generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[`+
		`{"text":"练习只用本机已有环境，不新增安装。","evidence_message_ids":[31],"source":"assistant"}]},`+
		`"whiteboard":{"schema_version":1,"constraints":[`+
		`{"key":"practice_env","text":"练习只用本机已有环境。","value":"只用本机已有环境",`+
		`"source_message_ids":[31],"origin":"assistant"}]}}`)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	products := 0
	for _, point := range result.Segment.Points {
		if point.PointSource() == model.SessionSummarySegmentSourceAssistant {
			products++
		}
	}
	if products == 0 {
		t.Fatalf("the assistant's product is not in the segment: %+v", result.Segment.Points)
	}
	if len(result.Whiteboard.Constraints) != 0 {
		t.Fatalf("the assistant's product is on the board: %+v", result.Whiteboard.Constraints)
	}
}

// A product the learner did not take on is still a product of the assistant's,
// and a reader can tell whose words it is.
func TestTheAssistantsProductIsLabelledAsTheAssistants(t *testing.T) {
	generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[`+
		`{"text":"练习只用本机已有环境。","evidence_message_ids":[31],"source":"assistant"}]},`+
		`"whiteboard":{"schema_version":1,"constraints":[`+
		`{"key":"practice_env","text":"练习只用本机已有环境。","value":"只用本机已有环境",`+
		`"source_message_ids":[31],"origin":"assistant"}]}}`)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	rendered := RenderSummarySegmentPoint(result.Segment.Points[0])
	if !strings.Contains(rendered, "助手") {
		t.Fatalf("the product is not labelled as the assistant's: %q", rendered)
	}
}

// The mode that does not record the assistant's products keeps the board it
// has always written: the learner's own value replaces the old one exactly as
// before, and nothing else changes.
func TestTheModeWithoutTheAssistantsProductsIsUnchanged(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 26 页", "26 页/次", "", 51),
		},
	}
	batch := assistantBatch(95, "栗矶改成每次 41 页。", 96, "往后按新的页数走。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", "", 95),
		},
	}, batch, model.SessionWhiteboardSchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Board.Constraints[0].Value != "41 页/次" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	if result.Board.SchemaVersion != model.SessionWhiteboardSchemaV1 {
		t.Fatalf("the board's version = %d, want %d",
			result.Board.SchemaVersion, model.SessionWhiteboardSchemaV1)
	}
	if result.Board.Constraints[0].Origin != "" {
		t.Fatalf("the entry carries an origin: %+v", result.Board.Constraints[0])
	}
}

// The board holds one value per subject. A model that renames the key of the
// entry it replaces does not get two values for one subject.
func TestTheRenamedEntryReplacesTheOneItNames(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV2,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("venue", "场地固定在青禾馆", "青禾馆", "", 63),
		},
	}
	batch := assistantBatch(97, "场地那边临时被占用，椿澧以后改到白鹭馆，原来那个地方不再去。", 98, "往后按新的场地记。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("plan_venue", "场地由青禾馆改到白鹭馆", "白鹭馆", model.SessionWhiteboardOriginAssistantAccepted, 97, 98),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 {
		t.Fatalf("the board carries two values for one subject: %+v", result.Board.Constraints)
	}
	if result.Board.Constraints[0].Value != "白鹭馆" {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
}

// The assistant's claim that it did something outside the conversation is a
// claim. It is not a fact about the world and it does not become the
// learner's state.
func TestTheAssistantsClaimOfExecutionIsNotAFact(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV2}
	batch := assistantBatch(30, "把场地定下来", 31, "场地我已经借好了。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Plan: []model.SessionWhiteboardEntry{
			ownEntry("venue_booked", "场地已借好", "已借好", model.SessionWhiteboardOriginAssistant, 31),
		},
	}, batch, model.SessionWhiteboardSchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Plan) != 0 {
		t.Fatalf("the assistant's claim is on the board: %+v", result.Board.Plan)
	}
	if result.Diagnostics.AssistantProposals != 1 {
		t.Fatalf("assistant proposals = %d, want 1", result.Diagnostics.AssistantProposals)
	}
}
