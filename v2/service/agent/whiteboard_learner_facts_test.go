package agent

// The learner's own facts, and the products of the assistant.
//
// The board used to ask the model for the learner's own words behind every
// entry and to check them against the words: the subject had to be in the
// quote, the attribute had to be in the quote, the value had to match the
// entry's value, and the model had to answer two questions about the
// sentence the same way. An update the learner had stated plainly was
// refused when the model wrote the subject in its own words, and the board
// kept the old value.
//
// The rules now are the ones a program can check as facts about the
// messages: the citation names a message of this session, the message has the
// role the entry needs, and a proposal of the assistant's that the learner
// took on cites the assistant's message and a learner message after it. What
// the learner said is what the learner said, and the board takes it.
//
// The products of the assistant are a separate thing. A proposal nobody took
// on stays in the summary segment, labelled as the assistant's, and it never
// becomes the learner's decision. A proposal the learner did take on becomes
// the board's current state, with both messages cited.

import (
	"context"
	"strings"
	"testing"

	"GopherAI/model"
)

// The learner changes a value and the new value is the current one.
//
// The entry's own text names the item the way the model summarised it, and
// the learner's message does not: the program does not compare the two. The
// citation is the learner's own message of this batch, and that is what the
// entry needs.
func TestTheLearnersNewValueIsTheCurrentValue(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("round_question_count", "每轮先做 14 道题。", "14 道", "", 59, 61),
		},
	}
	batch := learnerBatch(map[uint]string{93: "改成每轮 9 道题吧"}, 93)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("round_question_count", "每轮题量从 14 道改成每轮 9 道题。", "每轮 9 道题", "", 93),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if got := boardValues(result.Board)["round_question_count"]; got != "每轮 9 道题" {
		t.Fatalf("the board holds %q, want the learner's new value", got)
	}
	if result.Diagnostics.AppliedEntries != 1 {
		t.Fatalf("applied entries = %d, want 1: %+v", result.Diagnostics.AppliedEntries, result.Diagnostics)
	}
	if len(result.Changes) != 1 {
		t.Fatalf("changes = %+v, want the replacement", result.Changes)
	}
	change := result.Changes[0]
	if change.Key != "round_question_count" || change.OldValue != "14 道" || change.NewValue != "每轮 9 道题" {
		t.Fatalf("the change does not describe the replacement: %+v", change)
	}
	if len(result.Board.Unverified) != 0 {
		t.Fatalf("the board carries %+v, want no statement: the value was written",
			result.Board.Unverified)
	}
}

// The learner's own facts are written the same way whether the assistant's
// products are recorded or not.
//
// The switch decides what is remembered. It does not decide how a fact of the
// learner's is checked, and a deployment that turns it off must not start
// losing the values the learner stated.
func TestTheLearnersFactsAreWrittenTheSameWayInBothModes(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("round_question_count", "每轮先做 14 道题。", "14 道", "", 59, 61),
		},
	}
	proposal := WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("round_question_count", "每轮题量从 14 道改成每轮 9 道题。", "每轮 9 道题", "", 93),
		},
	}
	batch := learnerBatch(map[uint]string{93: "改成每轮 9 道题吧"}, 93)
	on, err := ApplyWhiteboardProposal(current, proposal, batch,
		assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	off, err := ApplyWhiteboardProposal(current, proposal, batch,
		learnerOnlyPolicy(model.SessionWhiteboardSchemaV1))
	if err != nil {
		t.Fatal(err)
	}
	if got := boardValues(on.Board)["round_question_count"]; got != "每轮 9 道题" {
		t.Fatalf("the board that records the assistant's products holds %q", got)
	}
	if got := boardValues(off.Board)["round_question_count"]; got != "每轮 9 道题" {
		t.Fatalf("the board that does not record them holds %q", got)
	}
	if len(on.Changes) != len(off.Changes) {
		t.Fatalf("the two modes recorded %d and %d changes", len(on.Changes), len(off.Changes))
	}
}

// The assistant proposed it and the learner said "good, let's do it that
// way". The proposal becomes the board's current state, with both messages
// cited.
func TestTheAcceptedProposalBecomesTheCurrentState(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3}
	batch := withLearnerMessage(
		assistantBatch(90, "给我三道动态规划的练习", 91, "1. 最长递增子序列\n2. 0-1 背包\n3. 区间 DP 入门"),
		92, "好，就按这三道来")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("practice_plan", "按这三道练习来", "三道", model.SessionWhiteboardOriginAssistantAccepted, 91, 92),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
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
	if !whiteboardCitesID(entry.SourceMessageIDs, 91) || !whiteboardCitesID(entry.SourceMessageIDs, 92) {
		t.Fatalf("the entry cites %v, want the proposal and the message that took it on",
			entry.SourceMessageIDs)
	}
	if result.Diagnostics.UnprovenAcceptances != 0 {
		t.Fatalf("unproven acceptances = %d, want 0", result.Diagnostics.UnprovenAcceptances)
	}
}

