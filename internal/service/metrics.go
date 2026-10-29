package service

import (
	"context"
	"errors"

	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/modelcall"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// The names below are measurement vocabulary. They are chosen by this package and
// never derived from user text, so a question that mentions "summary" cannot move
// its own tokens into the summary phase.

// Operations that get a task record. One exported user action is one task: a
// single message turn is not the same task as the learning session it happens to
// sit in, and neither is the whole conversation.
const (
	taskOperationMessageSend        = "message_send"
	taskOperationMessageStream      = "message_stream"
	taskOperationMessageAgentStream = "message_agent_stream"
	taskOperationQuizStart          = "quiz_start"
	taskOperationQuizAnswer         = "quiz_answer"
)

// Failure stages name where an operation stopped. They are a closed set so a
// dashboard cannot be poisoned by free text.
const (
	stageValidate        = "validate"
	stageAuthorize       = "authorize"
	stageLock            = "acquire_lock"
	stageOpenTurn        = "open_turn"
	stageSummary         = "summary"
	stageModel           = "model"
	stageConfirmBlock    = "confirm_block"
	stageSave            = "save"
	stageDeliver         = "deliver"
	stageReserveQuiz     = "reserve_quiz"
	stageEvidence        = "select_evidence"
	stageGeneration      = "generate_questions"
	stageStoreQuestions  = "store_questions"
	stageClaimAttempt    = "claim_attempt"
	stageGrading         = "grade_answer"
	stageCompleteAttempt = "complete_attempt"
	stageView            = "build_view"
)

// Reason codes classify the cause without carrying the cause's text. A provider
// error message can quote the prompt back, and a prompt has no business in a log
// line about measurement.
const (
	reasonInvalidRequest      = "invalid_request"
	reasonUnauthorized        = "unauthorized"
	reasonNotFound            = "not_found"
	reasonConflict            = "conflict"
	reasonBusy                = "busy"
	reasonClosed              = "closed"
	reasonNoEvidence          = "no_evidence"
	reasonGenerationRejected  = "generation_rejected"
	reasonGradingRejected     = "grading_rejected"
	reasonModelNotConfigured  = "model_not_configured"
	reasonCancelled           = "cancelled"
	reasonTimeout             = "timeout"
	reasonProviderStatus      = "provider_status"
	reasonProviderUnavailable = "provider_unavailable"
	reasonMalformedResponse   = "malformed_response"
	reasonEmptyCompletion     = "empty_completion"
	reasonStoreFailure        = "store_failure"
	reasonDeliveryFailure     = "delivery_failure"
	reasonBlockDisagrees      = "answer_block_disagrees"
	// reasonStoreInvalidArgument is this service breaking a store invariant: not a
	// learner's bad input and not a state conflict. Naming it either would send the
	// next reader to the wrong place.
	reasonStoreInvalidArgument = "store_invalid_argument"
	reasonInternal             = "internal"
	reasonOther                = "other"
)

// reasonFor maps an error onto one controlled code. It reads the sentinels the
// service already returns; it never stores the message.
//
// The provider and transport causes are matched before the learning workflow's own
// wrappers, because those wrappers quote them: a generation whose HTTP call failed
// with a 503 is wrapped as ErrLearningGeneration around a StatusError, and reading
// that as "the model wrote an unusable question set" would point the next operator
// at the prompt instead of at the outage.
func reasonFor(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return reasonCancelled
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, model.ErrTimeout):
		return reasonTimeout
	case errors.Is(err, model.ErrProviderStatus):
		return reasonProviderStatus
	case errors.Is(err, model.ErrUnavailable):
		return reasonProviderUnavailable
	case errors.Is(err, model.ErrMalformedResponse):
		return reasonMalformedResponse
	case errors.Is(err, model.ErrEmptyCompletion):
		return reasonEmptyCompletion
	case errors.Is(err, model.ErrInvalidRequest), errors.Is(err, model.ErrToolsUnsupported),
		errors.Is(err, model.ErrNotConfigured):
		return reasonInvalidRequest
	case errors.Is(err, ErrAnswerBlockDisagrees):
		return reasonBlockDisagrees
	case errors.Is(err, ErrInvalidRequest), errors.Is(err, ErrLearningSessionMode):
		return reasonInvalidRequest
	case errors.Is(err, ErrUnauthorized):
		return reasonUnauthorized
	case errors.Is(err, ErrNotFound), errors.Is(err, repository.ErrNotFound):
		// A missing row is its own fact. Reading it as a conflict would send the
		// next reader hunting for a lost update instead of a record that was
		// deleted or never written.
		return reasonNotFound
	case errors.Is(err, ErrConflict), errors.Is(err, repository.ErrConflict):
		// Only a state conflict is a conflict: the transition's precondition did
		// not hold against the row as it stands now.
		return reasonConflict
	case errors.Is(err, repository.ErrInvalidArgument):
		// The store refused the call itself - an invariant this service broke, not
		// a learner's input. It keeps its own code rather than borrowing
		// invalid_request or conflict.
		return reasonStoreInvalidArgument
	case errors.Is(err, ErrLearningSessionActive), errors.Is(err, ErrLearningBusy),
		errors.Is(err, ErrLearningSubmissionConflict):
		return reasonBusy
	case errors.Is(err, ErrLearningClosed), errors.Is(err, ErrLearningQuestionOrder):
		return reasonClosed
	case errors.Is(err, ErrLearningNoEvidence):
		return reasonNoEvidence
	// Only now the wrappers that also cover a rejected model output: with the
	// provider causes above already matched, what is left really is "the answer
	// arrived and this service refused it".
	case errors.Is(err, ErrLearningGeneration):
		return reasonGenerationRejected
	case errors.Is(err, ErrLearningGrading), errors.Is(err, ErrLearningInvalidGrade):
		return reasonGradingRejected
	case errors.Is(err, ErrLearningModelNotConfigured):
		return reasonModelNotConfigured
	case errors.Is(err, ErrInternal):
		return reasonInternal
	}
	return reasonOther
}

