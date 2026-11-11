package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// The mode that records the assistant's products has to survive a real
// conversation.
//
// The smoke that found the defect ran sixty and seventy turn conversations
// with answers of a few hundred tokens each, and every batch of them failed
// before the model was asked anything. The tests here run the same shape:
// a long conversation, the product's own budget for it, and the assertions
// that the summary came out of it.

// longConversationModel answers with a segment that cites the messages of the
// batch it was given.
//
// The answer has to cite the batch's own messages: a point that cites
// something else is dropped by the program, and a test double that answered
// with fixed ids would be testing the drop rather than the budget.
type longConversationModel struct {
	calls    int
	payloads []string
}

func (m *longConversationModel) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.calls++
	payload := ""
	if len(input) > 1 && input[1] != nil {
		payload = input[1].Content
	}
	m.payloads = append(m.payloads, payload)
	user, assistant := firstMessageIDs(payload)
	content := fmt.Sprintf(`{"schema_version":1,"segment":{"topic":"长对话","points":[`+
		`{"text":"学习者提出问题","evidence_message_ids":[%d],"source":"user"},`+
		`{"text":"助手给出了安排","evidence_message_ids":[%d],"source":"assistant"}]},`+
		`"whiteboard":{"schema_version":1,"constraints":[`+
		`{"key":"topic","text":"这段对话在做什么","source_message_ids":[%d],"origin":"assistant"}]}}`,
		user, assistant, user)
	return schema.AssistantMessage(content, nil), nil
}

func (m *longConversationModel) Stream(context.Context, []*schema.Message,
	...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	reader, writer := schema.Pipe[*schema.Message](1)
	writer.Send(schema.AssistantMessage("", nil), nil)
	writer.Close()
	return reader, nil
}

