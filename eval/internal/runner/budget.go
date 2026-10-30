package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/cloudwego/eino/components/embedding"

	"GopherAI/common/rag"
)

// ErrBudgetExhausted is returned instead of making a paid call the limit does not
// allow. It is a safety net behind the pre-run check, which refuses a run whose
// plan already exceeds the limit.
var ErrBudgetExhausted = errors.New("paid-call limit reached")

// Budget counts the paid calls a run makes and refuses the one that would
// exceed the limit. A call is one request to the provider, not one text.
type Budget struct {
	mu         sync.Mutex
	enforce    bool
	embedMax   int
	rerankMax  int
	embedUsed  int
	rerankUsed int
}

// NewBudget returns a budget. When enforce is false the calls are only counted;
// an offline run uses that, since its "calls" cost nothing.
func NewBudget(enforce bool, maxEmbeddingCalls, maxRerankCalls int) *Budget {
	return &Budget{enforce: enforce, embedMax: maxEmbeddingCalls, rerankMax: maxRerankCalls}
}

// Used reports the calls made so far.
func (b *Budget) Used() (embedding, rerank int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.embedUsed, b.rerankUsed
}

func (b *Budget) spendEmbedding() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.enforce && b.embedUsed >= b.embedMax {
		return fmt.Errorf("%w: embedding calls (limit %d)", ErrBudgetExhausted, b.embedMax)
	}
	b.embedUsed++
	return nil
}

func (b *Budget) spendRerank() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.enforce && b.rerankUsed >= b.rerankMax {
		return fmt.Errorf("%w: rerank calls (limit %d)", ErrBudgetExhausted, b.rerankMax)
	}
	b.rerankUsed++
	return nil
}

// countingEmbedder charges every call to the budget. Indexing uses it: each
// batch is a distinct text set, so there is nothing to cache.
type countingEmbedder struct {
	inner  embedding.Embedder
	budget *Budget
}

func (c countingEmbedder) EmbedStrings(ctx context.Context, texts []string, opts ...embedding.Option) ([][]float64, error) {
	if err := c.budget.spendEmbedding(); err != nil {
		return nil, err
	}
	return c.inner.EmbedStrings(ctx, texts, opts...)
}

// cachingEmbedder remembers the embedding of each query text, and the failure of
// one that failed, so a question is embedded once however many cells ask for it.
// Remembering a failure also means a failed question is not retried.
type cachingEmbedder struct {
	inner  embedding.Embedder
	budget *Budget

	mu    sync.Mutex
	cache map[string]embedResult
}

type embedResult struct {
	vector []float64
	err    error
}

func newCachingEmbedder(inner embedding.Embedder, budget *Budget) *cachingEmbedder {
	return &cachingEmbedder{inner: inner, budget: budget, cache: map[string]embedResult{}}
}

func (c *cachingEmbedder) EmbedStrings(ctx context.Context, texts []string, opts ...embedding.Option) ([][]float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var missing []string
	seen := map[string]bool{}
	for _, text := range texts {
		if _, ok := c.cache[text]; !ok && !seen[text] {
			missing = append(missing, text)
			seen[text] = true
		}
	}
	if len(missing) > 0 {
		if err := c.budget.spendEmbedding(); err != nil {
			return nil, err
		}
		vectors, err := c.inner.EmbedStrings(ctx, missing, opts...)
		switch {
		case err == nil && len(vectors) != len(missing):
			err = fmt.Errorf("embedder returned %d vectors for %d texts", len(vectors), len(missing))
			fallthrough
		case err != nil:
			if ctxErr := ctx.Err(); ctxErr != nil {
				// A cancelled run is not a property of the text; do not remember it.
				return nil, ctxErr
			}
			for _, text := range missing {
				c.cache[text] = embedResult{err: err}
			}
		default:
			for i, text := range missing {
				c.cache[text] = embedResult{vector: append([]float64(nil), vectors[i]...)}
			}
		}
	}
	out := make([][]float64, len(texts))
	for i, text := range texts {
		entry := c.cache[text]
		if entry.err != nil {
			return nil, entry.err
		}
		out[i] = append([]float64(nil), entry.vector...)
	}
	return out, nil
}

// cachingReranker remembers the scores for each (query, passages) pair. The
// fused pool a forced or production rerank sees is the same for every TopK of a
// question, so one question costs one rerank call however many cells rerank it.
// Failures are remembered too, for the same no-retry reason as the embedder.
type cachingReranker struct {
	inner  rag.Reranker
	budget *Budget

	mu    sync.Mutex
	cache map[string]rerankResult
}

type rerankResult struct {
	scores []rag.RerankScore
	err    error
}

func newCachingReranker(inner rag.Reranker, budget *Budget) *cachingReranker {
	return &cachingReranker{inner: inner, budget: budget, cache: map[string]rerankResult{}}
}

// Health is the startup check. It is one real call to a hosted provider, so it
// is charged to the budget like any other.
func (c *cachingReranker) Health(ctx context.Context) error {
	if err := c.budget.spendRerank(); err != nil {
		return err
	}
	return c.inner.Health(ctx)
}

func (c *cachingReranker) Rerank(ctx context.Context, query string, passages []string) ([]rag.RerankScore, error) {
	key := rerankKey(query, passages)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[key]
	if !ok {
		if err := c.budget.spendRerank(); err != nil {
			return nil, err
		}
		scores, err := c.inner.Rerank(ctx, query, passages)
		if ctxErr := ctx.Err(); ctxErr != nil && err != nil {
			return nil, ctxErr
		}
		entry = rerankResult{scores: append([]rag.RerankScore(nil), scores...), err: err}
		c.cache[key] = entry
	}
	if entry.err != nil {
		return nil, entry.err
	}
	// The retrieval service sorts the slice it is given, so hand out a copy.
	return append([]rag.RerankScore(nil), entry.scores...), nil
}

func rerankKey(query string, passages []string) string {
	sum := sha256.New()
	sum.Write([]byte(strconv.Itoa(len(query)) + ":" + query))
	for _, passage := range passages {
		sum.Write([]byte("|" + strconv.Itoa(len(passage)) + ":"))
		sum.Write([]byte(passage))
	}
	return hex.EncodeToString(sum.Sum(nil))
}
