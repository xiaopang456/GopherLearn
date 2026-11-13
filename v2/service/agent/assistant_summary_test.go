package agent

import (
	"context"
	"strings"
	"testing"

	"GopherAI/model"
)

// The mode that also records what the assistant produced.
//
// The tests here are about what the program refuses to keep: a point that
// claims the learner accepted a proposal the citations do not show, an
// assistant row that was never cited, and a segment written before the
// source field existed.

// assistantSegmentGenerator builds a generator in the mode that records the
// assistant's products.
func assistantSegmentGenerator(t *testing.T, content string) (*ModelSegmentGenerator, *stubSegmentModel) {
	t.Helper()
	stub := &stubSegmentModel{content: content}
	generator, err := NewModelSegmentGenerator(stub, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	return generator, stub
}

// assistantTestMessages is one batch: the assistant offers three exercises,
// the learner takes them on, and then says something else.
func assistantTestMessages() []model.Message {
	return []model.Message{
		{ID: 30, IsUser: true, Content: "给我三道动态规划的练习"},
		{ID: 31, IsUser: false, Content: "1. 最长递增子序列\n2. 0-1 背包\n3. 区间 DP 入门"},
		{ID: 32, IsUser: true, Content: "好，就按这三道来"},
		{ID: 33, IsUser: true, Content: "第一题我做完了"},
	}
}

func assistantTestInput() SegmentGenerationInput {
	return SegmentGenerationInput{
		Messages:       assistantTestMessages(),
		FirstMessageID: 30,
		LastMessageID:  33,
	}
}

// The mode that does not record the assistant's products behaves exactly as
// it did: the assistant's body is replaced by a placeholder, and the
// generator sends the prompt it always sent.
func TestTheAssistantModeIsOffByDefault(t *testing.T) {
	stub := &stubSegmentModel{content: `{"schema_version":1,"segment":{"topic":"t","points":[{"text":"p","evidence_message_ids":[30]}]}}`}
	generator, err := NewModelSegmentGenerator(stub, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if generator.AssistantContentEnabled() {
		t.Fatal("the generator records the assistant's products without being asked to")
	}
	if _, err := generator.GenerateSegment(context.Background(), assistantTestInput()); err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if len(stub.prompts) == 0 {
		t.Fatal("the generator asked nothing")
	}
	if stub.prompts[0].Content != segmentSystemPrompt {
		t.Fatal("the mode that is off sends a different prompt")
	}
	payload := stub.prompts[1].Content
	if !strings.Contains(payload, "[assistant body omitted: not authoritative durable user state]") {
		t.Fatalf("the assistant's body is not replaced: %q", payload)
	}
	if strings.Contains(payload, "最长递增子序列") {
		t.Fatalf("the assistant's body reaches the model: %q", payload)
	}
}

// The mode on shows the assistant's body, trimmed to what a product is made
// of, and says which role each message had.
func TestTheAssistantModeShowsTheAssistantBody(t *testing.T) {
	generator, stub := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[{"text":"p","evidence_message_ids":[30]}]}}`)
	if _, err := generator.GenerateSegment(context.Background(), assistantTestInput()); err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if stub.prompts[0].Content != segmentAssistantSystemPrompt {
		t.Fatal("the mode that is on sends the wrong prompt")
	}
	payload := stub.prompts[1].Content
	if !strings.Contains(payload, "[message_id=31 role=assistant]") {
		t.Fatalf("the assistant's message is not shown as an assistant's: %q", payload)
	}
	if !strings.Contains(payload, "最长递增子序列") {
		t.Fatalf("the assistant's products are not shown: %q", payload)
	}
}

// The knowledge base's citation markers are removed from the assistant's
// body.
//
// A bracketed number in an assistant message points at a document passage,
// and inside a summary it would read as a conversation message id.
func TestTheAssistantBodyDropsTheCitationMarkers(t *testing.T) {
	body := renderAssistantBody("下面是三道题 [1] 第二题 [12] 讲完了", assistantBodyTokens)
	if strings.Contains(body, "[1]") || strings.Contains(body, "[12]") {
		t.Fatalf("the citation markers survived: %q", body)
	}
	if !strings.Contains(body, "下面是三道题") {
		t.Fatalf("the body was lost: %q", body)
	}
}

// The prose around a product is dropped and the product is kept.
//
// An explanation can be written again when it is needed, and a summary has
// no room for it.
func TestTheAssistantBodyKeepsTheProductsAndNotTheProse(t *testing.T) {
	body := renderAssistantBody("这个知识点其实挺有意思的\n- 第一题：斐波那契\n- 第二题：动态规划\n最后祝你好运", assistantBodyTokens)
	if !strings.Contains(body, "第一题：斐波那契") {
		t.Fatalf("the product was dropped: %q", body)
	}
	if strings.Contains(body, "挺有意思的") {
		t.Fatalf("the prose was kept: %q", body)
	}
}

// A point about the assistant has to cite an assistant message, and a point
// about the learner has to cite a learner message.
//
// The rule is the program's, not the model's: a model that writes "user" on
// a point it made up is a point about something the learner never said.
func TestAPointIsCheckedAgainstItsSource(t *testing.T) {
	cases := []struct {
		name  string
		point string
		want  string
	}{
		{
			name:  "an assistant product citing the assistant's message",
			point: `{"text":"三道动态规划练习","evidence_message_ids":[31],"source":"assistant"}`,
			want:  model.SessionSummarySegmentSourceAssistant,
		},
		{
			name:  "a learner statement citing the learner's message",
			point: `{"text":"要三道动态规划的练习","evidence_message_ids":[30],"source":"user"}`,
			want:  model.SessionSummarySegmentSourceUser,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[`+
				testCase.point+`]}}`)
			result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
			if err != nil {
				t.Fatalf("generate segment: %v", err)
			}
			if len(result.Segment.Points) != 1 {
				t.Fatalf("points = %d, want 1", len(result.Segment.Points))
			}
			if got := result.Segment.Points[0].PointSource(); got != testCase.want {
				t.Fatalf("source = %q, want %q", got, testCase.want)
			}
		})
	}
}

