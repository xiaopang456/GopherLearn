package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"GopherAI/common/observability"
	"GopherAI/model"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// The generator turns one reading of a batch of turns into two documents. The
// tests here are about what it refuses to keep: a point that cites a message
// the batch never contained, a board entry the learner never said, and an
// output the generator cannot parse.

type stubSegmentModel struct {
	content  string
	calls    int
	prompts  []*schema.Message
	failWith error
}

func (m *stubSegmentModel) Generate(_ context.Context, input []*schema.Message, _ ...modelcomponent.Option) (*schema.Message, error) {
	m.calls++
	m.prompts = input
	if m.failWith != nil {
		return nil, m.failWith
	}
	return schema.AssistantMessage(m.content, nil), nil
}

func (m *stubSegmentModel) Stream(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	reader, writer := schema.Pipe[*schema.Message](1)
	writer.Send(schema.AssistantMessage(m.content, nil), nil)
	writer.Close()
	return reader, nil
}

func (m *stubSegmentModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

func segmentTestMessages() []model.Message {
	return []model.Message{
		{ID: 21, IsUser: true, Content: "改成 48 分钟吧"},
		{ID: 22, IsUser: false, Content: "好的，已经记下。"},
	}
}

func segmentTestInput() SegmentGenerationInput {
	return SegmentGenerationInput{
		Messages:       segmentTestMessages(),
		FirstMessageID: 21,
		LastMessageID:  22,
	}
}

func mustSegmentGenerator(t *testing.T, content string) (*ModelSegmentGenerator, *stubSegmentModel) {
	t.Helper()
	stub := &stubSegmentModel{content: content}
	generator, err := NewModelSegmentGenerator(stub, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	return generator, stub
}

// swapMemoryRecorder points the counters at a recorder the test owns.
func swapMemoryRecorder(t *testing.T) *observability.MemoryRecorder {
	t.Helper()
	previous := observability.Memory
	recorder := observability.NewMemoryRecorder(nil)
	observability.Memory = recorder
	t.Cleanup(func() { observability.Memory = previous })
	return recorder
}

// One call produces both documents. A board written by one call and a segment
// written by another can disagree about the same turns, and that disagreement
// has to be settled later by comparing two readings of the same thing.
func TestSegmentGeneratorProducesASegmentAndAWhiteboardInOneCall(t *testing.T) {
	generator, stub := mustSegmentGenerator(t, `{
      "schema_version": 1,
      "segment": {
        "topic": "学习时长调整",
        "points": [{"text": "用户把每天学习时长从 30 分钟改成 48 分钟", "evidence_message_ids": [21]}],
        "names": ["RPC"],
        "numbers": ["48 分钟"]
      },
      "whiteboard": {
        "schema_version": 1,
        "goal": {"text": "学完 RPC 粘包", "source_message_ids": [21]},
        "constraints": [{"key": "duration", "text": "每天学习 48 分钟", "value": "48 分钟", "source_message_ids": [21]}],
        "plan": [],
        "open_items": []
      }
    }`)
	result, err := generator.GenerateSegment(context.Background(), segmentTestInput())
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 {
		t.Fatalf("model calls = %d, want 1", stub.calls)
	}
	if result.Segment.Topic != "学习时长调整" || len(result.Segment.Points) != 1 {
		t.Fatalf("segment = %+v", result.Segment)
	}
	if len(result.Segment.Points[0].EvidenceMessageIDs) != 1 || result.Segment.Points[0].EvidenceMessageIDs[0] != 21 {
		t.Fatalf("the point does not cite the batch: %+v", result.Segment.Points[0])
	}
	if len(result.Whiteboard.Constraints) != 1 || result.Whiteboard.Constraints[0].Key != "duration" {
		t.Fatalf("whiteboard = %+v", result.Whiteboard)
	}
	if result.Whiteboard.Goal == nil || result.Whiteboard.Goal.Text != "学完 RPC 粘包" {
		t.Fatalf("the goal was lost: %+v", result.Whiteboard.Goal)
	}
	if result.SegmentContent == "" || result.WhiteboardContent == "" {
		t.Fatalf("the stored forms are missing: %+v", result)
	}
}

// The board is not written by the generator. It goes through the engine, so
// the rules about sources, limits and deletions hold whatever the model said.
func TestSegmentGeneratorRunsTheWhiteboardThroughTheEngine(t *testing.T) {
	generator, _ := mustSegmentGenerator(t, `{
      "schema_version": 1,
      "segment": {"topic": "t", "points": [{"text": "p", "evidence_message_ids": [21]}], "names": [], "numbers": []},
      "whiteboard": {
        "schema_version": 1,
        "constraints": [{"key": "duration", "text": "每天学习 48 分钟", "value": "48 分钟", "source_message_ids": [21]}],
        "plan": [],
        "open_items": []
      }
    }`)
	input := segmentTestInput()
	input.Whiteboard = `{"schema_version":1,"constraints":[{"key":"duration","text":"每天学习 30 分钟","value":"30 分钟","source_message_ids":[11]}]}`
	result, err := generator.GenerateSegment(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Whiteboard.Constraints) != 1 || result.Whiteboard.Constraints[0].Value != "48 分钟" {
		t.Fatalf("the value was not replaced: %+v", result.Whiteboard.Constraints)
	}
	if len(result.Changes) == 0 {
		t.Fatalf("the replacement was not recorded: %+v", result.Changes)
	}
	change := result.Changes[0]
	if change.Key != "duration" || change.OldValue != "30 分钟" || change.NewValue != "48 分钟" {
		t.Fatalf("the change does not describe the replacement: %+v", change)
	}
}

// A board entry the learner never said is dropped, and the rest of the
// proposal still lands.
func TestSegmentGeneratorDropsABoardEntryThatCitesAMessageItNeverSaw(t *testing.T) {
	generator, _ := mustSegmentGenerator(t, `{
      "schema_version": 1,
      "segment": {"topic": "t", "points": [{"text": "p", "evidence_message_ids": [21]}], "names": [], "numbers": []},
      "whiteboard": {
        "schema_version": 1,
        "constraints": [{"key": "invented", "text": "学员喜欢手写笔记", "value": "", "source_message_ids": [99]}],
        "plan": [],
        "open_items": []
      }
    }`)
	result, err := generator.GenerateSegment(context.Background(), segmentTestInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Whiteboard.Constraints) != 0 {
		t.Fatalf("the unsourced entry survived: %+v", result.Whiteboard.Constraints)
	}
	if result.Diagnostics.FabricatedEntries != 1 {
		t.Fatalf("the unsourced entry was not counted: %+v", result.Diagnostics)
	}
}

// The assistant's own words are not the learner's state, so they cannot be
// the source of a board entry.
func TestSegmentGeneratorDoesNotLetTheAssistantSourceABoardEntry(t *testing.T) {
	generator, _ := mustSegmentGenerator(t, `{
      "schema_version": 1,
      "segment": {"topic": "t", "points": [{"text": "p", "evidence_message_ids": [21]}], "names": [], "numbers": []},
      "whiteboard": {
        "schema_version": 1,
        "constraints": [{"key": "from_assistant", "text": "助手说的话", "value": "", "source_message_ids": [22]}],
        "plan": [],
        "open_items": []
      }
    }`)
	result, err := generator.GenerateSegment(context.Background(), segmentTestInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Whiteboard.Constraints) != 0 {
		t.Fatalf("an entry sourced from the assistant survived: %+v", result.Whiteboard.Constraints)
	}
}

// Too many points is a trimming problem, not a rejection.
func TestSegmentGeneratorTrimsAnOversizedSegmentInsteadOfRejectingIt(t *testing.T) {
	points := make([]map[string]any, 0, 50)
	for index := 0; index < 50; index++ {
		points = append(points, map[string]any{
			"text":                 "要点 " + string(rune('a'+index%26)) + string(rune('a'+index/26)),
			"evidence_message_ids": []int{21},
		})
	}
	payload, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"segment":        map[string]any{"topic": "t", "points": points, "names": []string{}, "numbers": []string{}},
		"whiteboard":     map[string]any{"schema_version": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	generator, _ := mustSegmentGenerator(t, string(payload))
	result, err := generator.GenerateSegment(context.Background(), segmentTestInput())
	if err != nil {
		t.Fatalf("an oversized segment must not fail: %v", err)
	}
	if len(result.Segment.Points) != model.SessionSummarySegmentPointLimit {
		t.Fatalf("points = %d, want %d", len(result.Segment.Points), model.SessionSummarySegmentPointLimit)
	}
}

// A point that cites a message this batch never contained is dropped on its
// own; the rest of the segment is still good.
func TestSegmentGeneratorDropsAPointThatCitesAMessageItNeverSaw(t *testing.T) {
	generator, _ := mustSegmentGenerator(t, `{
      "schema_version": 1,
      "segment": {
        "topic": "t",
        "points": [
          {"text": "凭空捏造", "evidence_message_ids": [99]},
          {"text": "真实发生的事", "evidence_message_ids": [21]}
        ],
        "names": [], "numbers": []
      },
      "whiteboard": {"schema_version": 1}
    }`)
	result, err := generator.GenerateSegment(context.Background(), segmentTestInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Segment.Points) != 1 || result.Segment.Points[0].Text != "真实发生的事" {
		t.Fatalf("points = %+v", result.Segment.Points)
	}
}

// Output the generator cannot read is a failure the caller has to see: it
// keeps the old board and the old watermark and writes nothing.
func TestSegmentGeneratorRejectsOutputItCannotParse(t *testing.T) {
	generator, _ := mustSegmentGenerator(t, "this is not json")
	if _, err := generator.GenerateSegment(context.Background(), segmentTestInput()); err == nil {
		t.Fatal("unparseable output must fail")
	}
}

// An output whose schema version is not known is a failure too.
func TestSegmentGeneratorRejectsAnUnknownSchemaVersion(t *testing.T) {
	generator, _ := mustSegmentGenerator(t, `{"schema_version": 2, "segment": {}, "whiteboard": {}}`)
	if _, err := generator.GenerateSegment(context.Background(), segmentTestInput()); err == nil {
		t.Fatal("an unknown schema version must fail")
	}
}

// The payload has to say which part is which, because the model has to know
// which of them is authoritative for what.
func TestSegmentGeneratorRendersTheBoardThePreviousSegmentAndTheMessages(t *testing.T) {
	generator, stub := mustSegmentGenerator(t, `{
      "schema_version": 1,
      "segment": {"topic": "t", "points": [{"text": "p", "evidence_message_ids": [21]}], "names": [], "numbers": []},
      "whiteboard": {"schema_version": 1}
    }`)
	input := segmentTestInput()
	input.Whiteboard = `{"schema_version":1,"goal":{"text":"目标","source_message_ids":[11]}}`
	input.PreviousSegment = `{"schema_version":1,"topic":"上一段"}`
	if _, err := generator.GenerateSegment(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if len(stub.prompts) != 2 {
		t.Fatalf("prompts = %d, want a system prompt and a payload", len(stub.prompts))
	}
	payload := stub.prompts[1].Content
	for _, want := range []string{"[WHITEBOARD]", "[PREVIOUS SEGMENT]", "[MESSAGES]", "目标", "上一段", "message_id=21"} {
		if !strings.Contains(payload, want) {
			t.Fatalf("the payload is missing %q: %s", want, payload)
		}
	}
	// The assistant's body is not the learner's state, so it is not sent.
	if strings.Contains(payload, "已经记下") {
		t.Fatalf("the assistant body reached the payload: %s", payload)
	}
}

// An output the generator cannot read has to be counted with its reason.
// A failure nobody counted reads as a turn that was never due, and the next
// run has nothing to compare against.
func TestSegmentGeneratorCountsTheReasonAFailedOutputGave(t *testing.T) {
	recorder := swapMemoryRecorder(t)
	generator, _ := mustSegmentGenerator(t, "this is not json")
	if _, err := generator.GenerateSegment(context.Background(), segmentTestInput()); err == nil {
		t.Fatal("unparseable output must fail")
	}
	snapshot := recorder.Snapshot()
	if snapshot.Counters["summary_segment_total{outcome=failed,reason=invalid_output}"] != 1 {
		t.Fatalf("the failure reason was not counted: %#v", snapshot.Counters)
	}
	if snapshot.Counters["summary_segment_failed_total{reason=invalid_output}"] != 1 {
		t.Fatalf("the failure was not counted: %#v", snapshot.Counters)
	}
}

// A schema version nobody knows is its own reason, not a generic one.
func TestSegmentGeneratorCountsAnUnknownSchemaVersionAsItsOwnReason(t *testing.T) {
	recorder := swapMemoryRecorder(t)
	generator, _ := mustSegmentGenerator(t, `{"schema_version": 2, "segment": {}, "whiteboard": {}}`)
	if _, err := generator.GenerateSegment(context.Background(), segmentTestInput()); err == nil {
		t.Fatal("an unknown schema version must fail")
	}
	snapshot := recorder.Snapshot()
	if snapshot.Counters["summary_segment_total{outcome=failed,reason=unknown_schema_version}"] != 1 {
		t.Fatalf("the schema version was not named in the counters: %#v", snapshot.Counters)
	}
}

// Points the generator had to drop are counted, because a segment that keeps
// losing points is a generator that is drifting.
func TestSegmentGeneratorCountsThePointsItDropped(t *testing.T) {
	recorder := swapMemoryRecorder(t)
	generator, _ := mustSegmentGenerator(t, `{
      "schema_version": 1,
      "segment": {
        "topic": "t",
        "points": [
          {"text": "凭空捏造", "evidence_message_ids": [99]},
          {"text": "真实发生的事", "evidence_message_ids": [21]}
        ],
        "names": [], "numbers": []
      },
      "whiteboard": {"schema_version": 1}
    }`)
	result, err := generator.GenerateSegment(context.Background(), segmentTestInput())
	if err != nil {
		t.Fatal(err)
	}
	if result.Diagnostics.DroppedPoints != 1 {
		t.Fatalf("dropped points = %d, want 1", result.Diagnostics.DroppedPoints)
	}
	snapshot := recorder.Snapshot()
	if snapshot.Totals["summary_segment_points_dropped_total"] != 1 {
		t.Fatalf("the dropped points were not counted: %#v", snapshot.Totals)
	}
	if snapshot.Totals["summary_segment_points_total"] != 1 {
		t.Fatalf("the kept points were not counted: %#v", snapshot.Totals)
	}
}

// A batch that lost every point it produced fails, and the points it lost are
// counted with the failure: a batch that lost everything is a different
// failure from one that never had a point.
func TestSegmentGeneratorCountsThePointsLostByABatchThatFailed(t *testing.T) {
	recorder := swapMemoryRecorder(t)
	generator, _ := mustSegmentGenerator(t, `{
      "schema_version": 1,
      "segment": {
        "topic": "t",
        "points": [
          {"text": "凭空捏造一", "evidence_message_ids": [98]},
          {"text": "凭空捏造二", "evidence_message_ids": [99]}
        ],
        "names": [], "numbers": []
      },
      "whiteboard": {"schema_version": 1}
    }`)
	if _, err := generator.GenerateSegment(context.Background(), segmentTestInput()); err == nil {
		t.Fatal("a segment with no usable point must fail")
	}
	snapshot := recorder.Snapshot()
	if snapshot.Counters["summary_segment_total{outcome=failed,reason=point_without_source}"] != 1 {
		t.Fatalf("the reason was not counted: %#v", snapshot.Counters)
	}
	if snapshot.Totals["summary_segment_points_dropped_total"] != 2 {
		t.Fatalf("the points lost by the failed batch were not counted: %#v", snapshot.Totals)
	}
}

// The board counters are reported even when the generation fails after the
// engine ran, because that work still happened. A board that had to be
// trimmed and then failed for another reason still cost the engine work, and
// that work is what the counters exist to show.
func TestSegmentGeneratorCountsTheBoardEvenWhenTheGenerationFailsAfterwards(t *testing.T) {
	recorder := swapMemoryRecorder(t)
	// Ten long entries make the board bigger than the output budget, so the
	// generation fails on the board after the engine has already merged it.
	entries := make([]map[string]any, 0, 10)
	for index := 0; index < 10; index++ {
		entries = append(entries, map[string]any{
			"key":                fmt.Sprintf("constraint-%d", index),
			"text":               strings.Repeat("字", model.SessionWhiteboardTextMaxRunes),
			"value":              "值",
			"source_message_ids": []int{21},
		})
	}
	payload, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"segment": map[string]any{
			"topic":   "t",
			"points":  []map[string]any{{"text": "p", "evidence_message_ids": []int{21}}},
			"names":   []string{},
			"numbers": []string{},
		},
		"whiteboard": map[string]any{
			"schema_version": 1,
			"constraints":   entries,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubSegmentModel{content: string(payload)}
	generator, err := NewModelSegmentGenerator(stub, 8000, 300)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.GenerateSegment(context.Background(), segmentTestInput()); err == nil {
		t.Fatal("a board over the output budget must fail")
	}
	snapshot := recorder.Snapshot()
	if snapshot.Counters["summary_segment_total{outcome=failed,reason=output_too_large}"] != 1 {
		t.Fatalf("the reason was not counted: %#v", snapshot.Counters)
	}
	if snapshot.Totals["whiteboard_applied_total"] == 0 {
		t.Fatalf("the board counters were not reported for the failed generation: %#v", snapshot.Totals)
	}
}
