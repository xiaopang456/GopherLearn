package rag

import (
	"GopherAI/common/stagetiming"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/cloudwego/eino/components/embedding"
)

var (
	ErrInvalidChunkIndexConfig = errors.New("invalid chunk index config")
	ErrInvalidChunkBatch       = errors.New("invalid chunk batch")
	ErrEmbeddingBatch          = errors.New("embed chunk batch")
)

// EmbeddingLimiter is shared by every document execution in one node process.
// The permit is held for the complete provider call, including retry backoff.
type EmbeddingLimiter interface {
	Acquire(context.Context) error
	Release()
}

// EmbeddingSemaphore bounds provider calls at node scope.
type EmbeddingSemaphore struct{ permits chan struct{} }

func NewEmbeddingSemaphore(limit int) (*EmbeddingSemaphore, error) {
	if limit < 1 || limit > 16 {
		return nil, fmt.Errorf("embedding concurrency must be in 1..16")
	}
	return &EmbeddingSemaphore{permits: make(chan struct{}, limit)}, nil
}

func (s *EmbeddingSemaphore) Acquire(ctx context.Context) error {
	select {
	case s.permits <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *EmbeddingSemaphore) Release() { <-s.permits }

// Reuse outcomes. These are the only values that may reach a log line; none of
// them carries document text, a vector, a Redis key, or a full hash.
const (
	ChunkReuseNotApplicable  = "not_applicable"
	ChunkReuseApplied        = "applied"
	ChunkReuseLegacyMetadata = "legacy_metadata"
	ChunkReuseFallback       = "fallback"

	ChunkReuseReasonSourceLoadFailed = "source_load_failed"
	ChunkReuseReasonNoUsableMetadata = "no_usable_metadata"
)

type ChunkIndexConfig struct {
	Dimension int
	BatchSize int
	// EmbeddingModel and EmbeddingContentVersion are the identity half of the
	// vector cache. They are injected from startup configuration rather than
	// read from the embedder, so a silent model swap cannot keep matching old
	// vectors. Credentials and base URLs are deliberately not part of it.
	EmbeddingModel          string
	EmbeddingContentVersion string
	// Limiter is optional; nil preserves the legacy serial behavior.
	Limiter EmbeddingLimiter
}

func (c ChunkIndexConfig) Validate() error {
	if c.Dimension <= 0 {
		return fmt.Errorf("%w: Dimension must be greater than zero", ErrInvalidChunkIndexConfig)
	}
	if c.BatchSize <= 0 {
		return fmt.Errorf("%w: BatchSize must be greater than zero", ErrInvalidChunkIndexConfig)
	}
	if c.EmbeddingModel == "" {
		return fmt.Errorf("%w: EmbeddingModel must not be empty", ErrInvalidChunkIndexConfig)
	}
	if c.EmbeddingContentVersion == "" {
		return fmt.Errorf("%w: EmbeddingContentVersion must not be empty", ErrInvalidChunkIndexConfig)
	}
	return nil
}

type EmbeddedChunk struct {
	UserID int64
	Chunk  Chunk
	Vector []float32
	// EmbeddingInputHash and EmbeddingFingerprint travel with the vector into
	// Redis so the next version of this document can prove reuse is safe.
	EmbeddingInputHash   string
	EmbeddingFingerprint string
}

type ChunkVectorRepository interface {
	EnsureIndex(ctx context.Context) error
	Store(ctx context.Context, chunks []EmbeddedChunk) error
	LoadDocumentVersionEmbeddings(ctx context.Context, userID int64, docID string, version int) ([]StoredChunkEmbedding, error)
}

// ChunkIndexRequest carries the document identity the indexer needs to decide
// whether a reuse source exists. ReuseFromVersion is the document's current
// ActiveVersion, which is still serving retrieval while this new version is
// being built.
type ChunkIndexRequest struct {
	UserID           int64
	Chunks           []Chunk
	ReuseFromVersion int
}

// ChunkIndexResult always satisfies TotalChunks == ReusedChunks + EmbeddedChunks.
type ChunkIndexResult struct {
	TotalChunks    int
	ReusedChunks   int
	EmbeddedChunks int
	ReuseStatus    string
	FallbackReason string
}

// KnowledgeChunkIndexer completes every embedding batch before it calls the
// repository, so an embedding failure cannot leave Redis chunks behind.
type KnowledgeChunkIndexer struct {
	embedder    embedding.Embedder
	repository  ChunkVectorRepository
	config      ChunkIndexConfig
	fingerprint string
}

func NewKnowledgeChunkIndexer(embedder embedding.Embedder, repository ChunkVectorRepository, config ChunkIndexConfig) (*KnowledgeChunkIndexer, error) {
	if embedder == nil {
		return nil, fmt.Errorf("%w: Embedder must not be nil", ErrInvalidChunkIndexConfig)
	}
	if repository == nil {
		return nil, fmt.Errorf("%w: Repository must not be nil", ErrInvalidChunkIndexConfig)
	}
	if config.EmbeddingContentVersion == "" {
		config.EmbeddingContentVersion = EmbeddingContentVersion
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &KnowledgeChunkIndexer{
		embedder: embedder, repository: repository, config: config,
		fingerprint: EmbeddingFingerprint(config.EmbeddingModel, config.Dimension, config.EmbeddingContentVersion),
	}, nil
}

// Index rebuilds one complete document version. Chunking is never incremental:
// only the vector of a chunk whose embedder input is byte-identical to a chunk
// of the current ActiveVersion is carried over. Everything else — chunk IDs,
// line numbers, Redis keys, metadata — belongs to the new version.
func (i *KnowledgeChunkIndexer) Index(ctx context.Context, request ChunkIndexRequest) (ChunkIndexResult, error) {
	if err := validateIndexBatch(request.UserID, request.Chunks); err != nil {
		return ChunkIndexResult{}, err
	}
	if err := i.repository.EnsureIndex(ctx); err != nil {
		return ChunkIndexResult{}, fmt.Errorf("ensure shared chunk index: %w", err)
	}

	chunks := request.Chunks
	result := ChunkIndexResult{TotalChunks: len(chunks), ReuseStatus: ChunkReuseNotApplicable}
	embedded := make([]EmbeddedChunk, len(chunks))
	inputHashes := make([]string, len(chunks))
	for index, chunk := range chunks {
		inputHashes[index] = EmbeddingInputHash(chunk.EmbeddingContent)
		embedded[index] = EmbeddedChunk{
			UserID: request.UserID, Chunk: chunk,
			EmbeddingInputHash: inputHashes[index], EmbeddingFingerprint: i.fingerprint,
		}
	}

	pending := make([]int, 0, len(chunks))
	candidates, status, reason := i.loadReuseCandidates(ctx, request)
	result.ReuseStatus, result.FallbackReason = status, reason
	if err := ctx.Err(); err != nil {
		return ChunkIndexResult{}, err
	}
	for index, chunk := range chunks {
		vector, reused := reuseVector(candidates, i.fingerprint, inputHashes[index], chunk.EmbeddingContent)
		if !reused {
			pending = append(pending, index)
			continue
		}
		embedded[index].Vector = vector
		result.ReusedChunks++
	}
	result.EmbeddedChunks = len(pending)

	type batchWork struct {
		start int
		end   int
		index []int
	}
	var batches []batchWork
	for start := 0; start < len(pending); start += i.config.BatchSize {
		end := start + i.config.BatchSize
		if end > len(pending) {
			end = len(pending)
		}
		batches = append(batches, batchWork{start: start, end: end, index: pending[start:end]})
	}
	processBatch := func(batchCtx context.Context, work batchWork) error {
		texts := make([]string, len(work.index))
		for offset, index := range work.index {
			texts[offset] = chunks[index].EmbeddingContent
		}
		vectors, err := i.embedder.EmbedStrings(batchCtx, texts)
		if err != nil {
			return fmt.Errorf("%w %d-%d: %w", ErrEmbeddingBatch, work.start, work.end-1, err)
		}
		if len(vectors) != len(texts) {
			return fmt.Errorf("%w: batch %d-%d returned %d vectors for %d chunks", ErrEmbeddingBatch, work.start, work.end-1, len(vectors), len(texts))
		}
		convertedVectors := make([][]float32, len(vectors))
		for offset, vector := range vectors {
			converted, err := validateAndConvertVector(vector, i.config.Dimension)
			if err != nil {
				return fmt.Errorf("%w: chunk %q: %v", ErrEmbeddingBatch, chunks[work.index[offset]].ID, err)
			}
			convertedVectors[offset] = converted
		}
		for offset, vector := range convertedVectors {
			embedded[work.index[offset]].Vector = vector
		}
		return nil
	}

	runBatches := func() error {
		if i.config.Limiter == nil {
			// Preserve the original call order and serial behavior for legacy callers.
			for _, work := range batches {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := processBatch(ctx, work); err != nil {
					return err
				}
			}
			return nil
		}

		batchCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		var wg sync.WaitGroup
		var firstErr error
		var errOnce sync.Once
		setBatchError := func(err error) {
			errOnce.Do(func() {
				firstErr = err
				cancel()
			})
		}
		for _, work := range batches {
			work := work
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := i.config.Limiter.Acquire(batchCtx); err != nil {
					return
				}
				defer i.config.Limiter.Release()
				if batchCtx.Err() != nil {
					return
				}
				if err := processBatch(batchCtx, work); err != nil {
					setBatchError(err)
				}
			}()
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return err
		}
		return firstErr
	}
	var batchErr error
	if len(batches) > 0 {
		embedDone := stagetiming.From(ctx).Start("embed")
		batchErr = runBatches()
		embedDone()
	} else {
		batchErr = ctx.Err()
	}
	// Keep the legacy serial error precedence: an embedder error is returned
	// directly even if the parent context is cancelled immediately afterward.
	if batchErr != nil && i.config.Limiter == nil {
		return ChunkIndexResult{}, batchErr
	}
	if err := ctx.Err(); err != nil {
		return ChunkIndexResult{}, err
	}
	if batchErr != nil {
		return ChunkIndexResult{}, batchErr
	}

	if err := i.repository.Store(ctx, embedded); err != nil {
		return ChunkIndexResult{}, fmt.Errorf("store embedded chunks: %w", err)
	}
	return result, nil
}

// loadReuseCandidates returns nil candidates whenever reuse is not provably
// safe. Reuse is a cost optimisation, so every uncertain path degrades to a
// full recomputation instead of failing the update.
func (i *KnowledgeChunkIndexer) loadReuseCandidates(ctx context.Context, request ChunkIndexRequest) (map[string][]StoredChunkEmbedding, string, string) {
	docID, newVersion := request.Chunks[0].DocID, request.Chunks[0].DocVersion
	// A first upload has no ActiveVersion, and an explicit reindex runs at
	// Version == ActiveVersion. Reindex must recompute: it is the only tool a
	// user has for repairing damaged vectors, so a cache hit would defeat it.
	if request.ReuseFromVersion <= 0 || newVersion <= request.ReuseFromVersion {
		return nil, ChunkReuseNotApplicable, ""
	}
	stored, err := i.repository.LoadDocumentVersionEmbeddings(ctx, request.UserID, docID, request.ReuseFromVersion)
	if err != nil {
		// The bounded reason is all that may be observed. A cancelled context is
		// not swallowed here: Index re-checks ctx.Err() before it embeds anything.
		return nil, ChunkReuseFallback, ChunkReuseReasonSourceLoadFailed
	}
	candidates := make(map[string][]StoredChunkEmbedding, len(stored))
	for _, record := range stored {
		// The repository already checks these against the stored HASH. Checking
		// again here means no repository or test double can hand this indexer a
		// vector belonging to another tenant, document, or version.
		if record.UserID != request.UserID || record.DocID != docID || record.DocVersion != request.ReuseFromVersion {
			continue
		}
		if record.EmbeddingFingerprint != i.fingerprint || record.EmbeddingInputHash == "" || record.EmbeddingContent == "" {
			continue
		}
		// Dimension and finite values are re-checked here too. The Redis loader
		// already drops damaged records, but a reused vector goes straight into
		// Redis without passing validateAndConvertVector, so this is the only
		// place that guards a non-Redis repository implementation.
		if !usableReuseVector(record.Vector, i.config.Dimension) {
			continue
		}
		key := reuseCandidateKey(record.EmbeddingFingerprint, record.EmbeddingInputHash)
		candidates[key] = append(candidates[key], record)
	}
	if len(candidates) == 0 {
		return nil, ChunkReuseLegacyMetadata, ChunkReuseReasonNoUsableMetadata
	}
	return candidates, ChunkReuseApplied, ""
}

// reuseVector never trusts the hash alone. Two chunks may only share a vector
// after their stored embedder input compares byte for byte, which also makes
// duplicate chunks inside one document reuse the same vector deterministically.
func reuseVector(candidates map[string][]StoredChunkEmbedding, fingerprint, inputHash, embeddingContent string) ([]float32, bool) {
	if len(candidates) == 0 {
		return nil, false
	}
	for _, record := range candidates[reuseCandidateKey(fingerprint, inputHash)] {
		if record.EmbeddingContent != embeddingContent {
			continue
		}
		return append([]float32(nil), record.Vector...), true
	}
	return nil, false
}

func usableReuseVector(vector []float32, dimension int) bool {
	if len(vector) != dimension {
		return false
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
	}
	return true
}

func reuseCandidateKey(fingerprint, inputHash string) string {
	return fingerprint + "\x1e" + inputHash
}

func validateIndexBatch(userID int64, chunks []Chunk) error {
	if userID <= 0 {
		return fmt.Errorf("%w: UserID must be greater than zero", ErrInvalidChunkBatch)
	}
	if len(chunks) == 0 {
		return fmt.Errorf("%w: at least one chunk is required", ErrInvalidChunkBatch)
	}
	docID, version := chunks[0].DocID, chunks[0].DocVersion
	if docID == "" || version <= 0 {
		return fmt.Errorf("%w: document identity is missing", ErrInvalidChunkBatch)
	}
	seen := make(map[string]struct{}, len(chunks))
	for position, chunk := range chunks {
		if chunk.DocID != docID || chunk.DocVersion != version {
			return fmt.Errorf("%w: all chunks must belong to one document version", ErrInvalidChunkBatch)
		}
		if chunk.ID == "" || chunk.EmbeddingContent == "" {
			return fmt.Errorf("%w: chunk %d is missing ID or embedding content", ErrInvalidChunkBatch, position)
		}
		if chunk.Index != position {
			return fmt.Errorf("%w: chunk %q index is %d, want %d", ErrInvalidChunkBatch, chunk.ID, chunk.Index, position)
		}
		if _, exists := seen[chunk.ID]; exists {
			return fmt.Errorf("%w: duplicate chunk ID %q", ErrInvalidChunkBatch, chunk.ID)
		}
		seen[chunk.ID] = struct{}{}
	}
	return nil
}

func validateAndConvertVector(vector []float64, dimension int) ([]float32, error) {
	if len(vector) != dimension {
		return nil, fmt.Errorf("vector dimension is %d, want %d", len(vector), dimension)
	}
	converted := make([]float32, len(vector))
	for index, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("vector value %d is not finite", index)
		}
		converted[index] = float32(value)
		if math.IsInf(float64(converted[index]), 0) {
			return nil, fmt.Errorf("vector value %d overflows FLOAT32", index)
		}
	}
	return converted, nil
}