// A point about the learner that cites an assistant message is dropped: it
// says the learner said something they did not.
func TestALearnerPointMayNotCiteTheAssistant(t *testing.T) {
	generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[
		{"text":"要三道练习","evidence_message_ids":[30],"source":"user"},
		{"text":"learner said it","evidence_message_ids":[31],"source":"user"}]}}`)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if len(result.Segment.Points) != 1 {
		t.Fatalf("points = %d, want 1", len(result.Segment.Points))
	}
	if result.Segment.Points[0].EvidenceMessageIDs[0] != 30 {
		t.Fatalf("the wrong point survived: %+v", result.Segment.Points[0])
	}
}

// A point that cites a message this batch never contained is dropped.
func TestAPointCitingAMessageThatIsNotThereIsDropped(t *testing.T) {
	generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[
		{"text":"a point","evidence_message_ids":[999],"source":"assistant"}]}}`)
	_, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err == nil {
		t.Fatal("a segment with no usable point must fail the batch")
	}
	if !strings.Contains(err.Error(), "no usable point") {
		t.Fatalf("the failure does not say why: %v", err)
	}
}

// An accepted proposal has to show the learner accepting it.
//
// The proof is a pair: the assistant's message and a learner's message after
// it. One without the other is not acceptance.
func TestAnAcceptedProposalNeedsTheAcceptanceEvidence(t *testing.T) {
	cases := []struct {
		name string
		ids  string
		want string
	}{
		{
			name: "the assistant's message and the learner's message after it",
			ids:  "[31,32]",
			want: model.SessionSummarySegmentSourceAssistantAccepted,
		},
		{
			name: "the learner's message before the assistant's message",
			ids:  "[30,31]",
			want: model.SessionSummarySegmentSourceAssistant,
		},
		{
			name: "the assistant's message alone",
			ids:  "[31]",
			want: model.SessionSummarySegmentSourceAssistant,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[
				{"text":"三道练习","evidence_message_ids":`+testCase.ids+`,"source":"assistant_accepted"}]}}`)
			result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
			if err != nil {
				t.Fatalf("generate segment: %v", err)
			}
			if len(result.Segment.Points) != 1 {
				t.Fatalf("points = %d, want 1", len(result.Segment.Points))
			}
			if got := result.Segment.Points[0].PointSource(); got != testCase.want {
				t.Fatalf("source = %q, want %q", got, testCase.want)
			}
		})
	}
}

// A board entry that claims the learner accepted a proposal is only on the
// board when the citations show it.
//
// A proposal nobody accepted is not a current fact, so it stays in the
// summary segment.
func TestTheBoardOnlyTakesAnAcceptedProposalThatIsProven(t *testing.T) {
	cases := []struct {
		name     string
		ids      string
		onBoard  bool
		unproven int
	}{
		{name: "proven", ids: "[31,32]", onBoard: true},
		{name: "the learner spoke first", ids: "[30,31]", onBoard: false, unproven: 1},
		{name: "no learner message at all", ids: "[31]", onBoard: false, unproven: 1},
		// A message that is not there is refused earlier: the citation
		// itself is the problem, and the entry is dropped for that reason
		// rather than for the acceptance.
		{name: "a message that is not there", ids: "[31,999]", onBoard: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[
				{"text":"三道练习","evidence_message_ids":[31],"source":"assistant"}]},
				"whiteboard":{"schema_version":1,"constraints":[
				{"key":"plan","text":"按三道练习来","value":"三道","source_message_ids":`+testCase.ids+`,"origin":"assistant_accepted"}]}}`)
			result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
			if err != nil {
				t.Fatalf("generate segment: %v", err)
			}
			onBoard := len(result.Whiteboard.Constraints) == 1
			if onBoard != testCase.onBoard {
				t.Fatalf("on the board = %v, want %v (%+v)", onBoard, testCase.onBoard, result.Whiteboard.Constraints)
			}
			if result.Diagnostics.UnprovenAcceptances != testCase.unproven {
				t.Fatalf("unproven acceptances = %d, want %d",
					result.Diagnostics.UnprovenAcceptances, testCase.unproven)
			}
		})
	}
}

