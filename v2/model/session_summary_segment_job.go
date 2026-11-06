package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The segment job is the queue entry for one pending summary segment.
//
// One row per session: the unique index on (user_id, session_id) is what keeps
// a session from running two segment generations at once. A second request
// while one is in flight updates the row's target rather than adding a second
// row, so the work stays one segment at a time and the newest target wins.
const (
	SessionSummarySegmentJobPending    = "pending"
	SessionSummarySegmentJobProcessing = "processing"
	SessionSummarySegmentJobCompleted  = "completed"
	SessionSummarySegmentJobFailed     = "failed"
)

// SessionSummarySegmentJob is the queue entry for generating one segment.
type SessionSummarySegmentJob struct {
	ID               string `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID           int64  `gorm:"not null;uniqueIndex:idx_summary_segment_jobs_owner_session,priority:1" json:"user_id"`
	SessionID        string `gorm:"type:varchar(36);not null;uniqueIndex:idx_summary_segment_jobs_owner_session,priority:2" json:"session_id"`
	DesiredMessageID uint   `gorm:"not null" json:"desired_message_id"`
	StartMessageID   uint   `gorm:"not null" json:"start_message_id"`
	// BatchMessageID is the end of the batch this job is working on.
	//
	// It is frozen when the first attempt of a batch starts and it does not
	// move while that batch is retried. The target may move forward, but the
	// batch may not: a retry has to face the same turns, or it is not a retry
	// of the same failure but a different attempt at a different question.
	BatchMessageID uint       `gorm:"not null;default:0" json:"batch_message_id"`
	Status         string     `gorm:"type:varchar(16);not null;index" json:"status"`
	Attempts       int        `gorm:"not null;default:0" json:"attempts"`
	AvailableAt    time.Time  `gorm:"not null;index" json:"available_at"`
	LeaseToken     string     `gorm:"type:varchar(36)" json:"-"`
	LeaseEndsAt    *time.Time `gorm:"index" json:"-"`
	ErrorCode      string     `gorm:"type:varchar(64)" json:"error_code"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// ErrSummarySegmentJobConflict is what a writer returns when another writer
// already holds the row.
var ErrSummarySegmentJobConflict = errors.New("summary segment job conflict")

// ErrSummarySegmentJobNotFound means the session has no job row.
//
// It lives here rather than in the repository because the reader that has to
// tell "no job yet" from "a job I may not read" is not the only one that
// needs to know.
var ErrSummarySegmentJobNotFound = errors.New("summary segment job not found")

// SummarySegmentGap is a conversation whose latest turn is not covered by any
// summary segment yet.
//
// It lives here because both the repository that finds the gaps and the
// service that repairs them have to agree on what a gap is.
type SummarySegmentGap struct {
	UserID          int64  `gorm:"column:user_id"`
	SessionID       string `gorm:"column:session_id"`
	LatestMessageID uint   `gorm:"column:latest_message_id"`
}

// Validate checks the job's shape.
func (j *SessionSummarySegmentJob) Validate() error {
	if j == nil {
		return fmt.Errorf("summary segment job is nil")
	}
	if strings.TrimSpace(j.ID) == "" {
		return fmt.Errorf("summary segment job id is required")
	}
	if j.UserID <= 0 || strings.TrimSpace(j.SessionID) == "" {
		return fmt.Errorf("summary segment job identity is required")
	}
	if j.DesiredMessageID == 0 {
		return fmt.Errorf("summary segment job target is required")
	}
	if j.StartMessageID > j.DesiredMessageID {
		return fmt.Errorf("summary segment job range is inverted")
	}
	switch j.Status {
	case SessionSummarySegmentJobPending, SessionSummarySegmentJobProcessing,
		SessionSummarySegmentJobCompleted, SessionSummarySegmentJobFailed:
	default:
		return fmt.Errorf("summary segment job has an unknown status %q", j.Status)
	}
	return nil
}
