package conversation

import (
	"context"
	"fmt"

	"GopherAI/model"
	"gorm.io/gorm"
)

// restoreIntoEmpty is deliberately unexported: a future isolated-instance
// manager must establish database provenance before it is reachable from a
// command. Empty tables alone do not prove that a database is safe to write.
func restoreIntoEmpty(ctx context.Context, db *gorm.DB, snapshot Snapshot) error {
	if db == nil {
		return fmt.Errorf("database is nil")
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, value := range []any{&model.Session{}, &model.Message{}, &model.SessionSummary{}} {
			var count int64
			if err := tx.Model(value).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("target contains existing conversation data")
			}
		}
		session := snapshot.Session
		if err := tx.Create(&session).Error; err != nil {
			return err
		}
		// Preserve message IDs; summary watermarks and evidence refer to them.
		messages := append([]model.Message(nil), snapshot.Messages...)
		if err := tx.Create(&messages).Error; err != nil {
			return err
		}
		if snapshot.Summary != nil {
			summary := *snapshot.Summary
			if err := tx.Omit("Session").Create(&summary).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
