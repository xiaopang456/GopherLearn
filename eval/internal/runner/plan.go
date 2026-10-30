package runner

import (
	"context"
	"fmt"
	"io"

	"GopherAI/common/rag"

	"github.com/xiaopang456/GopherLearn/eval/internal/config"
	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
	"github.com/xiaopang456/GopherLearn/eval/internal/index"
	"github.com/xiaopang456/GopherLearn/eval/internal/matrix"
)

// PlanInput is what an estimate needs; it touches no service.
type PlanInput struct {
	Config         *config.Config
	Corpus         *dataset.Corpus
	Questions      []dataset.Question
	Chunker        rag.ChunkerConfig
	MaxSourceBytes int64
	// IndexExists means the index is already built, so indexing costs nothing.
	IndexExists bool
}

// Estimate is the offline forecast of a run's work and paid calls.
type Estimate struct {
	Questions int `json:"questions"`
	// DistinctQueries is the number of distinct question texts: the unit a query
	// embedding or a rerank is cached by.
	DistinctQueries int `json:"distinct_queries"`
	Cells           int `json:"cells"`
	Retrievals      int `json:"retrievals"`

	IndexChunks int `json:"index_chunks"`
	// IndexEmbeddingCalls is zero when the index already exists.
	IndexEmbeddingCalls int `json:"index_embedding_calls"`
	QueryEmbeddingCalls int `json:"query_embedding_calls"`
	EmbeddingCalls      int `json:"embedding_calls"`

	RerankQueryCalls int `json:"rerank_query_calls"`
	// RerankHealthCalls is the startup check, one real call to a hosted reranker.
	RerankHealthCalls int `json:"rerank_health_calls"`
	RerankCalls       int `json:"rerank_calls"`

	Chunks index.ChunkPlan `json:"chunks"`
}

// MakePlan forecasts a run. It chunks the corpus through the product's ingestor
// (so the chunk count is exact) and derives the calls from the matrix; the
// figures are upper bounds that a run with healthy services meets exactly.
func MakePlan(ctx context.Context, in PlanInput) (Estimate, error) {
	cells := matrix.Expand(in.Config.Matrix)
	est := Estimate{Questions: len(in.Questions), Cells: len(cells)}
	est.Retrievals = est.Questions * est.Cells
	distinct := map[string]bool{}
	for _, q := range in.Questions {
		distinct[q.Question] = true
	}
	est.DistinctQueries = len(distinct)

	chunks, err := index.Plan(ctx, in.Config.Services.EvalUserID, in.Corpus, in.Chunker, index.DefaultBatchSize, in.MaxSourceBytes)
	if err != nil {
		return Estimate{}, err
	}
	est.Chunks = chunks
	est.IndexChunks = chunks.Total
	if !in.IndexExists {
		est.IndexEmbeddingCalls = chunks.EmbeddingCalls
	}
	if matrix.NeedsEmbedding(cells) {
		est.QueryEmbeddingCalls = est.DistinctQueries
	}
	est.EmbeddingCalls = est.IndexEmbeddingCalls + est.QueryEmbeddingCalls
	if matrix.NeedsReranker(cells) {
		est.RerankQueryCalls = est.DistinctQueries
		est.RerankHealthCalls = 1
	}
	est.RerankCalls = est.RerankQueryCalls + est.RerankHealthCalls
	return est, nil
}

// CheckLimits refuses an estimate the limits do not cover. A limit of zero
// allows no call, so a configuration without limits can never spend anything.
func (e Estimate) CheckLimits(l config.Limits) error {
	if e.EmbeddingCalls > l.MaxEmbeddingCalls {
		return fmt.Errorf("%w: the run needs %d embedding calls (%d to index, %d for queries) and limits.maxEmbeddingCalls is %d",
			ErrOverLimit, e.EmbeddingCalls, e.IndexEmbeddingCalls, e.QueryEmbeddingCalls, l.MaxEmbeddingCalls)
	}
	if e.RerankCalls > l.MaxRerankCalls {
		return fmt.Errorf("%w: the run needs %d rerank calls (%d for queries, %d startup check) and limits.maxRerankCalls is %d",
			ErrOverLimit, e.RerankCalls, e.RerankQueryCalls, e.RerankHealthCalls, l.MaxRerankCalls)
	}
	return nil
}

// ErrOverLimit is returned when the plan exceeds a configured limit.
var ErrOverLimit = fmt.Errorf("the plan exceeds a paid-call limit")

// Write prints the estimate for a person.
func (e Estimate) Write(w io.Writer, l config.Limits) {
	fmt.Fprintf(w, "questions        %d (%d distinct texts) x %d cells = %d retrievals\n", e.Questions, e.DistinctQueries, e.Cells, e.Retrievals)
	fmt.Fprintf(w, "index            %d chunks in %d documents\n", e.IndexChunks, len(e.Chunks.Chunks))
	fmt.Fprintf(w, "embedding calls  %d = %d indexing + %d queries   (limit %d)\n", e.EmbeddingCalls, e.IndexEmbeddingCalls, e.QueryEmbeddingCalls, l.MaxEmbeddingCalls)
	fmt.Fprintf(w, "rerank calls     %d = %d queries + %d startup check   (limit %d)\n", e.RerankCalls, e.RerankQueryCalls, e.RerankHealthCalls, l.MaxRerankCalls)
	if err := e.CheckLimits(l); err != nil {
		fmt.Fprintf(w, "limits           a real run would be refused: %v\n", err)
	} else {
		fmt.Fprintln(w, "limits           a real run is within the limits")
	}
}
