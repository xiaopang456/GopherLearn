package conversation

import (
	"context"
	"errors"
	"strconv"
	"strings"

	agent "GopherAI/service/agent"
)

// summaryFailureCause is the driver's own reading of a compaction failure:
// what caused it, and whose fault that is.
//
// The product's own reason (agent.SummaryFailureReasonOf) says which stage
// failed. This says whether the environment or the product did it, because
// the two are not the same finding. A timeout is the upstream's; a summary
// the model returned that the product could not use is the product's.
//
// The cooldown belongs to neither. It is what the provider does *after* a
// failure, so a run that names it as the cause has named the symptom and
// lost the reason: the same cooldown follows a timeout, an output that was
// too long, a body that would not parse and a storage failure.
type summaryFailureCause string

const (
	// summaryCauseNone means the error was not a failure of the compaction
	// itself: a cooldown (the failure before it) or a skipped stage.
	summaryCauseNone summaryFailureCause = ""
	// summaryCauseUnknown means a failure was seen but not one this code can
	// place. It is reported as unknown rather than folded into a cause it
	// cannot prove: evidence that is short of a conclusion is not a zero.
	summaryCauseUnknown summaryFailureCause = "unknown"
	// summaryCauseUpstreamTimeout means the summary request itself ran out
	// of time.
	summaryCauseUpstreamTimeout summaryFailureCause = "upstream_timeout"
	// summaryCauseOutputOverLimit means the model returned more than the
	// summary's output cap allows.
	summaryCauseOutputOverLimit summaryFailureCause = "output_over_limit"
	// summaryCauseFormatError means the model's answer could not be read as a
	// summary document.
	summaryCauseFormatError summaryFailureCause = "format_error"
	// summaryCauseStorage means the summary could not be read or written.
	//
	// It is its own cause because the product's reason vocabulary cannot
	// separate it: the product's rule reads the wording "rolling summary:",
	// which a storage failure carries, so it labels a storage failure
	// "structure". The driver's cause says whose failure it is, and a storage
	// failure is a different thing from a body that would not parse - one is
	// the database, the other is the model's answer. The product's own rule
	// is left as it is; only the cause is read more finely here.
	summaryCauseStorage summaryFailureCause = "storage_failure"
	// summaryCauseProductOther is every other product-side failure: an empty
	// summary, a document the product rejects, and the summarizer's own input
	// budget.
	summaryCauseProductOther summaryFailureCause = "product_failure"
)

// summaryFailure is one compaction failure, read once.
type summaryFailure struct {
	// Reason is the product's own reason for it.
	Reason agent.SummaryFailureReason
	// Cause is whose failure it is.
	Cause summaryFailureCause
	// Text is the error's message, kept short: it is what a reader needs to
	// tell this failure from another one.
	Text string
}

// maxFailureText bounds what a failure's message keeps. The message can
// carry a whole request line, and the point of keeping it is that a reader
// can tell two failures apart, not that the record grows with the request.
const maxFailureText = 240

// classifySummaryFailure reads one compaction failure.
//
// The product's rule is asked first, because it is the rule that decides
// what the product itself calls a timeout, a length failure and a parse
// failure. The message is consulted afterwards for the case where the chain
// was flattened on the way up: a client that wraps an error into a string
// loses the sentinel, and then the product's rule can only say "a model call
// did not answer", which would hide the difference between an upstream that
// timed out and a model that answered badly.
//
// The message is also what separates the two length failures, which the
// product's rule lumps together: an output that was too long is the model's,
// and an increment that does not fit the summarizer's input budget is the
// product's own configuration.
func classifySummaryFailure(err error) summaryFailure {
	if err == nil {
		return summaryFailure{}
	}
	reason := agent.SummaryFailureReasonOf(err)
	text := err.Error()
	trimmed := text
	if len(trimmed) > maxFailureText {
		trimmed = trimmed[:maxFailureText]
	}
	switch {
	case reason == agent.SummaryFailureCooldown || reason == agent.SummaryFailureSkipped:
		// Not a failure of the compaction: the cooldown is the failure before
		// it, and a skipped stage is a request that was merely late.
		return summaryFailure{Reason: reason, Text: trimmed}
	case reason == agent.SummaryFailureTimeout || looksLikeTimeout(text):
		return summaryFailure{Reason: reason, Cause: summaryCauseUpstreamTimeout, Text: trimmed}
	case strings.Contains(text, "exceeds output token limit"):
		return summaryFailure{Reason: reason, Cause: summaryCauseOutputOverLimit, Text: trimmed}
	case looksLikeStorageFailure(text):
		return summaryFailure{Reason: reason, Cause: summaryCauseStorage, Text: trimmed}
	case reason == agent.SummaryFailureParse || reason == agent.SummaryFailureStructure:
		return summaryFailure{Reason: reason, Cause: summaryCauseFormatError, Text: trimmed}
	default:
		return summaryFailure{Reason: reason, Cause: summaryCauseProductOther, Text: trimmed}
	}
}

