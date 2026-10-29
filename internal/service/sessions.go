package service

import (
	"context"
	"errors"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

type SessionService struct {
	repo repository.Repository
	// episodes is the episodic-memory delete hook. It is nil in a deployment
	// without the feature, which keeps Delete on the one path it always was.
	episodes SessionEpisodeDropper
}

// SessionEpisodeDropper drops one session's derived episodic retrieval view
// after the session itself is gone. The store rows - the card and its
// projection job - are already gone by then: they are deleted inside the
// session-delete transaction. The hook only cleans the derived view, and it is
// best-effort by contract: a view that lagged behind is filtered by the
// reload, not trusted. episodeIDs is what the caller captured while the
// session was still readable, so the view delete can name what to drop.
type SessionEpisodeDropper interface {
	DropSession(ctx context.Context, userID, sessionID string, episodeIDs []string)
}

// UseEpisodeDropper installs the delete hook.
func (s *SessionService) UseEpisodeDropper(dropper SessionEpisodeDropper) {
	s.episodes = dropper
}

func NewSessionService(repo repository.Repository) *SessionService {
	return &SessionService{repo: repo}
}

func (s *SessionService) Create(ctx context.Context, userID string, title string, mode domain.SessionMode) (*domain.Session, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	title = strings.TrimSpace(title)
	if len(title) > 200 || !mode.IsValid() {
		return nil, ErrInvalidRequest
	}
	created, err := s.repo.CreateSession(ctx, &domain.Session{UserID: userID, Title: title, Mode: mode})
	return created, mapRepositoryError(err)
}

func (s *SessionService) List(ctx context.Context, userID string) ([]domain.Session, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	items, err := s.repo.ListSessions(ctx, userID)
	return items, mapRepositoryError(err)
}

func (s *SessionService) Messages(ctx context.Context, userID, sessionID string) ([]domain.Message, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, ErrInvalidRequest
	}
	items, err := s.repo.ListMessages(ctx, userID, sessionID)
	return items, mapRepositoryError(err)
}

func (s *SessionService) Delete(ctx context.Context, userID, sessionID string) error {
	if strings.TrimSpace(userID) == "" {
		return ErrUnauthorized
	}
	if strings.TrimSpace(sessionID) == "" {
		return ErrInvalidRequest
	}
	// The card's id is captured while the session is still readable, so the
	// view cleanup below can name what to drop. The rows themselves go inside
	// the delete transaction: a card whose source session is gone must not
	// outlive the delete by even one crashed step.
	var episodeIDs []string
	if card, err := s.repo.GetEpisodeBySession(ctx, userID, sessionID); err == nil {
		episodeIDs = []string{card.ID}
	}
	if err := mapRepositoryError(s.repo.DeleteSession(ctx, userID, sessionID)); err != nil {
		return err
	}
	// The session is gone; its derived view is cleaned best-effort. Ownership
	// was already decided by the delete above, so the hook needs no second
	// decision and its failure cannot un-delete anything - it is cleanup, not
	// policy.
	if s.episodes != nil {
		s.episodes.DropSession(ctx, userID, sessionID, episodeIDs)
	}
	return nil
}

func (s *SessionService) Get(ctx context.Context, userID, sessionID string) (*domain.Session, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, ErrInvalidRequest
	}
	session, err := s.repo.GetSession(ctx, userID, sessionID)
	return session, mapRepositoryError(err)
}

func mapRepositoryError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, repository.ErrConflict):
		return ErrConflict
	case errors.Is(err, repository.ErrInvalidArgument):
		return ErrInvalidRequest
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return ErrInternal
	}
}
