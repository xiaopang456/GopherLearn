package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Giving an error its type where it is produced.
//
// STABILIZE-062 v1.3 classified by errors.Is against the Runner's sentinels,
// which stopped the keyword guessing and was the right shape. What it could not
// do is say anything about a failure that never became one of those sentinels.
// A provider transport fault, a Redis or MySQL fault, a tool that failed while
// running, an answer the Runner rejected - all of them arrived at the top of the
// stack as "some other error", landed in one bucket, and that bucket alone was
// enough to void a whole round of twenty-four questions.
//
// The v1.3 run ended exactly there: three turns in that bucket, and the artefact
// could not say which of four very different things had happened. It could not
// say afterwards either, because the original error is never stored - and it
// should not be.
//
// So the top of the stack stops guessing. Each origin wraps its own cause in a
// typed envelope at the point it happens, the classifier reads the type, and
// the envelope keeps the cause reachable for a local reader without ever
// carrying it into an artefact.

// The origin envelopes. Each is wrapped at the place the failure occurs, never
// inferred later.
var (
	// ErrProviderTransport is a fault reaching or reading from the model
	// provider: connection, authentication, quota, or a response that is not a
	// usable protocol message.
	ErrProviderTransport = errors.New("provider transport failure")
	// ErrRetrievalInfrastructure is a fault in the stores retrieval depends on:
	// MySQL, Redis, the index, or a DAO beneath them.
	ErrRetrievalInfrastructure = errors.New("retrieval infrastructure failure")
	// ErrToolInfrastructure is a tool that failed while running for a reason
	// that is not the model's doing.
	ErrToolInfrastructure = errors.New("tool infrastructure failure")
	// ErrGroundingRejected is an answer the Runner refused: a citation that
	// resolves to nothing, an evidence kind that does not match, or grounding
	// the deterministic check could not confirm. It is the model failing the
	// question, not the machinery failing to ask it.
	ErrGroundingRejected = errors.New("answer failed grounding or citation validation")
)

// WrapProviderFailure marks a cause as a provider transport fault.
//
// The cause stays reachable through errors.Is and errors.Unwrap so a person
// debugging locally can still see it. It never reaches an artefact: what is
// recorded is the category this function assigns, and nothing else.
func WrapProviderFailure(cause error) error {
	return wrapOrigin(ErrProviderTransport, cause)
}

// WrapRetrievalInfrastructureFailure marks a cause as a store fault.
func WrapRetrievalInfrastructureFailure(cause error) error {
	return wrapOrigin(ErrRetrievalInfrastructure, cause)
}

// WrapToolInfrastructureFailure marks a cause as a tool execution fault.
func WrapToolInfrastructureFailure(cause error) error {
	return wrapOrigin(ErrToolInfrastructure, cause)
}

// WrapGroundingRejection marks a cause as the answer failing validation.
func WrapGroundingRejection(cause error) error {
	return wrapOrigin(ErrGroundingRejected, cause)
}

func wrapOrigin(envelope, cause error) error {
	if cause == nil {
		return envelope
	}
	return fmt.Errorf("%w: %w", envelope, cause)
}

// FailureOrigin says whose failure this is.
//
// It is the question the instrument gate actually needs answered, and it is not
// the same question as "what went wrong". A model hitting a fuse and a model
// citing evidence that does not exist are different failures with the same
// origin: the method. A Redis outage and a malformed provider response are
// different failures with the same origin: the instrument.
type FailureOrigin string

const (
	// OriginNone is a run that did not fail.
	OriginNone FailureOrigin = "none"
	// OriginInstrument means the machinery could not ask the question properly,
	// so the turn produced no usable observation.
	OriginInstrument FailureOrigin = "instrument"
	// OriginMethod means the question was asked and the model or the method
	// failed it. The observation is real and the turn should be scored.
	OriginMethod FailureOrigin = "method"
	// OriginUnknown means this failure has no type yet. It must stop a round,
	// and the report must say that its cause is not known rather than assign
	// one - which is the whole reason the old catch-all was removed.
	OriginUnknown FailureOrigin = "unknown"
)

