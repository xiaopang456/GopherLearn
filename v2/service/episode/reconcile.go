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
	ListProjectionGaps(context.Context, int, model.EpisodeContentKind) ([]episodedao.ProjectionGap, error)
	UpsertDesired(context.Context, int64, string, uint, time.Time) (*model.EpisodeProjectionJob, error)
}

type Reconciler struct {
	repository  ReconcileRepository
	quietWindow time.Duration
	batchSize   int
	// kind says which derived row the reconciler looks for. One session may
	// hold an Episode and a card at the same time, and a gap in one is not a
	// gap in the other.
	kind model.EpisodeContentKind
	now  func() time.Time
	logf func(string, ...any)
}

func NewReconciler(repository ReconcileRepository, quietWindow time.Duration, batchSize int) (*Reconciler, error) {
	if repository == nil || quietWindow < 0 || quietWindow > 24*time.Hour || batchSize < 1 || batchSize > 1000 {
		return nil, fmt.Errorf("invalid episode reconciler configuration")
	}
	return &Reconciler{repository: repository, quietWindow: quietWindow, batchSize: batchSize,
		kind: model.EpisodeContentEpisode, now: time.Now, logf: log.Printf}, nil
}

// UseContentKind points the reconciler at one kind of derived row.
//
// It is a setter rather than a constructor argument because the default is
// the Episode: a deployment that says nothing reconciles the Episodes it
// always reconciled, and one that runs the card path says so.
func (r *Reconciler) UseContentKind(kind model.EpisodeContentKind) {
	if r == nil {
		return
	}
	r.kind = kind.Normalized()
}

func (r *Reconciler) RunOnce(ctx context.Context) (int, error) {
	gaps, err := r.repository.ListProjectionGaps(ctx, r.batchSize, r.kind)
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
