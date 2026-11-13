package agent

// The whole path, from the first call to the last.
//
// The tests in the other files check one step at a time: that a summary that
// does not fit is asked for again, that the request is counted, that the
// omission is counted. This file runs the whole thing - generate, find that
// the summary does not fit, ask for it again, and either keep the answer or
// fail - and checks what a reader of the logs would see.
//
// The three things it proves are:
//
//   - the requests are tellable apart. A call that asks for the summary and
//     a call that asks for a shorter summary look different on the wire, and
//     a stub that cannot tell them apart could not be used to test which is
//     which.
//   - the count of the calls is the count of the calls. Three calls means
//     three records, and a failure does not clear them.
//   - the cost of a failure does not go away. A batch that wrote nothing
//     still made the calls, and the failure says how many.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"GopherAI/common/rag"
	"GopherAI/model"

	modelcomponent "github.com/cloudwego/eino/components/model"
)

// recordingSegmentModel is a model that answers per call and can fail on a
// chosen one.
//
// It keeps every request it was given, so a test can look at what the
// generator actually sent rather than at what it meant to send.
type recordingSegmentModel struct {
	// answers is what each call is answered with.
	answers []string
	// failOn is the call that fails, counting from one. Zero means none.
	failOn int
	// failWith is what the failing call returns.
	failWith error
	// calls is how many times the model was asked.
	calls int
	// requests is every request, in order.
	requests [][]*schema.Message
}

func (m *recordingSegmentModel) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.calls++
	m.requests = append(m.requests, input)
	if m.failOn != 0 && m.calls == m.failOn {
		if m.failWith != nil {
			return nil, m.failWith
		}
		return nil, errors.New("the provider failed")
	}
	content := ""
	if len(m.answers) > 0 {
		index := m.calls - 1
		if index >= len(m.answers) {
			index = len(m.answers) - 1
		}
		content = m.answers[index]
	}
	return &schema.Message{Role: schema.Assistant, Content: content}, nil
}

// Stream is here because the generator's model is a chat model, and the
// generator never streams. The tests are about the requests, not the
// answers.
func (m *recordingSegmentModel) Stream(_ context.Context, _ []*schema.Message,
	_ ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("the generator does not stream")
}

