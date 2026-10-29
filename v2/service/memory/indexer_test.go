package memory

import (
	"context"
	"testing"

	memorydao "GopherAI/dao/memory"
	"GopherAI/model"

	"github.com/cloudwego/eino/components/embedding"
)

type reindexWriter struct {
	resetUser int64
	stored    int
}

func (w *reindexWriter) Store(_ context.Context, items []memorydao.IndexedMemory) error {
	w.stored += len(items)
	return nil
}
func (*reindexWriter) Delete(context.Context, int64, []string) error   { return nil }
func (w *reindexWriter) CountUser(context.Context, int64) (int, error) { return w.stored, nil }
func (w *reindexWriter) ResetUser(_ context.Context, userID int64) error {
	w.resetUser, w.stored = userID, 0
	return nil
}

type reindexSource struct{ rows []model.LongTermMemory }

func (s reindexSource) ListAllActive(context.Context, int64, model.MemoryScope, float64, int) ([]model.LongTermMemory, error) {
	return append([]model.LongTermMemory(nil), s.rows...), nil
}

type reindexEmbedder struct{ calls int }

func (e *reindexEmbedder) EmbedStrings(_ context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	e.calls++
	result := make([][]float64, len(texts))
	for i := range result {
		result[i] = []float64{1, 0}
	}
	return result, nil
}

func TestReindexResetsWholeUserBeforeWritingActiveRows(t *testing.T) {
	t.Parallel()
	writer, embedder := &reindexWriter{stored: 7}, &reindexEmbedder{}
	source := reindexSource{rows: []model.LongTermMemory{{ID: "active", UserID: 42, Scope: model.MemoryScopeTopic, Status: model.LongTermMemoryActive, Subject: "RPC", Content: "length field"}}}
	indexer, err := NewIndexer(writer, embedder, source, 2)
	if err != nil {
		t.Fatal(err)
	}
	report, err := indexer.Reindex(t.Context(), 42, 0.6, 100)
	if err != nil {
		t.Fatal(err)
	}
	if writer.resetUser != 42 || writer.stored != 1 || embedder.calls != 1 || !report.Complete() {
		t.Fatalf("whole-user reset did not precede rebuild: writer=%+v calls=%d report=%+v", writer, embedder.calls, report)
	}
}

func TestReindexEmptySourceStillResetsWholeUser(t *testing.T) {
	t.Parallel()
	writer, embedder := &reindexWriter{stored: 7}, &reindexEmbedder{}
	indexer, _ := NewIndexer(writer, embedder, reindexSource{}, 2)
	report, err := indexer.Reindex(t.Context(), 42, 0.6, 100)
	if err != nil {
		t.Fatal(err)
	}
	if writer.resetUser != 42 || writer.stored != 0 || embedder.calls != 0 || !report.Complete() {
		t.Fatalf("empty source left stale user keys: writer=%+v calls=%d report=%+v", writer, embedder.calls, report)
	}
}
