package knowledge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"GopherAI/common/rag"
	"GopherAI/model"
)

type IngestStage string

const (
	IngestStageSourceOpen   IngestStage = "source_open"
	IngestStageSourceRead   IngestStage = "source_read"
	IngestStageSourceVerify IngestStage = "source_verify"
	IngestStageParse        IngestStage = "parse"
	IngestStageChunk        IngestStage = "chunk"
	IngestStageEmbedding    IngestStage = "embedding"
	IngestStageVectorStore  IngestStage = "vector_store"
)

var ErrNoDocumentChunks = errors.New("document produced no chunks")

type IngestError struct {
	Stage IngestStage
	Err   error
}

func (e *IngestError) Error() string { return string(e.Stage) + ": " + e.Err.Error() }
func (e *IngestError) Unwrap() error { return e.Err }

type ChunkBuilder interface {
	Chunk(rag.ChunkInput) ([]rag.Chunk, error)
}

type VectorChunkIndexer interface {
	Index(context.Context, rag.ChunkIndexRequest) (rag.ChunkIndexResult, error)
}

type DocumentIngestor struct {
	storage        FileStorage
	chunker        ChunkBuilder
	indexer        VectorChunkIndexer
	maxSourceBytes int64
}

func NewDocumentIngestor(storage FileStorage, chunker ChunkBuilder, indexer VectorChunkIndexer, maxSourceBytes int64) (*DocumentIngestor, error) {
	if storage == nil || chunker == nil || indexer == nil || maxSourceBytes <= 0 {
		return nil, errors.New("invalid document ingestor dependency or source limit")
	}
	return &DocumentIngestor{storage: storage, chunker: chunker, indexer: indexer, maxSourceBytes: maxSourceBytes}, nil
}

// Ingest always reparses and rechunks the whole new version. The returned
// result carries the reuse statistics; TotalChunks keeps the exact meaning the
// MySQL ChunkCount column has always had.
func (i *DocumentIngestor) Ingest(ctx context.Context, document model.KnowledgeDocument) (rag.ChunkIndexResult, error) {
	source, err := i.storage.Open(document.UserID, document.ID, document.StoragePath)
	if err != nil {
		return rag.ChunkIndexResult{}, ingestError(IngestStageSourceOpen, err)
	}
	defer source.Close()

	content, err := io.ReadAll(io.LimitReader(source, i.maxSourceBytes+1))
	if err != nil {
		return rag.ChunkIndexResult{}, ingestError(IngestStageSourceRead, err)
	}
	if int64(len(content)) > i.maxSourceBytes || int64(len(content)) != document.FileSize || ContentHash(content) != document.ContentHash {
		return rag.ChunkIndexResult{}, ingestError(IngestStageSourceVerify, errors.New("source size or hash does not match metadata"))
	}
	if err := ctx.Err(); err != nil {
		return rag.ChunkIndexResult{}, err
	}

	var blocks []rag.Block
	switch strings.ToLower(strings.TrimPrefix(document.FileType, ".")) {
	case "md":
		blocks = rag.ParseMarkdown(string(content))
	case "txt":
		blocks = rag.ParseText(string(content))
	default:
		return rag.ChunkIndexResult{}, ingestError(IngestStageParse, ErrUnsupportedType)
	}
	chunks, err := i.chunker.Chunk(rag.ChunkInput{
		DocID: document.ID, DocVersion: document.Version,
		SourceFile: document.OriginalName, Blocks: blocks,
	})
	if err != nil {
		return rag.ChunkIndexResult{}, ingestError(IngestStageChunk, err)
	}
	if len(chunks) == 0 {
		return rag.ChunkIndexResult{}, ingestError(IngestStageChunk, ErrNoDocumentChunks)
	}
	// ActiveVersion is the version still answering queries. Handing it to the
	// indexer is what makes reuse possible, and it is also what keeps an
	// explicit reindex (Version == ActiveVersion) a full recomputation.
	result, err := i.indexer.Index(ctx, rag.ChunkIndexRequest{
		UserID: document.UserID, Chunks: chunks, ReuseFromVersion: document.ActiveVersion,
	})
	if err != nil {
		stage := IngestStageVectorStore
		if errors.Is(err, rag.ErrEmbeddingBatch) {
			stage = IngestStageEmbedding
		}
		return rag.ChunkIndexResult{}, ingestError(stage, err)
	}
	return result, nil
}

func ingestError(stage IngestStage, err error) error {
	return &IngestError{Stage: stage, Err: err}
}

// SafeIndexError intentionally persists only a bounded, whitelisted reason.
// External API errors may contain request details and must not be copied into
// MySQL or logs verbatim.
func SafeIndexError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "indexing canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "indexing timed out"
	}
	var staged *IngestError
	if !errors.As(err, &staged) {
		return "indexing operation failed"
	}
	switch {
	case errors.Is(staged.Err, rag.ErrCodeLineTooLong):
		return "chunk: code line exceeds token budget"
	case errors.Is(staged.Err, ErrNoDocumentChunks):
		return "chunk: document produced no searchable content"
	case staged.Stage == IngestStageSourceOpen:
		return "source_open: source file is unavailable"
	case staged.Stage == IngestStageSourceVerify:
		return "source_verify: source file does not match metadata"
	default:
		return fmt.Sprintf("%s: operation failed", staged.Stage)
	}
}
