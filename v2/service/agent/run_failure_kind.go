package agent

import (
	"context"
	"errors"
)

// Classifying a run failure by what it is.
//
// STABILIZE-062 v1.2 spent a real run learning why this has to be structural.
// Its evaluator matched keywords over the runner's error text, and the text of
// ErrRepeatedToolCall is "agent repeated the same tool call" - which contains
// "tool call". So a model that repeated a search was filed as a protocol error,
// and a protocol error voided the whole round.
//
// Two things were wrong with that, and only one of them is about wording. The
// deeper one is that a repeated tool call is not an instrument fault at all: it
// is the production safety boundary refusing something, which is a fact about
// the run. Treating it as evidence that nothing was measured throws away a
// measurement that did happen.
//
// The classification already existed in this package - classifyRunError has
// used errors.Is since it was written. What it did not have was a name the
// evaluator could import. This is that name, and classifyRunError now reads it,
// so the service's reason strings and statuses are produced by exactly the same
// decision the evaluator sees.

// ClassifyRunFailure names the cause of a failed run.
//
// parent is the caller's context, so a cancellation that happened outside the
// run is recognised as one; it may be nil.
func ClassifyRunFailure(parent context.Context, err error) RunFailureKind {
	if err == nil && (parent == nil || parent.Err() == nil) {
		return RunFailureNone
	}
	var parentErr error
	if parent != nil {
		parentErr = parent.Err()
	}
	switch {
	case errors.Is(parentErr, context.Canceled), errors.Is(err, context.Canceled),
		errors.Is(err, ErrEventDelivery):
		return RunFailureCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return RunFailureTurnDeadline
	// The origin envelopes come first among the rest: a store fault wrapped on
	// its way out must not be re-read as whatever sentinel happens to be
	// further up the chain.
	case errors.Is(err, ErrProviderTransport):
		return RunFailureProviderTransport
	case errors.Is(err, ErrRetrievalInfrastructure):
		return RunFailureRetrievalInfrastructure
	case errors.Is(err, ErrToolInfrastructure):
		return RunFailureToolInfrastructure
	case errors.Is(err, ErrGroundingRejected):
		return RunFailureGroundingRejected
	case errors.Is(err, ErrToolCallLimit):
		return RunFailureToolCallLimit
	case errors.Is(err, ErrModelCallLimit):
		return RunFailureModelCallLimit
	case errors.Is(err, ErrRepeatedToolCall):
		return RunFailureRepeatedToolCall
	case errors.Is(err, ErrUnknownTool):
		return RunFailureUnknownTool
	case errors.Is(err, ErrGraphStepLimit):
		return RunFailureGraphStepLimit
	case errors.Is(err, ErrIncompleteModelStream):
		return RunFailureIncompleteModelStream
	// An existing sentinel that no classifier covered, so it fell into the old
	// catch-all. Found while investigating why v1.3's bucket was unreadable.
	case errors.Is(err, ErrEmptyAgentAnswer):
		return RunFailureEmptyAnswer
	case err == nil:
		return RunFailureNone
	default:
		// Deliberately not a friendly name. The old catch-all could absorb four
		// unrelated faults and still let a round be judged; this one is meant
		// to be visible and to stop it.
		return RunFailureUnknown
	}
}
