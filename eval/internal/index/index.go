// Package index builds the evaluation's index through the product's own
// ingestion path.
//
// Every corpus document is handed to knowledge.DocumentIngestor, the component
// the application uses when a user uploads a file: it verifies the source, parses
// it, chunks it with rag.Chunker and indexes it through
// rag.KnowledgeChunkIndexer. The evaluation therefore searches the chunks the
// product would have produced, not a re-implementation of them.
package index

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/cloudwego/eino/components/embedding"

	"GopherAI/common/rag"
	"GopherAI/model"
	"GopherAI/service/knowledge"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// DefaultBatchSize is the product's embedding batch size (config.go, "BatchSize:10"
// in the knowledge worker's indexer).
const DefaultBatchSize = 10

// documentVersion is the version every corpus document is indexed as: a first
// upload, so no vector is reused from an earlier version.
const documentVersion = 1

// Params describes one indexing run.
type Params struct {
	UserID         int64
	Corpus         *dataset.Corpus
	Embedder       embedding.Embedder
	Repository     rag.ChunkVectorRepository
	Dimension      int
	EmbeddingModel string
	Chunker        rag.ChunkerConfig
	// BatchSize is the number of chunks per embedding call; zero means DefaultBatchSize.
	BatchSize int
	// MaxSourceBytes is the largest document the ingestor accepts. The product's
	// limit applies, so a document the product would refuse is refused here too.
	MaxSourceBytes int64
}

// Document is one indexed corpus document.
type Document struct {
	// File is the manifest name, the name evidence spans use.
	File string
	// DocID is the identifier the chunks are stored under.
	DocID string
	// Result is what the indexer reported for the document.
	Result rag.ChunkIndexResult
}

// Index is a built index: which documents exist and how to map results back.
type Index struct {
	UserID    int64
	Documents []Document

	fileByID map[string]string
}

// DocID returns the identifier a manifest file is stored under. It is a pure
// function of the file name, so two runs over the same corpus agree.
func DocID(file string) string {
	sum := sha256.Sum256([]byte(file))
	return "eval-" + hex.EncodeToString(sum[:8])
}

// FileOf maps a stored document id back to the manifest file name.
func (ix *Index) FileOf(docID string) (string, bool) {
	file, ok := ix.fileByID[docID]
	return file, ok
}

// Loader returns the active-document loader the retrieval service needs. Every
// corpus document is active, at its single version, for the evaluation user.
func (ix *Index) Loader() rag.ActiveDocumentLoader {
	return func(_ context.Context, userID int64) ([]rag.ActiveDocument, error) {
		if userID != ix.UserID {
			return nil, nil
		}
		active := make([]rag.ActiveDocument, len(ix.Documents))
		for i, doc := range ix.Documents {
			active[i] = rag.ActiveDocument{DocID: doc.DocID, Version: documentVersion, OriginalName: doc.File}
		}
		return active, nil
	}
}

// Attach describes an index that already exists in the backend: the documents of
// the corpus under their deterministic ids, with no ingestion. A run that reuses
// an index built earlier (same fingerprint) uses it.
func Attach(userID int64, corpus *dataset.Corpus) *Index {
	ix := &Index{UserID: userID, fileByID: map[string]string{}}
	for _, doc := range corpus.Documents {
		id := DocID(doc.File)
		ix.Documents = append(ix.Documents, Document{File: doc.File, DocID: id})
		ix.fileByID[id] = doc.File
	}
	return ix
}

// Chunks is the total number of chunks indexed.
func (ix *Index) Chunks() int {
	total := 0
	for _, doc := range ix.Documents {
		total += doc.Result.TotalChunks
	}
	return total
}

// Build ingests every corpus document, in manifest order.
func Build(ctx context.Context, p Params) (*Index, error) {
	if p.UserID <= 0 || p.Corpus == nil || p.Embedder == nil || p.Repository == nil {
		return nil, errors.New("index: user, corpus, embedder and repository are required")
	}
	batch := p.BatchSize
	if batch == 0 {
		batch = DefaultBatchSize
	}
	chunker, err := rag.NewChunker(p.Chunker)
	if err != nil {
		return nil, err
	}
	indexer, err := rag.NewKnowledgeChunkIndexer(p.Embedder, p.Repository, rag.ChunkIndexConfig{
		Dimension: p.Dimension, BatchSize: batch,
		EmbeddingModel: p.EmbeddingModel, EmbeddingContentVersion: rag.EmbeddingContentVersion,
	})
	if err != nil {
		return nil, err
	}
	return ingestAll(ctx, p, chunker, indexer)
}

