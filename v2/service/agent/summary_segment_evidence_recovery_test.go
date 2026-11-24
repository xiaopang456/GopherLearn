package agent

// An overlong summary that is still a summary, on a synthetic session.
//
// The session is made up: a learner sets a weekly morning-reading plan at 3
// times a week and later changes it to 8. The batches are ten turns each, the
// values have their own message ids, and the assistant offers products the
// learner never took on. Nothing here is a real conversation; the shape is
// the one the B2 eval had, and the values and ids are the test's own.
//
// The tests prove four things about the write path:
//
//   - a summary that is over the segment budget is stored, and the window
//     trims it when it renders it;
//   - the most recent usable answer survives a rewrite that comes back as
//     something the program cannot read;
//   - a batch that never produced a usable answer still fails and writes
//     nothing;
//   - the initial and the current value are in the block the final request
//     reads, each with the message it came from.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"GopherAI/model"

	modelcomponent "github.com/cloudwego/eino/components/model"
)

// syntheticTurn is one turn of the made-up session.
type syntheticTurn struct {
	user      string
	assistant string
}

// syntheticTurnsInitial is the batch that holds the initial value.
var syntheticTurnsInitial = []syntheticTurn{
	{"晨读的时间怎么排。", "先按一周固定几天排，别每天临时决定。"},
	{"那先从几天开始。", "从少一点开始，能稳定再加。"},
	{"我把每周固定的晨读安排定名叫“晨读”，每周做 3 次。", "好，晨读先按每周 3 次记着，之后按执行情况再调。"},
	{"次数记在哪里。", "记进日程，再在记录里留一条说明。"},
	{"改计划时旧记录要动吗。", "别改旧的，追加一条新的，并把旧的标成已替换。"},
	{"记录多了会不会乱。", "只要每条都有生效时间，回看就有依据。"},
	{"我怕记不清哪版在用。", "每条写上开始生效的日期，回看时取最后一条。"},
	{"日期要写绝对日期吗。", "写清楚更好，别用相对说法。"},
	{"日期要和实际对得上。", "对，日期错位比没写还糟。"},
	{"我最近早上要送孩子。", "送孩子占的是整块时间，正好影响晨读。"},
}

// syntheticTurnsCurrent is the batch that holds the current value.
var syntheticTurnsCurrent = []syntheticTurn{
	{"送孩子期间我只能拿碎片时间。", "那就把晨读切成能塞进碎片时间的小块。"},
	{"碎片时间能读吗。", "能，但一次只处理一个章节。"},
	{"那我得重新定频率。", "对，频率跟着单次的大小变。"},
	{"我不想再纠结几天。", "那就现在定死，新规则一次定完。"},
	{"送孩子把整块时间占掉了，晨读现在改成每周 8 次。", "好，晨读每周按 8 次执行，送孩子这段时间先按这个来。"},
	{"次数上去会不会都做不满。", "做不满就减内容，不要减频率。"},
	{"我怕变成打卡。", "那每次只读一个章节就算合格。"},
	{"这样其实更像日常。", "是，日常的小晨读比攒一大块时间更容易坚持。"},
	{"接着说选书那块。", "好，回到书单的输出。"},
	{"选完书想输出什么。", "先出一份清单，按主题排。"},
}

// syntheticBatchMessagesAt turns the made-up turns into messages. The first
// user message of the batch is base+1, and a turn is two messages.
func syntheticBatchMessagesAt(base uint, startTurn int, turns []syntheticTurn) []model.Message {
	messages := make([]model.Message, 0, len(turns)*2)
	for index, turn := range turns {
		turnNumber := startTurn + index
		at := time.Date(2026, 5, 6, 9, turnNumber, 0, 0, time.UTC)
		messages = append(messages,
			model.Message{ID: base + uint(2*index+1), IsUser: true, Content: turn.user, CreatedAt: at},
			model.Message{ID: base + uint(2*index+2), IsUser: false, Content: turn.assistant, CreatedAt: at.Add(time.Minute)},
		)
	}
	return messages
}