// The learner changes the accepted plan afterwards. The board carries the
// learner's latest state, and the entry is no longer the assistant's.
func TestTheLearnerChangesTheAcceptedProposalLater(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("practice_plan", "按这三道练习来", "三道", model.SessionWhiteboardOriginAssistantAccepted, 91, 92),
		},
	}
	batch := learnerBatch(map[uint]string{95: "前两道先做，第三道太难了"}, 95)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("practice_plan", "先做前两道练习", "两道", "", 95),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	entry := result.Board.Constraints[0]
	if entry.Value != "两道" {
		t.Fatalf("the board holds %q, want the learner's latest value", entry.Value)
	}
	if entry.EntryOrigin() != model.SessionWhiteboardOriginUser {
		t.Fatalf("the entry's origin = %q, want the learner's: the learner changed it",
			entry.EntryOrigin())
	}
	if len(result.Changes) != 1 || result.Changes[0].OldValue != "三道" {
		t.Fatalf("the change does not record what it replaced: %+v", result.Changes)
	}
}

// A proposal the learner did not take on is not a current fact.
//
// It stays in the summary segment as one of the assistant's products, and it
// never reaches the board: a proposal nobody agreed to is not a decision.
//
// The model writes it as the assistant's proposal, and the board takes it for
// one. A proposal the learner did not take on is not the learner's own words
// either, so nothing about it can be read back as their decision.
func TestTheProposalNobodyTookOnIsNotTheCurrentState(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3}
	batch := withLearnerMessage(
		assistantBatch(90, "给我三道动态规划的练习", 91, "1. 最长递增子序列\n2. 0-1 背包\n3. 区间 DP 入门"),
		92, "第三题有点难")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("practice_plan", "按这三道练习来", "三道", model.SessionWhiteboardOriginAssistant, 90, 91),
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
}

// The assistant's own claim that it did something is a claim. It is not a
// fact about the world and it does not become the learner's state.
func TestTheAssistantsClaimOfExecutionNeverBecomesTheLearnersState(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3}
	batch := assistantBatch(30, "把场地定下来", 31, "场地我已经借好了。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Plan: []model.SessionWhiteboardEntry{
			ownEntry("venue_booked", "场地已借好", "已借好", model.SessionWhiteboardOriginAssistant, 31),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
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

// The learner states a value and the assistant only confirms it. The entry is
// the learner's, whatever label the model wrote on it.
//
// The confirmation comes after the learner's message, so the acceptance is
// not shown - and the value is in the learner's own words, which is what
// says the entry is the learner's.
func TestTheLearnersOwnChangeLabelledAsTheAssistantsIsReadBackAsTheLearners(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("round_question_count", "每轮先做 14 道题。", "14 道", "", 59, 61),
		},
	}
	batch := assistantBatch(93, "改成每轮 9 道题吧", 94, "往后按新的题量走。")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("round_question_count", "每轮 9 道题", "每轮 9 道题",
				model.SessionWhiteboardOriginAssistantAccepted, 93, 94),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 {
		t.Fatalf("the board = %+v", result.Board.Constraints)
	}
	entry := result.Board.Constraints[0]
	if entry.Value != "每轮 9 道题" {
		t.Fatalf("the board holds %q, want the learner's new value", entry.Value)
	}
	if entry.EntryOrigin() != model.SessionWhiteboardOriginUser {
		t.Fatalf("the entry's origin = %q, want the learner's: the words were the learner's",
			entry.EntryOrigin())
	}
	if result.Diagnostics.MislabeledOrigins != 1 {
		t.Fatalf("mislabeled origins = %d, want 1", result.Diagnostics.MislabeledOrigins)
	}
}

// Another item's value does not overwrite this one's.
//
// The two entries are about two things, and they carry the same number. The
// board keeps both: the reader sees two entries, not one entry that changed
// its mind about what it is about.
func TestAnotherItemsValueDoesNotOverwriteThisOne(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("reading_pages", "读书计划每次读 26 页", "26 页", "", 51),
		},
	}
	batch := learnerBatch(map[uint]string{95: "另一本书也改成每次 41 页"}, 95)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("other_book_pages", "另一本书每次读 41 页", "41 页", "", 95),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["reading_pages"] != "26 页" {
		t.Fatalf("the reading plan holds %q, want the value it had", values["reading_pages"])
	}
	if values["other_book_pages"] != "41 页" {
		t.Fatalf("the other book holds %q, want the learner's new value", values["other_book_pages"])
	}
}

