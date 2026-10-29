package responder

import (
	"context"
	"errors"
	"sort"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// ErrStrategyNotConfigured reports a resolved answer strategy with no installed
// implementation. It is a wiring fault, so the service surfaces it as an
// internal error rather than blaming the request.
var ErrStrategyNotConfigured = errors.New("responder: no strategy installed for this answer strategy")

// Strategy answers one turn for exactly one resolved answer strategy. Splitting
// the strategies apart keeps a direct completion from being forced through
// retrieval code it does not need.
type Strategy interface {
	Strategy() domain.AnswerStrategy
	Respond(context.Context, Request) (Answer, error)
	Stream(context.Context, Request, func(string) error) (Answer, error)
}

// StrategyResponder dispatches on responder.Request.AnswerStrategy, the value
// the service derived from the stored session mode plus this turn's knowledge
// options. Nothing here re-reads a client-supplied mode.
type StrategyResponder struct {
	strategies map[domain.AnswerStrategy]Strategy
}

// NewStrategyResponder installs strategies and rejects gaps or duplicates: a
// deployment that registers only knowledge_qa would silently fail Chat.
func NewStrategyResponder(strategies ...Strategy) (*StrategyResponder, error) {
	index := make(map[domain.AnswerStrategy]Strategy, len(strategies))
	for _, strategy := range strategies {
		if strategy == nil {
			return nil, errors.New("responder: nil strategy")
		}
		kind := strategy.Strategy()
		if !kind.IsValid() {
			return nil, errors.New("responder: strategy reports an unknown answer strategy")
		}
		if _, exists := index[kind]; exists {
			return nil, errors.New("responder: duplicate strategy for " + string(kind))
		}
		index[kind] = strategy
	}
	return &StrategyResponder{strategies: index}, nil
}

var _ Responder = (*StrategyResponder)(nil)

// Strategies reports the installed strategies, for startup logs and tests.
func (r *StrategyResponder) Strategies() []domain.AnswerStrategy {
	names := make([]domain.AnswerStrategy, 0, len(r.strategies))
	for name := range r.strategies {
		names = append(names, name)
	}
	sort.Slice(names, func(left, right int) bool { return names[left] < names[right] })
	return names
}

func (r *StrategyResponder) strategy(request Request) (Strategy, error) {
	strategy, exists := r.strategies[request.AnswerStrategy]
	if !exists {
		return nil, ErrStrategyNotConfigured
	}
	return strategy, nil
}

func (r *StrategyResponder) Respond(ctx context.Context, request Request) (Answer, error) {
	strategy, err := r.strategy(request)
	if err != nil {
		return Answer{}, err
	}
	return strategy.Respond(ctx, request)
}

func (r *StrategyResponder) Stream(ctx context.Context, request Request, emit func(string) error) (Answer, error) {
	strategy, err := r.strategy(request)
	if err != nil {
		return Answer{}, err
	}
	return strategy.Stream(ctx, request, emit)
}

// StreamSteps reports progress while the turn runs, for the one strategy that has
// steps to report. A strategy without a StepSink cannot produce any, and is answered
// through its ordinary Stream: the two chat strategies' bytes are then produced by the
// same code they always used, rather than by a branch that happens to pass nil.
func (r *StrategyResponder) StreamSteps(ctx context.Context, request Request, emit func(string) error,
	onStep StepSink,
) (Answer, error) {
	strategy, err := r.strategy(request)
	if err != nil {
		return Answer{}, err
	}
	if streamer, ok := strategy.(StepStreamer); ok {
		return streamer.StreamSteps(ctx, request, emit, onStep)
	}
	return strategy.Stream(ctx, request, emit)
}

// SupportsSteps reports whether one resolved strategy can report progress at all. It
// exists so the HTTP layer can refuse a request for step events on a turn that has no
// steps to show, instead of accepting the flag and quietly doing nothing with it.
func (r *StrategyResponder) SupportsSteps(request Request) bool {
	strategy, err := r.strategy(request)
	if err != nil {
		return false
	}
	// Asked first, because a strategy may be able to report steps for some turns and not
	// others, and a wrapper around one may have the methods without the intention.
	// A type assertion alone would credit both with a capability they do not have.
	if reporter, ok := strategy.(interface{ SupportsSteps(Request) bool }); ok {
		return reporter.SupportsSteps(request)
	}
	_, ok := strategy.(StepStreamer)
	return ok
}

// EchoStrategy answers by echoing the caller's text, with no retrieval and no
// citations. It exists so a deployment can run end to end before a model is
// wired in: installing it for the agent strategy is a statement that the agent
// path is not implemented yet, not a claim that it ran.
type EchoStrategy struct {
	kind domain.AnswerStrategy
	body Responder
}

// NewEchoStrategy binds the echo behaviour to one answer strategy.
func NewEchoStrategy(kind domain.AnswerStrategy) (*EchoStrategy, error) {
	if !kind.IsValid() {
		return nil, errors.New("responder: echo strategy needs a known answer strategy")
	}
	return &EchoStrategy{kind: kind, body: EchoResponder{}}, nil
}

// Strategy implements responder.Strategy.
func (s *EchoStrategy) Strategy() domain.AnswerStrategy { return s.kind }

func (s *EchoStrategy) Respond(ctx context.Context, request Request) (Answer, error) {
	return s.body.Respond(ctx, request)
}

func (s *EchoStrategy) Stream(ctx context.Context, request Request, emit func(string) error) (Answer, error) {
	return s.body.Stream(ctx, request, emit)
}

var _ Strategy = (*EchoStrategy)(nil)