// An origin the engine does not know is an origin it cannot check.
func TestTheBoardRefusesAnUnknownOrigin(t *testing.T) {
	generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[
		{"text":"三道练习","evidence_message_ids":[31],"source":"assistant"}]},
		"whiteboard":{"schema_version":1,"constraints":[
		{"key":"plan","text":"按三道练习来","source_message_ids":[30],"origin":"the_model_made_this_up"}]}}`)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if len(result.Whiteboard.Constraints) != 0 {
		t.Fatalf("an entry with an unknown origin reached the board: %+v", result.Whiteboard.Constraints)
	}
	if result.Diagnostics.UnknownOrigins != 1 {
		t.Fatalf("unknown origins = %d, want 1", result.Diagnostics.UnknownOrigins)
	}
}

// An entry of the learner's may not cite the assistant's message.
//
// The origin decides the set of messages an entry may cite, and widening it
// for the accepted proposals must not widen it for the learner's own.
func TestALearnerEntryMayNotCiteTheAssistant(t *testing.T) {
	generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[
		{"text":"三道练习","evidence_message_ids":[31],"source":"assistant"}]},
		"whiteboard":{"schema_version":1,"constraints":[
		{"key":"plan","text":"按三道练习来","source_message_ids":[31],"origin":"user"}]}}`)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if len(result.Whiteboard.Constraints) != 0 {
		t.Fatalf("an entry of the learner's cited the assistant: %+v", result.Whiteboard.Constraints)
	}
}

// Over budget, the products nobody accepted go first.
//
// Nobody agreed to them, so they are the least the segment is worth keeping.
// The learner's own points and the products the learner took on are not
// given up at all: the renderer trims them for display, which is a
// different thing from the segment dropping them.
func TestTheUnacceptedProductsGoFirstWhenTheSegmentIsOverBudget(t *testing.T) {
	content := model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2,
		Topic:         "t",
		Points: []model.SessionSummarySegmentPoint{
			{Text: "旧的助手产物", EvidenceMessageIDs: []uint{31}, Source: model.SessionSummarySegmentSourceAssistant},
			{Text: "学习者说过的话", EvidenceMessageIDs: []uint{30}, Source: model.SessionSummarySegmentSourceUser},
			{Text: "被接受的助手产物", EvidenceMessageIDs: []uint{31, 32}, Source: model.SessionSummarySegmentSourceAssistantAccepted},
		},
	}
	trimmed, dropped := trimSegmentToRenderedBudget(content, 30, 33, 1, true)
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	for _, point := range trimmed.Points {
		if point.Source == model.SessionSummarySegmentSourceAssistant {
			t.Fatalf("the unaccepted product survived: %+v", trimmed.Points)
		}
	}
	if len(trimmed.Points) != 2 {
		t.Fatalf("points = %d, want 2: %+v", len(trimmed.Points), trimmed.Points)
	}
}