// Another unit is another item.
//
// A count of pages and a count of minutes are two values, and the board
// keeps them apart.
func TestAnotherUnitIsAnotherEntry(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("reading_pages", "读书计划每次读 26 页", "26 页", "", 51),
		},
	}
	batch := learnerBatch(map[uint]string{95: "阅读时间改成每次 26 分钟"}, 95)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("reading_minutes", "阅读时间每次 26 分钟", "26 分钟", "", 95),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	values := boardValues(result.Board)
	if values["reading_pages"] != "26 页" {
		t.Fatalf("the page count holds %q, want the value it had", values["reading_pages"])
	}
	if values["reading_minutes"] != "26 分钟" {
		t.Fatalf("the minutes hold %q, want the learner's new value", values["reading_minutes"])
	}
}

// A citation that names nothing the session can account for is refused, and
// the message it named is not authorised for reading back.
func TestACitationNobodyCanAccountForIsRefusedAndAuthorisesNothing(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3}
	batch := learnerBatch(map[uint]string{95: "改成每轮 9 道题吧"}, 95)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("round_question_count", "每轮 9 道题", "每轮 9 道题", "", 999),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the entry survived: %+v", result.Board.Constraints)
	}
	if result.Diagnostics.FabricatedEntries != 1 {
		t.Fatalf("fabricated entries = %d, want 1", result.Diagnostics.FabricatedEntries)
	}
	rendered := renderWhiteboardBlock(result.Board, 2000)
	for _, id := range rendered.EvidenceMessageIDs {
		if id == 999 {
			t.Fatalf("the window shows the message the entry named: %v", rendered.EvidenceMessageIDs)
		}
	}
	layered := LayeredSummarySnapshot{Board: &model.SessionWhiteboard{CoveredMessageID: 999, Version: 3}}
	for _, grant := range whiteboardHistoryGrants(rendered, layered, "session-1") {
		if grant.MessageID == 999 {
			t.Fatalf("the window authorised reading a message nobody can account for: %+v", grant)
		}
	}
}

// A board that carries the statements is read and written: the version stays
// what it was, the learner's own words stay in it, and the new value lands.
func TestTheExistingVersionThreeBoardIsReadAndUpdated(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("round_question_count", "每轮先做 14 道题。", "14 道", "", 59, 61),
		},
		Unverified: []model.SessionWhiteboardAnchor{
			{Key: "round_question_count", Value: "每轮 9 道题", MessageID: 93,
				Text: "改成每轮 9 道题吧", Complete: true,
				Reason: model.SessionWhiteboardAnchorUnplaced},
		},
	}
	batch := learnerBatch(map[uint]string{95: "改成每轮 11 道题吧"}, 95)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("round_question_count", "每轮 11 道题", "每轮 11 道题", "", 95),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if result.Board.SchemaVersion != model.SessionWhiteboardSchemaV3 {
		t.Fatalf("the board is version %d, want the version it had", result.Board.SchemaVersion)
	}
	if got := boardValues(result.Board)["round_question_count"]; got != "每轮 11 道题" {
		t.Fatalf("the board holds %q, want the learner's new value", got)
	}
	if len(result.Board.Unverified) != 1 {
		t.Fatalf("the board carries %+v, want the learner's own words to stay",
			result.Board.Unverified)
	}
	content, err := model.MarshalSessionWhiteboardDocument(result.Board)
	if err != nil {
		t.Fatalf("the board that would be stored is not a valid document: %v", err)
	}
	stored, err := model.DecodeSessionWhiteboardDocument(content)
	if err != nil {
		t.Fatalf("the board that would be stored cannot be read back: %v", err)
	}
	if len(stored.Unverified) != 1 {
		t.Fatalf("the stored board carries %+v, want the learner's own words", stored.Unverified)
	}
}

// The mode that does not record the assistant's products does not hold them.
//
// A proposal of the assistant's is refused in that mode too, and the board
// keeps the learner's own facts exactly as the other mode writes them.
func TestTheModeWithoutTheAssistantsProductsDoesNotHoldThem(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}
	batch := withLearnerMessage(
		assistantBatch(90, "给我三道动态规划的练习", 91, "1. 最长递增子序列\n2. 0-1 背包"),
		92, "好，就按这三道来")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("practice_plan", "按这三道练习来", "三道", model.SessionWhiteboardOriginAssistantAccepted, 91, 92),
		},
	}, batch, learnerOnlyPolicy(model.SessionWhiteboardSchemaV1))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the product of the assistant's is on the board: %+v", result.Board.Constraints)
	}
	if result.Diagnostics.AssistantProposals != 1 {
		t.Fatalf("assistant proposals = %d, want 1", result.Diagnostics.AssistantProposals)
	}
}

