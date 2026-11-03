package rag

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A reranker only improves the order of candidates that are already good enough to answer from.
// These tests inject the ways a hosted reranker fails - it times out, throttles, refuses the
// credential or request, or answers with an error nobody classified - and require the retrieval
// to try once more when a second attempt can help, and otherwise to keep the fused order, say so
// on every result, and not fail.

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
			ranker := &scriptedReranker{steps: []rerankStep{{err: failure.err}, {err: failure.err}}}
			service, _ := fallbackService(t, ranker)
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
			// Only a failure that a second attempt can fix is tried twice.
			wantCalls := 1
			if retryableRerankError(failure.err) {
				wantCalls = 2
			}
			if ranker.calls() != wantCalls {
				t.Fatalf("the reranker was asked %d times, want %d", ranker.calls(), wantCalls)
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

// scriptedReranker answers each call from a script; once the script runs out it keeps repeating
// its last step. It records how many times it was asked and with what time left.
type rerankStep struct {
	scores []RerankScore
	err    error
}

type scriptedReranker struct {
	steps     []rerankStep
	asked     atomic.Int32
	deadlines []time.Duration
}

func (r *scriptedReranker) Health(context.Context) error { return nil }

func (r *scriptedReranker) Rerank(ctx context.Context, _ string, _ []string) ([]RerankScore, error) {
	call := int(r.asked.Add(1)) - 1
	if deadline, ok := ctx.Deadline(); ok {
		r.deadlines = append(r.deadlines, time.Until(deadline))
	} else {
		r.deadlines = append(r.deadlines, -1)
	}
	step := r.steps[min(call, len(r.steps)-1)]
	return append([]RerankScore(nil), step.scores...), step.err
}

func (r *scriptedReranker) calls() int { return int(r.asked.Load()) }

var goodScores = []RerankScore{{Index: 0, Score: 1}, {Index: 1, Score: 3}, {Index: 2, Score: 2}}

func TestRAGServiceRetriesAFailedRerankOnceAndUsesTheSecondAnswer(t *testing.T) {
	for _, first := range []error{ErrRerankTimeout, fmt.Errorf("%w: status 503", ErrRerankUnavailable)} {
		ranker := &scriptedReranker{steps: []rerankStep{{err: first}, {scores: goodScores}}}
		service, _ := fallbackService(t, ranker)
		results, err := service.Retrieve(context.Background(), 73, "question", 2)
		if err != nil {
			t.Fatalf("first failure %v: %v", first, err)
		}
		if ranker.calls() != 2 {
			t.Fatalf("first failure %v: reranker asked %d times, want 2", first, ranker.calls())
		}
		if len(results) != 2 || results[0].ChunkID != "b" || results[1].ChunkID != "c" || !results[0].RerankApplied || results[0].RerankFellBack || results[1].RerankFellBack {
			t.Fatalf("first failure %v: the retry's scores were not used: %+v", first, results)
		}
	}
}

func TestRAGServiceRerankRetryIsOnlyOneExtraAttempt(t *testing.T) {
	ranker := &scriptedReranker{steps: []rerankStep{{err: ErrRerankTimeout}}}
	service, _ := fallbackService(t, ranker)
	results, err := service.Retrieve(context.Background(), 73, "question", 2)
	if err != nil {
		t.Fatal(err)
	}
	if ranker.calls() != 2 {
		t.Fatalf("a reranker that keeps failing was asked %d times, want exactly 2", ranker.calls())
	}
	if len(results) != 2 || results[0].ChunkID != "a" || !results[0].RerankFellBack {
		t.Fatalf("results after two failures = %+v", results)
	}
}

func TestRAGServiceRerankRetryBoundsItsTimeAndRespectsTheCallersBudget(t *testing.T) {
	// The retry gets its own short bound, never more than the caller has left.
	ranker := &scriptedReranker{steps: []rerankStep{{err: ErrRerankTimeout}, {scores: goodScores}}}
	service, _ := fallbackService(t, ranker)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := service.Retrieve(ctx, 73, "question", 2); err != nil {
		t.Fatal(err)
	}
	if len(ranker.deadlines) != 2 || ranker.deadlines[1] > rerankRetryBound || ranker.deadlines[1] <= 0 {
		t.Fatalf("time left for the first and second attempt = %v, want the second within %s", ranker.deadlines, rerankRetryBound)
	}

	// A caller with too little time left gets no retry, and still gets its results.
	short := &scriptedReranker{steps: []rerankStep{{err: ErrRerankTimeout}, {scores: goodScores}}}
	service, _ = fallbackService(t, short)
	ctx, cancel = context.WithTimeout(context.Background(), rerankRetryMinBudget/2)
	defer cancel()
	results, err := service.Retrieve(ctx, 73, "question", 2)
	if err != nil || short.calls() != 1 || len(results) != 2 || !results[0].RerankFellBack {
		t.Fatalf("with little time left: calls %d err %v results %+v", short.calls(), err, results)
	}
}

func TestRAGServiceDoesNotRetryOnceTheCallerHasGivenUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ranker := &scriptedReranker{steps: []rerankStep{{err: ErrRerankTimeout}}}
	cancel()
	service, _ := fallbackService(t, ranker)
	_, err := service.Retrieve(ctx, 73, "question", 2)
	if err == nil || ranker.calls() > 1 {
		t.Fatalf("a retrieval of a caller that gave up: err %v, reranker asked %d times", err, ranker.calls())
	}
}

// hostedRerankServer answers like the hosted reranker, except that its first `stalls` requests take
// far longer than the client's timeout.
func hostedRerankServer(t *testing.T, stalls int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var seen atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if seen.Add(1) <= stalls {
			time.Sleep(300 * time.Millisecond)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"output":{"results":[{"index":0,"relevance_score":0.1},{"index":1,"relevance_score":0.9},{"index":2,"relevance_score":0.5}]}}`))
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

// The real hosted client against a server that stalls once and then answers: the first attempt ends
// in the real timeout error, the retry reaches the answer, and the order is the reranker's.
func TestRAGServiceRecoversFromOneStalledHostedRerank(t *testing.T) {
	server, seen := hostedRerankServer(t, 1)
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
		t.Fatalf("one stalled rerank took the retrieval down: %v", err)
	}
	if seen.Load() != 2 || len(results) != 2 || results[0].ChunkID != "b" || !results[0].RerankApplied || results[0].RerankFellBack {
		t.Fatalf("server saw %d requests, results = %+v", seen.Load(), results)
	}
}

// With a server that always stalls, the retrieval ends in the fused order after exactly two requests.
func TestRAGServiceSurvivesAHostedRerankerThatKeepsTimingOut(t *testing.T) {
	server, seen := hostedRerankServer(t, 1000)
	reranker, err := NewDashScopeReranker(server.URL, "qwen3-rerank", hostedToken, 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := fallbackService(t, &retrievalFakeReranker{})
	if err := service.AttachReranker(reranker); err != nil {
		t.Fatal(err)
	}
	results, err := service.Retrieve(context.Background(), 73, "question", 2)
	if err != nil {
		t.Fatalf("a rerank timeout took the retrieval down: %v", err)
	}
	if seen.Load() != 2 || len(results) != 2 || results[0].ChunkID != "a" || !results[0].RerankFellBack {
		t.Fatalf("server saw %d requests, results = %+v", seen.Load(), results)
	}
}
