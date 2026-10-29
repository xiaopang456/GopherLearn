package knowledge

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"
)

func TestDocumentIngestorRunsParserChunkerAndIndexer(t *testing.T) {
	t.Parallel()

	document, storage := storedIngestDocument(t, "doc-ingest", "rabbit.md", "md", []byte("# RabbitMQ\r\n\r\n## Ack\r\n解释。\r\n\r\n```go\r\nmsg.Ack(false)\r\n```\r\n"))
	chunker := mustServiceChunker(t)
	indexer := &captureVectorIndexer{}
	ingestor, err := NewDocumentIngestor(storage, chunker, indexer, 1024)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ingestor.Ingest(context.Background(), document)
	if err != nil {
		t.Fatalf("Ingest() error: %v", err)
	}
	if result.TotalChunks != 1 || indexer.userID != document.UserID || len(indexer.chunks) != 1 {
		t.Fatalf("ingest count/user/chunks = %d/%d/%d", result.TotalChunks, indexer.userID, len(indexer.chunks))
	}
	chunk := indexer.chunks[0]
	if chunk.Type != rag.ChunkTypeMixed || chunk.CodeLanguage != "go" || chunk.DocID != document.ID || chunk.DocVersion != 1 {
		t.Fatalf("indexed chunk = %+v", chunk)
	}
}

func TestDocumentIngestorRejectsTamperedSourceBeforeIndex(t *testing.T) {
	t.Parallel()

	document, storage := storedIngestDocument(t, "doc-tamper", "redis.txt", "txt", []byte("original"))
	file, err := storage.Open(document.UserID, document.ID, document.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	path, err := storage.resolveDocumentRelative(document.UserID, document.ID, document.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	indexer := &captureVectorIndexer{}
	ingestor, _ := NewDocumentIngestor(storage, mustServiceChunker(t), indexer, 1024)
	_, err = ingestor.Ingest(context.Background(), document)
	var staged *IngestError
	if !errors.As(err, &staged) || staged.Stage != IngestStageSourceVerify || len(indexer.chunks) != 0 {
		t.Fatalf("tamper result = stage %v err %v indexed %d", staged, err, len(indexer.chunks))
	}
}

func TestDocumentIngestorClassifiesEmbeddingAndStoresSafeMessage(t *testing.T) {
	t.Parallel()

	document, storage := storedIngestDocument(t, "doc-secret", "gin.md", "md", []byte("# Gin\ncontent\n"))
	secret := "Bearer sk-secret-key document-private-content"
	indexer := &captureVectorIndexer{err: errors.Join(rag.ErrEmbeddingBatch, errors.New(secret))}
	ingestor, _ := NewDocumentIngestor(storage, mustServiceChunker(t), indexer, 1024)
	_, err := ingestor.Ingest(context.Background(), document)
	var staged *IngestError
	if !errors.As(err, &staged) || staged.Stage != IngestStageEmbedding {
		t.Fatalf("embedding stage = %+v, err %v", staged, err)
	}
	safe := SafeIndexError(err)
	if safe != "embedding: operation failed" || strings.Contains(safe, "secret") || strings.Contains(safe, "private") {
		t.Fatalf("unsafe persisted error %q", safe)
	}
}

func TestDocumentIngestorRejectsHeadingOnlyDocument(t *testing.T) {
	t.Parallel()

	document, storage := storedIngestDocument(t, "doc-empty-chunks", "empty.md", "md", []byte("# Only heading\n"))
	ingestor, _ := NewDocumentIngestor(storage, mustServiceChunker(t), &captureVectorIndexer{}, 1024)
	_, err := ingestor.Ingest(context.Background(), document)
	if !errors.Is(err, ErrNoDocumentChunks) || SafeIndexError(err) != "chunk: document produced no searchable content" {
		t.Fatalf("heading-only error/safe = %v/%q", err, SafeIndexError(err))
	}
}

// TestDocumentIngestorPassesActiveVersionAsReuseSource is the plumbing that
// decides between incremental reuse and a full recomputation. Getting it wrong
// silently disables the optimisation, or worse, disables reindex repair.
func TestDocumentIngestorPassesActiveVersionAsReuseSource(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		documentID    string
		version       int
		activeVersion int
	}{
		{"first upload has no reuse source", "doc-reuse-first", 1, 0},
		{"content update reuses the active version", "doc-reuse-update", 3, 2},
		{"explicit reindex reuses nothing new", "doc-reuse-reindex", 2, 2},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			document, storage := storedIngestDocument(t, testCase.documentID, "reuse.md", "md", []byte("# Redis\n\n## TTL\n过期时间说明。\n"))
			document.Version = testCase.version
			document.ActiveVersion = testCase.activeVersion
			indexer := &captureVectorIndexer{}
			ingestor, err := NewDocumentIngestor(storage, mustServiceChunker(t), indexer, 1024)
			if err != nil {
				t.Fatal(err)
			}
			result, err := ingestor.Ingest(context.Background(), document)
			if err != nil {
				t.Fatalf("Ingest() error: %v", err)
			}
			if indexer.reuseFromVersion != testCase.activeVersion {
				t.Fatalf("reuse source version = %d, want %d", indexer.reuseFromVersion, testCase.activeVersion)
			}
			if len(indexer.chunks) == 0 || indexer.chunks[0].DocVersion != testCase.version {
				t.Fatalf("indexed chunks belong to version %d, want %d", indexer.chunks[0].DocVersion, testCase.version)
			}
			// The chunk count reported to MySQL keeps its old meaning: every
			// chunk of the new version, reused or not.
			if result.TotalChunks != len(indexer.chunks) {
				t.Fatalf("TotalChunks = %d, want %d", result.TotalChunks, len(indexer.chunks))
			}
		})
	}
}

type captureVectorIndexer struct {
	userID           int64
	chunks           []rag.Chunk
	reuseFromVersion int
	calls            int
	err              error
}

func (c *captureVectorIndexer) Index(_ context.Context, request rag.ChunkIndexRequest) (rag.ChunkIndexResult, error) {
	c.calls++
	c.userID = request.UserID
	c.reuseFromVersion = request.ReuseFromVersion
	c.chunks = append([]rag.Chunk(nil), request.Chunks...)
	if c.err != nil {
		return rag.ChunkIndexResult{}, c.err
	}
	return rag.ChunkIndexResult{
		TotalChunks: len(request.Chunks), EmbeddedChunks: len(request.Chunks),
		ReuseStatus: rag.ChunkReuseNotApplicable,
	}, nil
}

func storedIngestDocument(t *testing.T, id, name, fileType string, content []byte) (model.KnowledgeDocument, *LocalFileStorage) {
	t.Helper()
	storage, err := NewLocalFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := storage.SaveTemp(context.Background(), 73, id, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	path, err := storage.Publish(stored.TempPath, 73, id, 1, fileType)
	if err != nil {
		t.Fatal(err)
	}
	return model.KnowledgeDocument{
		ID: id, UserID: 73, OriginalName: name, StoragePath: path, FileType: fileType,
		ContentHash: stored.ContentHash, FileSize: stored.FileSize, Version: 1,
		IndexStatus: model.KnowledgeStatusPending,
	}, storage
}

func mustServiceChunker(t *testing.T) *rag.Chunker {
	t.Helper()
	chunker, err := rag.NewChunker(rag.DefaultChunkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	return chunker
}