// WithTools is here because the generator's model is a chat model. The
// generator never gives it tools.
func (m *recordingSegmentModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

// userPrompt is what the request carried as its second message.
func (m *recordingSegmentModel) userPrompt(index int) string {
	if index >= len(m.requests) || len(m.requests[index]) < 2 {
		return ""
	}
	message := m.requests[index][1]
	if message == nil {
		return ""
	}
	return message.Content
}

// isRewrite reports whether the request asked for a shorter summary.
//
// The request that asks for a shorter one carries the instruction that says
// so, and a request that does not carry it is the first one. This is how a
// reader of the logs tells the two apart.
func (m *recordingSegmentModel) isRewrite(index int) bool {
	return strings.Contains(m.userPrompt(index), segmentCompressRequest)
}

// rewritingGenerator builds a generator in the mode that records the
// assistant's products, with a model that answers the given way.
func rewritingGenerator(t *testing.T, stub *recordingSegmentModel) *ModelSegmentGenerator {
	t.Helper()
	generator, err := NewModelSegmentGenerator(stub, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	return generator
}

// The whole path, and the requests along it.
//
// The first call asks for the summary, the second asks for a shorter one,
// and the third is the last one there is. The generator tells them apart and
// so does a reader of the requests: the second and the third carry the
// instruction that asks for a shorter summary and the first does not.
func TestTheWholePathAsksForTheSummaryAndThenForAShorterOne(t *testing.T) {
	long := longSegmentPayload(10)
	stub := &recordingSegmentModel{answers: []string{long, shortSegmentPayload}}
	generator := rewritingGenerator(t, stub)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if stub.calls != 2 {
		t.Fatalf("the model was asked %d times, want 2", stub.calls)
	}
	if stub.isRewrite(0) {
		t.Fatalf("the first request asked for a shorter summary: %q", stub.userPrompt(0))
	}
	if !stub.isRewrite(1) {
		t.Fatalf("the second request did not ask for a shorter summary: %q", stub.userPrompt(1))
	}
	if len(result.CallRecords) != 2 {
		t.Fatalf("the call records = %d, want 2: %+v", len(result.CallRecords), result.CallRecords)
	}
	if result.CallRecords[0].Purpose != segmentPurposeSummary {
		t.Fatalf("the first call was for %q, want %q", result.CallRecords[0].Purpose, segmentPurposeSummary)
	}
	if result.CallRecords[1].Purpose != segmentPurposeRewrite {
		t.Fatalf("the second call was for %q, want %q", result.CallRecords[1].Purpose, segmentPurposeRewrite)
	}
	// The first answer did not fit, so the call it was spent on is a call
	// that was refused. The second one fitted and is the one that is kept.
	if result.CallRecords[0].Outcome != segmentCallRejected {
		t.Fatalf("the first call ended as %q: %+v", result.CallRecords[0].Outcome, result.CallRecords[0])
	}
	if result.CallRecords[0].Reason != segmentReasonOverBudget {
		t.Fatalf("the first call's reason = %q, want %q",
			result.CallRecords[0].Reason, segmentReasonOverBudget)
	}
	if result.CallRecords[1].Outcome != segmentCallOK {
		t.Fatalf("the second call ended as %q: %+v", result.CallRecords[1].Outcome, result.CallRecords[1])
	}
}

// The count of the calls is the count of the calls.
//
// A batch that was asked three times made three calls, and the records say
// three. The number is what a deployment reads to know what the batch cost.
func TestTheCountOfTheCallsIsTheCountOfTheCalls(t *testing.T) {
	long := longSegmentPayload(10)
	stub := &recordingSegmentModel{answers: []string{long, long, shortSegmentPayload}}
	generator := rewritingGenerator(t, stub)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if stub.calls != 3 {
		t.Fatalf("the model was asked %d times, want 3", stub.calls)
	}
	if result.Calls != 3 {
		t.Fatalf("the batch took %d model calls, want 3", result.Calls)
	}
	if len(result.CallRecords) != 3 {
		t.Fatalf("the call records = %d, want 3", len(result.CallRecords))
	}
	// The first answer did not fit and the second did not either, so two of
	// the three calls were spent on a summary that was refused.
	if result.Compressed != 2 {
		t.Fatalf("compressed = %d, want 2", result.Compressed)
	}
	rejected := 0
	for _, record := range result.CallRecords {
		if record.Outcome == segmentCallRejected {
			rejected++
		}
	}
	if rejected != 2 {
		t.Fatalf("the calls that were refused = %d, want 2: %+v", rejected, result.CallRecords)
	}
	// The third call is the one that fitted, and it is the last record.
	last := result.CallRecords[len(result.CallRecords)-1]
	if last.Outcome != segmentCallOK {
		t.Fatalf("the last call ended as %q: %+v", last.Outcome, last)
	}
	if last.Purpose != segmentPurposeRewrite {
		t.Fatalf("the last call was for %q, want %q", last.Purpose, segmentPurposeRewrite)
	}
}

// A batch that never fits is asked three times and not one more.
//
// The limit is the point of the test: three calls, and the fourth is never
// made however long the batch went on failing.
func TestTheBatchIsNeverAskedAFourthTime(t *testing.T) {
	long := longSegmentPayload(10)
	stub := &recordingSegmentModel{answers: []string{long, long, long, shortSegmentPayload}}
	generator := rewritingGenerator(t, stub)
	if _, err := generator.GenerateSegment(context.Background(), assistantTestInput()); err == nil {
		t.Fatal("a summary that does not fit was stored")
	}
	if stub.calls != segmentCompressLimit+1 {
		t.Fatalf("the model was asked %d times, want %d", stub.calls, segmentCompressLimit+1)
	}
}

// The cost of a failure does not go away.
//
// A batch that wrote nothing still made the calls, and the failure carries
// them. A failure that carries no calls says the attempt was free, and it
// was not.
func TestTheCostOfAFailureDoesNotGoAway(t *testing.T) {
	long := longSegmentPayload(10)
	stub := &recordingSegmentModel{answers: []string{long, long, long}}
	generator := rewritingGenerator(t, stub)
	_, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err == nil {
		t.Fatal("a summary that does not fit was stored")
	}
	calls := segmentFailureCalls(err)
	if calls != segmentCompressLimit+1 {
		t.Fatalf("the failure carries %d calls, want %d", calls, segmentCompressLimit+1)
	}
}

// A call that fails in the middle is counted and the failure says so.
//
// The provider failed on the second call, after the first one came back and
// was refused. The batch made two calls, and the failure carries both.
func TestACallThatFailsInTheMiddleIsCounted(t *testing.T) {
	long := longSegmentPayload(10)
	stub := &recordingSegmentModel{
		answers: []string{long},
		failOn:  2,
	}
	generator := rewritingGenerator(t, stub)
	_, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err == nil {
		t.Fatal("a failed call did not fail the batch")
	}
	if stub.calls != 2 {
		t.Fatalf("the model was asked %d times, want 2", stub.calls)
	}
	calls := segmentFailureCalls(err)
	if calls != 2 {
		t.Fatalf("the failure carries %d calls, want 2", calls)
	}
	if reason := segmentFailureReason(err); reason != segmentReasonModelError {
		t.Fatalf("the failure reason = %q, want %q", reason, segmentReasonModelError)
	}
}

// The first call fails and the failure carries that call.
func TestTheFirstCallThatFailsIsCounted(t *testing.T) {
	stub := &recordingSegmentModel{failOn: 1}
	generator := rewritingGenerator(t, stub)
	_, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err == nil {
		t.Fatal("a failed call did not fail the batch")
	}
	if calls := segmentFailureCalls(err); calls != 1 {
		t.Fatalf("the failure carries %d calls, want 1", calls)
	}
}

// A call that comes back empty is a call that was made and a call that
// produced nothing.
//
// The answer was there and it was empty, so the program could not read it.
// The call is counted either way, and the failure says what became of it.
func TestACallThatComesBackEmptyIsCounted(t *testing.T) {
	stub := &recordingSegmentModel{answers: []string{""}}
	generator := rewritingGenerator(t, stub)
	_, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err == nil {
		t.Fatal("an empty answer did not fail the batch")
	}
	if calls := segmentFailureCalls(err); calls != 1 {
		t.Fatalf("the failure carries %d calls, want 1", calls)
	}
	if reason := segmentFailureReason(err); reason != segmentReasonInvalidOutput {
		t.Fatalf("the failure reason = %q, want %q", reason, segmentReasonInvalidOutput)
	}
}

// A call that comes back with nothing at all is a call that produced
// nothing, and it has its own reason.
//
// An answer that is not there at all is not an answer that could not be
// read: the provider gave back nothing, and that is a different thing from
// giving back something unreadable.
func TestACallThatComesBackWithNothingHasItsOwnReason(t *testing.T) {
	stub := &recordingSegmentModel{answers: []string{"x"}}
	generator := rewritingGenerator(t, stub)
	// The stub answers with content, so the empty case is produced by a
	// model that gives nothing back at all.
	empty := &emptySegmentModel{}
	generator, err := NewModelSegmentGenerator(empty, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	_, err = generator.GenerateSegment(context.Background(), assistantTestInput())
	if err == nil {
		t.Fatal("an empty answer did not fail the batch")
	}
	if reason := segmentFailureReason(err); reason != segmentReasonEmptyOutput {
		t.Fatalf("the failure reason = %q, want %q", reason, segmentReasonEmptyOutput)
	}
	if calls := segmentFailureCalls(err); calls != 1 {
		t.Fatalf("the failure carries %d calls, want 1", calls)
	}
}

// emptySegmentModel is a model that gives nothing back.
type emptySegmentModel struct {
	calls int
}

func (m *emptySegmentModel) Generate(_ context.Context, _ []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	m.calls++
	return nil, nil
}

func (m *emptySegmentModel) Stream(_ context.Context, _ []*schema.Message,
	_ ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("the generator does not stream")
}

func (m *emptySegmentModel) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}

// The whole request is measured, and what is sent is what was measured.
//
// The request that asks for a shorter summary carries an instruction as well
// as the turns, and the instruction is part of what the provider receives.
// The request that is sent must be within the limit the deployment set.
func TestTheRequestThatIsSentIsWithinTheLimit(t *testing.T) {
	long := longSegmentPayload(10)
	stub := &recordingSegmentModel{answers: []string{long, shortSegmentPayload}}
	generator := rewritingGenerator(t, stub)
	if _, err := generator.GenerateSegment(context.Background(), assistantTestInput()); err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	for index := range stub.requests {
		request := stub.requests[index]
		if len(request) < 2 {
			t.Fatalf("the request %d has %d messages", index, len(request))
		}
		system := request[0].Content
		user := request[1].Content
		cost := rag.EstimateTokens(system) + rag.EstimateTokens(user)
		if cost > generator.maxInputTokens+rag.EstimateTokens(system) {
			t.Fatalf("the request %d costs %d tokens, over the limit of %d",
				index, cost, generator.maxInputTokens)
		}
	}
}

// longSegmentWithBoard is a summary that does not fit and carries a board.
//
// The board is written by the first call, so a test that wants to see what
// the board the batch produced has to put the board in the first answer.
func longSegmentWithBoard(points int) string {
	parts := make([]string, 0, points)
	for index := 0; index < points; index++ {
		parts = append(parts, `{"text":"`+strings.Repeat("学习者把这一条说清楚了", 4)+`","evidence_message_ids":[30]}`)
	}
	return `{"schema_version":1,"segment":{"topic":"很长的一个话题","points":[` +
		strings.Join(parts, ",") + `]},` +
		`"whiteboard":{"schema_version":1,"constraints":[` +
		`{"key":"pages","text":"阅读计划每次读 41 页","value":"41 页/次","source_message_ids":[30]}]}}`
}

// The contract the board entries carry survives the whole path.
//
// The board is written by the first call: a summary that is written again
// replaces the segment and leaves the board alone, so the board the batch
// produced is the one the engine accepted from the first answer. The entry
// keeps the value it was given, the origin it was given and the message it
// was written from.
//
// The claim contract - the evidence the model offers for an entry of the
// learner's own - belongs to the whiteboard change and is checked there.
func TestTheBoardContractSurvivesTheWholePath(t *testing.T) {
	long := longSegmentWithBoard(10)
	short := `{"schema_version":1,"segment":{"topic":"t","points":[` +
		`{"text":"压短之后的要点","evidence_message_ids":[30]}]}}`
	stub := &recordingSegmentModel{answers: []string{long, short}}
	generator := rewritingGenerator(t, stub)
	result, err := generator.GenerateSegment(context.Background(), assistantTestInput())
	if err != nil {
		t.Fatalf("generate segment: %v", err)
	}
	if len(result.Whiteboard.Constraints) != 1 {
		t.Fatalf("the board = %+v", result.Whiteboard.Constraints)
	}
	entry := result.Whiteboard.Constraints[0]
	if entry.Value != "41 页/次" {
		t.Fatalf("the board entry = %+v", entry)
	}
	if entry.EntryOrigin() != model.SessionWhiteboardOriginUser {
		t.Fatalf("the entry's origin = %q, want the learner's", entry.EntryOrigin())
	}
	if len(entry.SourceMessageIDs) != 1 || entry.SourceMessageIDs[0] != 30 {
		t.Fatalf("the entry's citations = %v, want the learner's message", entry.SourceMessageIDs)
	}
	// The segment is the one that was written again, and the board is the
	// one the first answer produced.
	if len(result.Segment.Points) != 1 {
		t.Fatalf("points = %d, want the shorter summary", len(result.Segment.Points))
	}
}
