package agent

// What a summary segment gives up when it does not fit its budget.
//
// A point is two things at once: what was said, and the name of the message
// it was said in. The name is what lets the reader read that message back,
// and the reader can only read what the segment names. The original is in
// the database, and a model cannot open the database.
//
// So the order in which a segment spends a budget it does not have is: the
// words that say the same thing twice go first, then the words go, and only
// then is a point given up - and only when every message it names is named
// by a point that is kept.
//
// The tests here are about that order, and about the case the order has to
// get right: a point that is the only thing naming a message.

import (
	"strings"
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// budgetSegment is a segment with the points it is given.
func budgetSegment(points ...model.SessionSummarySegmentPoint) model.SessionSummarySegmentContent {
	return model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2,
		Topic:         "练习",
		Points:        points,
	}
}

// segmentNames is every message the segment names.
func segmentNames(points []model.SessionSummarySegmentPoint) map[uint]bool {
	names := map[uint]bool{}
	for _, point := range points {
		for _, id := range point.EvidenceMessageIDs {
			names[id] = true
		}
	}
	return names
}

// The assistant's product is the only thing naming the message it came from,
// and the segment does not fit.
//
// The product is kept and the message stays named. The words are cut
// instead, and the cut is taken from the point that costs the most - the
// learner's own point, which can be hundreds of runes long - and not from
// the product, which is short by construction.
//
// This is the case that lost a message before: the product was given up, the
// message stopped being pointed at, and a learner asking about what the
// assistant had said was told that nothing had ever been said.
func TestTheProductThatNamesAMessageSurvivesATightSegment(t *testing.T) {
	content := budgetSegment(
		model.SessionSummarySegmentPoint{
			Text:               strings.Repeat("学习者把每次学习总时长定为三十分钟", 12),
			EvidenceMessageIDs: []uint{65},
		},
		model.SessionSummarySegmentPoint{
			Text:               "练习只用本机已有环境，不新增安装",
			EvidenceMessageIDs: []uint{66},
			Source:             model.SessionSummarySegmentSourceAssistant,
		},
	)
	trimmed, dropped := trimSegmentToRenderedBudget(content, 65, 66, 450, true)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0: %+v", dropped, trimmed.Points)
	}
	names := segmentNames(trimmed.Points)
	if !names[65] || !names[66] {
		t.Fatalf("a message lost its name: %+v", trimmed.Points)
	}
	product := ""
	for _, point := range trimmed.Points {
		if point.PointSource() == model.SessionSummarySegmentSourceAssistant {
			product = point.Text
		}
	}
	if product != "练习只用本机已有环境，不新增安装" {
		t.Fatalf("the product was cut: %q", product)
	}
	if size := SegmentContentTokens(trimmed, 65, 66); size > 450 {
		t.Fatalf("the segment is %d tokens, over the budget of 450", size)
	}
}

// The point of the learner's is the only thing naming its message.
//
// It is not given up, and neither is the product of the assistant's that
// names another message alone. The rule is the same on both sides: the
// point that is the last name of a message is kept, because the message
// would stop being readable otherwise.
func TestTheLearnersPointThatNamesAMessageIsNotGivenUp(t *testing.T) {
	content := budgetSegment(
		model.SessionSummarySegmentPoint{
			Text:               "学习者说过的一句话",
			EvidenceMessageIDs: []uint{30},
		},
		model.SessionSummarySegmentPoint{
			Text:               "助手说过的另一句话",
			EvidenceMessageIDs: []uint{31},
			Source:             model.SessionSummarySegmentSourceAssistant,
		},
	)
	trimmed, dropped := trimSegmentToRenderedBudget(content, 30, 31, 1, true)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0: %+v", dropped, trimmed.Points)
	}
	names := segmentNames(trimmed.Points)
	if !names[30] || !names[31] {
		t.Fatalf("a message lost its name: %+v", trimmed.Points)
	}
}

