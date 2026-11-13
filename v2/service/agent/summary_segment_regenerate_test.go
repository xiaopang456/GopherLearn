package agent

// A summary that does not fit is written again.
//
// The segment is read under a budget, and a summary that costs more than
// that budget is not read at all. Cutting the words is not an option: a cut
// point says something else, and what it says is often the opposite of what
// was said.
//
// So the question goes back to the model that wrote it. The same turns are
// sent again with a request to write it shorter, and a summary that still
// does not fit is not stored at all: a batch that wrote nothing is a hole in
// the session's memory, and a hole that is counted is a hole somebody can
// see.
//
// The tests here are about that: the second answer is the one that is kept,
// the model is asked at most twice more, a summary that never fits is a
// failure and not a stored summary, and the mode that does not record the
// assistant's products keeps the path it always had.

import (
	"context"
	"strings"
	"testing"
)

// longSegmentPayload is a summary that does not fit the window.
//
// The points are the learner's own and each of them carries a fact, so the
// summary is long without saying anything twice.
func longSegmentPayload(points int) string {
	parts := make([]string, 0, points)
	for index := 0; index < points; index++ {
		parts = append(parts, `{"text":"`+strings.Repeat("学习者把这一条说清楚了", 4)+`","evidence_message_ids":[30]}`)
	}
	return `{"schema_version":1,"segment":{"topic":"很长的一个话题","points":[` + strings.Join(parts, ",") + `]}}`
}

// shortSegmentPayload is a summary that fits.
const shortSegmentPayload = `{"schema_version":1,"segment":{"topic":"t","points":[` +
	`{"text":"压短之后的要点","evidence_message_ids":[30]}]}}`

// A summary that does not fit the window is asked for again, and the second
// answer is the one that is kept.
func TestASummaryThatDoesNotFitIsAskedForAgain(t *testing.T) {
	long := longSegmentPayload(10)
	generator, stub := assistantSegmentGenerator(t, long)
	stub.byCall = []string{long, shortSegmentPayload}
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if stub.calls != 2 {
		t.Fatalf("the model was asked %d times, want 2: one answer and one compression", stub.calls)
	}
	if len(result.Segment.Points) != 1 {
		t.Fatalf("points = %d, want 1: %+v", len(result.Segment.Points), result.Segment.Points)
	}
	if !strings.Contains(result.Segment.Points[0].Text, "压短之后的要点") {
		t.Fatalf("the summary that was not asked for again was kept: %+v", result.Segment.Points)
	}
	if result.Calls != 2 {
		t.Fatalf("the batch took %d model calls, want 2", result.Calls)
	}
}

// The compression is asked for with the same turns and a request to write it
// shorter.
func TestTheCompressionIsAskedForWithTheSameTurns(t *testing.T) {
	long := longSegmentPayload(10)
	generator, stub := assistantSegmentGenerator(t, long)
	stub.byCall = []string{long, shortSegmentPayload}
	if _, err := generator.GenerateSegment(context.Background(), assistantTestInput()); err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if len(stub.prompts) < 4 {
		t.Fatalf("the generator made %d calls, want at least two", len(stub.prompts)/2)
	}
	second := stub.prompts[2]
	third := stub.prompts[3]
	if second == nil || third == nil {
		t.Fatal("the generator did not ask again")
	}
	if !strings.Contains(third.Content, segmentCompressRequest) {
		t.Fatalf("the second answer was not asked for a shorter summary: %q", third.Content)
	}
	if !strings.Contains(third.Content, "[message_id=30 role=user]") {
		t.Fatalf("the second answer did not see the same turns: %q", third.Content)
	}
}

// The model is asked at most twice more, and a summary that still does not
// fit is a failure: the batch wrote nothing, and the reason is counted.
func TestASummaryThatNeverFitsIsNotStored(t *testing.T) {
	long := longSegmentPayload(10)
	generator, stub := assistantSegmentGenerator(t, long)
	stub.byCall = []string{long, long, long}
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err == nil {
		t.Fatalf("a summary that does not fit was stored: %+v", result.Segment.Points)
	}
	if stub.calls != segmentCompressLimit+1 {
		t.Fatalf("the model was asked %d times, want %d", stub.calls, segmentCompressLimit+1)
	}
	if reason := segmentFailureReason(err); reason != segmentReasonOverBudget {
		t.Fatalf("the failure reason = %q, want %q", reason, segmentReasonOverBudget)
	}
	if len(result.Segment.Points) != 0 || result.SegmentContent != "" {
		t.Fatalf("a summary was stored although the batch failed: %+v", result.Segment.Points)
	}
}

// The mode that does not record the assistant's products keeps the path it
// always had: the tail goes first and the model is asked once.
func TestTheOffModeIsAskedOnceAndTrimsTheTail(t *testing.T) {
	long := longSegmentPayload(10)
	stub := &stubSegmentModel{content: long}
	generator, err := NewModelSegmentGenerator(stub, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("the model was asked %d times, want 1", stub.calls)
	}
	if len(result.Segment.Points) == 0 {
		t.Fatal("the summary lost every point")
	}
	if len(result.Segment.Points) >= 10 {
		t.Fatalf("points = %d, want the tail to have gone", len(result.Segment.Points))
	}
}
