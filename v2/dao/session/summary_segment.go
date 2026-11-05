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
	// ErrSummarySegmentNotFound means the session has no such segment.
	ErrSummarySegmentNotFound = model.ErrSummarySegmentNotFound
	// ErrSummarySegmentConflict means another writer already claimed this
	// segment's sequence number.
	ErrSummarySegmentConflict = errors.New("summary segment sequence conflict")
)

// GetSummarySegment returns one segment of a session by its sequence number.
func (r *Repository) GetSummarySegment(ctx context.Context, userID int64, sessionID string, seq int) (*model.SessionSummarySegment, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("session repository is not initialized")
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" || seq <= 0 {
		return nil, ErrSummarySegmentNotFound
	}
	var segment model.SessionSummarySegment
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ? AND seq = ?", userID, sessionID, seq).
		First(&segment).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSummarySegmentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &segment, nil
}

// ListSummarySegments returns a session's segments in order.
//
// The list is bounded by the caller's limit because a long session accumulates
// many segments and no reader needs all of them at once.
func (r *Repository) ListSummarySegments(ctx context.Context, userID int64, sessionID string, limit int) ([]model.SessionSummarySegment, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("session repository is not initialized")
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 32
	}
	if limit > 512 {
		limit = 512
	}
	var segments []model.SessionSummarySegment
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("seq ASC").
		Limit(limit).
		Find(&segments).Error
	if err != nil {
		return nil, err
	}
	return segments, nil
}

// GetLatestSummarySegment returns the session's newest segment.
//
// It is the segment a generator needs as the one before the batch it is about
// to write. Asking for a list and taking the last of it is not the same
// thing: a list is read in order from the oldest, so a bounded list answers
// with the oldest segments rather than the newest.
func (r *Repository) GetLatestSummarySegment(ctx context.Context, userID int64, sessionID string) (*model.SessionSummarySegment, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("session repository is not initialized")
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, ErrSummarySegmentNotFound
	}
	var segment model.SessionSummarySegment
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("seq DESC").
		First(&segment).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSummarySegmentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &segment, nil
}

// ListLatestSummarySegments returns the session's newest segments, newest
// first.
//
// The list is read backwards because the reader wants the segments that
// cover the most recent turns: a bounded list read forwards answers with the
// oldest segments instead.
func (r *Repository) ListLatestSummarySegments(ctx context.Context, userID int64, sessionID string, limit int) ([]model.SessionSummarySegment, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("session repository is not initialized")
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 2
	}
	if limit > 512 {
		limit = 512
	}
	var segments []model.SessionSummarySegment
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("seq DESC").
		Limit(limit).
		Find(&segments).Error
	if err != nil {
		return nil, err
	}
	return segments, nil
}

// ListSummarySegmentsAfter returns the segments that cover messages after the
// given watermark, oldest first.
//
// The watermark is the last message a reader already holds. Segments that end
// at or below it are already known to that reader and are not returned.
func (r *Repository) ListSummarySegmentsAfter(ctx context.Context, userID int64, sessionID string, afterMessageID uint, limit int) ([]model.SessionSummarySegment, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("session repository is not initialized")
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 32
	}
	if limit > 512 {
		limit = 512
	}
	var segments []model.SessionSummarySegment
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ? AND end_message_id > ?", userID, sessionID, afterMessageID).
		Order("seq ASC").
		Limit(limit).
		Find(&segments).Error
	if err != nil {
		return nil, err
	}
	return segments, nil
}

// SaveSummarySegment writes one segment.
//
// A segment is written once. The unique index on (user_id, session_id, seq)
// means a second writer for the same sequence number loses, which is what keeps
// two concurrent generations from both claiming to be segment N.
func (r *Repository) SaveSummarySegment(ctx context.Context, segment *model.SessionSummarySegment) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("session repository is not initialized")
	}
	if err := segment.Validate(); err != nil {
		return err
	}
	// A JSON column cannot hold an empty string. A segment that changed
	// nothing stores an empty list, which is also what a reader should see.
	if strings.TrimSpace(segment.WhiteboardChanges) == "" {
		segment.WhiteboardChanges = "[]"
	}
	var owned int64
	if err := r.db.WithContext(ctx).Model(&model.Session{}).
		Where("id = ? AND user_id = ?", segment.SessionID, segment.UserID).
		Count(&owned).Error; err != nil {
		return err
	}
	if owned != 1 {
		return ErrSummarySegmentNotFound
	}
	if err := r.db.WithContext(ctx).Create(segment).Error; err != nil {
		if isDuplicateKey(err) {
			return ErrSummarySegmentConflict
		}
		return err
	}
	return nil
}

// NextSummarySegmentSeq returns the sequence number the next segment of a
// session takes.
//
// It is read inside the caller's transaction so that two writers racing for
// the same number collide on the unique index rather than both believing they
// won.
func (r *Repository) NextSummarySegmentSeq(ctx context.Context, userID int64, sessionID string) (int, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("session repository is not initialized")
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return 0, ErrSummarySegmentNotFound
	}
	var highest *int
	err := r.db.WithContext(ctx).Model(&model.SessionSummarySegment{}).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Select("MAX(seq)").Scan(&highest).Error
	if err != nil {
		return 0, err
	}
	if highest == nil {
		return 1, nil
	}
	return *highest + 1, nil
}

// DeleteSummarySegments removes a session's segments.
func (r *Repository) DeleteSummarySegments(ctx context.Context, userID int64, sessionID string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("session repository is not initialized")
	}
	return r.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Delete(&model.SessionSummarySegment{}).Error
}
