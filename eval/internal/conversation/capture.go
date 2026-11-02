package conversation

import (
	"context"
	"errors"
	"fmt"

	"GopherAI/model"
	"gorm.io/gorm"
)

// Capture reads only one owned session. The caller must ensure the DB is an
// isolated evaluation instance before calling; this function cannot certify a
// database's origin. Call while all writers are stopped.
func Capture(ctx context.Context, db *gorm.DB, userID int64, sessionID string) (Snapshot, error) {
	var result Snapshot
	if db == nil || userID <= 0 || sessionID == "" {
		return result, fmt.Errorf("invalid capture identity")
	}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", sessionID, userID).First(&result.Session).Error; err != nil {
			return err
		}
		if err := tx.Where("session_id = ?", sessionID).Order("id ASC").Find(&result.Messages).Error; err != nil {
			return err
		}
		var summary model.SessionSummary
		err := tx.Where("session_id = ? AND user_id = ?", sessionID, userID).First(&summary).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil {
			result.Summary = &summary
		}
		return result.Validate()
	})
	if err != nil {
		return Snapshot{}, err
	}
	return result, nil
}