// RunFailureKind is the structured cause of a failed run.
type RunFailureKind string

const (
	RunFailureNone RunFailureKind = "none"

	// Method origins: the envelope refused something, or the model failed.
	RunFailureToolCallLimit    RunFailureKind = "tool_call_limit"
	RunFailureModelCallLimit   RunFailureKind = "model_call_limit"
	RunFailureGraphStepLimit   RunFailureKind = "graph_step_limit"
	RunFailureRepeatedToolCall RunFailureKind = "repeated_tool_call"
	// RunFailureEmptyAnswer is the Runner rejecting an empty answer. It was an
	// existing sentinel that no classifier covered, so it fell into the old
	// catch-all - found while investigating why v1.3's bucket was unreadable.
	RunFailureEmptyAnswer RunFailureKind = "empty_answer"
	// RunFailureGroundingRejected is a citation, reference, evidence kind or
	// grounding check the answer did not pass.
	RunFailureGroundingRejected RunFailureKind = "grounding_rejected"
	// RunFailureTurnDeadline is a turn that ran out of its own time. Whether
	// that is the method's failure or the instrument's depends on when it
	// happened, which the caller knows and this package does not, so the stage
	// decides it rather than this kind.
	RunFailureTurnDeadline RunFailureKind = "deadline_exceeded"

	// Instrument origins: the machinery could not ask the question.
	RunFailureProviderTransport       RunFailureKind = "provider_transport"
	RunFailureRetrievalInfrastructure RunFailureKind = "retrieval_infrastructure"
	RunFailureToolInfrastructure      RunFailureKind = "tool_infrastructure"
	RunFailureIncompleteModelStream   RunFailureKind = "incomplete_model_stream"
	RunFailureCancelled               RunFailureKind = "cancelled"

	// RunFailureUnknownTool is the model naming a tool that was never
	// registered. Whose failure that is depends on whether the tool table was
	// complete, which this package cannot see - so its origin is unknown here
	// and the caller resolves it with the table it actually built.
	RunFailureUnknownTool RunFailureKind = "unknown_tool"

	// RunFailureUnknown is an error nobody has typed. There is deliberately no
	// friendlier name: the old catch-all could quietly absorb four unrelated
	// faults, and this one is meant to be visible and to stop a round.
	RunFailureUnknown RunFailureKind = "unknown_origin"
)

