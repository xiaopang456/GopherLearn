package session

import (
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

var (
	// ErrWhiteboardNotFound means the session has no board yet.
	ErrWhiteboardNotFound = errors.New("session whiteboard not found")
	// ErrWhiteboardConflict means another writer moved the board on. It is not
	// a failure: the caller re-reads and decides.
	ErrWhiteboardConflict = errors.New("session whiteboard version conflict")
)

// GetWhiteboard returns the board only when both the session and user match.
//
// The board belongs to one session of one user, and a read for a session the
// caller does not own must look exactly like a read for a session that does
// not exist.
func (r *Repository) GetWhiteboard(ctx context.Context, userID int64, sessionID string) (*model.SessionWhiteboard, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("session repository is not initialized")
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, ErrWhiteboardNotFound
	}
	var board model.SessionWhiteboard
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		First(&board).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrWhiteboardNotFound
	}
	if err != nil {
		return nil, err
	}
	return &board, nil
}

// SaveWhiteboard writes the board using optimistic concurrency.
//
// expectedVersion=0 creates the first row; later writes must match the current
// version and move the coverage forward. The version check and the coverage
// check are one statement, so a writer cannot commit a board whose coverage is
// behind the one already stored.
func (r *Repository) SaveWhiteboard(ctx context.Context, board *model.SessionWhiteboard, expectedVersion uint64) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("session repository is not initialized")
	}
	if err := board.Validate(); err != nil {
		return err
	}
	var owned int64
	if err := r.db.WithContext(ctx).Model(&model.Session{}).
		Where("id = ? AND user_id = ?", board.SessionID, board.UserID).
		Count(&owned).Error; err != nil {
		return err
	}
	if owned != 1 {
		return ErrWhiteboardNotFound
	}
	if expectedVersion == 0 {
		board.Version = 1
		if err := r.db.WithContext(ctx).Create(board).Error; err != nil {
			if isDuplicateKey(err) {
				return ErrWhiteboardConflict
			}
			return err
		}
		return nil
	}
	result := r.db.WithContext(ctx).Model(&model.SessionWhiteboard{}).
		Where("user_id = ? AND session_id = ? AND version = ? AND covered_message_id < ?",
			board.UserID, board.SessionID, expectedVersion, board.CoveredMessageID).
		Updates(map[string]any{
			"content": board.Content, "covered_message_id": board.CoveredMessageID,
			"token_estimate": board.TokenEstimate, "version": expectedVersion + 1,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrWhiteboardConflict
	}
	board.Version = expectedVersion + 1
	return nil
}

// DeleteWhiteboard removes the board of one session.
func (r *Repository) DeleteWhiteboard(ctx context.Context, userID int64, sessionID string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("session repository is not initialized")
	}
	return r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Delete(&model.SessionWhiteboard{}).Error
}