// ChunkPlan is the offline result of chunking the corpus without embedding it.
type ChunkPlan struct {
	// Chunks is the number of chunks per document, in manifest order.
	Chunks []DocumentChunks
	// Total is the sum of Chunks.
	Total int
	// EmbeddingCalls is how many embedding calls indexing will make: the product
	// embeds BatchSize chunks per call, per document.
	EmbeddingCalls int
}

// DocumentChunks is one document's chunk count.
type DocumentChunks struct {
	File   string
	Chunks int
}

// Plan chunks the corpus exactly as Build would, through the same ingestor, and
// counts instead of embedding. It touches no service.
func Plan(ctx context.Context, userID int64, corpus *dataset.Corpus, chunkerConfig rag.ChunkerConfig, batchSize int, maxSourceBytes int64) (ChunkPlan, error) {
	if batchSize == 0 {
		batchSize = DefaultBatchSize
	}
	chunker, err := rag.NewChunker(chunkerConfig)
	if err != nil {
		return ChunkPlan{}, err
	}
	counter := &countingIndexer{}
	p := Params{UserID: userID, Corpus: corpus, MaxSourceBytes: maxSourceBytes}
	if _, err := ingestAll(ctx, p, chunker, counter); err != nil {
		return ChunkPlan{}, err
	}
	var plan ChunkPlan
	for i, doc := range corpus.Documents {
		n := counter.counts[i]
		plan.Chunks = append(plan.Chunks, DocumentChunks{File: doc.File, Chunks: n})
		plan.Total += n
		plan.EmbeddingCalls += (n + batchSize - 1) / batchSize
	}
	return plan, nil
}

// countingIndexer records chunk counts and embeds nothing.
type countingIndexer struct{ counts []int }

func (c *countingIndexer) Index(_ context.Context, request rag.ChunkIndexRequest) (rag.ChunkIndexResult, error) {
	c.counts = append(c.counts, len(request.Chunks))
	return rag.ChunkIndexResult{TotalChunks: len(request.Chunks), EmbeddedChunks: len(request.Chunks), ReuseStatus: rag.ChunkReuseNotApplicable}, nil
}

func ingestAll(ctx context.Context, p Params, chunker knowledge.ChunkBuilder, indexer knowledge.VectorChunkIndexer) (*Index, error) {
	if p.UserID <= 0 || p.Corpus == nil {
		return nil, errors.New("index: user and corpus are required")
	}
	sources := &corpusStorage{files: map[string]string{}}
	for _, doc := range p.Corpus.Documents {
		sources.files[doc.File] = doc.Path
	}
	maxBytes := p.MaxSourceBytes
	if maxBytes <= 0 {
		return nil, errors.New("index: MaxSourceBytes must be positive")
	}
	ingestor, err := knowledge.NewDocumentIngestor(sources, chunker, indexer, maxBytes)
	if err != nil {
		return nil, err
	}

	ix := &Index{UserID: p.UserID, fileByID: map[string]string{}}
	for _, doc := range p.Corpus.Documents {
		content, err := os.ReadFile(doc.Path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", doc.File, err)
		}
		fileType := strings.TrimPrefix(strings.ToLower(path.Ext(doc.File)), ".")
		docID := DocID(doc.File)
		result, err := ingestor.Ingest(ctx, model.KnowledgeDocument{
			ID: docID, UserID: p.UserID, OriginalName: doc.File, StoragePath: doc.File,
			FileType: fileType, ContentHash: knowledge.ContentHash(content), FileSize: int64(len(content)),
			Version: documentVersion, ActiveVersion: 0,
		})
		if err != nil {
			return nil, fmt.Errorf("index %s: %w", doc.File, err)
		}
		ix.Documents = append(ix.Documents, Document{File: doc.File, DocID: docID, Result: result})
		ix.fileByID[docID] = doc.File
	}
	return ix, nil
}

// corpusStorage serves corpus files to the ingestor. Only Open is real: the
// evaluation never uploads, publishes or deletes a document.
type corpusStorage struct{ files map[string]string }

var _ knowledge.FileStorage = (*corpusStorage)(nil)

func (s *corpusStorage) Open(_ int64, _ string, storagePath string) (io.ReadCloser, error) {
	file, ok := s.files[storagePath]
	if !ok {
		return nil, fmt.Errorf("corpus has no document %q", storagePath)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

var errReadOnly = errors.New("the evaluation corpus is read-only")

func (*corpusStorage) SaveTemp(context.Context, int64, string, io.Reader) (knowledge.StoredFile, error) {
	return knowledge.StoredFile{}, errReadOnly
}
func (*corpusStorage) DiscardTemp(string, int64, string) error { return errReadOnly }
func (*corpusStorage) Publish(string, int64, string, int, string) (string, error) {
	return "", errReadOnly
}
func (*corpusStorage) Delete(int64, string, string) error { return errReadOnly }
func (*corpusStorage) DeleteDocument(int64, string) error { return errReadOnly }
