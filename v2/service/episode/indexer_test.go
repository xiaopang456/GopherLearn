package episode

import (
	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/embedding"
)

func TestIndexLagRejectsMissingAndFutureTimestamps(t *testing.T) {
	started := time.Now()
	if got := indexLag(started, time.Time{}); got != 0 {
		t.Fatalf("zero timestamp lag=%s", got)
	}
	if got := indexLag(started, started.Add(time.Second)); got != 0 {
		t.Fatalf("future timestamp lag=%s", got)
	}
	if got := indexLag(started, started.Add(-3*time.Second)); got != 3*time.Second {
		t.Fatalf("valid lag=%s", got)
	}
}

type testEpisodeIndex struct {
	storeErr error
	stored   int
	reset    bool
}

func (f *testEpisodeIndex) EnsureIndex(context.Context) error { return nil }
func (f *testEpisodeIndex) Store(_ context.Context, _ episodedao.IndexedEpisode) error {
	if f.storeErr != nil {
		return f.storeErr
	}
	f.stored++
	return nil
}
func (f *testEpisodeIndex) Delete(context.Context, int64, []string) error { return nil }
func (f *testEpisodeIndex) CountUser(context.Context, int64) (int, error) { return f.stored, nil }
func (f *testEpisodeIndex) ResetUser(context.Context, int64) error {
	f.reset = true
	f.stored = 0
	return nil
}

type testEpisodeSource struct {
	status model.EpisodeIndexStatus
	rows   []model.EpisodeMemory
}

func (f *testEpisodeSource) UpdateIndexStatus(_ context.Context, _ int64, _ string, _ uint64, s model.EpisodeIndexStatus) error {
	f.status = s
	return nil
}
func (f *testEpisodeSource) ListActiveForReindex(context.Context, int64, string, int) ([]model.EpisodeMemory, error) {
	rows := f.rows
	f.rows = nil
	return rows, nil
}

type testEinoEmbedder struct{ err error }

func (f testEinoEmbedder) EmbedStrings(context.Context, []string, ...embedding.Option) ([][]float64, error) {
	if f.err != nil {
		return nil, f.err
	}
	return [][]float64{{1, 0}}, nil
}

func TestIndexerMarksFailureWithoutLosingEpisode(t *testing.T) {
	index := &testEpisodeIndex{storeErr: errors.New("redis down")}
	source := &testEpisodeSource{}
	indexer, _ := NewIndexer(index, source, testEinoEmbedder{}, 2)
	episode := &model.EpisodeMemory{ID: "e", UserID: 7, SearchText: "rpc", Version: 1, Status: model.EpisodeStatusActive, LastMessageAt: time.Now()}
	if err := indexer.Index(context.Background(), episode); err == nil || source.status != model.EpisodeIndexFailed {
		t.Fatalf("err=%v status=%s", err, source.status)
	}
}

func TestReindexResetsStaleKeysAndReportsFullCoverage(t *testing.T) {
	index := &testEpisodeIndex{stored: 3}
	source := &testEpisodeSource{rows: []model.EpisodeMemory{{ID: "active", UserID: 7, SearchText: "rpc", Version: 1, Status: model.EpisodeStatusActive}}}
	indexer, _ := NewIndexer(index, source, testEinoEmbedder{}, 2)
	report, err := indexer.Reindex(context.Background(), 7)
	if err != nil || !index.reset || !report.Complete() || report.Source != 1 || report.Stored != 1 {
		t.Fatalf("report=%+v reset=%t err=%v", report, index.reset, err)
	}
}
