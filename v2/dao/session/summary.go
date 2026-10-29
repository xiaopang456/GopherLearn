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
	ErrSummaryNotFound = errors.New("session summary not found")
	ErrSummaryConflict = errors.New("session summary version conflict")
)

// GetSummary returns the summary only when both the session and user match.
func (r *Repository) GetSummary(ctx context.Context, userID int64, sessionID string) (*model.SessionSummary, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("session repository is not initialized")
	}
	if userID <= 0 || sessionID == "" {
		return nil, ErrSummaryNotFound
	}
	var summary model.SessionSummary
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		First(&summary).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSummaryNotFound
	}
	if err != nil {
		return nil, err
	}
	return &summary, nil
}

// SaveSummary uses optimistic concurrency. expectedVersion=0 creates the
// first row; later writes must match the current version and move the message
// high-water mark forward.
func (r *Repository) SaveSummary(ctx context.Context, summary *model.SessionSummary, expectedVersion uint64) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("session repository is not initialized")
	}
	if err := summary.Validate(); err != nil {
		return err
	}
	var owned int64
	if err := r.db.WithContext(ctx).Model(&model.Session{}).
		Where("id = ? AND user_id = ?", summary.SessionID, summary.UserID).
		Count(&owned).Error; err != nil {
		return err
	}
	if owned != 1 {
		return ErrSummaryNotFound
	}
	if expectedVersion == 0 {
		summary.Version = 1
		if err := r.db.WithContext(ctx).Create(summary).Error; err != nil {
			if isDuplicateKey(err) {
				return ErrSummaryConflict
			}
			return err
		}
		return nil
	}
	result := r.db.WithContext(ctx).Model(&model.SessionSummary{}).
		Where("user_id = ? AND session_id = ? AND version = ? AND covered_message_id < ?",
			summary.UserID, summary.SessionID, expectedVersion, summary.CoveredMessageID).
		Updates(map[string]any{
			"content": summary.Content, "covered_message_id": summary.CoveredMessageID,
			"token_estimate": summary.TokenEstimate, "version": expectedVersion + 1,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrSummaryConflict
	}
	summary.Version = expectedVersion + 1
	return nil
}

func (r *Repository) DeleteSummary(ctx context.Context, userID int64, sessionID string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("session repository is not initialized")
	}
	return r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Delete(&model.SessionSummary{}).Error
}

func isDuplicateKey(err error) bool {
	// MySQL driver error 1062 is wrapped by GORM. Keeping this package free of
	// a driver-specific type also preserves sqlite-based unit-test portability.
	return err != nil && (errors.Is(err, gorm.ErrDuplicatedKey) || containsDuplicateKey(err.Error()))
}

func containsDuplicateKey(value string) bool {
	for _, marker := range []string{"Error 1062", "Duplicate entry", "UNIQUE constraint failed"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}