// A product that another point also names is given up first: the message
// stays named, so nothing is lost but the product's own words.
func TestTheProductThatAnotherPointAlsoNamesGoesFirst(t *testing.T) {
	content := budgetSegment(
		model.SessionSummarySegmentPoint{
			Text:               "助手先给的方案",
			EvidenceMessageIDs: []uint{31},
			Source:             model.SessionSummarySegmentSourceAssistant,
		},
		model.SessionSummarySegmentPoint{
			Text:               "学习者说过的另一件事",
			EvidenceMessageIDs: []uint{30},
		},
		model.SessionSummarySegmentPoint{
			Text:               "助手后给的方案",
			EvidenceMessageIDs: []uint{31},
			Source:             model.SessionSummarySegmentSourceAssistant,
		},
	)
	trimmed, dropped := trimSegmentToRenderedBudget(content, 30, 31, 1, true)
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1: %+v", dropped, trimmed.Points)
	}
	names := segmentNames(trimmed.Points)
	if !names[30] || !names[31] {
		t.Fatalf("a message lost its name: %+v", trimmed.Points)
	}
	for _, point := range trimmed.Points {
		if point.Text == "助手先给的方案" {
			t.Fatalf("the older product was kept: %+v", trimmed.Points)
		}
	}
}

// Two points that carry the same text are one point written twice.
//
// The one that is left names every message both of them named, so the merge
// loses no message.
func TestTheDuplicatePointsAreMergedAndTheNamesAreKept(t *testing.T) {
	content := budgetSegment(
		model.SessionSummarySegmentPoint{
			Text: "同一件事", EvidenceMessageIDs: []uint{30},
		},
		model.SessionSummarySegmentPoint{
			Text: "同一件事", EvidenceMessageIDs: []uint{31},
		},
	)
	trimmed, dropped := trimSegmentToRenderedBudget(content, 30, 31, 450, true)
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1: %+v", dropped, trimmed.Points)
	}
	if len(trimmed.Points) != 1 {
		t.Fatalf("points = %d, want 1: %+v", len(trimmed.Points), trimmed.Points)
	}
	names := segmentNames(trimmed.Points)
	if !names[30] || !names[31] {
		t.Fatalf("the merge lost a name: %+v", trimmed.Points)
	}
}

// namedBudget is a budget that holds the names of the points and not their
// words.
//
// It is the budget the rule is about: the segment can afford to say that
// something happened in a message, and not to say what.
func namedBudget(t *testing.T, points []model.SessionSummarySegmentPoint) int {
	t.Helper()
	named := make([]model.SessionSummarySegmentPoint, 0, len(points))
	for _, point := range points {
		point.Text = "…"
		named = append(named, point)
	}
	raw, err := model.MarshalSessionSummarySegmentContent(budgetSegment(named...))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	segment := model.SessionSummarySegment{Seq: 1, StartMessageID: 30, EndMessageID: 31, Content: raw}
	block, _, _ := renderSummarySegment(segment, model.SessionWhiteboardDocument{}, 31, 0)
	return rag.EstimateTokens(block) + 4*len(points) + 2
}

// The renderer gives up the same way: a point that is the last name of a
// message is not given up, so the message stays readable.
//
// The budget here holds the names and not the words, which is the case the
// rule is about: what the segment gives up is the words.
func TestTheRendererKeepsThePointThatNamesAMessageAlone(t *testing.T) {
	points := []model.SessionSummarySegmentPoint{
		{Text: "学习者说过的一句话", EvidenceMessageIDs: []uint{30}},
		{Text: "助手说过的另一句话", EvidenceMessageIDs: []uint{31},
			Source: model.SessionSummarySegmentSourceAssistant},
	}
	raw, err := model.MarshalSessionSummarySegmentContent(budgetSegment(points...))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	segment := model.SessionSummarySegment{Seq: 1, StartMessageID: 30, EndMessageID: 31, Content: raw}
	block, evidence, trimmed := renderSummarySegment(segment, model.SessionWhiteboardDocument{}, 31,
		namedBudget(t, points))
	if !trimmed {
		t.Fatal("the segment was not cut, so the test proves nothing")
	}
	readable := map[uint]bool{}
	for _, id := range evidence {
		readable[id] = true
	}
	if !readable[30] || !readable[31] {
		t.Fatalf("a message is not readable any more: %v", evidence)
	}
	if !strings.Contains(block, "[message_ids=30]") || !strings.Contains(block, "[message_ids=31]") {
		t.Fatalf("the block does not name both messages: %q", block)
	}
}
