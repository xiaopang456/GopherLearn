package service

import (
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// Only constructor wiring differs from the frozen ordinary test suite.
func newFrozenOrdinaryMessageService(repo repository.ConversationStore, assistant responder.Responder, options ...MessageServiceOption) *MessageService {
	return NewMessageService(repo, assistant, append(options, WithFrozenOrdinarySummary())...)
}
