package agent

import (
	"context"
	"strings"
	"testing"

	"GopherAI/model"

	"github.com/cloudwego/eino/schema"
)

// The re-ask ends the turn.
//
// The learner's answer to a clarification did not identify one conversation,
// so the server asked again. The turn STOPS there: the question is waiting
// for the learner's reply, and the answer comes in their next message.
//
// The model is not called. A question the learner has not answered yet cannot
// be answered, and a model asked to answer it anyway answers it with a
// guess - which is exactly what the clarification exists to stop.

// reaskBuilder is a window that comes back with the server's re-ask.
//
// It is the shape the context builder produces when the learner's clue did
// not identify one conversation: the original question, kept, and the
// conversations this reading found.
type reaskBuilder struct {
	reask *model.SessionClarification
}

func (b reaskBuilder) Build(_ context.Context, input RunContext) (ContextWindow, error) {
	window := ContextWindow{
		Messages:           []*schema.Message{schema.SystemMessage(agentSystemPrompt), schema.UserMessage(input.Question)},
		ClarificationReask: b.reask,
	}
	window.Budget.System = messageTokenCost(agentSystemPrompt)
	window.Budget.CurrentQuestion = messageTokenCost(input.Question)
	window.Budget.Assembled = window.Budget.System + window.Budget.CurrentQuestion
	return window, nil
}

// The re-ask makes no answering model call.
//
// The count is the point: a turn that asked and then called the model anyway
// spent a call on a question nobody had answered, and the answer it produced
// was a guess.
func TestTheReaskMakesNoAnsweringModelCall(t *testing.T) {
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage("我猜是上次那次。", nil), nil
	}}
	reask := &model.SessionClarification{
		ClarificationID:    "clarification-2",
		Question:           "上次你给我排的那三道题里，第二道练的是什么？",
		AllowFreeText:      true,
		AllowHistoryBrowse: true,
		Candidates: []model.SessionClarificationCandidate{
			{SessionID: "session-a", Title: "洄漪"},
			{SessionID: "session-b", Title: "浔漳"},
		},
	}
	runner, err := NewRunner(chat, StaticToolRegistry{}, newMemoryStepRepository(),
		reaskBuilder{reask: reask}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatalf("the turn failed: %v", err)
	}
	if chat.calls != 0 {
		t.Fatalf("the model was called %d times, want none", chat.calls)
	}
	if result.ModelCalls != 0 {
		t.Fatalf("the result claims %d model calls, want none", result.ModelCalls)
	}
	if result.Clarification == nil {
		t.Fatal("the result carries no clarification: the waiting state was not saved")
	}
	if result.Clarification.Question != reask.Question {
		t.Fatalf("the saved clarification carries %q, want the original question", result.Clarification.Question)
	}
	if len(result.Clarification.Candidates) != 2 {
		t.Fatalf("the saved clarification offers %d candidates, want both", len(result.Clarification.Candidates))
	}
	if !result.Clarification.AllowHistoryBrowse {
		t.Fatal("the saved clarification does not offer the learner's own conversation list")
	}
}

// The turn that ended by asking says so.
//
// "answered" and "asked" are different facts about a run, and a reader - or
// an evaluation - has to be able to tell them apart.
func TestTheTurnThatEndedByAskingSaysSo(t *testing.T) {
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage("我猜是上次那次。", nil), nil
	}}
	reask := &model.SessionClarification{
		ClarificationID: "clarification-2", Question: "哪一次？", AllowFreeText: true,
		AllowHistoryBrowse: true,
		Candidates:         []model.SessionClarificationCandidate{{SessionID: "session-a", Title: "洄漪"}},
	}
	runner, err := NewRunner(chat, StaticToolRegistry{}, newMemoryStepRepository(),
		reaskBuilder{reask: reask}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatalf("the turn failed: %v", err)
	}
	if result.Closeout != TerminationWaitingChoice {
		t.Fatalf("the closeout is %q, want %q", result.Closeout, TerminationWaitingChoice)
	}
	if result.Closeout == TerminationAnswered {
		t.Fatal("a turn that asked is recorded as one that answered")
	}
}

// The turn that ended by asking says SOMETHING.
//
// The learner sees a message, and the message carries the question. The
// sentence is fixed and not a model's: the turn ends before the model is
// asked anything.
func TestTheTurnThatEndedByAskingSaysSomething(t *testing.T) {
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage("我猜是上次那次。", nil), nil
	}}
	reask := &model.SessionClarification{
		ClarificationID: "clarification-2", Question: "哪一次？", AllowFreeText: true,
		AllowHistoryBrowse: true,
		Candidates:         []model.SessionClarificationCandidate{{SessionID: "session-a", Title: "洄漪"}},
	}
	runner, err := NewRunner(chat, StaticToolRegistry{}, newMemoryStepRepository(),
		reaskBuilder{reask: reask}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatalf("the turn failed: %v", err)
	}
	if strings.TrimSpace(result.Answer) == "" {
		t.Fatal("the turn that ended by asking produced no answer at all")
	}
	if result.Answer != WaitingChoiceAnswer {
		t.Fatalf("the answer is %q, want the fixed waiting sentence", result.Answer)
	}
}

// A turn with no re-ask is unchanged.
//
// The rule is about the turn that asked again, and a turn that did not ask
// again keeps the behaviour it had.
func TestATurnWithoutAReaskStillCallsTheModel(t *testing.T) {
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage("第二道练的是读写拆分。", nil), nil
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{}, newMemoryStepRepository(),
		MinimalContextBuilder{}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatalf("the turn failed: %v", err)
	}
	if chat.calls == 0 {
		t.Fatal("an ordinary turn made no model call")
	}
	if result.Closeout != TerminationAnswered {
		t.Fatalf("the closeout is %q, want %q", result.Closeout, TerminationAnswered)
	}
}