// Two products, each the only thing naming the message it came from.
//
// Neither is given up. The message would stop being readable, and the reader
// can only read what the segment names - the original is in the database,
// and a model cannot open the database. What is cut instead is the words.
func TestTheProductsThatNameAMessageAreNotGivenUp(t *testing.T) {
	content := model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2,
		Topic:         "t",
		Points: []model.SessionSummarySegmentPoint{
			{Text: "新的助手产物", EvidenceMessageIDs: []uint{34}, Source: model.SessionSummarySegmentSourceAssistant},
			{Text: "旧的助手产物", EvidenceMessageIDs: []uint{31}, Source: model.SessionSummarySegmentSourceAssistant},
		},
	}
	trimmed, dropped := trimSegmentToRenderedBudget(content, 30, 34, 1, true)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0: %+v", dropped, trimmed.Points)
	}
	if len(trimmed.Points) != 2 {
		t.Fatalf("points = %d, want 2: %+v", len(trimmed.Points), trimmed.Points)
	}
	names := map[uint]bool{}
	for _, point := range trimmed.Points {
		for _, id := range point.EvidenceMessageIDs {
			names[id] = true
		}
	}
	if !names[31] || !names[34] {
		t.Fatalf("a message lost its name: %+v", trimmed.Points)
	}
}

// The mode that is off keeps the old rule exactly: the tail goes first.
func TestTheOffModeStillDropsTheTail(t *testing.T) {
	content := model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2,
		Topic:         "t",
		Points: []model.SessionSummarySegmentPoint{
			{Text: "第一点", EvidenceMessageIDs: []uint{30}},
			{Text: "第二点", EvidenceMessageIDs: []uint{31}},
			{Text: "第三点", EvidenceMessageIDs: []uint{32}},
		},
	}
	trimmed, dropped := trimSegmentToRenderedBudget(content, 30, 33, 1, false)
	if dropped == 0 {
		t.Fatal("nothing was dropped")
	}
	if trimmed.Points[0].Text != "第一点" {
		t.Fatalf("the first point did not survive: %+v", trimmed.Points)
	}
	if trimmed.Points[len(trimmed.Points)-1].Text == "第三点" {
		t.Fatalf("the tail did not go first: %+v", trimmed.Points)
	}
}

// A segment written before the source field existed is the learner's.
//
// It is read, not rewritten: the field is absent, and the reader knows what
// an absent field means.
func TestASegmentWrittenBeforeTheSourceFieldIsTheLearners(t *testing.T) {
	content := `{"schema_version":1,"topic":"t","points":[{"text":"学习者说过的话","evidence_message_ids":[30]}]}`
	document, err := model.DecodeSessionSummarySegmentContent(content)
	if err != nil {
		t.Fatalf("decode an old segment: %v", err)
	}
	if len(document.Points) != 1 {
		t.Fatalf("points = %d", len(document.Points))
	}
	if got := document.Points[0].PointSource(); got != model.SessionSummarySegmentSourceUser {
		t.Fatalf("source = %q, want %q", got, model.SessionSummarySegmentSourceUser)
	}
}

// A board written before the origin field existed is the learner's too.
func TestABoardWrittenBeforeTheOriginFieldIsTheLearners(t *testing.T) {
	content := `{"schema_version":1,"constraints":[{"key":"duration","text":"每天 48 分钟","source_message_ids":[30]}]}`
	document, err := model.DecodeSessionWhiteboardDocument(content)
	if err != nil {
		t.Fatalf("decode an old board: %v", err)
	}
	if len(document.Constraints) != 1 {
		t.Fatalf("constraints = %d", len(document.Constraints))
	}
	if got := document.Constraints[0].EntryOrigin(); got != model.SessionWhiteboardOriginUser {
		t.Fatalf("origin = %q, want %q", got, model.SessionWhiteboardOriginUser)
	}
}