// outcomeFor picks the record's outcome from the error the caller is about to
// receive. It reuses the call taxonomy in modelcall so a message and a log line
// cannot disagree about what happened.
func outcomeFor(err error) modelcall.Outcome {
	switch reason := reasonFor(err); reason {
	case "":
		return modelcall.OutcomeOK
	case reasonCancelled:
		return modelcall.OutcomeCancelled
	case reasonTimeout:
		return modelcall.OutcomeTimeout
	case reasonInvalidRequest, reasonUnauthorized:
		return modelcall.OutcomeRefused
	case reasonProviderStatus, reasonProviderUnavailable:
		return modelcall.OutcomeProviderError
	case reasonMalformedResponse, reasonGenerationRejected, reasonGradingRejected:
		return modelcall.OutcomeMalformed
	case reasonEmptyCompletion:
		return modelcall.OutcomeEmpty
	case reasonDeliveryFailure:
		return modelcall.OutcomeCallbackFailed
	}
	return modelcall.OutcomeOther
}

// seal finishes one task record. It is called from a deferred closure in each
// operation, so every exit path - success, refusal, cancellation, a write that
// could not be delivered - leaves exactly one line.
// noteFailure records what actually failed, before the caller maps it into the
// error its client sees. It is the only way the record can say "store fault"
// while the HTTP layer still says "internal error": both would arrive at the
// deferred seal as ErrInternal.
func noteFailure(task *modelcall.TaskHandle, cause error, stage string) {
	if task == nil {
		return
	}
	task.NoteFailure(modelcall.Failure{
		Outcome: outcomeFor(cause),
		Stage:   stage,
		Reason:  reasonFor(cause),
	})
}

// noteDeliveryFailure records a frame that could not leave the process. The cause
// of a failed write is the write itself, so an unclassifiable error becomes
// delivery_failure rather than "other"; a client that hung up or outlived its
// deadline keeps that classification, which is the difference an operator reads.
func noteDeliveryFailure(task *modelcall.TaskHandle, cause error) {
	if task == nil {
		return
	}
	switch {
	case errors.Is(cause, context.Canceled):
		task.NoteFailure(modelcall.Failure{
			Outcome: modelcall.OutcomeCancelled, Stage: stageDeliver, Reason: reasonCancelled,
		})
	case errors.Is(cause, context.DeadlineExceeded), errors.Is(cause, model.ErrTimeout):
		task.NoteFailure(modelcall.Failure{
			Outcome: modelcall.OutcomeTimeout, Stage: stageDeliver, Reason: reasonTimeout,
		})
	default:
		task.NoteFailure(modelcall.Failure{
			Outcome: modelcall.OutcomeCallbackFailed, Stage: stageDeliver, Reason: reasonDeliveryFailure,
		})
	}
}

// noteMapped notes the store's own error and then returns the mapped one, so a
// reader of the record sees "conflict", "not found" or "unavailable" rather than
// whatever single service-level sentinel the HTTP layer was already going to see.
func noteMapped(task *modelcall.TaskHandle, cause error, stage string, mapErr func(error) error) error {
	noteFailure(task, cause, stage)
	return mapErr(cause)
}

func seal(task *modelcall.TaskHandle, err error, stage string) {
	if task == nil {
		return
	}
	if err == nil {
		// A stage is an answer to "where did it stop". On a turn that did not stop
		// there is no such question, so the field stays empty rather than reporting
		// the last phase the operation happened to pass through.
		task.Seal(modelcall.OutcomeOK, "", "")
		return
	}
	outcome := outcomeFor(err)
	if outcome == modelcall.OutcomeOther && task.DeliveryFailed() {
		// The stream contract survives the mapping this service already does: a turn
		// that ended because a frame could not leave the process arrives here as
		// ErrInternal, and reading that as "some internal fault" would erase the one
		// fact an operator needs - the learner stopped listening.
		task.Seal(modelcall.OutcomeCallbackFailed, stageDeliver, reasonDeliveryFailure)
		return
	}
	task.Seal(outcome, stage, reasonFor(err))
}