// syntheticBatchInput is the input one batch had.
func syntheticBatchInput(base uint, startTurn int, turns []syntheticTurn, board string) SegmentGenerationInput {
	return SegmentGenerationInput{
		Whiteboard:     board,
		Messages:       syntheticBatchMessagesAt(base, startTurn, turns),
		FirstMessageID: base + 1,
		LastMessageID:  base + uint(2*len(turns)),
	}
}

// syntheticEvidenceAnswer builds the answer the stand-in gives: the value
// point first, the batch's other learner points after it, and the
// assistant's products after them. It is over the 450-token segment budget,
// and the rewrite calls get the same answer.
func syntheticEvidenceAnswer(t *testing.T, base uint, startTurn int, turns []syntheticTurn,
	valueText, value, key string, valueID uint) string {
	t.Helper()
	userID := func(turn int) uint { return base + uint(2*(turn-startTurn)+1) }
	points := make([]map[string]any, 0, len(turns)+9)
	points = append(points, map[string]any{
		"text": valueText, "evidence_message_ids": []uint{valueID}, "source": "user",
	})
	for index, turn := range turns {
		id := userID(startTurn + index)
		if id == valueID {
			continue
		}
		points = append(points, map[string]any{
			"text": turn.user, "evidence_message_ids": []uint{id}, "source": "user",
		})
	}
	for index := 0; index < 8; index++ {
		turn := turns[index%len(turns)]
		id := userID(startTurn+index%len(turns)) + 1
		text := []rune(turn.assistant)
		if len(text) > 40 {
			text = text[:40]
		}
		points = append(points, map[string]any{
			"text": string(text), "evidence_message_ids": []uint{id}, "source": "assistant",
		})
	}
	document := map[string]any{
		"schema_version": 1,
		"segment": map[string]any{
			"topic":  "晨读频率",
			"points": points,
		},
		"whiteboard": map[string]any{
			"schema_version": 1,
			"constraints": []map[string]any{{
				"key": key, "text": valueText, "value": value,
				"source_message_ids": []uint{valueID}, "origin": "user",
			}},
		},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// syntheticModel answers every call the same way.
type syntheticModel struct {
	answer string
	calls  int
}

func (m *syntheticModel) Generate(_ context.Context, _ []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.calls++
	return schema.AssistantMessage(m.answer, nil), nil
}

func (m *syntheticModel) Stream(context.Context, []*schema.Message,
	...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, fmt.Errorf("the generator does not stream")
}

func (m *syntheticModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

// syntheticSegment is the stored segment the renderer reads.
func syntheticSegment(result SegmentGenerationResult, seq int, input SegmentGenerationInput) model.SessionSummarySegment {
	return model.SessionSummarySegment{
		Seq: seq, StartMessageID: input.FirstMessageID, EndMessageID: input.LastMessageID,
		EndAt: time.Date(2026, 5, 6, 14, 0, 0, 0, time.UTC), Content: result.SegmentContent,
	}
}

// TestAnOverlongSummaryIsStoredAndItsValuesAreRead runs the two batches of
// the made-up session. Both are over the segment budget; both are stored,
// and the initial and current values are in the block the final request
// reads.
func TestAnOverlongSummaryIsStoredAndItsValuesAreRead(t *testing.T) {
	initialAnswer := syntheticEvidenceAnswer(t, 200, 31, syntheticTurnsInitial,
		"我把每周固定的晨读安排定名叫“晨读”，每周做 3 次。", "每周 3 次", "morning_reading_freq", 205)
	currentAnswer := syntheticEvidenceAnswer(t, 300, 51, syntheticTurnsCurrent,
		"送孩子把整块时间占掉了，晨读现在改成每周 8 次。", "每周 8 次", "morning_reading_freq", 309)

	initialModel := &syntheticModel{answer: initialAnswer}
	initialGenerator, err := NewModelSegmentGenerator(initialModel, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	initialGenerator.UseAssistantContent(true)
	initialInput := syntheticBatchInput(200, 31, syntheticTurnsInitial, "")
	initial, initialErr := initialGenerator.GenerateSegment(context.Background(), initialInput)

	currentModel := &syntheticModel{answer: currentAnswer}
	currentGenerator, err := NewModelSegmentGenerator(currentModel, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	currentGenerator.UseAssistantContent(true)
	currentInput := syntheticBatchInput(300, 51, syntheticTurnsCurrent, initial.WhiteboardContent)
	current, currentErr := currentGenerator.GenerateSegment(context.Background(), currentInput)

	if initialErr != nil || currentErr != nil {
		t.Fatalf("the batches that hold the two values were lost: initial=%v current=%v",
			initialErr, currentErr)
	}
	if initialModel.calls != segmentCompressLimit+1 {
		t.Fatalf("the model was asked %d times for the initial value, want %d",
			initialModel.calls, segmentCompressLimit+1)
	}
	if currentModel.calls != segmentCompressLimit+1 {
		t.Fatalf("the model was asked %d times for the current value, want %d",
			currentModel.calls, segmentCompressLimit+1)
	}
	if cost := SegmentContentTokens(initial.Segment, initialInput.FirstMessageID, initialInput.LastMessageID); cost <= DefaultAssistantSegmentTokens {
		t.Fatalf("the reproduced segment costs %d tokens, not over the %d budget: the input did not reproduce the failure",
			cost, DefaultAssistantSegmentTokens)
	}

	initialSegment := syntheticSegment(initial, 4, initialInput)
	currentSegment := syntheticSegment(current, 6, currentInput)
	initialBlock := renderSummarySegmentBlock(initialSegment, model.SessionWhiteboardDocument{},
		initialInput.LastMessageID, DefaultAssistantSegmentTokens)
	if !strings.Contains(initialBlock.Text, "3 次") {
		t.Fatalf("the initial value is not in the segment block: %q", initialBlock.Text)
	}
	if !strings.Contains(initialBlock.Text, "[message_ids=205]") {
		t.Fatalf("the initial value names no evidence: %q", initialBlock.Text)
	}
	currentBlock := renderSummarySegmentBlock(currentSegment, model.SessionWhiteboardDocument{},
		currentInput.LastMessageID, DefaultAssistantSegmentTokens)
	if !strings.Contains(currentBlock.Text, "8 次") {
		t.Fatalf("the current value is not in the segment block: %q", currentBlock.Text)
	}
	if !strings.Contains(currentBlock.Text, "[message_ids=309]") {
		t.Fatalf("the current value names no evidence: %q", currentBlock.Text)
	}

	combined := renderSummarySegments([]model.SessionSummarySegment{initialSegment, currentSegment},
		current.Whiteboard, 2000, DefaultAssistantSegmentTokens, true)
	if !strings.Contains(combined.Text, "3 次") || !strings.Contains(combined.Text, "8 次") {
		t.Fatalf("the final request does not carry both values: %q", combined.Text)
	}
	board := renderWhiteboardBlock(current.Whiteboard, 1000)
	if !strings.Contains(board.Text, "8 次") {
		t.Fatalf("the current value is not on the board: %q", board.Text)
	}
	if !strings.Contains(board.Text, "309") {
		t.Fatalf("the board entry names no evidence: %q", board.Text)
	}
}

// TestASummaryThatFitsIsAskedOnce is the control: a batch whose summary fits
// is asked once, and its value is read.
func TestASummaryThatFitsIsAskedOnce(t *testing.T) {
	answer := `{"schema_version":1,"segment":{"topic":"晨读频率","points":[` +
		`{"text":"送孩子把整块时间占掉了，晨读现在改成每周 8 次。","evidence_message_ids":[309],"source":"user"}]},` +
		`"whiteboard":{"schema_version":1,"constraints":[` +
		`{"key":"morning_reading_freq","text":"晨读现在每周 8 次","value":"每周 8 次","source_message_ids":[309],"origin":"user"}]}}`
	stub := &syntheticModel{answer: answer}
	generator, err := NewModelSegmentGenerator(stub, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	input := syntheticBatchInput(300, 51, syntheticTurnsCurrent, "")
	result, err := generator.GenerateSegment(context.Background(), input)
	if err != nil {
		t.Fatalf("a summary that fits was refused: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("the model was asked %d times, want 1", stub.calls)
	}
	if result.Calls != 1 {
		t.Fatalf("the batch took %d calls, want 1", result.Calls)
	}
	block := renderSummarySegmentBlock(syntheticSegment(result, 1, input), result.Whiteboard,
		input.LastMessageID, DefaultAssistantSegmentTokens)
	if !strings.Contains(block.Text, "8 次") {
		t.Fatalf("the control's value is not in the block: %q", block.Text)
	}
}

// TestAValidSummarySurvivesARewriteThatBreaksTheJSON: the first answer is
// usable but over the budget, and the rewrite comes back as something the
// program cannot read. The first answer is stored.
func TestAValidSummarySurvivesARewriteThatBreaksTheJSON(t *testing.T) {
	answer := syntheticEvidenceAnswer(t, 200, 31, syntheticTurnsInitial,
		"我把每周固定的晨读安排定名叫“晨读”，每周做 3 次。", "每周 3 次", "morning_reading_freq", 205)
	stub := &recordingSegmentModel{answers: []string{answer, "this is not the JSON the generator asked for"}}
	generator, err := NewModelSegmentGenerator(stub, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	input := syntheticBatchInput(200, 31, syntheticTurnsInitial, "")
	result, err := generator.GenerateSegment(context.Background(), input)
	if err != nil {
		t.Fatalf("the usable first answer was lost when the rewrite broke: %v", err)
	}
	if stub.calls != 2 {
		t.Fatalf("the model was asked %d times, want 2 (one answer and one broken rewrite)", stub.calls)
	}
	if result.Calls != 2 {
		t.Fatalf("the batch took %d calls, want 2", result.Calls)
	}
	if result.Compressed != 1 {
		t.Fatalf("compressed = %d, want 1: the broken rewrite was still a call", result.Compressed)
	}
	if result.SegmentContent == "" || len(result.Segment.Points) == 0 {
		t.Fatal("the first usable answer was not stored")
	}
	block := renderSummarySegmentBlock(syntheticSegment(result, 1, input), result.Whiteboard,
		input.LastMessageID, DefaultAssistantSegmentTokens)
	if !strings.Contains(block.Text, "3 次") || !strings.Contains(block.Text, "[message_ids=205]") {
		t.Fatalf("the initial value is not in the block the reader gets: %q", block.Text)
	}
	board := renderWhiteboardBlock(result.Whiteboard, 1000)
	if !strings.Contains(board.Text, "3 次") || !strings.Contains(board.Text, "205") {
		t.Fatalf("the initial value is not on the board: %q", board.Text)
	}
	if len(result.CallRecords) != 2 {
		t.Fatalf("the call records = %d, want 2: %+v", len(result.CallRecords), result.CallRecords)
	}
	if result.CallRecords[0].Outcome != segmentCallRejected || result.CallRecords[0].Reason != segmentReasonOverBudget {
		t.Fatalf("the first call = %+v, want refused for over_budget", result.CallRecords[0])
	}
	if result.CallRecords[1].Outcome != segmentCallFailed || result.CallRecords[1].Reason != segmentReasonInvalidOutput {
		t.Fatalf("the broken rewrite = %+v, want failed for invalid_output", result.CallRecords[1])
	}
}

// TestASummaryWithNoUsableAnswerStillFails: when no answer in the batch can
// be stored, the batch still fails and writes nothing. The two forms are an
// answer that cannot be read and an answer whose every point cites a message
// the batch never contained.
func TestASummaryWithNoUsableAnswerStillFails(t *testing.T) {
	cases := []struct {
		name   string
		answer string
	}{
		{"unreadable", "not json"},
		{"no usable point", `{"schema_version":1,"segment":{"topic":"t","points":[` +
			`{"text":"p","evidence_message_ids":[999],"source":"user"}]}}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			stub := &recordingSegmentModel{answers: []string{test.answer}}
			generator, err := NewModelSegmentGenerator(stub, 8000, 2000)
			if err != nil {
				t.Fatal(err)
			}
			generator.UseAssistantContent(true)
			result, err := generator.GenerateSegment(context.Background(),
				syntheticBatchInput(200, 31, syntheticTurnsInitial, ""))
			if err == nil {
				t.Fatalf("a batch with no usable summary was stored: %+v", result.Segment.Points)
			}
			if result.SegmentContent != "" || len(result.Segment.Points) != 0 {
				t.Fatalf("a segment was stored although the batch failed: %+v", result.Segment.Points)
			}
			if stub.calls != 1 {
				t.Fatalf("the model was asked %d times, want 1: there was nothing to rewrite", stub.calls)
			}
		})
	}
}
