package agent

import (
	"errors"
	"fmt"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/model"

	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// ErrStepDelivery reports that a step could not be handed to whoever was watching the
// turn. It is a distinct failure because it has a distinct meaning downstream: the loop
// stopped because the audience left, not because the model, a tool or a budget said so,
// and reporting it as any of those would misfile a cancelled turn as a broken one.
var ErrStepDelivery = errors.New("agent: an agent step could not be delivered")

// stepReporter numbers one turn's steps and puts them on the sink.
//
// The counter is shared by both kinds of step, so the numbers a client sees are dense
// and increasing across model calls and tool calls together - which is the only reason
// a client can tell "the tool step with sequence 4" from "the model call that asked for
// it". A reporter with no sink is a plain turn: numbering still advances, nothing is
// sent, and no code path differs.
type stepReporter struct {
	sink     responder.StepSink
	sequence int
}

func (s *stepReporter) sending() bool { return s != nil && s.sink != nil }

func (s *stepReporter) send(step responder.Step) error {
	if !s.sending() {
		return nil
	}
	if err := s.sink(step); err != nil {
		return fmt.Errorf("%w: %w", ErrStepDelivery, err)
	}
	return nil
}

// modelCall opens a plan step: the loop is about to spend one model call.
func (s *stepReporter) modelCall(call int) error {
	if !s.sending() {
		return nil
	}
	s.sequence++
	return s.send(responder.Step{
		Phase:    responder.StepStarted,
		Sequence: s.sequence,
		StepType: responder.StepTypePlan,
		Message:  fmt.Sprintf("model generation %d", call),
	})
}

// toolCall opens a tool step and names the capability about to run, on one shared
// sequence: the step and the call are the same event seen from two sides.
func (s *stepReporter) toolCall(call int, toolName string) (int, error) {
	if !s.sending() {
		return 0, nil
	}
	s.sequence++
	sequence := s.sequence
	if err := s.send(responder.Step{
		Phase:    responder.StepStarted,
		Sequence: sequence,
		StepType: responder.StepTypeTool,
		Message:  fmt.Sprintf("tool call %d", call),
	}); err != nil {
		return 0, err
	}
	// The source protocol names the tool on tool_started alone and gives it no step type:
	// step_started is what says which kind of step this is, and the client carries the
	// name onto that step from here.
	if err := s.send(responder.Step{
		Phase:    responder.ToolStarted,
		Sequence: sequence,
		ToolName: toolName,
	}); err != nil {
		return 0, err
	}
	return sequence, nil
}

// toolResult closes nothing; it reports what one finished call did. Content is the
// tool's own sentence about itself, which is the only summary this server is willing to
// show: the observation text is model input, and a step is not a place to quote it.
func (s *stepReporter) toolResult(sequence int, toolName string, observation Observation, elapsed time.Duration) error {
	if !s.sending() {
		return nil
	}
	if err := s.send(responder.Step{
		Phase:          responder.ToolResult,
		Sequence:       sequence,
		ToolName:       toolName,
		Content:        observation.Summary,
		DurationMillis: elapsed.Milliseconds(),
	}); err != nil {
		return err
	}
	if observation.Draft == nil {
		return nil
	}
	// Only a turn that really stored a draft says so, and it hands over the row it stored:
	// a card built by looking the pending draft up again could point at another
	// conversation's. The count is the server's own fact, and the four projected fields are
	// what a decision needs - the draft's owner, sources, confidence and timestamps stay
	// where the memory page shows them.
	return s.send(responder.Step{
		Phase:    responder.WaitingConfirmation,
		Sequence: sequence,
		Message:  "本回合生成了 1 条待确认记忆草稿，请在当前回复下方的确认卡片中处理。",
		Draft:    observation.Draft,
	})
}

// terminatedTurn is an agent failure that carries why it happened, in the vocabulary the
// step protocol publishes, plus the one sentence a learner may be shown.
//
// It exists so the loop's reason does not have to be re-derived upstream: the mapping is
// made where the sentinels are raised, and Unwrap keeps the original error intact for any
// caller that classifies by it instead.
type terminatedTurn struct {
	cause   error
	reason  string
	message string
}

func (e *terminatedTurn) Error() string { return e.cause.Error() }
func (e *terminatedTurn) Unwrap() error { return e.cause }

func (e *terminatedTurn) TerminationReason() string  { return e.reason }
func (e *terminatedTurn) TerminationMessage() string { return e.message }

// terminate names the reason a turn ended, for a client that is watching steps. The
// vocabulary is the source protocol's, and an unmapped failure keeps its own
// "unknown_origin" rather than being dressed up as something explained.
func terminate(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, model.ErrTruncatedStream):
		// The provider stopped mid-answer. Whatever the learner already read stays on
		// screen, and this is the reason that says so: the text is a prefix, not a
		// finished reply, and no sentence about it may claim otherwise.
		return &terminatedTurn{cause: err, reason: "incomplete_model_stream"}
	case errors.Is(err, ErrStepDelivery):
		// The audience left. This is the one reason that is not a fault at all, and
		// reporting it as one would turn a closed tab into a broken answer.
		return &terminatedTurn{cause: err, reason: "cancelled", message: "本轮已取消。"}
	case errors.Is(err, ErrModelCallBudget):
		return &terminatedTurn{cause: err, reason: "model_call_limit", message: "本轮已达到模型调用上限，回答未完成。"}
	case errors.Is(err, ErrToolCallBudget):
		return &terminatedTurn{cause: err, reason: "tool_call_limit", message: "本轮已达到工具调用上限，回答未完成。"}
	case errors.Is(err, ErrStepBudget):
		return &terminatedTurn{cause: err, reason: "graph_step_limit", message: "本轮已达到回合步数上限，回答未完成。"}
	case errors.Is(err, ErrDeadline):
		return &terminatedTurn{cause: err, reason: "deadline_exceeded", message: "本轮已超时，回答未完成。"}
	case errors.Is(err, ErrUnknownTool):
		// A model that asked for a capability nobody registered is this server's own
		// story, and no sentence about it is safe to show a learner.
		return &terminatedTurn{cause: err, reason: "unknown_tool"}
	case errors.Is(err, model.ErrEmptyCompletion):
		return &terminatedTurn{cause: err, reason: "empty_answer"}
	default:
		return &terminatedTurn{cause: err, reason: "unknown_origin"}
	}
}
