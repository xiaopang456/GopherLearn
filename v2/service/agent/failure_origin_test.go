package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// STABILIZE-062 v1.4: an error gets its type where it is produced.
//
// v1.3 classified by errors.Is against the Runner's own sentinels, which was the
// right shape and stopped the keyword guessing. What it could not do is say
// anything about a failure that never became one of those sentinels: a provider
// transport fault, a Redis or MySQL fault, a tool that failed while running, an
// answer the Runner rejected. All of those arrived as RunFailureOther, and
// RunFailureOther was enough on its own to void a whole round.
//
// So the categories are no longer decided by the top of the stack. Each origin
// wraps its own cause in a typed envelope, and the classifier reads the type.

// Every origin is recognised, wrapped or bare, and the cause survives.
func TestOriginTypedFailuresAreRecognised(t *testing.T) {
	for name, expected := range map[string]struct {
		err    error
		kind   RunFailureKind
		origin FailureOrigin
	}{
		"provider transport": {
			WrapProviderFailure(errors.New("connection reset")),
			RunFailureProviderTransport, OriginInstrument},
		"retrieval store": {
			WrapRetrievalInfrastructureFailure(errors.New("dial tcp: refused")),
			RunFailureRetrievalInfrastructure, OriginInstrument},
		"tool infrastructure": {
			WrapToolInfrastructureFailure(errors.New("index missing")),
			RunFailureToolInfrastructure, OriginInstrument},
		"empty answer":     {ErrEmptyAgentAnswer, RunFailureEmptyAnswer, OriginMethod},
		"repeated call":    {ErrRepeatedToolCall, RunFailureRepeatedToolCall, OriginMethod},
		"tool call limit":  {ErrToolCallLimit, RunFailureToolCallLimit, OriginMethod},
		"model call limit": {ErrModelCallLimit, RunFailureModelCallLimit, OriginMethod},
		"graph steps":      {ErrGraphStepLimit, RunFailureGraphStepLimit, OriginMethod},
		"incomplete stream": {ErrIncompleteModelStream,
			RunFailureIncompleteModelStream, OriginInstrument},
		"unknown tool": {ErrUnknownTool, RunFailureUnknownTool, OriginUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			kind := ClassifyRunFailure(context.Background(), expected.err)
			if kind != expected.kind {
				t.Fatalf("kind = %s, want %s", kind, expected.kind)
			}
			if got := kind.Origin(); got != expected.origin {
				t.Fatalf("origin = %s, want %s", got, expected.origin)
			}
			// Wrapped the way a real call stack wraps it.
			wrapped := fmt.Errorf("agent run abc: %w", expected.err)
			if got := ClassifyRunFailure(context.Background(), wrapped); got != expected.kind {
				t.Fatalf("wrapped kind = %s, want %s", got, expected.kind)
			}
		})
	}
}

// The cause is preserved for a local reader and never becomes the category.
func TestWrappedCauseSurvivesWithoutDecidingTheCategory(t *testing.T) {
	cause := errors.New("redis: dial tcp 127.0.0.1:36379: connect: refused")
	wrapped := WrapRetrievalInfrastructureFailure(cause)
	if !errors.Is(wrapped, cause) {
		t.Fatal("the original cause was lost")
	}
	if !errors.Is(wrapped, ErrRetrievalInfrastructure) {
		t.Fatal("the envelope is not recognisable by its sentinel")
	}
	// A message that looks like something else cannot move the category.
	disguised := WrapProviderFailure(errors.New("agent repeated the same tool call"))
	if got := ClassifyRunFailure(context.Background(), disguised); got != RunFailureProviderTransport {
		t.Fatalf("wording moved the category: %s", got)
	}
}

// Nothing unknown is silently absorbed: an unrecognised error is named unknown
// and its origin is unknown, which is the one thing that must still stop a round.
func TestUnknownErrorsAreNamedUnknownRatherThanRunnerError(t *testing.T) {
	kind := ClassifyRunFailure(context.Background(), errors.New("something nobody typed"))
	if kind != RunFailureUnknown {
		t.Fatalf("kind = %s, want %s", kind, RunFailureUnknown)
	}
	if kind.Origin() != OriginUnknown {
		t.Fatalf("origin = %s, want %s", kind.Origin(), OriginUnknown)
	}
	// And the old catch-all name is gone from the vocabulary.
	for _, kind := range AllRunFailureKinds() {
		if string(kind) == "other" || string(kind) == "runner_error" {
			t.Fatalf("the catch-all %q survives", kind)
		}
	}
}

// The service's reason strings and statuses are unchanged for every kind that
// existed before this round.
func TestServiceOutputUnchangedForPreExistingKinds(t *testing.T) {
	for _, expected := range []struct {
		err    error
		reason string
	}{
		{ErrToolCallLimit, "tool_call_limit"},
		{ErrModelCallLimit, "model_call_limit"},
		{ErrRepeatedToolCall, "repeated_tool_call"},
		{ErrUnknownTool, "unknown_tool"},
		{ErrGraphStepLimit, "graph_step_limit"},
		{ErrIncompleteModelStream, "incomplete_model_stream"},
		{context.Canceled, "cancelled"},
		{context.DeadlineExceeded, "timeout"},
		{errors.New("anything else"), "runner_failed"},
	} {
		reason, _ := classifyRunError(context.Background(), expected.err)
		if reason != expected.reason {
			t.Errorf("service reason = %s, want %s", reason, expected.reason)
		}
	}
}
