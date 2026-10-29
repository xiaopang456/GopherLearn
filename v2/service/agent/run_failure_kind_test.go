package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"GopherAI/model"
)

// The shared classifier decides by sentinel, and the service's reason strings
// and statuses are exactly what they were.
//
// The second half matters as much as the first. classifyRunError feeds a
// persisted status and a user-visible sentence; STABILIZE-062 needed the
// classification, not a change to what anyone sees.
func TestClassifyRunFailureIsStructuralAndServiceOutputIsUnchanged(t *testing.T) {
	for name, expected := range map[string]struct {
		err    error
		kind   RunFailureKind
		reason string
		status model.AgentRunStatus
	}{
		"tool call limit": {ErrToolCallLimit, RunFailureToolCallLimit,
			"tool_call_limit", model.AgentRunFailed},
		"model call limit": {ErrModelCallLimit, RunFailureModelCallLimit,
			"model_call_limit", model.AgentRunFailed},
		"repeated call": {ErrRepeatedToolCall, RunFailureRepeatedToolCall,
			"repeated_tool_call", model.AgentRunFailed},
		"unknown tool": {ErrUnknownTool, RunFailureUnknownTool,
			"unknown_tool", model.AgentRunFailed},
		"graph steps": {ErrGraphStepLimit, RunFailureGraphStepLimit,
			"graph_step_limit", model.AgentRunFailed},
		"incomplete stream": {ErrIncompleteModelStream, RunFailureIncompleteModelStream,
			"incomplete_model_stream", model.AgentRunFailed},
		"event delivery": {ErrEventDelivery, RunFailureCancelled,
			"cancelled", model.AgentRunCancelled},
		"cancelled": {context.Canceled, RunFailureCancelled,
			"cancelled", model.AgentRunCancelled},
		"timeout": {context.DeadlineExceeded, RunFailureTurnDeadline,
			"timeout", model.AgentRunTimeout},
		"anything else": {errors.New("provider 500 mentioning a tool call"), RunFailureUnknown,
			"runner_failed", model.AgentRunFailed},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ClassifyRunFailure(context.Background(), expected.err); got != expected.kind {
				t.Fatalf("kind = %s, want %s", got, expected.kind)
			}
			// Wrapped, which is how the Runner returns most of them.
			wrapped := fmt.Errorf("agent run: %w", expected.err)
			if got := ClassifyRunFailure(context.Background(), wrapped); got != expected.kind {
				t.Fatalf("wrapped kind = %s, want %s", got, expected.kind)
			}
			reason, status := classifyRunError(context.Background(), expected.err)
			if reason != expected.reason || status != expected.status {
				t.Fatalf("service reported %s/%s, want %s/%s",
					reason, status, expected.reason, expected.status)
			}
			// And the user-visible sentence is untouched.
			if publicRunError(reason) == "" {
				t.Fatal("the public message is empty")
			}
		})
	}
}

// A cancelled parent context is recognised even when the run's own error is
// something else, which is the behaviour the service already had.
func TestCancelledParentStillWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := ClassifyRunFailure(ctx, ErrToolCallLimit); got != RunFailureCancelled {
		t.Fatalf("kind = %s, want %s", got, RunFailureCancelled)
	}
	reason, status := classifyRunError(ctx, ErrToolCallLimit)
	if reason != "cancelled" || status != model.AgentRunCancelled {
		t.Fatalf("service reported %s/%s", reason, status)
	}
}

// The safety boundary is a named property, not a list repeated at each caller.
func TestSafetyBoundaryNamesTheEnvelopesOwnRefusals(t *testing.T) {
	for _, kind := range []RunFailureKind{RunFailureToolCallLimit, RunFailureModelCallLimit,
		RunFailureGraphStepLimit, RunFailureRepeatedToolCall} {
		if !kind.IsSafetyBoundary() {
			t.Errorf("%s is not recognised as the envelope refusing something", kind)
		}
	}
	for _, kind := range []RunFailureKind{RunFailureIncompleteModelStream, RunFailureUnknownTool,
		RunFailureCancelled, RunFailureTurnDeadline, RunFailureUnknown, RunFailureNone} {
		if kind.IsSafetyBoundary() {
			t.Errorf("%s is wrongly recognised as a safety boundary", kind)
		}
	}
}