// The reader sees whose words a line holds.
//
// The field is what the program checks, and the label is what the model
// reads; a model that cannot tell them apart will quote an assistant's
// product as if the learner had said it.
func TestTheRenderedLinesSayWhoseWordsTheyHold(t *testing.T) {
	document := model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2,
		Topic:         "t",
		Points: []model.SessionSummarySegmentPoint{
			{Text: "学习者说过的话", EvidenceMessageIDs: []uint{30}, Source: model.SessionSummarySegmentSourceUser},
			{Text: "助手出的三道题", EvidenceMessageIDs: []uint{31}, Source: model.SessionSummarySegmentSourceAssistant},
			{Text: "被接受的计划", EvidenceMessageIDs: []uint{31, 32}, Source: model.SessionSummarySegmentSourceAssistantAccepted},
		},
	}
	raw, err := model.MarshalSessionSummarySegmentContent(document)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	segment := model.SessionSummarySegment{
		StartMessageID: 30, EndMessageID: 33, Content: raw,
	}
	block, _, _ := renderSummarySegment(segment, model.SessionWhiteboardDocument{}, 33, 0)
	if !strings.Contains(block, "（助手提议，用户未表示同意）助手出的三道题") {
		t.Fatalf("the assistant's product is not labelled: %q", block)
	}
	if !strings.Contains(block, "（助手提议，用户已同意）被接受的计划") {
		t.Fatalf("the accepted proposal is not labelled: %q", block)
	}
	if strings.Contains(block, "（助手提议，用户未表示同意）学习者说过的话") {
		t.Fatalf("the learner's own words are labelled as the assistant's: %q", block)
	}
	entry := model.SessionWhiteboardEntry{
		Key: "plan", Text: "按三道练习来", Origin: model.SessionWhiteboardOriginAssistantAccepted,
		SourceMessageIDs: []uint{31, 32},
	}
	line := renderWhiteboardEntry(entry)
	if !strings.Contains(line, "（助手提议，用户已同意）") {
		t.Fatalf("the board entry is not labelled: %q", line)
	}
}

// The assistant's products are found by the search that finds everything
// else.
//
// A product the learner will later ask about has to be reachable by the
// same preselection the learner's own statements are found by.
func TestTheAssistantProductsAreSearched(t *testing.T) {
	content := model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV2,
		Topic:         "练习",
		Points: []model.SessionSummarySegmentPoint{
			{Text: "学习者要三道题", EvidenceMessageIDs: []uint{30}, Source: model.SessionSummarySegmentSourceUser},
			{Text: "最长递增子序列", EvidenceMessageIDs: []uint{31}, Source: model.SessionSummarySegmentSourceAssistant},
		},
	}
	raw, err := model.MarshalSessionSummarySegmentContent(content)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	segment := model.SessionSummarySegment{Content: raw, Topic: "练习"}
	if !strings.Contains(segment.SearchText(), "最长递增子序列") {
		t.Fatalf("the assistant's product is not in the search text: %q", segment.SearchText())
	}
}

