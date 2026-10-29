package rag

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/cloudwego/eino/components/embedding"
)

func TestKnowledgeChunkIndexerEmbedsAllBatchesBeforeOneStore(t *testing.T) {
	t.Parallel()

	chunks := testIndexChunks(t, "doc-index", 5)
	embedder := &fakeChunkEmbedder{dimension: 4}
	repository := &fakeChunkVectorRepository{}
	indexer := mustKnowledgeChunkIndexer(t, embedder, repository, testIndexConfig(4, 2))

	result, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 42, Chunks: chunks})
	if err != nil {
		t.Fatalf("Index() error: %v", err)
	}
	if result.TotalChunks != 5 || result.ReusedChunks != 0 || result.EmbeddedChunks != 5 || result.ReuseStatus != ChunkReuseNotApplicable {
		t.Fatalf("first upload result = %+v", result)
	}
	if !repository.ensureCalled || repository.storeCalls != 1 {
		t.Fatalf("repository calls ensure=%v store=%d", repository.ensureCalled, repository.storeCalls)
	}
	if got, want := embedder.batchSizes, []int{2, 2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("embedding batches = %v, want %v", got, want)
	}
	if len(repository.stored) != 5 {
		t.Fatalf("stored %d chunks, want 5", len(repository.stored))
	}
	for index, stored := range repository.stored {
		if stored.UserID != 42 || stored.Chunk.ID != chunks[index].ID || len(stored.Vector) != 4 {
			t.Errorf("stored[%d] = %+v", index, stored)
		}
	}
}

func TestKnowledgeChunkIndexerEmbeddingFailureWritesNothing(t *testing.T) {
	t.Parallel()

	embedder := &fakeChunkEmbedder{dimension: 4, failCall: 2}
	repository := &fakeChunkVectorRepository{}
	indexer := mustKnowledgeChunkIndexer(t, embedder, repository, testIndexConfig(4, 2))
	_, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 42, Chunks: testIndexChunks(t, "doc-fail", 5)})
	if !errors.Is(err, ErrEmbeddingBatch) {
		t.Fatalf("Index() error = %v, want ErrEmbeddingBatch", err)
	}
	if repository.storeCalls != 0 || len(repository.stored) != 0 {
		t.Fatalf("embedding failure wrote repository data: calls=%d chunks=%d", repository.storeCalls, len(repository.stored))
	}
}

func TestKnowledgeChunkIndexerRejectsInvalidVectorBeforeStore(t *testing.T) {
	t.Parallel()

	embedder := &fakeChunkEmbedder{dimension: 3}
	repository := &fakeChunkVectorRepository{}
	indexer := mustKnowledgeChunkIndexer(t, embedder, repository, testIndexConfig(4, 2))
	_, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 42, Chunks: testIndexChunks(t, "doc-dimension", 2)})
	if !errors.Is(err, ErrEmbeddingBatch) || repository.storeCalls != 0 {
		t.Fatalf("dimension error/store calls = %v/%d", err, repository.storeCalls)
	}
}

func TestKnowledgeChunkIndexerPropagatesEnsureAndStoreFailures(t *testing.T) {
	t.Parallel()

	chunks := testIndexChunks(t, "doc-repository", 1)
	ensureFailure := errors.New("redis unavailable")
	repository := &fakeChunkVectorRepository{ensureErr: ensureFailure}
	indexer := mustKnowledgeChunkIndexer(t, &fakeChunkEmbedder{dimension: 4}, repository, testIndexConfig(4, 2))
	if _, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 42, Chunks: chunks}); !errors.Is(err, ensureFailure) {
		t.Fatalf("ensure failure = %v", err)
	}
	if repository.storeCalls != 0 {
		t.Fatalf("store called after ensure failure")
	}

	storeFailure := errors.New("transaction failed")
	repository = &fakeChunkVectorRepository{storeErr: storeFailure}
	indexer = mustKnowledgeChunkIndexer(t, &fakeChunkEmbedder{dimension: 4}, repository, testIndexConfig(4, 2))
	if _, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 42, Chunks: chunks}); !errors.Is(err, storeFailure) {
		t.Fatalf("store failure = %v", err)
	}
}

