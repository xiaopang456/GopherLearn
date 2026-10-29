package episode

import (
	"GopherAI/common/observability"
	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
	memoryservice "GopherAI/service/memory"
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/embedding"
)

type EpisodeIndex interface {
	EnsureIndex(context.Context) error
	Store(context.Context, episodedao.IndexedEpisode) error
	Delete(context.Context, int64, []string) error
	CountUser(context.Context, int64) (int, error)
	ResetUser(context.Context, int64) error
}

type EpisodeIndexSource interface {
	UpdateIndexStatus(context.Context, int64, string, uint64, model.EpisodeIndexStatus) error
	ListActiveForReindex(context.Context, int64, string, int) ([]model.EpisodeMemory, error)
}

type Indexer struct {
	index     EpisodeIndex
	source    EpisodeIndexSource
	embedder  embedding.Embedder
	dimension int
}

func NewIndexer(index EpisodeIndex, source EpisodeIndexSource, embedder embedding.Embedder, dimension int) (*Indexer, error) {
	if index == nil || source == nil || embedder == nil || dimension <= 0 {
		return nil, fmt.Errorf("episode indexer dependencies are required")
	}
	return &Indexer{index: index, source: source, embedder: embedder, dimension: dimension}, nil
}

// Index publishes the derived Redis view after MySQL has committed. Failure is
// recorded on that exact Episode version and never turns a successful
// projection into another model retry.
func (i *Indexer) Index(ctx context.Context, episode *model.EpisodeMemory) (returnErr error) {
	if episode == nil {
		return nil
	}
	started := time.Now()
	metricStatus := "indexed"
	defer func() {
		if returnErr != nil {
			metricStatus = "failed"
		}
		observability.Memory.Index(metricStatus, time.Since(started), indexLag(started, episode.UpdatedAt))
	}()
	if episode.Status != model.EpisodeStatusActive {
		metricStatus = "deleted"
		return i.index.Delete(ctx, episode.UserID, []string{episode.ID})
	}
	vectors, err := i.embedder.EmbedStrings(ctx, []string{episode.SearchText})
	if err == nil && len(vectors) != 1 {
		err = fmt.Errorf("episode embedder returned %d vectors", len(vectors))
	}
	var vector []float32
	if err == nil {
		vector, err = memoryservice.ToFloat32Vector(vectors[0], i.dimension)
	}
	if err == nil {
		err = i.index.Store(ctx, episodedao.IndexedEpisode{Episode: *episode, Vector: vector})
	}
	status := model.EpisodeIndexIndexed
	if err != nil {
		status = model.EpisodeIndexFailed
	}
	statusCtx, cancelStatus := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelStatus()
	if statusErr := i.source.UpdateIndexStatus(statusCtx, episode.UserID, episode.ID, episode.Version, status); statusErr != nil {
		if err != nil {
			return fmt.Errorf("index episode: %v; mark %s: %w", err, status, statusErr)
		}
		return fmt.Errorf("mark episode %s: %w", status, statusErr)
	}
	if err != nil {
		return fmt.Errorf("index episode: %w", err)
	}
	episode.IndexStatus = status
	return nil
}

func indexLag(started, updatedAt time.Time) time.Duration {
	if updatedAt.IsZero() || updatedAt.After(started) {
		return 0
	}
	return started.Sub(updatedAt)
}

type ReindexReport struct {
	UserID                  int64
	Source, Indexed, Stored int
}

func (r ReindexReport) Complete() bool { return r.Source == r.Indexed && r.Stored == r.Indexed }

func (i *Indexer) Reindex(ctx context.Context, userID int64) (ReindexReport, error) {
	report := ReindexReport{UserID: userID}
	if userID <= 0 {
		return report, fmt.Errorf("reindex requires user id")
	}
	if err := i.index.EnsureIndex(ctx); err != nil {
		return report, err
	}
	var all []model.EpisodeMemory
	for after := ""; ; {
		batch, err := i.source.ListActiveForReindex(ctx, userID, after, 100)
		if err != nil {
			return report, err
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		after = batch[len(batch)-1].ID
	}
	if err := i.index.ResetUser(ctx, userID); err != nil {
		return report, err
	}
	report.Source = len(all)
	for idx := range all {
		if err := i.Index(ctx, &all[idx]); err != nil {
			return report, err
		}
		report.Indexed++
	}
	stored, err := i.index.CountUser(ctx, userID)
	report.Stored = stored
	return report, err
}
