package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// The deployment's thinking choice is what a request inherits, and a request
// that states one wins.
//
// The proof is the run's identity: a run is identified by what it was asked
// to do, so two requests that behave the same are the same run and two that
// do not are not. That is what makes the difference observable without a
// provider in the loop.

// Thinking is off until a deployment turns it on, so a request that says
// nothing and a request that says "no" are the same run.
func TestServiceThinkingDefaultsToOff(t *testing.T) {
	service, _ := newThinkingTestService(t)

	first, err := service.Run(context.Background(), agentTestRequest(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	stated := agentTestRequest()
	stated.EnableThinking = boolPointer(false)
	replay, err := service.Run(context.Background(), stated, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.RunID != first.RunID {
		t.Fatalf("replay=%+v first=%+v", replay, first)
	}
}

// A deployment that turns thinking on gets it on every run that does not
// ask otherwise, and a request that asks otherwise is a different run.
func TestServiceThinkingFollowsTheDeploymentDefault(t *testing.T) {
	service, _ := newThinkingTestService(t, WithThinkingEnabled(true))

	first, err := service.Run(context.Background(), agentTestRequest(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	stated := agentTestRequest()
	stated.EnableThinking = boolPointer(true)
	replay, err := service.Run(context.Background(), stated, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.RunID != first.RunID {
		t.Fatalf("replay=%+v first=%+v", replay, first)
	}
	off := agentTestRequest()
	off.EnableThinking = boolPointer(false)
	result, err := service.Run(context.Background(), off, DiscardEvents)
	if !errors.Is(err, ErrRunRequestConflict) || result.RunID != first.RunID {
		t.Fatalf("thinking-off request result=%+v err=%v", result, err)
	}
}

// A request that states its own choice wins over the deployment's, in both
// directions: the deployment cannot force thinking onto a run that asked for
// none, and a run that asked for thinking gets it even where the deployment
// would have left it off.
func TestServiceThinkingRequestOverridesTheDeploymentDefault(t *testing.T) {
	service, _ := newThinkingTestService(t, WithThinkingEnabled(true))

	off := agentTestRequest()
	off.EnableThinking = boolPointer(false)
	first, err := service.Run(context.Background(), off, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	alsoOff := agentTestRequest()
	alsoOff.EnableThinking = boolPointer(false)
	replay, err := service.Run(context.Background(), alsoOff, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.RunID != first.RunID {
		t.Fatalf("replay=%+v first=%+v", replay, first)
	}
	inherit := agentTestRequest()
	result, err := service.Run(context.Background(), inherit, DiscardEvents)
	if !errors.Is(err, ErrRunRequestConflict) || result.RunID != first.RunID {
		t.Fatalf("inherited request result=%+v err=%v", result, err)
	}
}

func newThinkingTestService(t *testing.T, options ...ServiceOption) (*Service, *memoryRunRepository) {
	t.Helper()
	chatModel := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
		return schema.AssistantMessage("答", nil), nil
	}}
	repository := newMemoryRunRepository(agentTestSession())
	runner := mustRunner(t, chatModel, StaticToolRegistry{}, repository, DefaultRunnerConfig())
	service, err := NewService(repository, repository, &memoryMessageStore{}, runner, options...)
	if err != nil {
		t.Fatal(err)
	}
	return service, repository
}

func boolPointer(value bool) *bool {
	return &value
}
