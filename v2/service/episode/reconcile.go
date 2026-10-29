package episode

import (
	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
	"context"
	"fmt"
	"log"
	"time"
)

type ReconcileRepository interface {
	ListProjectionGaps(context.Context, int) ([]episodedao.ProjectionGap, error)
	UpsertDesired(context.Context, int64, string, uint, time.Time) (*model.EpisodeProjectionJob, error)
}

type Reconciler struct {
	repository  ReconcileRepository
	quietWindow time.Duration
	batchSize   int
	now         func() time.Time
	logf        func(string, ...any)
}

func NewReconciler(repository ReconcileRepository, quietWindow time.Duration, batchSize int) (*Reconciler, error) {
	if repository == nil || quietWindow < 0 || quietWindow > 24*time.Hour || batchSize < 1 || batchSize > 1000 {
		return nil, fmt.Errorf("invalid episode reconciler configuration")
	}
	return &Reconciler{repository: repository, quietWindow: quietWindow, batchSize: batchSize, now: time.Now, logf: log.Printf}, nil
}

func (r *Reconciler) RunOnce(ctx context.Context) (int, error) {
	gaps, err := r.repository.ListProjectionGaps(ctx, r.batchSize)
	if err != nil {
		return 0, err
	}
	repaired := 0
	for _, gap := range gaps {
		if _, err := r.repository.UpsertDesired(ctx, gap.UserID, gap.SessionID, gap.LatestMessageID, r.now().Add(r.quietWindow)); err != nil {
			if ctx.Err() != nil {
				return repaired, ctx.Err()
			}
			r.logf("episode reconciliation enqueue failed: session=%s error_type=%T", gap.SessionID, err)
			continue
		}
		repaired++
	}
	return repaired, nil
}
