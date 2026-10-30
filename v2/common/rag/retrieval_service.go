package rag

import (
	"GopherAI/common/observability"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/embedding"
)

const defaultRetrievalTimeout = 30 * time.Second

var (
	ErrInvalidRetrievalRequest = errors.New("invalid RAG retrieval request")
	ErrRetrievalUnavailable    = errors.New("RAG retrieval service unavailable")
	ErrRetrievalFailed         = errors.New("RAG retrieval failed")
)

type RetrievalUnavailableReason string

const (
	RetrievalReasonEmbedding        RetrievalUnavailableReason = "embedding_unavailable"
	RetrievalReasonVector           RetrievalUnavailableReason = "vector_unavailable"
	RetrievalReasonLexical          RetrievalUnavailableReason = "lexical_unavailable"
	RetrievalReasonIndex            RetrievalUnavailableReason = "index_unavailable"
	RetrievalReasonDocumentMetadata RetrievalUnavailableReason = "document_metadata_unavailable"
	RetrievalReasonRerank           RetrievalUnavailableReason = "rerank_unavailable"
)

// RetrievalUnavailableError preserves the dependency error for logs while
// exposing only a bounded reason code to callers and user-facing protocols.
// It deliberately never contains query or document content.
type RetrievalUnavailableError struct {
	Reason RetrievalUnavailableReason
	Cause  error
}

func (e *RetrievalUnavailableError) Error() string {
	if e == nil {
		return ErrRetrievalUnavailable.Error()
	}
	return fmt.Sprintf("%s: %s", ErrRetrievalUnavailable, e.Reason)
}

func (e *RetrievalUnavailableError) Unwrap() []error {
	if e == nil || e.Cause == nil {
		return []error{ErrRetrievalUnavailable}
	}
	return []error{ErrRetrievalUnavailable, e.Cause}
}

func RetrievalUnavailableReasonOf(err error) (RetrievalUnavailableReason, bool) {
	var unavailable *RetrievalUnavailableError
	if !errors.As(err, &unavailable) || unavailable == nil {
		return "", false
	}
	return unavailable.Reason, true
}

func retrievalUnavailable(reason RetrievalUnavailableReason, cause error) error {
	return &RetrievalUnavailableError{Reason: reason, Cause: cause}
}

type ActiveDocument struct {
	DocID        string
	Version      int
	OriginalName string
}

type ActiveDocumentLoader func(context.Context, int64) ([]ActiveDocument, error)

type ActiveChunkSearcher interface {
	EnsureIndex(context.Context) error
	SearchActiveByUserVector(context.Context, int64, []float32, int, map[string]int) ([]RedisChunkSearchResult, error)
	SearchActiveByUserText(context.Context, int64, string, int, map[string]int) ([]RedisChunkSearchResult, error)
}

type RetrievalResult struct {
	ExecutionID      string `json:"-"`
	RedisKey         string
	ChunkID          string
	DocID            string
	DocVersion       int
	DocumentName     string
	ChunkIndex       int
	ChunkType        string
	TitlePath        []string
	StartLine        int
	EndLine          int
	RawContent       string
	EmbeddingContent string
	ContentHash      string
	CodeLanguage     string
	Symbols          []string
	TokenEstimate    int
	PartIndex        int
	PartTotal        int
	Distance         float64
	Score            float64
	VectorScore      float64
	LexicalScore     float64
	FusionScore      float64
	VectorRank       int
	LexicalRank      int
	MatchKind        string
	RerankScore      float64
	RerankRank       int
	RerankApplied    bool
}

// RAGService is immutable after construction and is safe to reuse for the
// whole application lifecycle. It never scans upload directories and never
// accepts user-supplied document filters.
type RAGService struct {
	embedder   embedding.Embedder
	searcher   ActiveChunkSearcher
	loadActive ActiveDocumentLoader
	dimension  int
	timeout    time.Duration
	reranker   Reranker
}

func NewRAGService(ctx context.Context, embedder embedding.Embedder, searcher ActiveChunkSearcher, loadActive ActiveDocumentLoader, dimension int) (*RAGService, error) {
	if embedder == nil || searcher == nil || loadActive == nil || dimension <= 0 {
		return nil, fmt.Errorf("%w: dependencies and dimension are required", ErrRetrievalUnavailable)
	}
	if err := searcher.EnsureIndex(ctx); err != nil {
		return nil, retrievalUnavailable(RetrievalReasonIndex, err)
	}
	return &RAGService{
		embedder: embedder, searcher: searcher, loadActive: loadActive,
		dimension: dimension, timeout: defaultRetrievalTimeout,
	}, nil
}

