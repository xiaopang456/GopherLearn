package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"GopherAI/service/agent/topicconsent"
)

// ErrTopicSuggestionsUnavailable means this deployment does not run the
// user-confirmed mode, so there is nothing to suggest and no ticket to sign.
//
// It is a distinct error rather than an empty list because the two mean
// opposite things to a client: an empty list says "no topic of yours looks
// related to this question", and this says "this server never asks". A UI that
// could not tell them apart would render an empty confirmation strip on every
// deployment that has the feature switched off.
var ErrTopicSuggestionsUnavailable = errors.New("topic suggestions are not enabled")

// TopicSuggestionService is the read-side confirmation's first half: propose,
// sign, and hand back. It reads no personal state and never will - the
// suggester it holds cannot reach mastery or notes, by the shape of its types.
type TopicSuggestionService struct {
	suggester *topicconsent.Suggester
	signer    *topicconsent.Signer
}

// TopicSuggestion is the response one question gets.
type TopicSuggestion struct {
	Candidates []topicconsent.Candidate `json:"candidates"`
	// Ticket is empty when there is nothing to confirm. A client that receives
	// no candidates has nothing to send back, and issuing a ticket for an empty
	// list would be a signature over a permission to read nothing.
	Ticket string `json:"ticket,omitempty"`
	// ExpiresInSeconds lets the browser stop offering a stale choice rather
	// than discovering on submit that the ticket died.
	ExpiresInSeconds int `json:"expiresInSeconds,omitempty"`
	// MaxSelectable is served rather than hardcoded in the client so the two
	// cannot disagree about the ceiling the server actually enforces.
	MaxSelectable int `json:"maxSelectable"`
}

// TopicCataloguePage is an explicit fallback, not another suggestion model.
// It lists only names from the authenticated learner's current catalogue and
// carries no ticket: browsing alone never authorises a personal read.
type TopicCataloguePage struct {
	Candidates []topicconsent.Candidate `json:"candidates"`
	NextOffset int                      `json:"nextOffset"`
	HasMore    bool                     `json:"hasMore"`
	PageSize   int                      `json:"pageSize"`
}

func NewTopicSuggestionService(suggester *topicconsent.Suggester, signer *topicconsent.Signer) (*TopicSuggestionService, error) {
	if suggester == nil || signer == nil {
		return nil, fmt.Errorf("topic suggestion service requires a suggester and a signer")
	}
	return &TopicSuggestionService{suggester: suggester, signer: signer}, nil
}

// Suggest proposes candidates and signs a ticket over exactly those candidates.
func (s *TopicSuggestionService) Suggest(ctx context.Context, userID int64, sessionID, requestID,
	question string) (TopicSuggestion, error) {
	if s == nil {
		return TopicSuggestion{}, ErrTopicSuggestionsUnavailable
	}
	candidates, err := s.suggester.Suggest(ctx, userID, question)
	if err != nil {
		return TopicSuggestion{}, err
	}
	out := TopicSuggestion{Candidates: candidates, MaxSelectable: topicconsent.MaxSelectableTopics}
	if len(candidates) == 0 {
		return out, nil
	}
	ticket, claims, err := s.signer.Issue(userID, sessionID, requestID, question, candidates)
	if err != nil {
		return TopicSuggestion{}, err
	}
	out.Ticket = ticket
	out.ExpiresInSeconds = int(claims.ExpiresAt - claims.IssuedAt)
	return out, nil
}

// BrowseCatalogue returns one stable page of topic identities without issuing
// any authority to read their personal state.
func (s *TopicSuggestionService) BrowseCatalogue(ctx context.Context, userID int64, query string,
	offset, limit int) (TopicCataloguePage, error) {
	if s == nil {
		return TopicCataloguePage{}, ErrTopicSuggestionsUnavailable
	}
	page, err := s.suggester.Browse(ctx, userID, query, offset, limit)
	if err != nil {
		return TopicCataloguePage{}, err
	}
	return TopicCataloguePage{
		Candidates: page.Candidates,
		NextOffset: page.NextOffset,
		HasMore:    page.HasMore,
		PageSize:   len(page.Candidates),
	}, nil
}

// IssueSelection rechecks ownership against the current catalogue, then signs
// only the one or two topics the learner actually selected. A browse response
// from an earlier point in time is never trusted as current authority.
func (s *TopicSuggestionService) IssueSelection(ctx context.Context, userID int64, sessionID, requestID,
	question string, selected []string) (TopicSuggestion, error) {
	if s == nil {
		return TopicSuggestion{}, ErrTopicSuggestionsUnavailable
	}
	candidates, err := s.suggester.ResolveSelection(ctx, userID, selected)
	if err != nil {
		return TopicSuggestion{}, err
	}
	ticket, claims, err := s.signer.Issue(userID, sessionID, requestID, question, candidates)
	if err != nil {
		return TopicSuggestion{}, err
	}
	return TopicSuggestion{
		Candidates:       candidates,
		Ticket:           ticket,
		ExpiresInSeconds: int(claims.ExpiresAt - claims.IssuedAt),
		MaxSelectable:    topicconsent.MaxSelectableTopics,
	}, nil
}

// The suggestion service is a process-wide singleton for the same reason the
// agent service is: the HTTP layer must not construct one, because constructing
// one requires a signing key and a catalogue, and a handler that could build
// its own would be a second place where the mode is decided.
var (
	topicSuggestionMu      sync.RWMutex
	defaultTopicSuggestion *TopicSuggestionService
)

// SetDefaultTopicSuggestionService is called once from wiring, only under the
// mode that has a signing key.
func SetDefaultTopicSuggestionService(service *TopicSuggestionService) {
	topicSuggestionMu.Lock()
	defer topicSuggestionMu.Unlock()
	defaultTopicSuggestion = service
}

// DefaultTopicSuggestionService returns the configured service, or an error
// when the deployment does not run the confirmed mode.
func DefaultTopicSuggestionService() (*TopicSuggestionService, error) {
	topicSuggestionMu.RLock()
	defer topicSuggestionMu.RUnlock()
	if defaultTopicSuggestion == nil {
		return nil, ErrTopicSuggestionsUnavailable
	}
	return defaultTopicSuggestion, nil
}
