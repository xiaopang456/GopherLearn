package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// steppingResponder is the agent harness as the message service sees it: a strategy that
// can report progress, and can say that it can. It exists so these tests are about what
// the service puts on the stream - order, correlation, refusal, failure - and not about
// whether a tool loop works, which internal/agent already proves.
type steppingResponder struct {
	answer string
	chunks []string
	steps  []responder.Step
	// failAfter reports this many steps and then ends the turn with err.
	failAfter int
	err       error
	// onStep runs inside the step callback, so a test can cancel mid-progress.
	onStep      func(index int)
	streamCalls int
	stepCalls   int
}

func (f *steppingResponder) Respond(context.Context, responder.Request) (responder.Answer, error) {
	return responder.Answer{Content: f.answer}, nil
}

func (f *steppingResponder) Stream(_ context.Context, _ responder.Request, emit func(string) error) (responder.Answer, error) {
	f.streamCalls++
	for _, chunk := range f.chunks {
		if err := emit(chunk); err != nil {
			return responder.Answer{}, err
		}
	}
	return responder.Answer{Content: f.answer}, nil
}

func (f *steppingResponder) StreamSteps(_ context.Context, _ responder.Request, emit func(string) error,
	onStep responder.StepSink,
) (responder.Answer, error) {
	f.stepCalls++
	for index, step := range f.steps {
		if f.onStep != nil {
			f.onStep(index)
		}
		if f.err != nil && f.failAfter > 0 && index == f.failAfter {
			return responder.Answer{}, f.err
		}
		if err := onStep(step); err != nil {
			return responder.Answer{}, err
		}
	}
	if f.err != nil {
		return responder.Answer{}, f.err
	}
	for _, chunk := range f.chunks {
		if err := emit(chunk); err != nil {
			return responder.Answer{}, err
		}
	}
	return responder.Answer{Content: f.answer}, nil
}

// SupportsSteps answers by the resolved strategy, the way the real responder does: a
// capability the turn cannot use is refused, not accepted and forgotten.
func (f *steppingResponder) SupportsSteps(request responder.Request) bool {
	return request.AnswerStrategy == domain.AnswerStrategyAgent
}

// terminatingError is what an agent loop hands back when it stopped for a reason it can
// name; the service relays that name instead of inventing one.
type terminatingError struct {
	cause  error
	reason string
	text   string
}

func (e *terminatingError) Error() string              { return e.reason + ": " + e.cause.Error() }
func (e *terminatingError) Unwrap() error              { return e.cause }
func (e *terminatingError) TerminationReason() string  { return e.reason }
func (e *terminatingError) TerminationMessage() string { return e.text }

func agentSteps() []responder.Step {
	return []responder.Step{
		{Phase: responder.StepStarted, Sequence: 1, StepType: responder.StepTypePlan, Message: "model generation 1"},
		{Phase: responder.StepStarted, Sequence: 2, StepType: responder.StepTypeTool, Message: "tool call 1"},
		{Phase: responder.ToolStarted, Sequence: 2, ToolName: "memory_search"},
		{Phase: responder.ToolResult, Sequence: 2, ToolName: "memory_search", Content: "读到 2 条记忆", DurationMillis: 7},
		{Phase: responder.StepStarted, Sequence: 3, StepType: responder.StepTypePlan, Message: "model generation 2"},
	}
}

func TestStreamStepsAddsProgressBesideAnUnchangedAnswerLifecycle(t *testing.T) {
	assistant := &steppingResponder{answer: "按记忆回答。", chunks: []string{"按", "记忆回答。"}, steps: agentSteps()}
	service, store, userID, sessionID := newMessageFixture(t, assistant, domain.SessionModeLearningAgent)

	var names []string
	var events []StreamEvent
	err := service.StreamSteps(context.Background(), userID, sessionID, "问题", domain.KnowledgeOptions{},
		func(event StreamEvent) error {
			names = append(names, event.Name)
			events = append(events, event)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"message.started",
		"step_started", "step_started", "tool_started", "tool_result", "step_started",
		"message.delta", "message.delta", "message.completed",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("event order = %v, want %v", names, want)
	}
	if assistant.stepCalls != 1 || assistant.streamCalls != 0 {
		t.Fatalf("streamCalls=%d stepCalls=%d, want the turn run once through the progress path",
			assistant.streamCalls, assistant.stepCalls)
	}
	// The answer's own frames are untouched: same names, same order, same content.
	if events[len(events)-1].Content != "按记忆回答。" || events[len(events)-1].MessageID == "" {
		t.Fatalf("completed frame = %+v", events[len(events)-1])
	}
	runID := events[1].RunID
	if runID == "" {
		t.Fatal("a step frame carried no run id to correlate by")
	}
	for index, event := range events {
		if index > 0 && event.RunID != runID {
			t.Fatalf("frame %d carries run id %q, want the turn's %q", index+1, event.RunID, runID)
		}
	}
	if events[3].ToolName != "memory_search" || events[3].Sequence != 2 {
		t.Fatalf("tool_started frame = %+v, want the call's own name and number", events[3])
	}
	if events[4].Content != "读到 2 条记忆" || events[4].DurationMillis != 7 {
		t.Fatalf("tool_result frame = %+v", events[4])
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Content != "按记忆回答。" {
		t.Fatalf("stored turn = %+v", messages)
	}
}

func TestStreamStepsRefusedWhenTheTurnHasNoSteps(t *testing.T) {
	assistant := &steppingResponder{answer: "普通回答", chunks: []string{"普通回答"}}
	service, store, userID, sessionID := newMessageFixture(t, assistant, domain.SessionModeChat)

	var names []string
	err := service.StreamSteps(context.Background(), userID, sessionID, "问题", domain.KnowledgeOptions{},
		func(event StreamEvent) error {
			names = append(names, event.Name)
			return nil
		})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("refusal = %v, want an invalid request", err)
	}
	// Nothing on the wire: a stream that began and then admitted it had no steps would be
	// the fake event this test exists to catch.
	if len(names) != 0 {
		t.Fatalf("a refused step stream emitted %v", names)
	}
	if assistant.stepCalls != 0 || assistant.streamCalls != 0 {
		t.Fatalf("the model was reached anyway: step=%d stream=%d", assistant.stepCalls, assistant.streamCalls)
	}
	if err := service.ValidateStreamTurn(context.Background(), userID, sessionID, "问题",
		domain.KnowledgeOptions{}, true); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ValidateStreamTurn with steps = %v, want the same refusal", err)
	}
	// The same turn without steps is still an ordinary, working chat stream, and the
	// validator agrees it needs nothing new.
	if err := service.ValidateStreamTurn(context.Background(), userID, sessionID, "问题",
		domain.KnowledgeOptions{}, false); err != nil {
		t.Fatalf("plain validation failed: %v", err)
	}
	if err := service.Stream(context.Background(), userID, sessionID, "问题", domain.KnowledgeOptions{},
		func(event StreamEvent) error { names = append(names, event.Name); return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"message.started", "message.delta", "message.completed"}) {
		t.Fatalf("plain chat stream = %v", names)
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("chat turn stored %d messages, want the question and the answer", len(messages))
	}
}