// UseReranker validates the fixed local model before the service is shared.
// Configuration is intentionally startup-only; callers must not mutate a
// live RAGService while Retrieve calls are in flight.
func (s *RAGService) UseReranker(ctx context.Context, reranker Reranker) error {
	if s == nil || reranker == nil {
		return fmt.Errorf("%w: reranker is required", ErrRetrievalUnavailable)
	}
	if err := CheckReranker(ctx, reranker); err != nil {
		return err
	}
	return s.AttachReranker(reranker)
}

// CheckReranker verifies a configured reranker once while assembling a runtime. Candidate
// retrieval calls this at startup, matching the legacy assembly's fail-fast behavior.
func CheckReranker(ctx context.Context, reranker Reranker) error {
	if reranker == nil {
		return fmt.Errorf("%w: reranker is required", ErrRetrievalUnavailable)
	}
	if err := reranker.Health(nonNilContext(ctx)); err != nil {
		return retrievalUnavailable(RetrievalReasonRerank, err)
	}
	return nil
}

// AttachReranker mounts a reranker already checked during startup. It performs no network call
// and must be called before the RAGService is shared with concurrent Retrieve calls.
func (s *RAGService) AttachReranker(reranker Reranker) error {
	if s == nil || reranker == nil {
		return fmt.Errorf("%w: reranker is required", ErrRetrievalUnavailable)
	}
	s.reranker = reranker
	return nil
}

// Retrieve returns the top-K chunks for a query with the production defaults:
// hybrid retrieval, and a rerank when a reranker is attached and topK is at most
// DefaultRerankMaxTopK. It is RetrieveWithOptions with every option left at its
// zero value; its behaviour is pinned by retrieval_golden_test.go and
// retrieval_differential_test.go.
func (s *RAGService) Retrieve(ctx context.Context, userID int64, query string, topK int) ([]RetrievalResult, error) {
	return s.retrieve(ctx, userID, query, RetrieveOptions{TopK: topK}, nil)
}

