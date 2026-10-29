package repository

import (
	"context"
	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// ConversationStore is the persistence surface already used by MessageService.
// A full Repository continues to satisfy it; storage adapters need no unrelated methods.
type ConversationStore interface {
	GetSession(context.Context, string, string) (*domain.Session, error)
	ListMessages(context.Context, string, string) ([]domain.Message, error)
	AppendMessage(context.Context, string, string, *domain.Message) (*domain.Message, error)
	GetSessionSummary(context.Context, string, string) (*domain.SessionSummary, error)
	CommitSessionSummary(context.Context, *domain.SessionSummary, int64) (*domain.SessionSummary, error)
}