// looksLikeStorageFailure is the wording the product uses when the summary
// itself could not be read or written.
//
// The product's own reason cannot separate this from a body that would not
// parse, because both carry the wording "rolling summary:" and the reason's
// rule is closed. The cause can: the storage's failures all name the store
// they failed on.
func looksLikeStorageFailure(text string) bool {
	lowered := strings.ToLower(text)
	for _, marker := range []string{
		"save rolling summary",
		"read rolling summary",
		"read summary increment",
		"store rolling summary",
		"load rolling summary",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// looksLikeTimeout is the text fallback for a timeout whose error chain was
// flattened on the way up.
//
// The product's rule reads the chain, and the chain survives only while
// every wrapper uses %w. A client that turns an error into a string keeps
// the wording and loses the sentinel, so the wording is what is left to
// read. This is a fallback, not a rule of its own: it is asked after the
// product's rule, so a body that merely mentions a timeout in passing does
// not override a parse failure the product identified.
func looksLikeTimeout(text string) bool {
	lowered := strings.ToLower(text)
	return strings.Contains(lowered, "context deadline exceeded") ||
		strings.Contains(lowered, "deadline exceeded") ||
		strings.Contains(lowered, "client.timeout") ||
		strings.Contains(lowered, "i/o timeout")
}

// summaryOutcome is what the truncated arm's write phase and its window add up
// to.
//
// The three are different findings and they are kept apart. An environment
// failure is a run the upstream decided, and it is neither a right answer nor
// a wrong one. A write failure is the product's own writer failing, and the
// run's result stands: the answer was given, and the report says the
// compression behind it was not what it should have been. An unknown is a run
// whose cause the evidence does not reach: it is not counted as an
// environment failure and it is not counted as a product failure either.
type summaryOutcome struct {
	// EnvironmentFailure names a run the upstream decided.
	EnvironmentFailure string
	// WriteFailure names a run the product's own writer decided.
	WriteFailure string
	// Unknown names a run whose cause the evidence does not reach.
	Unknown string
}

// classifySummaryOutcome reads what the write phase did and what the window
// carried, and says what the run is.
//
// The environment failure needs two things and not one: the first failure
// was the upstream's, and the window carried no summary. A timeout that left
// a usable summary behind is not an environment failure - the arm still
// answered from a compression, and the write phase's failure is a fact to
// report, not a reason to void the answer.
//
// A timeout with no cooldown behind it is still an environment failure: the
// cooldown is what the provider does after a failure, and a run that
// required one to have happened would miss every failure that was the last
// of its kind.
//
// A product-side cause - an output that was too long, a body that would not
// parse, a storage failure - is a write failure and never an environment
// failure. Washing it into the environment's column would take the
// product's own failures off its sheet.
//
// A cooldown with no observed cause is neither: the failure behind it was not
// seen, and the honest reading is unknown.
func classifySummaryOutcome(arm string, replay rollingReplay, summaryTokens int) summaryOutcome {
	if arm != LayeredArmTruncated {
		return summaryOutcome{}
	}
	outcome := summaryOutcome{}
	failure := replay.firstFailure
	switch failure.Cause {
	case summaryCauseNone:
		// No failure was recorded, or the only error was the cooldown that
		// follows one. If a cooldown was hit, the failure behind it was not
		// observed, so the cause is unknown - not zero.
		if replay.cooldowns > 0 {
			outcome.Unknown = "the failure cooldown refused " + strconv.Itoa(replay.cooldowns) +
				" compaction(s) and the failure behind it was not observed"
		}
	case summaryCauseUpstreamTimeout:
		if summaryTokens == 0 {
			outcome.EnvironmentFailure = "the rolling summary compaction timed out (" + failure.Text +
				"); the window carried no summary"
		} else {
			outcome.WriteFailure = "the rolling summary compaction timed out (" + failure.Text +
				") but the window carried a summary, so the answer stands"
		}
	default:
		// The product's own writer failed. The result stands and is listed
		// apart: this is a product failure, not the environment's.
		outcome.WriteFailure = "the rolling summary compaction failed (" + string(failure.Reason) + "): " + failure.Text
	}
	return outcome
}

// isCooldownError reports whether an error is the provider's cooldown.
//
// It is a separate function because the cooldown is the one error whose
// meaning depends on what came before it: on its own it says nothing, and
// the driver must not read it as a cause.
func isCooldownError(err error) bool {
	return errors.Is(err, agent.ErrSummaryCoolingDown)
}

// contextDeadlineError builds the error an upstream timeout produces, the way
// the http client builds it: the deadline travels inside a url error, which
// wraps the sentinel.
func contextDeadlineError(message string) error {
	return &timeoutError{message: message}
}

// timeoutError is a deadline error that keeps the sentinel, so the product's
// own rule can see it.
type timeoutError struct{ message string }

func (e *timeoutError) Error() string { return e.message }

func (e *timeoutError) Unwrap() error { return context.DeadlineExceeded }
