package rag

// Frozen copy of RAGService.Retrieve exactly as it was at commit 07b53a4
// ("chore: remove internal development traces"), taken before the E2 refactor
// that moved the body into RAGService.retrieve.
//
// WHY THIS FILE EXISTS
//
// TestRetrieveMatchesLegacy runs this copy and the live Retrieve side by side on
// thousands of generated inputs and requires identical results, errors, calls to
// every dependency and metric counters. It exists only to prove that the E2
// refactor changed nothing observable on the default path.
//
// RULES FOR THE FUTURE
//
//  1. legacyRetrieve is a historical record, not live code. Never edit it, and
//     never edit it to make a failing test pass. Only the one-line signature
//     (a method became a function taking the service) differs from the original.
//  2. If the default retrieval behaviour is ever changed ON PURPOSE, do it in a
//     single commit that (a) regenerates the fixed-scenario data with
//     `go test ./common/rag -run TestRetrieveDefaultGolden -update-retrieval-golden`
//     and (b) states in the commit message why the default changed and which
//     scenarios moved. Review the diff of testdata/retrieve_default.golden.json
//     line by line; it is the record of what changed.
//  3. In that same commit TestRetrieveMatchesLegacy will fail by design, because
//     the frozen copy no longer describes the default. Remove this file together
//     with that test (and the differential test file) instead of changing the
//     frozen body. The golden scenarios keep guarding the new default.
//
// These tests use fake dependencies. They prove that the default path is
// unchanged under those fakes; they say nothing about the quality of retrieval
// against a real index.

import (
	"GopherAI/common/observability"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// legacyRetrieve is the body of RAGService.Retrieve at 07b53a4.
func legacyRetrieve(s *RAGService, ctx context.Context, userID int64, query string, topK int) (results []RetrievalResult, resultErr error) {
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
	vector, err := retrievalVector(vectors[0], s.dimension)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRetrievalFailed, err)
	}
	candidateK := topK * 6
	if candidateK < 30 {
		candidateK = 30
	}
	useReranker := s.reranker != nil && topK <= 8
	if useReranker {
		candidateK = 48
	}
	if candidateK > 100 {
		candidateK = 100
	}
	var vectorCandidates, lexicalCandidates []RedisChunkSearchResult
	var vectorErr, lexicalErr error
	var searches sync.WaitGroup
	searches.Add(2)
	go func() {
		defer searches.Done()
		vectorCandidates, vectorErr = s.searcher.SearchActiveByUserVector(ctx, userID, vector, candidateK, activeVersions)
	}()
	go func() {
		defer searches.Done()
		lexicalCandidates, lexicalErr = s.searcher.SearchActiveByUserText(ctx, userID, query, candidateK, activeVersions)
	}()
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
	if useReranker {
		preRerank := FuseRRF(vectorCandidates, lexicalCandidates, RerankCandidateK)
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
		}
	} else if s.reranker != nil {
		observability.Memory.KnowledgeRerank("bypassed", 0)
	}
	results = make([]RetrievalResult, 0, len(candidates))
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