// AllRunFailureKinds lists the closed vocabulary, for freezes and reports.
func AllRunFailureKinds() []RunFailureKind {
	kinds := []RunFailureKind{
		RunFailureNone, RunFailureToolCallLimit, RunFailureModelCallLimit,
		RunFailureGraphStepLimit, RunFailureRepeatedToolCall, RunFailureEmptyAnswer,
		RunFailureGroundingRejected, RunFailureTurnDeadline,
		RunFailureProviderTransport, RunFailureRetrievalInfrastructure,
		RunFailureToolInfrastructure, RunFailureIncompleteModelStream,
		RunFailureCancelled, RunFailureUnknownTool, RunFailureUnknown,
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds
}

// Origin says whose failure this kind is.
//
// Two kinds deliberately answer OriginUnknown here rather than guessing:
// an unknown tool, whose origin depends on whether the tool table was complete,
// and an untyped error. Both are resolved by the caller, which has the context
// this package does not.
func (k RunFailureKind) Origin() FailureOrigin {
	switch k {
	case RunFailureNone:
		return OriginNone
	case RunFailureToolCallLimit, RunFailureModelCallLimit, RunFailureGraphStepLimit,
		RunFailureRepeatedToolCall, RunFailureEmptyAnswer, RunFailureGroundingRejected:
		return OriginMethod
	case RunFailureProviderTransport, RunFailureRetrievalInfrastructure,
		RunFailureToolInfrastructure, RunFailureIncompleteModelStream,
		RunFailureCancelled:
		return OriginInstrument
	}
	// A deadline and an unknown tool both depend on where they happened.
	return OriginUnknown
}

// IsSafetyBoundary reports whether this kind is the production envelope
// refusing something rather than something going wrong.
func (k RunFailureKind) IsSafetyBoundary() bool {
	switch k {
	case RunFailureToolCallLimit, RunFailureModelCallLimit,
		RunFailureGraphStepLimit, RunFailureRepeatedToolCall:
		return true
	}
	return false
}

// TypeProviderFailure and TypeToolFailure are the idempotent forms used at the
// seams. They are what production actually calls.
//
// The rule they share is the reason they exist as separate functions from the
// Wrap ones above. An error that already carries an origin, or that is one of
// this package's own sentinels, or that is a context outcome, must be left
// exactly as it is: the first typing wins, because it happened closest to the
// failure. Re-typing on the way out is how a tool fault becomes a provider
// fault three frames later, which is the mistake this whole round is about.
func TypeProviderFailure(err error) error {
	if !needsOrigin(err) {
		return err
	}
	return WrapProviderFailure(err)
}

func TypeToolFailure(err error) error {
	if !needsOrigin(err) {
		return err
	}
	return WrapToolInfrastructureFailure(err)
}

func TypeRetrievalFailure(err error) error {
	if !needsOrigin(err) {
		return err
	}
	return WrapRetrievalInfrastructureFailure(err)
}

// FailureComponent says which component inside a tool failed.
//
// STABILIZE-062 v1.4.1 lost its round to two tool_infrastructure turns and the
// artefact could not say which component of the second Episode search had
// failed - the same turn runs an embedding purchase, an index search, a
// governed expansion and a render, and the single kind covered them all. The
// component is typed at the seam that produces the error, the first typing
// wins like the origins above, and what is recorded is the enum value and
// never the cause's text.
type FailureComponent string

const (
	// ComponentNone is an error with no component typing, or no error.
	ComponentNone FailureComponent = ""
	// ComponentEmbeddingProvider is the query-vector purchase: the request to
	// the embedding provider, typed where the embedder wrapper calls it.
	ComponentEmbeddingProvider FailureComponent = "embedding_provider"
	// ComponentRetrievalStore is the store side of retrieval: index search,
	// state reload, lifecycle filters.
	ComponentRetrievalStore FailureComponent = "retrieval_store"
	// ComponentGovernedExpansion is the governed assertion expansion that
	// replaces rendered cards with their slots.
	ComponentGovernedExpansion FailureComponent = "governed_expansion"
	// ComponentObservationRender is the observation a tool renders from its
	// result.
	ComponentObservationRender FailureComponent = "observation_render"
	// ComponentEvidenceRegistry is the evidence registry a tool registers
	// references with.
	ComponentEvidenceRegistry FailureComponent = "evidence_registry"
)

type componentEnvelope struct {
	component FailureComponent
}

func (e componentEnvelope) Error() string {
	return "failed tool component: " + string(e.component)
}

// WrapComponentFailure marks which component an error came from. The first
// component wins: a seam closer to the failure has already said where it was,
// and re-typing on the way out is how an embedding fault becomes a store fault.
func WrapComponentFailure(err error, component FailureComponent) error {
	if err == nil || component == ComponentNone {
		return err
	}
	if _, present := ComponentOf(err); present {
		return err
	}
	return fmt.Errorf("%w: %w", componentEnvelope{component}, err)
}

// ComponentOf extracts the component an error was typed with.
func ComponentOf(err error) (FailureComponent, bool) {
	var envelope componentEnvelope
	if errors.As(err, &envelope) {
		return envelope.component, true
	}
	return ComponentNone, false
}

// needsOrigin reports whether an error is still untyped.
func needsOrigin(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) {
		return false
	}
	for _, envelope := range []error{ErrProviderTransport, ErrRetrievalInfrastructure,
		ErrToolInfrastructure, ErrGroundingRejected} {
		if errors.Is(err, envelope) {
			return false
		}
	}
	for _, sentinel := range []error{ErrToolCallLimit, ErrModelCallLimit, ErrRepeatedToolCall,
		ErrUnknownTool, ErrGraphStepLimit, ErrEventDelivery, ErrEmptyAgentAnswer,
		ErrIncompleteModelStream} {
		if errors.Is(err, sentinel) {
			return false
		}
	}
	return true
}