// The stub model is asked the questions that decide the mode.
//
// Three fixed conversations: the learner takes the products on, the learner
// only says thank you, and the learner changes a time the assistant gave.
func TestTheGeneratorAsksTheQuestionsThatDecideTheMode(t *testing.T) {
	cases := []struct {
		name     string
		messages []model.Message
		answer   string
		want     string
	}{
		{
			name: "the learner takes the products on",
			messages: []model.Message{
				{ID: 40, IsUser: true, Content: "出三道题"},
				{ID: 41, IsUser: false, Content: "1. 斐波那契\n2. 背包\n3. 区间 DP"},
				{ID: 42, IsUser: true, Content: "好，就按这三道来"},
			},
			answer: `{"schema_version":1,"segment":{"topic":"练习","points":[
				{"text":"三道练习：斐波那契、背包、区间 DP","evidence_message_ids":[41,42],"source":"assistant_accepted"}]},
				"whiteboard":{"schema_version":1,"constraints":[
				{"key":"plan","text":"按三道练习来","source_message_ids":[41,42],"origin":"assistant_accepted"}]}}`,
			want: model.SessionSummarySegmentSourceAssistantAccepted,
		},
		{
			name: "the learner only says thank you",
			messages: []model.Message{
				{ID: 40, IsUser: true, Content: "出三道题"},
				{ID: 41, IsUser: false, Content: "1. 斐波那契\n2. 背包\n3. 区间 DP"},
				{ID: 42, IsUser: true, Content: "谢谢"},
			},
			answer: `{"schema_version":1,"segment":{"topic":"练习","points":[
				{"text":"三道练习：斐波那契、背包、区间 DP","evidence_message_ids":[41],"source":"assistant"}]},
				"whiteboard":{"schema_version":1,"constraints":[]}}`,
			want: model.SessionSummarySegmentSourceAssistant,
		},
		{
			name: "the learner changes a time the assistant gave",
			messages: []model.Message{
				{ID: 40, IsUser: true, Content: "每天练多久"},
				{ID: 41, IsUser: false, Content: "建议每天 30 分钟"},
				{ID: 42, IsUser: true, Content: "改成 48 分钟"},
			},
			answer: `{"schema_version":1,"segment":{"topic":"时长","points":[
				{"text":"助手建议每天 30 分钟","evidence_message_ids":[41],"source":"assistant"},
				{"text":"每天改成 48 分钟","evidence_message_ids":[42],"source":"user"}]},
				"whiteboard":{"schema_version":1,"constraints":[
				{"key":"duration","text":"每天 48 分钟","value":"48 分钟","source_message_ids":[42],"origin":"user"}]}}`,
			want: model.SessionSummarySegmentSourceUser,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			generator, stub := assistantSegmentGenerator(t, testCase.answer)
			input := SegmentGenerationInput{
				Messages:       testCase.messages,
				FirstMessageID: testCase.messages[0].ID,
				LastMessageID:  testCase.messages[len(testCase.messages)-1].ID,
			}
			result, err := generator.GenerateSegment(context.Background(), input)
			if err != nil {
				t.Fatalf("generate segment: %v", err)
			}
			if len(result.Segment.Points) == 0 {
				t.Fatal("the segment carries nothing")
			}
			found := false
			for _, point := range result.Segment.Points {
				if point.PointSource() == testCase.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("no point carries the source %q: %+v", testCase.want, result.Segment.Points)
			}
			if stub.calls == 0 {
				t.Fatal("the generator asked nothing")
			}
		})
	}
}

// The board the assistant mode produces keeps the entries of the learner
// exactly as the mode that is off does.
//
// The only thing that may be added is an accepted proposal, and only with
// its proof.
func TestTheAssistantModeLeavesTheLearnersEntriesAlone(t *testing.T) {
	answer := `{"schema_version":1,"segment":{"topic":"t","points":[
		{"text":"学习者说过的话","evidence_message_ids":[30],"source":"user"}]},
		"whiteboard":{"schema_version":1,"constraints":[
		{"key":"duration","text":"每天 48 分钟","value":"48 分钟","source_message_ids":[30],"origin":"user"}]}}`
	generator, _ := assistantSegmentGenerator(t, answer)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if len(result.Whiteboard.Constraints) != 1 {
		t.Fatalf("constraints = %d", len(result.Whiteboard.Constraints))
	}
	entry := result.Whiteboard.Constraints[0]
	if entry.EntryOrigin() != model.SessionWhiteboardOriginUser {
		t.Fatalf("origin = %q, want %q", entry.EntryOrigin(), model.SessionWhiteboardOriginUser)
	}
	if entry.Value != "48 分钟" {
		t.Fatalf("value = %q", entry.Value)
	}
}

// The generator's answers are still refused when they are not what was
// asked for.
//
// The mode that records the assistant's products does not relax the checks
// the mode that does not have.
func TestTheAssistantModeStillRefusesAFabricatedPoint(t *testing.T) {
	generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[
		{"text":"a point","evidence_message_ids":[999],"source":"user"}]}}`)
	_, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err == nil {
		t.Fatal("a segment with no usable point must fail the batch")
	}
}

// The tool the model reads is the same one the mode that is off reads.
//
// The prompts are the ones the generator sends, and the stub answers them.
func TestTheAssistantModeSendsItsOwnPrompt(t *testing.T) {
	generator, stub := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[
		{"text":"p","evidence_message_ids":[30],"source":"user"}]}}`)
	if _, err := generator.GenerateSegment(context.Background(), assistantTestInput()); err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if !strings.Contains(stub.prompts[0].Content, "assistant_accepted") {
		t.Fatalf("the prompt does not describe the sources: %q", stub.prompts[0].Content)
	}
}