// retrieve is the one implementation behind Retrieve and RetrieveWithOptions.
// Every option at its zero value reproduces the original Retrieve exactly.
// diagnostics may be nil; the production path passes nil.
func (s *RAGService) retrieve(ctx context.Context, userID int64, query string, opts RetrieveOptions, diagnostics *retrievalDiagnostics) (results []RetrievalResult, resultErr error) {
	topK := opts.TopK
	started := time.Now()
	defer func() {
		status, reason := "complete", "none"
		if resultErr != nil {
			if unavailableReason, ok := RetrievalUnavailableReasonOf(resultErr); ok {
				status, reason = "unavailable", string(unavailableReason)
			} else {
				status, reason = "failed", "invalid_result"
			}
		} else if len(results) == 0 {
			status, reason = "no_evidence", string(KnowledgeReasonNoCandidate)
		}
		observability.Memory.KnowledgeRetrieval(status, reason, time.Since(started))
		for _, result := range results {
			observability.Memory.KnowledgeCandidate(result.MatchKind)
		}
	}()
	query = strings.TrimSpace(query)
	if userID <= 0 || query == "" || topK <= 0 || topK > 100 {
		return nil, ErrInvalidRetrievalRequest
	}
	if err := opts.validate(s.reranker != nil); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	activeDocuments, err := s.loadActive(ctx, userID)
	if err != nil {
		return nil, retrievalUnavailable(RetrievalReasonDocumentMetadata, err)
	}
	activeVersions := make(map[string]int, len(activeDocuments))
	documentNames := make(map[string]string, len(activeDocuments))
	for _, document := range activeDocuments {
		if document.DocID == "" || document.Version <= 0 {
			return nil, fmt.Errorf("%w: active document metadata is invalid", ErrRetrievalFailed)
		}
		activeVersions[document.DocID] = document.Version
		documentNames[document.DocID] = document.OriginalName
	}
	if len(activeVersions) == 0 {
		return []RetrievalResult{}, nil
	}
	var vector []float32
	if opts.Mode != RetrievalModeKeyword { // keyword-only retrieval never embeds the query
		vectors, err := s.embedder.EmbedStrings(ctx, []string{query})
		if err != nil {
			if errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled) {
				return nil, fmt.Errorf("%w: embed query: %w", ErrRetrievalFailed, err)
			}
			return nil, retrievalUnavailable(RetrievalReasonEmbedding, err)
		}
		if len(vectors) != 1 {
			return nil, fmt.Errorf("%w: query embedder returned %d vectors", ErrRetrievalFailed, len(vectors))
		}
		vector, err = retrievalVector(vectors[0], s.dimension)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrRetrievalFailed, err)
		}
	}
	candidateK := topK * 6
	if candidateK < 30 {
		candidateK = 30
	}
	useReranker := s.reranker != nil && topK <= DefaultRerankMaxTopK
	switch opts.Rerank {
	case RerankForceOn:
		useReranker = s.reranker != nil
	case RerankForceOff:
		useReranker = false
	}
	if useReranker {
		candidateK = 48
	}
	if candidateK > 100 {
		candidateK = 100
	}
	if opts.CandidateDepth > 0 {
		candidateK = opts.CandidateDepth
	}
	var vectorCandidates, lexicalCandidates []RedisChunkSearchResult
	var vectorErr, lexicalErr error
	var searches sync.WaitGroup
	if opts.Mode != RetrievalModeKeyword {
		searches.Add(1)
		go func() {
			defer searches.Done()
			vectorCandidates, vectorErr = s.searcher.SearchActiveByUserVector(ctx, userID, vector, candidateK, activeVersions)
		}()
	}
	if opts.Mode != RetrievalModeVector {
		searches.Add(1)
		go func() {
			defer searches.Done()
			lexicalCandidates, lexicalErr = s.searcher.SearchActiveByUserText(ctx, userID, query, candidateK, activeVersions)
		}()
	}
	searches.Wait()
	if vectorErr != nil {
		if errors.Is(vectorErr, context.Canceled) && errors.Is(ctx.Err(), context.Canceled) {
			return nil, fmt.Errorf("%w: vector search: %w", ErrRetrievalFailed, vectorErr)
		}
		return nil, retrievalUnavailable(RetrievalReasonVector, vectorErr)
	}
	if lexicalErr != nil {
		if errors.Is(lexicalErr, context.Canceled) && errors.Is(ctx.Err(), context.Canceled) {
			return nil, fmt.Errorf("%w: lexical search: %w", ErrRetrievalFailed, lexicalErr)
		}
		return nil, retrievalUnavailable(RetrievalReasonLexical, lexicalErr)
	}
	a0Candidates := FuseRRF(vectorCandidates, lexicalCandidates, topK)
	candidates := a0Candidates
	rerankScores := map[string]RerankScore{}
	rerankStatus := RerankNotAttached
	if s.reranker != nil {
		rerankStatus = RerankBypassed
	}
	var preRerank []RedisChunkSearchResult
	if useReranker {
		preRerank = FuseRRF(vectorCandidates, lexicalCandidates, RerankCandidateK)
		if err := validateActiveCandidates(preRerank, activeVersions); err != nil {
			return nil, err
		}
		passages := make([]string, len(preRerank))
		for index, candidate := range preRerank {
			passages[index] = strings.TrimSpace(candidate.EmbeddingContent)
			if passages[index] == "" {
				passages[index] = strings.TrimSpace(candidate.RawContent)
			}
			if passages[index] == "" {
				return nil, fmt.Errorf("%w: rerank candidate content is empty", ErrRetrievalFailed)
			}
		}
		rerankStarted := time.Now()
		scores, rerankErr := s.reranker.Rerank(ctx, query, passages)
		if rerankErr != nil {
			if errors.Is(rerankErr, ErrRerankUnavailable) {
				observability.Memory.KnowledgeRerank("fallback", time.Since(rerankStarted))
				candidates = a0Candidates
				rerankStatus = RerankFallback
			} else {
				observability.Memory.KnowledgeRerank("failed", time.Since(rerankStarted))
				return nil, fmt.Errorf("%w: rerank candidates: %w", ErrRetrievalFailed, rerankErr)
			}
		} else {
			if err := validateRerankScores(scores, len(preRerank)); err != nil {
				observability.Memory.KnowledgeRerank("failed", time.Since(rerankStarted))
				return nil, fmt.Errorf("%w: %w", ErrRetrievalFailed, err)
			}
			sort.SliceStable(scores, func(i, j int) bool {
				if scores[i].Score != scores[j].Score {
					return scores[i].Score > scores[j].Score
				}
				return scores[i].Index < scores[j].Index
			})
			candidates = make([]RedisChunkSearchResult, 0, min(topK, len(scores)))
			for rank, score := range scores[:min(topK, len(scores))] {
				candidate := preRerank[score.Index]
				candidates = append(candidates, candidate)
				rerankScores[retrievalIdentity(candidate)] = RerankScore{Index: rank + 1, Score: score.Score}
			}
			observability.Memory.KnowledgeRerank("applied", time.Since(rerankStarted))
			rerankStatus = RerankApplied
		}
	} else if s.reranker != nil {
		observability.Memory.KnowledgeRerank("bypassed", 0)
	}
	if diagnostics != nil {
		diagnostics.rerankStatus, diagnostics.depth = rerankStatus, candidateK
		if opts.WantCandidates {
			if preRerank == nil { // the reranker did not run; the pool is still the top of the fused list
				preRerank = FuseRRF(vectorCandidates, lexicalCandidates, RerankCandidateK)
			}
			pool, err := buildRetrievalResults(preRerank, activeVersions, documentNames, nil)
			if err != nil {
				return nil, err
			}
			diagnostics.candidates = pool
		}
	}
	results, err = buildRetrievalResults(candidates, activeVersions, documentNames, rerankScores)
	if err != nil {
		return nil, err
	}
	return results, nil
}

