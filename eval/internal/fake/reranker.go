package fake

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"GopherAI/common/rag"
)

// Reranker scores each passage by the overlap of its tokens with the query's,
// and, like the real services, returns one score per passage ordered best first.
// Like the real reranker it refuses more than rag.RerankCandidateK passages.
type Reranker struct {
	mu          sync.Mutex
	calls       int
	passages    int
	healthCalls int
	healthErr   error
	rerankErr   error
	brokenScore bool
}

var _ rag.Reranker = (*Reranker)(nil)

// NewReranker returns a working fake reranker.
func NewReranker() *Reranker { return &Reranker{} }

// FailHealth makes Health return err; nil restores it.
func (r *Reranker) FailHealth(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healthErr = err
}

// FailRerank makes Rerank return err; nil restores it. The retrieval service
// answers any rerank error (a timeout, throttling, an unclassified failure) with
// the fused order and the status "fallback"; it retries a timeout or an
// unavailable service once first.
func (r *Reranker) FailRerank(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rerankErr = err
}

// BreakScores makes Rerank answer with scores that break the protocol (an index
// outside the candidate list), which the retrieval service does not paper over:
// that retrieval fails. It is the one rerank failure that still fails. Passing
// false restores it.
func (r *Reranker) BreakScores(broken bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.brokenScore = broken
}

// Unavailable returns the error a degraded provider yields: one that wraps
// rag.ErrRerankUnavailable.
func Unavailable() error { return fmt.Errorf("fake reranker is down: %w", rag.ErrRerankUnavailable) }

// Calls is the number of Rerank calls, failed ones included.
func (r *Reranker) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// Passages is the total number of passages sent to Rerank.
func (r *Reranker) Passages() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.passages
}

// HealthCalls is the number of Health calls.
func (r *Reranker) HealthCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.healthCalls
}

// Health implements rag.Reranker.
func (r *Reranker) Health(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healthCalls++
	return r.healthErr
}

// Rerank implements rag.Reranker.
func (r *Reranker) Rerank(ctx context.Context, query string, passages []string) ([]rag.RerankScore, error) {
	r.mu.Lock()
	r.calls++
	r.passages += len(passages)
	failure := r.rerankErr
	broken := r.brokenScore
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(passages) == 0 || len(passages) > rag.RerankCandidateK {
		return nil, fmt.Errorf("fake reranker: %d passages, want 1..%d", len(passages), rag.RerankCandidateK)
	}
	if failure != nil {
		return nil, failure
	}
	if broken {
		return []rag.RerankScore{{Index: len(passages) + 5, Score: 1}}, nil
	}
	queryTokens := tokenSet(query)
	scores := make([]rag.RerankScore, len(passages))
	for i, passage := range passages {
		overlap := 0
		for token := range tokenSet(passage) {
			if _, ok := queryTokens[token]; ok {
				overlap++
			}
		}
		scores[i] = rag.RerankScore{Index: i, Score: float64(overlap) / float64(len(queryTokens)+1)}
	}
	sort.SliceStable(scores, func(a, b int) bool {
		if scores[a].Score != scores[b].Score {
			return scores[a].Score > scores[b].Score
		}
		return scores[a].Index < scores[b].Index
	})
	return scores, nil
}

func tokenSet(text string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, token := range Tokens(text) {
		set[token] = struct{}{}
	}
	return set
}
