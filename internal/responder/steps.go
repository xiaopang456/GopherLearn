package responder

import (
	"context"
	"errors"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// StepPhase names one moment of an agent turn's progress. The three started/result
// phases are the ones the source protocol actually puts on the wire; there is
// deliberately no phase that closes a step, because the protocol this migrates has
// none: a client learns a step ended from the next step starting, or from that step's
// own result frame.
//
// The three text phases are this server's own addition, and they close a step in the one
// place a client genuinely cannot infer it: which kind of text just finished. They ride on
// the same channel as the steps above because order between the two is the fact a client is
// reading - a process note has to arrive before the tool call it led to, and a candidate
// answer before the completion that confirms it - and one ordered callback is what keeps
// that true without a client-side guess.
type StepPhase string

const (
	// StepStarted opens one step. StepType says which kind.
	StepStarted StepPhase = "step_started"
	// ToolStarted says one tool is about to run, and carries the same sequence as the
	// step that opened it.
	ToolStarted StepPhase = "tool_started"
	// ToolResult reports what one executed call did. It never rides on a call that
	// failed: a failed call produced no result, and a frame claiming otherwise would be
	// the progress channel's version of a fabricated citation.
	ToolResult StepPhase = "tool_result"
	// WaitingConfirmation says a memory draft from this turn is waiting for the user's
	// decision. It is emitted only when a draft really exists.
	WaitingConfirmation StepPhase = "waiting_confirmation"
	// TextStarted opens one model call's public text block. It is sent with the first
	// fragment of that call, never before it, so a round that says nothing publicly
	// produces no empty block.
	TextStarted StepPhase = "agent.text.started"
	// TextDelta carries one fragment of a block, as the provider produced it.
	TextDelta StepPhase = "agent.text.delta"
	// TextEnded closes one block and names what it turned out to be. It is the frame that
	// makes an announced block finishable, and it is sent for every block that started.
	TextEnded StepPhase = "agent.text.ended"
)

// TextKind is the closed vocabulary a finished text block is classified as. There are exactly
// three because there are exactly three things a model call's public text can turn out to be,
// and the difference between them is the difference between a note, an answer and a failure:
//
//   - process: the call asked for a tool. Its text stays visible as what the model said while
//     it decided, and it is never the turn's answer.
//   - answer_candidate: the call finished without asking for anything. This is the text the
//     turn intends to store - but it is a candidate until the answer passes validation and is
//     written, which is what message.completed and its final_block_id are for.
//   - failed: the stream broke inside this call. The fragments already shown stay shown, and
//     this says plainly that they are not a finished answer.
type TextKind = string

const (
	TextKindProcess   TextKind = "process"
	TextKindCandidate TextKind = "answer_candidate"
	TextKindFailed    TextKind = "failed"
)

// TextBlockKindIsValid is the closed set the wire is allowed to carry. A client that receives
// anything else discards it rather than guessing which of the three it should have been:
// guessing is how a failed block becomes a displayed answer.
func TextBlockKindIsValid(kind string) bool {
	switch kind {
	case TextKindProcess, TextKindCandidate, TextKindFailed:
		return true
	default:
		return false
	}
}

// StepType is the source protocol's two-value vocabulary: a step is either a model
// decision or a tool call. It is not a claim that a plan document exists - the source
// project calls the model's turn "plan", and migrating a name is cheaper than
// migrating a disagreement about what the frame meant.
const (
	StepTypePlan = "plan"
	StepTypeTool = "tool"
)

// MemoryDraft is the bounded projection of a pending memory row that this turn created,
// as a progress frame may carry it.
//
// It is its own type rather than a *domain.Memory so that widening the stored record can
// never widen what a frame holds: owner, session, source messages, confidence and
// timestamps are not fields here at all, which is the only reason they cannot leak. What
// is here is the minimum a learner needs in order to decide - the id the confirmation
// endpoint addresses, and the sentence being decided on.
type MemoryDraft struct {
	DraftID  string
	Scope    domain.MemoryScope
	TopicKey string
	Content  string
}

// Step is one progress fact about an agent turn, as the strategy reports it and the
// service decides whether to put it on the wire.
//
// A step carries no stored content and no identifier the user has to guard: Content on a
// result frame is a count-sized sentence a tool wrote about itself, and the identifiers a
// step would name (source id, message id, session id, the arguments a tool was handed) stay
// on the server side. The two exceptions are deliberate and named. A waiting_confirmation
// step carries the id of a draft this turn stored, because without an address the decision it
// asks for cannot be sent back. A text frame carries the model's own public text, because
// that is what the learner is waiting to read - and it is only ever the text the provider
// sent as answer content: this server's provider adapter parses no reasoning field, and tool
// arguments and raw tool results are not representable on these fields at all.
//
// BlockID and Round belong to the text frames alone. The id correlates a block's fragments
// with each other and with message.completed's final_block_id; it is a server-generated
// handle, not something a page displays. Round is the model call the block came from, which
// is what lets a client fold "what the model said while deciding" away without knowing
// anything about what it said.
type Step struct {
	Phase    StepPhase
	RunID    string
	Sequence int
	StepType string
	ToolName string
	Content  string
	Message  string
	// DurationMillis is how long one tool call ran. Zero means the tool did not finish,
	// which is also why no result frame is sent for it.
	DurationMillis int64
	// Draft is set on a waiting_confirmation step alone, and only when this turn really
	// stored the row it projects.
	Draft *MemoryDraft
	// BlockID is the text frame's correlation handle: one block per model call, so a call
	// that narrated and then asked for a tool has its text in exactly one place.
	BlockID string
	// Round is the one-based model call that produced a text block. It is a fact about the
	// loop's order, not a claim about what the text means.
	Round int
	// Kind is set on a text block's ending frame alone, from the closed TextKind set.
	Kind TextKind
}

// StepSink receives one step. Returning an error stops the turn: a client that has
// hung up must not be billed for further model calls, and a step the loop kept going
// to produce would be progress nobody is watching.
type StepSink func(Step) error

// StepStreamer is implemented by the strategies that can report progress while they
// work. It is a separate face rather than a parameter on Stream, for the reason the
// package already uses for StreamingGenerator: the two chat strategies have no steps to
// report, and making them carry a nil callback would put a fake capability in front of
// the one path whose bytes must not move.
//
// A strategy that reports text blocks on the step channel does not also call emit for the
// same text: a learner's screen is fed by one source per turn, and a frame that arrives
// twice through two channels is a turn that can show the same sentence twice.
type StepStreamer interface {
	StreamSteps(ctx context.Context, request Request, emit func(string) error, onStep StepSink) (Answer, error)
}

// Termination is implemented by an error that can say why an agent turn stopped. The
// strategy raises these, and the service only relays them: the vocabulary belongs to the
// loop that decided it, and a service that re-derived it would need to know which
// sentinel means what.
//
// The reason is this server's own fact - a name it chose - and never text from a tool, a
// provider or the model, so naming it leaks nothing.
type Termination interface {
	error
	TerminationReason() string
	TerminationMessage() string
}

// DescribeTermination reports what the strategy said about why the turn ended. ok is
// false when nothing in the chain could say, which is the same answer a client got before
// step events existed.
func DescribeTermination(err error) (reason, message string, ok bool) {
	var termination Termination
	if err == nil || !errors.As(err, &termination) {
		return "", "", false
	}
	return termination.TerminationReason(), termination.TerminationMessage(), true
}
