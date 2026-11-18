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
		session, err := captureSession(ctx, tx, userID, sessionID)
		if err != nil {
			return err
		}
		result.Sessions = []SessionSnapshot{session}
		return result.Validate()
	})
	if err != nil {
		return Snapshot{}, err
	}
	return result, nil
}

// CaptureMany reads the named conversations of one learner.
//
// The order is the order of the names: the attempt's own conversation comes
// first, and the rest are the ones the retrieval has to choose between.
//
// The memory rows of those conversations are read with them: the archive
// carries the memories the preparation projected, so that a query restores
// them instead of projecting them again.
func CaptureMany(ctx context.Context, db *gorm.DB, userID int64, sessionIDs []string) (Snapshot, error) {
	var result Snapshot
	if db == nil || userID <= 0 || len(sessionIDs) == 0 {
		return result, fmt.Errorf("invalid capture identity")
	}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, sessionID := range sessionIDs {
			session, err := captureSession(ctx, tx, userID, sessionID)
			if err != nil {
				return fmt.Errorf("conversation %s: %w", sessionID, err)
			}
			result.Sessions = append(result.Sessions, session)
		}
		var memories []model.EpisodeMemory
		if err := tx.Where("user_id = ? AND session_id IN ?", userID, sessionIDs).
			Order("session_id ASC, version DESC").Find(&memories).Error; err != nil {
			return err
		}
		// One conversation has one current memory. The rows are ordered by
		// version, so the first row of a conversation is the one that was
		// current when the archive was taken.
		seen := map[string]struct{}{}
		for _, memory := range memories {
			if _, exists := seen[memory.SessionID]; exists {
				continue
			}
			seen[memory.SessionID] = struct{}{}
			result.Memories = append(result.Memories, memory)
		}
		// The compressed state of those conversations travels with them.
		//
		// The full card carries its own body, so an archive without the
		// segments still holds a readable memory. The thin card does not: it
		// points at the segments, and the body is read from there. An
		// archive that restored the thin cards without the segments would
		// restore a directory with no rooms behind it.
		var segments []model.SessionSummarySegment
		if err := tx.Where("user_id = ? AND session_id IN ?", userID, sessionIDs).
			Order("session_id ASC, seq ASC").Find(&segments).Error; err != nil {
			return err
		}
		result.Segments = segments
		var whiteboards []model.SessionWhiteboard
		if err := tx.Where("user_id = ? AND session_id IN ?", userID, sessionIDs).
			Order("session_id ASC").Find(&whiteboards).Error; err != nil {
			return err
		}
		result.Whiteboards = whiteboards
		return result.Validate()
	})
	if err != nil {
		return Snapshot{}, err
	}
	return result, nil
}

// captureSession reads one conversation of one learner.
func captureSession(ctx context.Context, tx *gorm.DB, userID int64, sessionID string) (SessionSnapshot, error) {
	var session SessionSnapshot
	if err := tx.Where("id = ? AND user_id = ?", sessionID, userID).First(&session.Session).Error; err != nil {
		return session, err
	}
	if err := tx.Where("session_id = ?", sessionID).Order("id ASC").Find(&session.Messages).Error; err != nil {
		return session, err
	}
	var summary model.SessionSummary
	err := tx.Where("session_id = ? AND user_id = ?", sessionID, userID).First(&summary).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return session, err
	}
	if err == nil {
		session.Summary = &summary
	}
	return session, nil
}
