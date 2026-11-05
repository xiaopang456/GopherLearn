package agent

import (
	"context"
	"fmt"
	"log"
	"time"

	"GopherAI/model"
)

// The reconciler repairs the sessions whose enqueue was missed.
//
// An enqueue happens when a turn is answered. If the process is down, or the
// write fails, or the turn was answered by a path that does not enqueue, the
// session has turns that no segment covers and no job to cover them with.
// The reconciler finds those sessions and enqueues them.
//
// It is a repair, not the normal path. The normal path enqueues immediately;
// the reconciler exists so that a missed enqueue does not stay missed.

// SummarySegmentReconcileRepository is what the reconciler reads and writes.
type SummarySegmentReconcileRepository interface {
	// ListSummarySegmentGaps finds the conversations that owe a segment.
	ListSummarySegmentGaps(ctx context.Context, limit int) ([]model.SummarySegmentGap, error)
	// UpsertSummarySegmentJob records that a session wants a segment.
	UpsertSummarySegmentJob(ctx context.Context, job *model.SessionSummarySegmentJob) error
	// GetSummarySegmentWatermark returns the message id the session's segments
	// cover and the next sequence number.
	GetSummarySegmentWatermark(ctx context.Context, userID int64, sessionID string) (uint, int, error)
}

// SummarySegmentReconciler enqueues the sessions that owe a segment.
type SummarySegmentReconciler struct {
	repository SummarySegmentReconcileRepository
	batchSize  int
	now        func() time.Time
	logf       func(string, ...any)
}

// NewSummarySegmentReconciler builds a reconciler.
func NewSummarySegmentReconciler(repository SummarySegmentReconcileRepository, batchSize int) (*SummarySegmentReconciler, error) {
	if repository == nil || batchSize < 1 || batchSize > 1000 {
		return nil, fmt.Errorf("invalid summary segment reconciler configuration")
	}
	return &SummarySegmentReconciler{
		repository: repository,
		batchSize:  batchSize,
		now:        time.Now,
		logf:       log.Printf,
	}, nil
}

// RunOnce finds the sessions that owe a segment and enqueues them.
//
// The enqueue names the first turn after the session's own watermark as the
// start, so a segment covers only the turns that are not covered yet. A
// session that already has a job keeps its job: the job row is unique per
// session, and the upsert moves its target forward rather than adding a
// second one.
func (r *SummarySegmentReconciler) RunOnce(ctx context.Context) (int, error) {
	gaps, err := r.repository.ListSummarySegmentGaps(ctx, r.batchSize)
	if err != nil {
		return 0, err
	}
	repaired := 0
	for _, gap := range gaps {
		watermark, _, err := r.repository.GetSummarySegmentWatermark(ctx, gap.UserID, gap.SessionID)
		if err != nil {
			if ctx.Err() != nil {
				return repaired, ctx.Err()
			}
			r.logf("summary segment reconciliation read failed: session=%s error_type=%T", gap.SessionID, err)
			continue
		}
		job := &model.SessionSummarySegmentJob{
			UserID:           gap.UserID,
			SessionID:        gap.SessionID,
			DesiredMessageID: gap.LatestMessageID,
			// The start is the first turn after the watermark. The watermark
			// is the last turn a segment already covers, and covering it
			// again would answer for a turn twice.
			StartMessageID: watermark + 1,
			Status:         model.SessionSummarySegmentJobPending,
			AvailableAt:    r.now(),
		}
		if err := r.repository.UpsertSummarySegmentJob(ctx, job); err != nil {
			if ctx.Err() != nil {
				return repaired, ctx.Err()
			}
			r.logf("summary segment reconciliation enqueue failed: session=%s error_type=%T", gap.SessionID, err)
			continue
		}
		repaired++
	}
	return repaired, nil
}