func (m *longConversationModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

// firstMessageIDs reads the first learner message and the first assistant
// message out of a payload.
func firstMessageIDs(payload string) (int, int) {
	user, assistant := 0, 0
	for _, line := range strings.Split(payload, "\n") {
		if !strings.HasPrefix(line, "[message_id=") {
			continue
		}
		rest := line[len("[message_id="):]
		end := strings.Index(rest, " ")
		if end < 0 {
			continue
		}
		id, err := strconv.Atoi(rest[:end])
		if err != nil {
			continue
		}
		if strings.Contains(line, "role=user]") && user == 0 {
			user = id
		}
		if strings.Contains(line, "role=assistant]") && assistant == 0 {
			assistant = id
		}
	}
	return user, assistant
}

// longConversation builds a conversation of the shape the smoke ran: the
// learner asks and the assistant answers with a few hundred tokens of
// products.
func longConversation(rounds int) []model.Message {
	messages := make([]model.Message, 0, rounds*2)
	id := uint(1)
	for round := 0; round < rounds; round++ {
		messages = append(messages, model.Message{
			ID: id, IsUser: true,
			Content: fmt.Sprintf("第 %d 轮：我想把接口层的错误整理一下，现在主要是 %d 处", round+1, round+1),
		})
		id++
		body := make([]string, 0, 12)
		body = append(body, fmt.Sprintf("## 第 %d 轮的建议", round+1))
		for item := 1; item <= 8; item++ {
			body = append(body, fmt.Sprintf("- **改动 %d**：把错误分类改成哨兵值，页数按 %d 计算", item, round*10+item))
		}
		body = append(body, "这段解释是散文，摘要里不需要它。")
		messages = append(messages, model.Message{
			ID: id, IsUser: false, Content: strings.Join(body, "\n"),
		})
		id++
	}
	return messages
}

// A conversation of sixty to seventy rounds is one batch, and it is one model
// call.
//
// The budget the mode runs on is the model's own, not the placeholder
// mode's eight thousand tokens, so the batch fits and nothing is split.
func TestALongConversationIsOneCallAndNotASplit(t *testing.T) {
	messages := longConversation(65)
	stub := &longConversationModel{}
	generator, err := NewModelSegmentGenerator(stub, 100000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	result, err := generator.GenerateSegment(context.Background(), SegmentGenerationInput{
		Messages: messages, FirstMessageID: 1, LastMessageID: messages[len(messages)-1].ID,
	})
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if result.Calls != 1 {
		t.Fatalf("the batch took %d model calls, want 1", result.Calls)
	}
	if len(result.Segment.Points) == 0 {
		t.Fatal("the segment has no points")
	}
	products := 0
	for _, point := range result.Segment.Points {
		if point.PointSource() == model.SessionSummarySegmentSourceAssistant {
			products++
		}
	}
	if products == 0 {
		t.Fatalf("the segment carries none of the assistant's products: %+v", result.Segment.Points)
	}
}

// The assistant's words must never make a batch fail.
//
// With the old eight thousand token budget the same conversation does not
// fit as it was: the assistant's answers are in the input now, and the batch
// is three times as long as the budget. The excerpts are shortened to what
// the batch can afford, and the batch is generated anyway.
func TestTheAssistantsWordsNeverMakeABatchFail(t *testing.T) {
	messages := longConversation(65)
	stub := &longConversationModel{}
	generator, err := NewModelSegmentGenerator(stub, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	result, err := generator.GenerateSegment(context.Background(), SegmentGenerationInput{
		Messages: messages, FirstMessageID: 1, LastMessageID: messages[len(messages)-1].ID,
	})
	if err != nil {
		t.Fatalf("the batch failed because of the assistant's words: %v", err)
	}
	if len(result.Segment.Points) == 0 {
		t.Fatal("the segment has no points")
	}
	for _, payload := range stub.payloads {
		if cost := rag.EstimateTokens(payload); cost > 8000 {
			t.Fatalf("the payload costs %d tokens, over the budget of 8000", cost)
		}
	}
}

// The split is the last resort.
//
// A batch whose input does not fit even with the excerpts shortened is split
// at the boundary between a turn and the answer to it - and the batch is
// generated anyway.
func TestTheBatchIsSplitOnlyWhenItCannotFit(t *testing.T) {
	messages := longConversation(65)
	stub := &longConversationModel{}
	generator, err := NewModelSegmentGenerator(stub, 1200, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	result, err := generator.GenerateSegment(context.Background(), SegmentGenerationInput{
		Messages: messages, FirstMessageID: 1, LastMessageID: messages[len(messages)-1].ID,
	})
	if err != nil {
		t.Fatalf("the batch failed: %v", err)
	}
	if result.Calls < 2 {
		t.Fatalf("the batch took %d model calls; the input did not fit the budget", result.Calls)
	}
	for _, payload := range stub.payloads {
		if cost := rag.EstimateTokens(payload); cost > 1200 {
			t.Fatalf("the payload costs %d tokens, over the budget of 1200", cost)
		}
	}
}

// A pair is never split: the answer to a turn belongs to the turn.
func TestTheSplitKeepsThePairsTogether(t *testing.T) {
	messages := longConversation(65)
	stub := &longConversationModel{}
	generator, err := NewModelSegmentGenerator(stub, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	if _, err := generator.GenerateSegment(context.Background(), SegmentGenerationInput{
		Messages: messages, FirstMessageID: 1, LastMessageID: messages[len(messages)-1].ID,
	}); err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	for _, payload := range stub.payloads {
		ids := messageIDsOf(payload)
		for index := 0; index+1 < len(ids); index += 2 {
			if ids[index+1] != ids[index]+1 {
				t.Fatalf("a pair was split: %v", ids)
			}
		}
	}
}

// messageIDsOf reads the message ids of a payload, in order.
func messageIDsOf(payload string) []uint {
	ids := make([]uint, 0, 64)
	for _, line := range strings.Split(payload, "\n") {
		if !strings.HasPrefix(line, "[message_id=") {
			continue
		}
		rest := line[len("[message_id="):]
		end := strings.Index(rest, " ")
		if end < 0 {
			continue
		}
		id, err := strconv.Atoi(rest[:end])
		if err != nil {
			continue
		}
		ids = append(ids, uint(id))
	}
	return ids
}

// The learner's own words are not thrown away with a wrong label.
//
// The model wrote that the learner had taken a proposal on, and cited the
// learner's own message about it. The citations do not show the acceptance,
// but what they do show is the learner's words - and those are the learner's,
// whatever the label said.
func TestTheLearnersOwnWordsSurviveAWrongLabel(t *testing.T) {
	generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[`+
		`{"text":"学习者说的话","evidence_message_ids":[30],"source":"user"}]},`+
		`"whiteboard":{"schema_version":1,"constraints":[`+
		`{"key":"changes","text":"主要是三处改动","source_message_ids":[30],"origin":"assistant_accepted"}]}}`)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if len(result.Whiteboard.Constraints) != 1 {
		t.Fatalf("the learner's own words were dropped: %+v", result.Whiteboard.Constraints)
	}
	if got := result.Whiteboard.Constraints[0].EntryOrigin(); got != model.SessionWhiteboardOriginUser {
		t.Fatalf("the entry's origin = %q, want the learner's", got)
	}
	if result.Diagnostics.MislabeledOrigins != 1 {
		t.Fatalf("mislabeled origins = %d, want 1", result.Diagnostics.MislabeledOrigins)
	}
	if result.Diagnostics.UnprovenAcceptances != 0 {
		t.Fatalf("unproven acceptances = %d, want 0", result.Diagnostics.UnprovenAcceptances)
	}
}

// A proposal of the assistant's nobody took on is not a current fact, so it
// does not go on the board - but it is not lost either: it becomes one of the
// assistant's products in the segment.
func TestTheAssistantsProposalBecomesASegmentProduct(t *testing.T) {
	generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[`+
		`{"text":"学习者说的话","evidence_message_ids":[30],"source":"user"}]},`+
		`"whiteboard":{"schema_version":1,"constraints":[`+
		`{"key":"plan","text":"按三道练习来","source_message_ids":[31],"origin":"assistant"}]}}`)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if len(result.Whiteboard.Constraints) != 0 {
		t.Fatalf("a proposal nobody took on reached the board: %+v", result.Whiteboard.Constraints)
	}
	found := false
	for _, point := range result.Segment.Points {
		if point.PointSource() == model.SessionSummarySegmentSourceAssistant &&
			strings.Contains(point.Text, "按三道练习来") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the proposal was dropped instead of becoming a product: %+v", result.Segment.Points)
	}
	if result.Diagnostics.AssistantProposals != 1 {
		t.Fatalf("assistant proposals = %d, want 1", result.Diagnostics.AssistantProposals)
	}
}

// One bad entry does not take the other entries of the batch with it.
func TestOneBadEntryDoesNotRejectTheOthers(t *testing.T) {
	generator, _ := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[`+
		`{"text":"学习者说的话","evidence_message_ids":[30],"source":"user"}]},`+
		`"whiteboard":{"schema_version":1,"constraints":[`+
		`{"key":"bad","text":"说了来源不明的话","source_message_ids":[30],"origin":"the_model_made_this_up"},`+
		`{"key":"good","text":"学习者自己的约束","source_message_ids":[30]}]}}`)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if len(result.Whiteboard.Constraints) != 1 {
		t.Fatalf("the good entry was rejected with the bad one: %+v", result.Whiteboard.Constraints)
	}
	if result.Whiteboard.Constraints[0].Key != "good" {
		t.Fatalf("the wrong entry survived: %+v", result.Whiteboard.Constraints)
	}
	if result.Diagnostics.UnknownOrigins != 1 {
		t.Fatalf("unknown origins = %d, want 1", result.Diagnostics.UnknownOrigins)
	}
}

// The wording of the learner is kept.
//
// The program cannot check the wording: it checks the sources, not the
// words. What it can do is ask the model for the learner's own words, and
// the test double below reads that rule out of the prompt and keeps it.
func TestThePromptAsksForTheLearnersOwnWords(t *testing.T) {
	if !strings.Contains(segmentAssistantSystemPrompt, "主要是") ||
		!strings.Contains(segmentAssistantSystemPrompt, "已定") {
		t.Fatal("the prompt does not carry the rule about the learner's hedging")
	}
	if !strings.Contains(segmentAssistantSystemPrompt, "origin") ||
		!strings.Contains(segmentAssistantSystemPrompt, "assistant|assistant_accepted") {
		t.Fatal("the prompt does not name the origins the program reads")
	}
	if strings.Contains(segmentSystemPrompt, "主要是") {
		t.Fatal("the mode that is off sends the other mode's prompt")
	}
}

// The trust order says what a line marked as the assistant's is.
func TestTheTrustOrderSaysTheAssistantsProposalsAreNotDecisions(t *testing.T) {
	withProducts := renderTrustOrder(TrustOrderBlocks{
		RecentMessages: true, SummarySegments: true, AssistantProducts: true,
	})
	if !strings.Contains(withProducts, "not what the learner decided") {
		t.Fatalf("the trust order does not say it: %q", withProducts)
	}
	without := renderTrustOrder(TrustOrderBlocks{RecentMessages: true, SummarySegments: true})
	if strings.Contains(without, "not what the learner decided") {
		t.Fatalf("the rule is written where it does not apply: %q", without)
	}
}

// The model's own answer comes back with the result, so a deployment that
// asks for it can keep it.
func TestTheRawAnswerComesBackWithTheResult(t *testing.T) {
	generator, stub := assistantSegmentGenerator(t, `{"schema_version":1,"segment":{"topic":"t","points":[`+
		`{"text":"p","evidence_message_ids":[30]}]}}`)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if result.RawOutput != stub.content {
		t.Fatalf("raw output = %q, want the model's own answer", result.RawOutput)
	}
}
