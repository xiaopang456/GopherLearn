package rag

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/embedding"
)

func TestRAGServiceRetrievesMultipleActiveDocumentsForOneUser(t *testing.T) {
	embedder := &retrievalFakeEmbedder{dimension: 4}
	searcher := &retrievalFakeSearcher{results: []RedisChunkSearchResult{
		{RedisKey: "key-a", ChunkID: "chunk-a", DocID: "doc-a", DocVersion: 2, SourceFile: "gin.md", TitlePath: "Gin > Router", RawContent: "gin content", Symbols: "gin.Context", Distance: 0.1},
		{RedisKey: "key-b", ChunkID: "chunk-b", DocID: "doc-b", DocVersion: 1, SourceFile: "redis.md", TitlePath: "Redis > TTL", RawContent: "redis content", Distance: 0.2},
	}}
	service, err := NewRAGService(context.Background(), embedder, searcher, func(_ context.Context, userID int64) ([]ActiveDocument, error) {
		if userID != 73 {
			t.Fatalf("loader userID = %d", userID)
		}
		return []ActiveDocument{{DocID: "doc-a", Version: 2, OriginalName: "gin.md"}, {DocID: "doc-b", Version: 1, OriginalName: "redis.md"}}, nil
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	results, err := service.Retrieve(context.Background(), 73, "如何设置 TTL", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].DocumentName != "gin.md" || results[1].DocumentName != "redis.md" {
		t.Fatalf("retrieval results = %+v", results)
	}
	if searcher.userID != 73 || !reflect.DeepEqual(searcher.active, map[string]int{"doc-a": 2, "doc-b": 1}) {
		t.Fatalf("search isolation = user %d active %+v", searcher.userID, searcher.active)
	}
	if !reflect.DeepEqual(results[0].TitlePath, []string{"Gin", "Router"}) || results[0].Score != 1 || results[0].VectorScore != 0.9 || results[0].MatchKind != "vector" {
		t.Fatalf("structured result = %+v", results[0])
	}

	// The same lifecycle service and embedder are reused for later questions.
	if _, err := service.Retrieve(context.Background(), 73, "Gin Context", 3); err != nil {
		t.Fatal(err)
	}
	if searcher.ensureCalls != 1 || embedder.calls != 2 {
		t.Fatalf("lifecycle calls = ensure %d embedding %d", searcher.ensureCalls, embedder.calls)
	}
}

func TestRAGServiceNoActiveDocumentsIsEmptyNotFailure(t *testing.T) {
	embedder := &retrievalFakeEmbedder{dimension: 4}
	searcher := &retrievalFakeSearcher{}
	service, err := NewRAGService(context.Background(), embedder, searcher, func(context.Context, int64) ([]ActiveDocument, error) {
		return []ActiveDocument{}, nil
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	results, err := service.Retrieve(context.Background(), 73, "question", 5)
	if err != nil || len(results) != 0 || embedder.calls != 0 || searcher.searchCalls != 0 {
		t.Fatalf("empty knowledge result = (%+v, %v), embed=%d search=%d", results, err, embedder.calls, searcher.searchCalls)
	}
}

func TestRAGServiceFusesVectorAndLexicalCandidates(t *testing.T) {
	searcher := &retrievalFakeSearcher{
		results: []RedisChunkSearchResult{
			{RedisKey: "a", ChunkID: "a", DocID: "doc", DocVersion: 1, Distance: 0.1},
			{RedisKey: "b", ChunkID: "b", DocID: "doc", DocVersion: 1, Distance: 0.2},
		},
		textResults: []RedisChunkSearchResult{
			{RedisKey: "b", ChunkID: "b", DocID: "doc", DocVersion: 1, LexicalScore: 10},
			{RedisKey: "c", ChunkID: "c", DocID: "doc", DocVersion: 1, LexicalScore: 9},
		},
	}
	service, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, searcher, func(context.Context, int64) ([]ActiveDocument, error) {
		return []ActiveDocument{{DocID: "doc", Version: 1, OriginalName: "locks.md"}}, nil
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	results, err := service.Retrieve(context.Background(), 73, "GetOrCreateAIHelper RLock", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ChunkID != "b" || results[0].MatchKind != "vector+lexical" || results[0].VectorRank != 2 || results[0].LexicalRank != 1 {
		t.Fatalf("hybrid results = %+v", results)
	}
	if searcher.vectorTopK != 30 || searcher.textTopK != 30 {
		t.Fatalf("candidate pool = vector %d text %d", searcher.vectorTopK, searcher.textTopK)
	}
}

func TestRAGServiceDistinguishesUnavailableFailedAndInvalid(t *testing.T) {
	t.Run("unavailable at initialization", func(t *testing.T) {
		_, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, &retrievalFakeSearcher{ensureErr: errors.New("redis down")}, func(context.Context, int64) ([]ActiveDocument, error) { return nil, nil }, 4)
		reason, ok := RetrievalUnavailableReasonOf(err)
		if !errors.Is(err, ErrRetrievalUnavailable) || !ok || reason != RetrievalReasonIndex {
			t.Fatalf("initialization error = %v", err)
		}
	})
	t.Run("vector runtime unavailable", func(t *testing.T) {
		redisFailure := errors.New("redis down")
		service, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, &retrievalFakeSearcher{searchErr: redisFailure}, func(context.Context, int64) ([]ActiveDocument, error) {
			return []ActiveDocument{{DocID: "doc", Version: 1}}, nil
		}, 4)
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Retrieve(context.Background(), 73, "question", 5)
		reason, ok := RetrievalUnavailableReasonOf(err)
		if !errors.Is(err, ErrRetrievalUnavailable) || !errors.Is(err, redisFailure) || !ok || reason != RetrievalReasonVector {
			t.Fatalf("runtime error = %v", err)
		}
	})
	t.Run("embedding deadline remains observable", func(t *testing.T) {
		service, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4, waitForContext: true}, &retrievalFakeSearcher{}, func(context.Context, int64) ([]ActiveDocument, error) {
			return []ActiveDocument{{DocID: "doc", Version: 1}}, nil
		}, 4)
		if err != nil {
			t.Fatal(err)
		}
		service.timeout = 10 * time.Millisecond
		_, err = service.Retrieve(context.Background(), 73, "question", 5)
		reason, ok := RetrievalUnavailableReasonOf(err)
		if !errors.Is(err, ErrRetrievalUnavailable) || !errors.Is(err, context.DeadlineExceeded) || !ok || reason != RetrievalReasonEmbedding {
			t.Fatalf("embedding deadline error = %v", err)
		}
	})
	t.Run("lexical runtime failure", func(t *testing.T) {
		service, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, &retrievalFakeSearcher{textErr: errors.New("text index down")}, func(context.Context, int64) ([]ActiveDocument, error) {
			return []ActiveDocument{{DocID: "doc", Version: 1}}, nil
		}, 4)
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Retrieve(context.Background(), 73, "GetOrCreateAIHelper", 5)
		reason, ok := RetrievalUnavailableReasonOf(err)
		if !errors.Is(err, ErrRetrievalUnavailable) || !ok || reason != RetrievalReasonLexical {
			t.Fatalf("lexical runtime error = %v", err)
		}
	})
	t.Run("document metadata unavailable", func(t *testing.T) {
		metadataFailure := errors.New("mysql down")
		service, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, &retrievalFakeSearcher{}, func(context.Context, int64) ([]ActiveDocument, error) {
			return nil, metadataFailure
		}, 4)
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Retrieve(context.Background(), 73, "question", 5)
		reason, ok := RetrievalUnavailableReasonOf(err)
		if !errors.Is(err, ErrRetrievalUnavailable) || !errors.Is(err, metadataFailure) || !ok || reason != RetrievalReasonDocumentMetadata {
			t.Fatalf("metadata error = %v", err)
		}
	})
	t.Run("invalid request", func(t *testing.T) {
		service, _ := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, &retrievalFakeSearcher{}, func(context.Context, int64) ([]ActiveDocument, error) { return nil, nil }, 4)
		_, err := service.Retrieve(context.Background(), 0, "", 0)
		if !errors.Is(err, ErrInvalidRetrievalRequest) {
			t.Fatalf("invalid request error = %v", err)
		}
	})
}

