// Package conversation exposes the existing ordinary MessageService across
// module boundaries. It does not register Agent, knowledge or memory collaborators.
package conversation

import (
	"context"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/service"
	"github.com/xiaopang456/GopherLearn/internal/summary"
)

type Store = repository.ConversationStore
type Session = domain.Session
type Message = domain.Message
type SessionSummary = domain.SessionSummary
type Model = model.ChatModel
type Request = model.Request
type ModelMessage = model.Message
type Completion = model.Completion
type DeltaHandler = model.DeltaHandler
type Pair = service.MessagePair
type Event = service.StreamEvent
type Budget = service.ContextBudget
type SummaryPolicy = summary.Policy

const ChatMode = domain.SessionModeChat

var (
	ErrNotFound        = repository.ErrNotFound
	ErrConflict        = repository.ErrConflict
	ErrInvalidArgument = repository.ErrInvalidArgument
	ErrInvalidRequest  = service.ErrInvalidRequest
	ErrInternal        = service.ErrInternal
)

type Options struct {
	Budget         Budget
	SummaryPolicy  SummaryPolicy
	SummaryEnabled bool
}
type Service struct{ inner *service.MessageService }

func New(store Store, chat Model, options Options) (*Service, error) {
	strategy, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, chat, responder.DirectSystemPrompt)
	if err != nil {
		return nil, err
	}
	assistant, err := responder.NewStrategyResponder(strategy)
	if err != nil {
		return nil, err
	}
	opts := []service.MessageServiceOption{service.WithConversationBudget(options.Budget), service.WithFrozenOrdinarySummary()}
	if options.SummaryEnabled {
		summarizer, err := summary.NewOrdinaryModelSummarizer(chat, options.SummaryPolicy)
		if err != nil {
			return nil, err
		}
		opts = append(opts, service.WithRollingSummary(summarizer, options.SummaryPolicy))
	}
	return &Service{inner: service.NewMessageService(store, assistant, opts...)}, nil
}
func (s *Service) Send(ctx context.Context, userID, sessionID, content string) (Pair, error) {
	return s.inner.Send(ctx, userID, sessionID, content, domain.KnowledgeOptions{})
}
func (s *Service) Stream(ctx context.Context, userID, sessionID, content string, emit func(Event) error) error {
	return s.inner.Stream(ctx, userID, sessionID, content, domain.KnowledgeOptions{}, emit)
}
