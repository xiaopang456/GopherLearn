package conversation

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"

	"GopherAI/model"
	agent "GopherAI/service/agent"
)

// summaryFailureCause is the driver's own reading of a compaction failure:
// what caused it, and whose fault that is.
//
// The product's own reason (agent.SummaryFailureReasonOf) says which stage
// failed. This says whether the environment or the product did it, because
// the two are not the same finding. A timeout of the summary MODEL request is
// the upstream's; a timeout of the store is the database's; a summary the
// model returned that the product could not use is the product's.
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
	// summaryCauseUnknown means the evidence does not reach the cause: the
	// stage the failure happened at could not be read, so a timeout in it
	// cannot be attributed to the model request. It is reported as unknown
	// rather than folded into a cause it cannot prove: evidence that is short
	// of a conclusion is not a zero.
	summaryCauseUnknown summaryFailureCause = "unknown"
	// summaryCauseUpstreamTimeout means the summary MODEL request ran out of
	// time. Only a timeout whose stage is confirmed to be the model request
	// is the upstream's.
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

// summaryFailureStage is where a compaction failed.
//
// The stage is read before the cause, because it decides whose failure this
// is. A stage that is already identified is never overridden by a timeout
// wording later in the same message: a store read that ran out of time is the
// database's timeout, and a document that would not parse is the model's
// answer, whatever else the message says.
type summaryFailureStage string

const (
	// summaryStageUnknown means the stage could not be read from the failure.
	summaryStageUnknown summaryFailureStage = ""
	// summaryStageModelRequest is the summary model call itself.
	summaryStageModelRequest summaryFailureStage = "model_request"
	// summaryStageStorage is the store the summary is read from and written to.
	summaryStageStorage summaryFailureStage = "storage"
	// summaryStageDocument is the document stage: decoding, encoding and
	// validating what the model returned.
	summaryStageDocument summaryFailureStage = "document"
	// summaryStageBudget is the summarizer's own input or output budget.
	summaryStageBudget summaryFailureStage = "budget"
)

// The product's own wording for each stage. These are the strings the
// product wraps its own errors with (v2/service/agent/summarizer.go), and
// they are what tells one stage from another after the error chain is gone.
//
// The model request is the one that has to be CONFIRMED: a timeout somewhere
// else - the store, the transport, something the product does not wrap - is
// not the model's request, and saying it is would put a failure that is not
// the upstream's into the environment's column.
var (
	summaryStorageMarkers = []string{
		"read rolling summary",
		"read summary increment",
		"publish rolling summary",
		"reload concurrent rolling summary",
		"save rolling summary",
		"store rolling summary",
		"load rolling summary",
	}
	summaryDocumentMarkers = []string{
		"decode rolling summary",
		"decode previous rolling summary",
		"encoding rolling summary",
		"rolling summary must use schema version",
		"rolling summary created evidence without a message id",
		"rolling summary cited non-user or unavailable message",
		"rolling summary evidence:",
		"session summary evidence",
		"rolling summary item is too long",
		"rolling summary dependencies are required",
		"invalid session summary",
	}
	summaryModelRequestMarker = "generate rolling summary"
)