func TestRAGServiceRejectsSearcherReturningStaleVersion(t *testing.T) {
	service, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, &retrievalFakeSearcher{results: []RedisChunkSearchResult{{DocID: "doc", DocVersion: 1, ChunkID: "stale"}}}, func(context.Context, int64) ([]ActiveDocument, error) {
		return []ActiveDocument{{DocID: "doc", Version: 2}}, nil
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Retrieve(context.Background(), 73, "question", 5)
	if !errors.Is(err, ErrRetrievalFailed) {
		t.Fatalf("stale search result error = %v", err)
	}
}

func TestRAGServiceAppliesRerankerToTop24AndReturnsRequestedTopK(t *testing.T) {
	searcher := &retrievalFakeSearcher{results: []RedisChunkSearchResult{
		{RedisKey: "a", ChunkID: "a", DocID: "doc", DocVersion: 1, RawContent: "alpha", Distance: 0.1},
		{RedisKey: "b", ChunkID: "b", DocID: "doc", DocVersion: 1, RawContent: "beta", Distance: 0.2},
		{RedisKey: "c", ChunkID: "c", DocID: "doc", DocVersion: 1, RawContent: "gamma", Distance: 0.3},
	}}
	ranker := &retrievalFakeReranker{scores: []RerankScore{{Index: 0, Score: -1}, {Index: 1, Score: 3}, {Index: 2, Score: 2}}}
	service, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, searcher, func(context.Context, int64) ([]ActiveDocument, error) {
		return []ActiveDocument{{DocID: "doc", Version: 1, OriginalName: "note.md"}}, nil
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.UseReranker(context.Background(), ranker); err != nil {
		t.Fatal(err)
	}
	results, err := service.Retrieve(context.Background(), 73, "question", 2)
	if err != nil {
		t.Fatal(err)
	}
	if searcher.vectorTopK != 48 || searcher.textTopK != 48 || ranker.passages != 3 {
		t.Fatalf("candidate depths = vector %d lexical %d rerank %d", searcher.vectorTopK, searcher.textTopK, ranker.passages)
	}
	if len(results) != 2 || results[0].ChunkID != "b" || results[1].ChunkID != "c" || !results[0].RerankApplied || results[0].RerankRank != 1 || results[0].RerankScore != 3 {
		t.Fatalf("reranked results = %+v", results)
	}
}

func TestRAGServiceRerankerUnavailableFallsBackToOriginalRRF(t *testing.T) {
	searcher := &retrievalFakeSearcher{results: []RedisChunkSearchResult{
		{RedisKey: "a", ChunkID: "a", DocID: "doc", DocVersion: 1, RawContent: "alpha", Distance: 0.1},
		{RedisKey: "b", ChunkID: "b", DocID: "doc", DocVersion: 1, RawContent: "beta", Distance: 0.2},
	}}
	service, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, searcher, func(context.Context, int64) ([]ActiveDocument, error) {
		return []ActiveDocument{{DocID: "doc", Version: 1}}, nil
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.UseReranker(context.Background(), &retrievalFakeReranker{err: ErrRerankUnavailable}); err != nil {
		t.Fatal(err)
	}
	results, err := service.Retrieve(context.Background(), 73, "question", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ChunkID != "a" || results[0].RerankApplied {
		t.Fatalf("fallback results = %+v", results)
	}
}

func TestRAGServiceRerankerProtocolFailureFailsClosed(t *testing.T) {
	searcher := &retrievalFakeSearcher{results: []RedisChunkSearchResult{{RedisKey: "a", ChunkID: "a", DocID: "doc", DocVersion: 1, RawContent: "alpha"}}}
	service, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, searcher, func(context.Context, int64) ([]ActiveDocument, error) {
		return []ActiveDocument{{DocID: "doc", Version: 1}}, nil
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.UseReranker(context.Background(), &retrievalFakeReranker{scores: []RerankScore{{Index: 9, Score: 1}}}); err != nil {
		t.Fatal(err)
	}
	_, err = service.Retrieve(context.Background(), 73, "question", 1)
	if !errors.Is(err, ErrRetrievalFailed) {
		t.Fatalf("protocol error = %v, want ErrRetrievalFailed", err)
	}
}

func TestRAGServiceBypassesRerankerForCandidatePoolRequests(t *testing.T) {
	rows := make([]RedisChunkSearchResult, 0, 30)
	for index := 0; index < 30; index++ {
		rows = append(rows, RedisChunkSearchResult{RedisKey: string(rune('a' + index)), ChunkID: string(rune('a' + index)), DocID: "doc", DocVersion: 1, RawContent: "content", Distance: float64(index) / 100})
	}
	searcher := &retrievalFakeSearcher{results: rows}
	ranker := &retrievalFakeReranker{}
	service, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, searcher, func(context.Context, int64) ([]ActiveDocument, error) {
		return []ActiveDocument{{DocID: "doc", Version: 1}}, nil
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.UseReranker(context.Background(), ranker); err != nil {
		t.Fatal(err)
	}
	results, err := service.Retrieve(context.Background(), 73, "topic", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 30 || ranker.passages != 0 || searcher.vectorTopK != 100 || searcher.textTopK != 100 {
		t.Fatalf("candidate-pool request changed: results=%d rerank=%d vector=%d lexical=%d", len(results), ranker.passages, searcher.vectorTopK, searcher.textTopK)
	}
}

type retrievalFakeEmbedder struct {
	dimension      int
	calls          int
	err            error
	waitForContext bool
}

func (f *retrievalFakeEmbedder) EmbedStrings(ctx context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	f.calls++
	if f.waitForContext {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	vectors := make([][]float64, len(texts))
	for index := range vectors {
		vectors[index] = make([]float64, f.dimension)
		vectors[index][0] = 1
	}
	return vectors, nil
}

type retrievalFakeSearcher struct {
	mu          sync.Mutex
	ensureCalls int
	searchCalls int
	ensureErr   error
	searchErr   error
	textErr     error
	results     []RedisChunkSearchResult
	textResults []RedisChunkSearchResult
	vectorTopK  int
	textTopK    int
	userID      int64
	active      map[string]int
}

type retrievalFakeReranker struct {
	healthErr error
	err       error
	scores    []RerankScore
	passages  int
}

func (f *retrievalFakeReranker) Health(context.Context) error { return f.healthErr }

func (f *retrievalFakeReranker) Rerank(_ context.Context, _ string, passages []string) ([]RerankScore, error) {
	f.passages = len(passages)
	if f.err != nil {
		return nil, f.err
	}
	return append([]RerankScore(nil), f.scores...), nil
}

func (f *retrievalFakeSearcher) EnsureIndex(context.Context) error {
	f.ensureCalls++
	return f.ensureErr
}

func (f *retrievalFakeSearcher) SearchActiveByUserVector(_ context.Context, userID int64, _ []float32, topK int, active map[string]int) ([]RedisChunkSearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchCalls++
	f.vectorTopK = topK
	f.userID = userID
	f.active = make(map[string]int, len(active))
	for id, version := range active {
		f.active[id] = version
	}
	return append([]RedisChunkSearchResult(nil), f.results...), f.searchErr
}

func (f *retrievalFakeSearcher) SearchActiveByUserText(_ context.Context, userID int64, _ string, topK int, active map[string]int) ([]RedisChunkSearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchCalls++
	f.textTopK = topK
	f.userID = userID
	f.active = make(map[string]int, len(active))
	for id, version := range active {
		f.active[id] = version
	}
	return append([]RedisChunkSearchResult(nil), f.textResults...), f.textErr
}
