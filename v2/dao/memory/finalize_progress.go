package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"GopherAI/model"

	"gorm.io/gorm"
)

// The finalize progress is one watermark per session: the newest message id
// the candidate extraction has processed. It lives next to the memory tables
// because it is the write side's own progress - the read path never touches
// it, and a lost row costs a re-run, not a wrong answer.

// GetFinalizeProgress returns the session's watermark, zero when the session
// was never finalized. A missing row is a normal state, not an error.
func (r *Repository) GetFinalizeProgress(ctx context.Context, userID int64, sessionID string) (uint, error) {
	if r == nil || r.db == nil || userID <= 0 || sessionID == "" {
		return 0, fmt.Errorf("invalid finalize progress query")
	}
	var row model.SessionFinalizeProgress
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return row.CoveredMessageID, nil
}

// SaveFinalizeProgress stores the watermark. The move is monotonic: a stale
// writer can never pull the watermark backwards over progress another trigger
// already covered, because re-running a covered progress is harmless but
// skipping an uncovered one loses candidates.
func (r *Repository) SaveFinalizeProgress(ctx context.Context, userID int64, sessionID string, coveredMessageID uint) error {
	if r == nil || r.db == nil || userID <= 0 || sessionID == "" {
		return fmt.Errorf("invalid finalize progress write")
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row model.SessionFinalizeProgress
		err := tx.Where("user_id = ? AND session_id = ?", userID, sessionID).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Create(&model.SessionFinalizeProgress{
				UserID: userID, SessionID: sessionID, CoveredMessageID: coveredMessageID,
			}).Error
		}
		if err != nil {
			return err
		}
		if row.CoveredMessageID >= coveredMessageID {
			return nil
		}
		return tx.Model(&model.SessionFinalizeProgress{}).
			Where("id = ?", row.ID).
			Update("covered_message_id", coveredMessageID).Error
	})
}

// DueFinalizeSession is one session the idle fallback should finalize.
type DueFinalizeSession struct {
	UserID           int64
	SessionID        string
	NewestMessageID  uint
	LatestActivityAt time.Time
}

// ListDueFinalizeSessions returns the sessions whose newest message is older
// than the idle threshold and whose watermark does not yet cover it - the
// conversations the learner closed without a finalize notification reaching
// the server, or whose extraction never ran. The query is bounded twice: only
// messages recent enough to be worth the lookback enter the scan, and the
// result set is capped, so the sweep is a bounded cost per interval.
func (r *Repository) ListDueFinalizeSessions(ctx context.Context, idleBefore time.Time, lookback time.Time, batch int) ([]DueFinalizeSession, error) {
	if r == nil || r.db == nil || batch < 1 || batch > 200 {
		return nil, fmt.Errorf("invalid finalize sweep query")
	}
	rows := []DueFinalizeSession{}
	// The progress column is pulled through an aggregate and referenced by
	// its select alias: MySQL 8 rejects a bare joined column inside HAVING
	// (Unknown column 'p.covered_message_id'), which made every sweep fail
	// until the entry-point acceptance ran this query for real. The left
	// join yields at most one progress row per session - the (user, session)
	// pair is unique - so MAX over it is the value itself, NULL when the
	// session was never finalized.
	err := r.db.WithContext(ctx).Raw(`
		SELECT m.session_id AS session_id, s.user_id AS user_id,
		       MAX(m.id) AS newest_message_id, MAX(m.created_at) AS latest_activity_at,
		       MAX(p.covered_message_id) AS covered_message_id
		FROM messages m
		JOIN sessions s ON s.id = m.session_id AND s.deleted_at IS NULL
		LEFT JOIN session_finalize_progresses p
		       ON p.session_id = m.session_id AND p.user_id = s.user_id
		WHERE m.created_at >= ? AND m.created_at < ?
		GROUP BY m.session_id, s.user_id
		HAVING MAX(m.created_at) < ?
		   AND (covered_message_id IS NULL OR covered_message_id < MAX(m.id))
		ORDER BY latest_activity_at DESC
		LIMIT ?`, lookback, time.Now(), idleBefore, batch).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list due finalize sessions: %w", err)
	}
	return rows, nil
}
