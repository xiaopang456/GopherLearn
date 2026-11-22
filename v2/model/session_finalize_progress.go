package model

import (
	"fmt"
	"strings"
	"time"
)

// SessionFinalizeProgress is the processing watermark of one session's
// finalize work: the newest message id the candidate extraction has processed.
//
// One row per session of one user. The trigger reads it before an extraction
// and stores it back after; a run whose every candidate reached a terminal
// state - persisted at the draft entry or explicitly skipped - moves the
// watermark forward, and a run with anything still undecided leaves it where
// it was so the same progress is retried. The watermark is progress, not
// state: losing the row only means the next trigger re-runs the session, and
// the draft entry's idempotency keeps one candidate.
type SessionFinalizeProgress struct {
	ID               uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID           int64     `gorm:"not null;uniqueIndex:idx_session_finalize_progress_session,priority:1" json:"user_id"`
	SessionID        string    `gorm:"type:varchar(36);not null;uniqueIndex:idx_session_finalize_progress_session,priority:2" json:"session_id"`
	CoveredMessageID uint      `gorm:"not null;default:0" json:"covered_message_id"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Validate checks the stored row.
func (p *SessionFinalizeProgress) Validate() error {
	if p == nil {
		return fmt.Errorf("session finalize progress is nil")
	}
	if p.UserID <= 0 || strings.TrimSpace(p.SessionID) == "" {
		return fmt.Errorf("session finalize progress identity is required")
	}
	if len([]rune(p.SessionID)) > 36 {
		return fmt.Errorf("session finalize progress session id exceeds the column limit")
	}
	return nil
}
