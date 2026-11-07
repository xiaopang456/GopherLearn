package summaryconvert

import (
	"context"
	"errors"
	"fmt"

	"GopherAI/common/rag"
	"GopherAI/model"

	"gorm.io/gorm"
)

// ErrRowChanged means the row moved between the read and the write, so the
// conversion that was planned against it is no longer the conversion to make.
var ErrRowChanged = errors.New("summary row changed underneath the conversion")

// Row is one stored summary, with the session facts a report has to name.
type Row struct {
	UserID           int64
	SessionID        string
	Mode             model.SessionMode
	Content          string
	CoveredMessageID uint
	Version          uint64
}

// Store reads and writes the summary rows of one deployment.
//
// The write is deliberately not the session DAO's SaveSummary: that path is the
// rolling compaction's, and its compare-and-set only accepts a write whose
// watermark has moved forward. A conversion moves no watermark - it re-expresses
// the same covered range in another shape - so it needs its own guarded write,
// and the guard here is the version together with the watermark the row had when
// it was read. Reusing SaveSummary would either refuse every conversion or
// require advancing the watermark, and advancing it would skip messages that no
// summary has ever covered.
type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// List returns every summary row with its session's mode. A row whose session
// is gone still comes back, with no mode: it is reported rather than skipped,
// because "how many old shapes are in here" is the question this command
// exists to answer.
func (s *Store) List(ctx context.Context, limit int) ([]Row, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("summary store is not initialized")
	}
	if limit <= 0 {
		limit = 512
	}
	var rows []Row
	// The join is a LEFT JOIN and the mode is coalesced, because a row whose
	// session is gone must still be counted: the number of old shapes in a
	// database is only honest when nothing drops out of the join. Whether a
	// missing mode arrives as an empty string or as a read error depends on how
	// the driver turns a NULL into a string, so the SQL says what to read rather
	// than leaving that to the driver. The requirement itself - an orphan row is
	// listed, with no mode, and is still converted - is what the test pins.
	err := s.db.WithContext(ctx).
		Table("session_summaries").
		Select("session_summaries.user_id, session_summaries.session_id, COALESCE(sessions.mode, '') AS mode, session_summaries.content, session_summaries.covered_message_id, session_summaries.version").
		Joins("LEFT JOIN sessions ON sessions.id = session_summaries.session_id").
		Order("session_summaries.user_id ASC, session_summaries.session_id ASC").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list session summaries: %w", err)
	}
	return rows, nil
}

// HasMore reports whether more rows exist than List returned.
func (s *Store) HasMore(ctx context.Context, limit int) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("summary store is not initialized")
	}
	if limit <= 0 {
		limit = 512
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&model.SessionSummary{}).Count(&count).Error; err != nil {
		return false, fmt.Errorf("count session summaries: %w", err)
	}
	return count > int64(limit), nil
}

// Write stores a converted document in place of the row's content. The
// watermark is held by the predicate as well as by the value written, so a row
// that moved in between is refused instead of clobbered.
func (s *Store) Write(ctx context.Context, row Row, content string) error {
	if s == nil || s.db == nil {
		return errors.New("summary store is not initialized")
	}
	if row.UserID <= 0 || row.SessionID == "" {
		return errors.New("summary row identity is required")
	}
	result := s.db.WithContext(ctx).Model(&model.SessionSummary{}).
		Where("user_id = ? AND session_id = ? AND version = ? AND covered_message_id = ?",
			row.UserID, row.SessionID, row.Version, row.CoveredMessageID).
		Updates(map[string]any{
			"content": content,
			// The token estimate is what the context budget reads, so it
			// describes the content that is actually stored.
			"token_estimate": rag.EstimateTokens(content),
			"version":        row.Version + 1,
		})
	if result.Error != nil {
		return fmt.Errorf("write converted summary: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrRowChanged
	}
	return nil
}