func TestKnowledgeChunkIndexerValidatesOneOrderedDocumentVersion(t *testing.T) {
	t.Parallel()

	chunks := testIndexChunks(t, "doc-valid", 2)
	chunks[1].DocID = "another-doc"
	repository := &fakeChunkVectorRepository{}
	indexer := mustKnowledgeChunkIndexer(t, &fakeChunkEmbedder{dimension: 4}, repository, testIndexConfig(4, 2))
	if _, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 42, Chunks: chunks}); !errors.Is(err, ErrInvalidChunkBatch) {
		t.Fatalf("mixed document error = %v", err)
	}
	if repository.ensureCalled {
		t.Fatalf("invalid batch reached repository")
	}
	if _, err := indexer.Index(context.Background(), ChunkIndexRequest{UserID: 42}); !errors.Is(err, ErrInvalidChunkBatch) {
		t.Fatalf("empty batch error = %v, want ErrInvalidChunkBatch", err)
	}
}

type fakeChunkEmbedder struct {
	dimension  int
	failCall   int
	calls      int
	batchSizes []int
}

func (f *fakeChunkEmbedder) EmbedStrings(_ context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	f.calls++
	f.batchSizes = append(f.batchSizes, len(texts))
	if f.failCall == f.calls {
		return nil, errors.New("fake embedding failure")
	}
	vectors := make([][]float64, len(texts))
	for index := range vectors {
		vectors[index] = make([]float64, f.dimension)
		for dimension := range vectors[index] {
			vectors[index][dimension] = float64(index + dimension + 1)
		}
	}
	return vectors, nil
}

type fakeChunkVectorRepository struct {
	ensureCalled bool
	ensureErr    error
	storeErr     error
	storeCalls   int
	stored       []EmbeddedChunk

	loadCalls  int
	loadErr    error
	loadResult []StoredChunkEmbedding
	loadedFrom []loadedVersionKey
}

type loadedVersionKey struct {
	userID  int64
	docID   string
	version int
}

func (f *fakeChunkVectorRepository) LoadDocumentVersionEmbeddings(_ context.Context, userID int64, docID string, version int) ([]StoredChunkEmbedding, error) {
	f.loadCalls++
	f.loadedFrom = append(f.loadedFrom, loadedVersionKey{userID, docID, version})
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return append([]StoredChunkEmbedding(nil), f.loadResult...), nil
}

func (f *fakeChunkVectorRepository) EnsureIndex(context.Context) error {
	f.ensureCalled = true
	return f.ensureErr
}

func (f *fakeChunkVectorRepository) Store(_ context.Context, chunks []EmbeddedChunk) error {
	f.storeCalls++
	if f.storeErr != nil {
		return f.storeErr
	}
	f.stored = append([]EmbeddedChunk(nil), chunks...)
	return nil
}

func mustKnowledgeChunkIndexer(t *testing.T, embedder embedding.Embedder, repository ChunkVectorRepository, config ChunkIndexConfig) *KnowledgeChunkIndexer {
	t.Helper()
	indexer, err := NewKnowledgeChunkIndexer(embedder, repository, config)
	if err != nil {
		t.Fatalf("NewKnowledgeChunkIndexer() error: %v", err)
	}
	return indexer
}

func testIndexChunks(t *testing.T, docID string, count int) []Chunk {
	t.Helper()
	markdown := "# 索引测试\n\n"
	for index := 0; index < count; index++ {
		markdown += fmt.Sprintf("## 第%d节\n内容%d\n", index, index)
	}
	chunker := mustChunker(t, DefaultChunkerConfig())
	chunks, err := chunker.Chunk(ChunkInput{DocID: docID, DocVersion: 1, SourceFile: docID + ".md", Blocks: ParseMarkdown(markdown)})
	if err != nil {
		t.Fatalf("prepare chunks: %v", err)
	}
	if len(chunks) != count {
		t.Fatalf("prepared %d chunks, want %d", len(chunks), count)
	}
	return chunks
}

func testIndexConfig(dimension, batchSize int) ChunkIndexConfig {
	return ChunkIndexConfig{
		Dimension: dimension, BatchSize: batchSize,
		EmbeddingModel: "test-embedding-model", EmbeddingContentVersion: EmbeddingContentVersion,
	}
}
