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
//
// It restores every conversation of the attempt, and it writes only the three
// conversation tables. The memory rows are written by the product's own
// projection, in the answer phase, on purpose: a projection that the harness
// faked would not be a measurement.
func restoreIntoEmpty(ctx context.Context, db *gorm.DB, snapshot Snapshot) error {
	if db == nil {
		return fmt.Errorf("database is nil")
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	primary, _ := snapshot.Primary()
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The sandbox is seeded with exactly one evaluation user. A matching
		// session username alone does not establish ownership.
		var users, owned int64
		if err := tx.Model(&model.User{}).Count(&users).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.User{}).Where("id = ? AND username = ?", primary.Session.UserID, primary.Session.UserName).Count(&owned).Error; err != nil {
			return err
		}
		if users != 1 || owned != 1 {
			return fmt.Errorf("target evaluation user does not match snapshot")
		}
		for _, value := range []any{&model.Session{}, &model.Message{}, &model.SessionSummary{}} {
			var count int64
			if err := tx.Model(value).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("target contains existing conversation data")
			}
		}
		for _, conversation := range snapshot.Sessions {
			session := conversation.Session
			if err := tx.Create(&session).Error; err != nil {
				return err
			}
			// Preserve message IDs; summary watermarks and evidence refer to them.
			messages := append([]model.Message(nil), conversation.Messages...)
			if err := tx.Create(&messages).Error; err != nil {
				return err
			}
			if conversation.Summary != nil {
				summary := *conversation.Summary
				if err := tx.Omit("Session").Create(&summary).Error; err != nil {
					return err
				}
			}
		}
		// The memory rows come back with the conversations. The shared index
		// still points at them, so a query that restores them finds the same
		// memories the preparation projected, and it does not have to pay
		// for the projection again.
		for _, memory := range snapshot.Memories {
			row := memory
			if err := tx.Omit("Session").Create(&row).Error; err != nil {
				return err
			}
		}
		// The compressed state comes back too.
		//
		// The thin card does not carry the body: it points at the summary
		// segments and at the board, and the body is read from there. An
		// archive that restored the cards without them would restore a
		// directory with no rooms behind it.
		for _, segment := range snapshot.Segments {
			row := segment
			row.ID = 0
			if err := tx.Omit("Session").Create(&row).Error; err != nil {
				return err
			}
		}
		for _, whiteboard := range snapshot.Whiteboards {
			row := whiteboard
			row.ID = 0
			if err := tx.Omit("Session").Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