// buildRetrievalResults turns fused candidates into results: it re-checks each
// candidate against the active document versions, keeps the first occurrence of
// a chunk and attaches the rerank score of the chunk, if it has one. It was the
// tail of Retrieve and is unchanged; a nil rerankScores map means "no rerank".
func buildRetrievalResults(candidates []RedisChunkSearchResult, activeVersions map[string]int, documentNames map[string]string, rerankScores map[string]RerankScore) ([]RetrievalResult, error) {
	results := make([]RetrievalResult, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if err := validateActiveCandidates([]RedisChunkSearchResult{candidate}, activeVersions); err != nil {
			return nil, err
		}
		identity := candidate.RedisKey
		if identity == "" {
			identity = candidate.DocID + ":" + candidate.ChunkID
		}
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		vectorScore := 0.0
		if candidate.VectorRank > 0 {
			vectorScore = 1 - candidate.Distance
		}
		rerank := rerankScores[identity]
		results = append(results, RetrievalResult{
			ExecutionID: candidate.ExecutionID, RedisKey: candidate.RedisKey, ChunkID: candidate.ChunkID,
			DocID: candidate.DocID, DocVersion: candidate.DocVersion, DocumentName: documentNames[candidate.DocID],
			ChunkIndex: candidate.ChunkIndex, ChunkType: candidate.ChunkType,
			TitlePath: splitMetadataList(candidate.TitlePath, " > "), StartLine: candidate.StartLine, EndLine: candidate.EndLine,
			RawContent: candidate.RawContent, EmbeddingContent: candidate.EmbeddingContent,
			ContentHash: candidate.ContentHash, CodeLanguage: candidate.CodeLanguage,
			Symbols: splitMetadataList(candidate.Symbols, " "), TokenEstimate: candidate.TokenEstimate,
			PartIndex: candidate.PartIndex, PartTotal: candidate.PartTotal,
			Distance: candidate.Distance, Score: candidate.FusionScore,
			VectorScore: vectorScore, LexicalScore: candidate.LexicalScore, FusionScore: candidate.FusionScore,
			VectorRank: candidate.VectorRank, LexicalRank: candidate.LexicalRank, MatchKind: candidate.MatchKind,
			RerankScore: rerank.Score, RerankRank: rerank.Index, RerankApplied: rerank.Index > 0,
		})
	}
	return results, nil
}

func validateActiveCandidates(candidates []RedisChunkSearchResult, activeVersions map[string]int) error {
	for _, candidate := range candidates {
		version, exists := activeVersions[candidate.DocID]
		if !exists || version != candidate.DocVersion {
			return fmt.Errorf("%w: searcher returned a non-active document version", ErrRetrievalFailed)
		}
	}
	return nil
}

func retrievalIdentity(candidate RedisChunkSearchResult) string {
	if candidate.RedisKey != "" {
		return candidate.RedisKey
	}
	return candidate.DocID + ":" + candidate.ChunkID
}

func retrievalVector(vector []float64, dimension int) ([]float32, error) {
	if len(vector) != dimension {
		return nil, fmt.Errorf("query vector dimension is %d, want %d", len(vector), dimension)
	}
	converted := make([]float32, len(vector))
	for index, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("query vector value %d is not finite", index)
		}
		converted[index] = float32(value)
		if math.IsInf(float64(converted[index]), 0) {
			return nil, fmt.Errorf("query vector value %d overflows FLOAT32", index)
		}
	}
	return converted, nil
}

func splitMetadataList(value, separator string) []string {
	if strings.TrimSpace(value) == "" {
		return []string{}
	}
	parts := strings.Split(value, separator)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}