// summaryFailure is one compaction failure, read once.
type summaryFailure struct {
	// Reason is the product's own reason for it.
	Reason agent.SummaryFailureReason
	// Stage is where it happened.
	Stage summaryFailureStage
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

// summaryFailureStageOf reads the stage a failure happened at.
//
// The wording is read first, because the product wraps each stage's errors
// with its own words. The product's reason is consulted only for what the
// wording does not settle.
func summaryFailureStageOf(reason agent.SummaryFailureReason, text string) summaryFailureStage {
	lowered := strings.ToLower(text)
	switch {
	case containsAny(lowered, summaryStorageMarkers):
		return summaryStageStorage
	case containsAny(lowered, summaryDocumentMarkers):
		return summaryStageDocument
	case strings.Contains(lowered, "exceeds output token limit"),
		strings.Contains(lowered, "exceeds token limit"),
		strings.Contains(lowered, "exceeds summary input budget"):
		return summaryStageBudget
	case strings.Contains(lowered, summaryModelRequestMarker):
		return summaryStageModelRequest
	}
	// The wording did not settle it; the product's own reason may.
	switch reason {
	case agent.SummaryFailureParse, agent.SummaryFailureStructure:
		return summaryStageDocument
	case agent.SummaryFailureLength:
		return summaryStageBudget
	case agent.SummaryFailureStorage:
		return summaryStageStorage
	}
	return summaryStageUnknown
}

// containsAny reports whether the text carries one of the markers.
func containsAny(lowered string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// classifySummaryFailure reads one compaction failure.
//
// The stage is decided first, and it is what the cause follows from:
//
//   - the store: the product could not read or write the summary. A store
//     that ran out of time is the DATABASE's timeout, not the model's.
//   - the document: the model's answer could not be read, encoded or
//     validated. Whatever the message mentions, this is the product's.
//   - the budget: the summarizer's own input or output cap. The product's.
//   - the model request: the summary model call. Only a timeout HERE is the
//     upstream's.
//   - unknown: the stage could not be read. A timeout in it is not confirmed
//     to be the model's request, so it is 待核, not the environment's.
//
// The product's rule (agent.SummaryFailureReasonOf) is asked for the reason
// it reports, and the message is consulted for the timeout itself: a client
// that wraps an error into a string loses the sentinel, and then the
// product's rule can only say "a model call did not answer", which would
// hide the difference between an upstream that timed out and a model that
// answered badly.
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
	// The cooldown is the failure BEFORE this one, and a skipped stage is not
	// a failure at all: neither is a cause.
	if reason == agent.SummaryFailureCooldown || reason == agent.SummaryFailureSkipped {
		return summaryFailure{Reason: reason, Stage: summaryStageUnknown, Text: trimmed}
	}
	stage := summaryFailureStageOf(reason, text)
	timedOut := reason == agent.SummaryFailureTimeout || looksLikeTimeout(text)
	switch stage {
	case summaryStageStorage:
		return summaryFailure{Reason: reason, Stage: stage, Cause: summaryCauseStorage, Text: trimmed}
	case summaryStageDocument:
		return summaryFailure{Reason: reason, Stage: stage, Cause: summaryCauseFormatError, Text: trimmed}
	case summaryStageBudget:
		if strings.Contains(text, "exceeds output token limit") {
			return summaryFailure{Reason: reason, Stage: stage, Cause: summaryCauseOutputOverLimit, Text: trimmed}
		}
		return summaryFailure{Reason: reason, Stage: stage, Cause: summaryCauseProductOther, Text: trimmed}
	case summaryStageModelRequest:
		if timedOut {
			return summaryFailure{Reason: reason, Stage: stage, Cause: summaryCauseUpstreamTimeout, Text: trimmed}
		}
		return summaryFailure{Reason: reason, Stage: stage, Cause: summaryCauseProductOther, Text: trimmed}
	default:
		// The stage could not be read. A timeout here is not confirmed to be
		// the model's request: it might have been the store, the transport,
		// or something the product does not wrap at all. The honest reading
		// is 待核, and it is not the environment's.
		if timedOut {
			return summaryFailure{Reason: reason, Stage: stage, Cause: summaryCauseUnknown, Text: trimmed}
		}
		return summaryFailure{Reason: reason, Stage: stage, Cause: summaryCauseProductOther, Text: trimmed}
	}
}

// looksLikeTimeout is the text fallback for a timeout whose error chain was
// flattened on the way up.
//
// The product's rule reads the chain, and the chain survives only while
// every wrapper uses %w. A client that turns an error into a string keeps
// the wording and loses the sentinel, so the wording is what is left to
// read. This is a fallback, not a rule of its own: it is asked after the
// stage, so a body that merely mentions a timeout in passing does not
// override a stage that was already identified.
func looksLikeTimeout(text string) bool {
	lowered := strings.ToLower(text)
	return strings.Contains(lowered, "context deadline exceeded") ||
		strings.Contains(lowered, "deadline exceeded") ||
		strings.Contains(lowered, "client.timeout") ||
		strings.Contains(lowered, "i/o timeout") ||
		strings.Contains(lowered, "timed out")
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
// A cause the evidence does not reach - a cooldown whose failure behind it
// was not observed, or a timeout whose stage could not be read - is neither:
// it is 待核, and it stays in the statistics.
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
	case summaryCauseUnknown:
		outcome.Unknown = "the stage of the first failure could not be read from the evidence (" +
			failure.Text + "), so it is not confirmed to be the summary model request"
	case summaryCauseUpstreamTimeout:
		if summaryTokens == 0 {
			outcome.EnvironmentFailure = "the rolling summary compaction timed out at the model request (" +
				failure.Text + "); the window carried no summary"
		} else {
			outcome.WriteFailure = "the rolling summary compaction timed out at the model request (" +
				failure.Text + ") but the window carried a summary, so the answer stands"
		}
	default:
		// The product's own writer failed. The result stands and is listed
		// apart: this is a product failure, not the environment's.
		outcome.WriteFailure = "the rolling summary compaction failed (" + string(failure.Reason) +
			", " + string(failure.Stage) + "): " + failure.Text
	}
	return outcome
}

// countingSummaryRepository counts the summaries that were actually written.
//
// The provider's own answer is not proof of a write: SessionSummary returns
// no error when the compaction was never triggered, when the stored summary
// was simply read, and when the same snapshot is read again. Only a summary
// that reached the store is a write, so the count is taken here.
type countingSummaryRepository struct {
	inner  agent.SummaryRepository
	writes atomic.Int64
}

func (r *countingSummaryRepository) GetSummary(ctx context.Context, userID int64, sessionID string) (*model.SessionSummary, error) {
	return r.inner.GetSummary(ctx, userID, sessionID)
}

func (r *countingSummaryRepository) SaveSummary(ctx context.Context, summary *model.SessionSummary, expectedVersion uint64) error {
	if err := r.inner.SaveSummary(ctx, summary, expectedVersion); err != nil {
		return err
	}
	r.writes.Add(1)
	return nil
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