func TestPlainStreamNeverSeesStepFieldsEvenFromAStepCapableStrategy(t *testing.T) {
	assistant := &steppingResponder{answer: "回答", chunks: []string{"回", "答"}, steps: agentSteps()}
	service, _, userID, sessionID := newMessageFixture(t, assistant, domain.SessionModeLearningAgent)

	var events []StreamEvent
	if err := service.Stream(context.Background(), userID, sessionID, "问题", domain.KnowledgeOptions{},
		func(event StreamEvent) error {
			events = append(events, event)
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(eventNames(events),
		[]string{"message.started", "message.delta", "message.delta", "message.completed"}) {
		t.Fatalf("plain agent stream = %v, want the four message frames only", eventNames(events))
	}
	for index, event := range events {
		if event.RunID != "" || event.Sequence != 0 || event.StepType != "" || event.ToolName != "" ||
			event.TerminationReason != "" {
			t.Fatalf("frame %d carries step fields on a plain stream: %+v", index+1, event)
		}
	}
	if assistant.stepCalls != 0 || assistant.streamCalls != 1 {
		t.Fatalf("stepCalls=%d streamCalls=%d, want the ordinary path", assistant.stepCalls, assistant.streamCalls)
	}
}

func TestStreamStepsNamesWhyTheTurnEndedAndKeepsTheFailureFrameHonest(t *testing.T) {
	assistant := &steppingResponder{
		answer:    "不该出现",
		failAfter: 1,
		steps:     agentSteps(),
		err: &terminatingError{
			cause:  errors.New("the model-call budget for this turn is exhausted: 1 calls"),
			reason: "model_call_limit",
			text:   "本轮已达到模型调用上限，回答未完成。",
		},
	}
	service, store, userID, sessionID := newMessageFixture(t, assistant, domain.SessionModeLearningAgent)

	var events []StreamEvent
	err := service.StreamSteps(context.Background(), userID, sessionID, "问题", domain.KnowledgeOptions{},
		func(event StreamEvent) error {
			events = append(events, event)
			return nil
		})
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("error = %v, want the caller's internal failure", err)
	}
	if eventNames(events)[len(events)-1] != "message.failed" {
		t.Fatalf("the turn did not end in a failure frame: %v", eventNames(events))
	}
	failed := events[len(events)-1]
	if failed.TerminationReason != "model_call_limit" || failed.Message != "本轮已达到模型调用上限，回答未完成。" {
		t.Fatalf("failed frame = %+v, want the reason the loop named", failed)
	}
	if failed.Code != "internal_error" {
		t.Fatalf("failed code = %q, want the code the contract already has", failed.Code)
	}
	// No answer was produced, so no answer may be stored or streamed.
	for _, event := range events {
		if event.Name == "message.completed" || event.Name == "message.delta" {
			t.Fatalf("a failed turn emitted %q", event.Name)
		}
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("failed turn stored %+v", messages)
	}
}

func TestStreamStepsCancellationStopsProgressAndFabricatesNothing(t *testing.T) {
	assistant := &steppingResponder{answer: "不该出现", chunks: []string{"不该"}, steps: agentSteps()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The client hangs up while the second step is being reported.
	assistant.onStep = func(index int) {
		if index == 1 {
			cancel()
		}
	}
	service, store, userID, sessionID := newMessageFixture(t, assistant, domain.SessionModeLearningAgent)

	var events []StreamEvent
	err := service.StreamSteps(ctx, userID, sessionID, "问题", domain.KnowledgeOptions{},
		func(event StreamEvent) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			events = append(events, event)
			return nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the caller's cancellation", err)
	}
	// The stop frame a cancelled turn must not receive: the client is gone, and a failure
	// event would be this server talking to itself about a tab nobody left open.
	for _, event := range events {
		if event.Name == "message.failed" || event.Name == "message.completed" {
			t.Fatalf("a cancelled stream emitted %q", event.Name)
		}
	}
	if len(events) != 2 {
		t.Fatalf("frames after the hang-up = %v, want the stream to stop where the client left", eventNames(events))
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("cancelled turn stored %+v", messages)
	}
}

func eventNames(events []StreamEvent) []string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, event.Name)
	}
	return names
}
