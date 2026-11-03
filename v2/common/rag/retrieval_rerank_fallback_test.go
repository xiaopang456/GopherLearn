package rag

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A reranker only improves the order of candidates that are already good enough to answer from.
// These tests inject the ways a hosted reranker fails - it times out, throttles, refuses the
// credential or request, or answers with an error nobody classified - and require the retrieval
// to keep the fused order, say so on every result, and not fail.

func fallbackService(t *testing.T, reranker Reranker) (*RAGService, *retrievalFakeSearcher) {
	t.Helper()
	searcher := &retrievalFakeSearcher{results: []RedisChunkSearchResult{
		{RedisKey: "a", ChunkID: "a", DocID: "doc", DocVersion: 1, RawContent: "alpha", Distance: 0.1},
		{RedisKey: "b", ChunkID: "b", DocID: "doc", DocVersion: 1, RawContent: "beta", Distance: 0.2},
		{RedisKey: "c", ChunkID: "c", DocID: "doc", DocVersion: 1, RawContent: "gamma", Distance: 0.3},
	}}
	service, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, searcher, func(context.Context, int64) ([]ActiveDocument, error) {
		return []ActiveDocument{{DocID: "doc", Version: 1, OriginalName: "note.md"}}, nil
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.UseReranker(context.Background(), reranker); err != nil {
		t.Fatal(err)
	}
	return service, searcher
}

func TestRAGServiceRerankerFailuresKeepTheFusedOrderAndSaySo(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"timeout", fmt.Errorf("%w: the rerank service did not answer inside the configured timeout", ErrRerankTimeout)},
		{"rate limited", fmt.Errorf("%w: status 429", ErrRerankRateLimited)},
		{"credential refused", fmt.Errorf("%w: status 401", ErrRerankRejected)},
		{"request refused", fmt.Errorf("%w: status 400", ErrRerankConfig)},
		{"unavailable", fmt.Errorf("%w: status 503", ErrRerankUnavailable)},
		{"unclassified", errors.New("something else broke")},
	} {
		t.Run(failure.name, func(t *testing.T) {
			service, _ := fallbackService(t, &retrievalFakeReranker{err: failure.err})
			results, err := service.Retrieve(context.Background(), 73, "question", 2)
			if err != nil {
				t.Fatalf("a failed reranker took the retrieval down: %v", err)
			}
			if len(results) != 2 || results[0].ChunkID != "a" || results[1].ChunkID != "b" {
				t.Fatalf("results are not the fused order: %+v", results)
			}
			for _, result := range results {
				if result.RerankApplied || result.RerankRank != 0 || !result.RerankFellBack {
					t.Fatalf("a fallback result is marked wrongly: %+v", result)
				}
			}
		})
	}
}

func TestRAGServiceRerankFallbackIsVisibleToDiagnosticsAndMetrics(t *testing.T) {
	service, _ := fallbackService(t, &retrievalFakeReranker{err: ErrRerankTimeout})
	outcome, err := service.RetrieveWithOptions(context.Background(), 73, "question", RetrieveOptions{TopK: 2})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.RerankStatus != RerankFallback || len(outcome.Results) != 2 {
		t.Fatalf("status %q results %d", outcome.RerankStatus, len(outcome.Results))
	}
}

func TestRAGServiceRerankerWorkingResultsAreNotMarked(t *testing.T) {
	service, _ := fallbackService(t, &retrievalFakeReranker{scores: []RerankScore{{Index: 0, Score: 1}, {Index: 1, Score: 3}, {Index: 2, Score: 2}}})
	results, err := service.Retrieve(context.Background(), 73, "question", 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if !result.RerankApplied || result.RerankFellBack {
			t.Fatalf("a reranked result is marked as a fallback: %+v", result)
		}
	}
	// A request that never reaches the reranker is not a fallback either.
	bypassed, err := service.Retrieve(context.Background(), 73, "question", 30)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range bypassed {
		if result.RerankFellBack {
			t.Fatalf("a bypassed rerank is marked as a fallback: %+v", result)
		}
	}
}

// cancellingReranker gives up the caller's context while it is being asked, then reports a timeout.
type cancellingReranker struct{ cancel context.CancelFunc }

func (r cancellingReranker) Health(context.Context) error { return nil }

func (r cancellingReranker) Rerank(context.Context, string, []string) ([]RerankScore, error) {
	r.cancel()
	return nil, ErrRerankTimeout
}

func TestRAGServiceCallerGivingUpIsNotAFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, _ := fallbackService(t, cancellingReranker{cancel: cancel})
	_, err := service.Retrieve(ctx, 73, "question", 2)
	if !errors.Is(err, ErrRetrievalFailed) || !errors.Is(err, ErrRerankTimeout) {
		t.Fatalf("a retrieval whose caller gave up must still fail, got %v", err)
	}
}

// The real hosted reranker, against a server that answers too slowly: the error it produces for a
// timeout is the one the retrieval used to treat as fatal.
func TestRAGServiceSurvivesTheHostedRerankerTimingOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	reranker, err := NewDashScopeReranker(server.URL, "qwen3-rerank", hostedToken, 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := fallbackService(t, &retrievalFakeReranker{})
	if err := service.AttachReranker(reranker); err != nil { // the startup check is not what is being tested
		t.Fatal(err)
	}
	results, err := service.Retrieve(context.Background(), 73, "question", 2)
	if err != nil {
		t.Fatalf("a rerank timeout took the retrieval down: %v", err)
	}
	if len(results) != 2 || results[0].ChunkID != "a" || !results[0].RerankFellBack {
		t.Fatalf("results after a rerank timeout = %+v", results)
	}
}
