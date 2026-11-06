package session

import (
	"GopherAI/model"
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// SaveSummarySegmentAndWhiteboard writes one segment and the board it implies
// as a single unit.
//
// The two claim the same stretch of the conversation, so they are written
// together or not at all. A board that says it covers a turn no segment
// records would be a board that answered for turns nobody can read back, and
// a segment that landed without its board would leave the board describing a
// state that never happened.
//
// The board's version check is inside the transaction, so a writer that lost
// the race rolls back its segment with it: the stretch is summarized again
// from the same start rather than half-written.
func (r *Repository) SaveSummarySegmentAndWhiteboard(
	ctx context.Context,
	segment *model.SessionSummarySegment,
	board *model.SessionWhiteboard,
	expectedVersion uint64,
) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("session repository is not initialized")
	}
	if segment == nil || board == nil {
		return fmt.Errorf("summary segment and whiteboard are required")
	}
	if err := segment.Validate(); err != nil {
		return err
	}
	if err := board.Validate(); err != nil {
		return err
	}
	if segment.UserID != board.UserID || segment.SessionID != board.SessionID {
		return fmt.Errorf("summary segment and whiteboard belong to different sessions")
	}
	if strings.TrimSpace(segment.WhiteboardChanges) == "" {
		segment.WhiteboardChanges = "[]"
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var owned int64
		if err := tx.Model(&model.Session{}).
			Where("id = ? AND user_id = ?", segment.SessionID, segment.UserID).
			Count(&owned).Error; err != nil {
			return err
		}
		if owned != 1 {
			return ErrSummarySegmentNotFound
		}
		if err := tx.Create(segment).Error; err != nil {
			if isDuplicateKey(err) {
				return ErrSummarySegmentConflict
			}
			return err
		}
		if expectedVersion == 0 {
			board.Version = 1
			if err := tx.Create(board).Error; err != nil {
				if isDuplicateKey(err) {
					return ErrWhiteboardConflict
				}
				return err
			}
			return nil
		}
		result := tx.Model(&model.SessionWhiteboard{}).
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
	})
	if err != nil {
		return err
	}
	return nil
}

// ListSummarySegmentMessages returns the turns a segment batch covers.
//
// The range is inclusive of both ends and the session join is the ownership
// boundary: a read for a session the caller does not own returns nothing
// rather than another account's turns.
func (r *Repository) ListSummarySegmentMessages(
	ctx context.Context,
	userID int64,
	sessionID string,
	startMessageID, endMessageID uint,
	limit int,
) ([]model.Message, error) {
	if r == nil || r.db == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("invalid summary segment message read")
	}
	if endMessageID < startMessageID {
		return nil, fmt.Errorf("summary segment message range is inverted")
	}
	if limit <= 0 {
		limit = 256
	}
	if limit > 1024 {
		limit = 1024
	}
	var messages []model.Message
	err := r.db.WithContext(ctx).
		Select("messages.*").
		Joins("JOIN sessions ON sessions.id = messages.session_id AND sessions.deleted_at IS NULL").
		Where("sessions.user_id = ? AND messages.session_id = ? AND messages.id >= ? AND messages.id <= ?",
			userID, sessionID, startMessageID, endMessageID).
		Order("messages.id ASC").
		Limit(limit).
		Find(&messages).Error
	if err != nil {
		return nil, err
	}
	return messages, nil
}

// ErrSummarySegmentWhiteboardConflict is returned when the board moved on
// while a segment was being written.
//
// It is a conflict and not a failure: the work was done, the write was
// refused, and the next attempt reads the board that won.
var ErrSummarySegmentWhiteboardConflict = ErrWhiteboardConflict

// The compiler checks that the repository offers everything the layered
// summary's write path needs. A missing method is caught here rather than at
// the point where a segment would have been written.
var _ interface {
	ListSummarySegmentMessages(context.Context, int64, string, uint, uint, int) ([]model.Message, error)
	GetWhiteboard(context.Context, int64, string) (*model.SessionWhiteboard, error)
	GetSummarySegmentWatermark(context.Context, int64, string) (uint, int, error)
	ListSummarySegments(context.Context, int64, string, int) ([]model.SessionSummarySegment, error)
	GetLatestSummarySegment(context.Context, int64, string) (*model.SessionSummarySegment, error)
	SaveSummarySegmentAndWhiteboard(context.Context, *model.SessionSummarySegment, *model.SessionWhiteboard, uint64) error
	NextSummarySegmentSeq(context.Context, int64, string) (int, error)
	ListDueSummarySegmentJobs(context.Context, int, int, time.Time) ([]model.SessionSummarySegmentJob, error)
	ClaimSummarySegmentJob(context.Context, int64, string, time.Time, time.Duration, int) (*model.SessionSummarySegmentJob, error)
	CompleteSummarySegmentJob(context.Context, *model.SessionSummarySegmentJob, uint, time.Time) error
	RetrySummarySegmentJob(context.Context, *model.SessionSummarySegmentJob, string, time.Time, int) error
	RecoverExpiredSummarySegmentJobs(context.Context, time.Time, int) (int64, error)
	ListSummarySegmentGaps(context.Context, int) ([]model.SummarySegmentGap, error)
	UpsertSummarySegmentJob(context.Context, *model.SessionSummarySegmentJob) error
	GetSummarySegmentJobBySession(context.Context, int64, string) (*model.SessionSummarySegmentJob, error)
	TakeOverSummarySegmentJob(context.Context, int64, string, uint, uint, time.Time, time.Duration) (*model.SessionSummarySegmentJob, error)
} = (*Repository)(nil)