// The product of the assistant's is in the summary segment, it says whose
// words it holds, and the window authorises reading the message back.
//
// This is the whole point of the mode: the exercises, the names and the
// numbers the assistant gave are findable later.
func TestTheAssistantsProductIsInTheSegmentAndCanBeReadBack(t *testing.T) {
	generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"练习","points":[`+
		`{"text":"给了三道动态规划的练习：最长递增子序列、0-1 背包、区间 DP 入门。",`+
		`"evidence_message_ids":[31],"source":"assistant"}]},`+
		`"whiteboard":{"schema_version":1,"constraints":[`+
		`{"key":"practice_set","text":"三道动态规划的练习","value":"三道",`+
		`"source_message_ids":[31],"origin":"assistant"}]}}`)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	products := 0
	for _, point := range result.Segment.Points {
		if point.PointSource() != model.SessionSummarySegmentSourceAssistant {
			continue
		}
		products++
		if !whiteboardCitesID(point.EvidenceMessageIDs, 31) {
			t.Fatalf("the product cites %v, want the assistant's own message",
				point.EvidenceMessageIDs)
		}
	}
	if products == 0 {
		t.Fatalf("the product of the assistant's is not in the segment: %+v", result.Segment.Points)
	}
	// The product is not the learner's decision.
	if len(result.Whiteboard.Constraints) != 0 {
		t.Fatalf("the product is on the board: %+v", result.Whiteboard.Constraints)
	}
	// And the window authorises reading the assistant's message back.
	segment := model.SessionSummarySegment{
		StartMessageID: 30, EndMessageID: 33, Content: result.SegmentContent,
	}
	rendered := renderSummarySegments([]model.SessionSummarySegment{segment},
		model.SessionWhiteboardDocument{}, 4000, 450, true)
	shown := []uint{}
	for _, carried := range rendered.Segments {
		shown = append(shown, carried.EvidenceMessageIDs...)
	}
	found := false
	for _, id := range shown {
		if id == 31 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the window shows %v, want the assistant's message among them", shown)
	}
	grants := summarySegmentHistoryGrants(rendered.Segments, "session-1")
	granted := false
	for _, grant := range grants {
		if grant.MessageID == 31 {
			granted = true
		}
	}
	if !granted {
		t.Fatalf("the read-back grants = %+v, want the assistant's message", grants)
	}
}

// The learner's own words are not compared with the model's summary of them.
//
// The value lands, the entry's text says what the model said, and the board
// does not insist that the two use the same words: the learner who says
// "改成每轮 9 道题吧" has said what the learner said.
func TestTheModelDoesNotHaveToSummariseTheLearnersWords(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3}
	batch := learnerBatch(map[uint]string{93: "改成每轮 9 道题吧"}, 93)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("round_question_count", "题量已调整为 9 道/轮", "9 道/轮", "", 93),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if got := boardValues(result.Board)["round_question_count"]; got != "9 道/轮" {
		t.Fatalf("the board holds %q, want the learner's new value", got)
	}
	if len(result.Board.Unverified) != 0 {
		t.Fatalf("the board carries %+v, want no statement: nothing was refused",
			result.Board.Unverified)
	}
	if result.Diagnostics.DroppedEntries != 0 {
		t.Fatalf("dropped entries = %d, want 0: the learner's own words are not checked",
			result.Diagnostics.DroppedEntries)
	}
}

// The board says whose words an entry holds.
//
// A reader has to be able to tell the learner's decision from the
// assistant's proposal that the learner took on.
func TestTheBoardSaysWhoseWordsAnEntryHolds(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3}
	batch := withLearnerMessage(
		assistantBatch(90, "给我三道动态规划的练习", 91, "1. 最长递增子序列\n2. 0-1 背包"),
		92, "好，就按这三道来")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("practice_plan", "按这三道练习来", "三道", model.SessionWhiteboardOriginAssistantAccepted, 91, 92),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	rendered := renderWhiteboardBlock(result.Board, 2000)
	if !strings.Contains(rendered.Text, "助手") {
		t.Fatalf("the block does not say whose words the entry holds: %s", rendered.Text)
	}
}
