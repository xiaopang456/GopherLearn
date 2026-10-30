package runner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"GopherAI/common/rag"
)

// Hit is one retrieved chunk as the raw record stores it: where it is and how it
// scored. It carries no chunk text, so a results directory never holds the
// user's document content.
type Hit struct {
	Rank        int     `json:"rank"`
	File        string  `json:"file"`
	DocID       string  `json:"doc_id"`
	Version     int     `json:"version"`
	ChunkIndex  int     `json:"chunk_index"`
	Start       int     `json:"start"`
	End         int     `json:"end"`
	Score       float64 `json:"score"`
	Distance    float64 `json:"distance,omitempty"`
	FusionScore float64 `json:"fusion_score,omitempty"`
	VectorRank  int     `json:"vector_rank,omitempty"`
	LexicalRank int     `json:"lexical_rank,omitempty"`
	MatchKind   string  `json:"match_kind,omitempty"`
	RerankScore float64 `json:"rerank_score,omitempty"`
	RerankRank  int     `json:"rerank_rank,omitempty"`
}

// Record is the outcome of one question in one cell.
type Record struct {
	QuestionID string `json:"question_id"`
	Cell       string `json:"cell"`
	Method     string `json:"method"`
	Rule       string `json:"rule,omitempty"`
	TopK       int    `json:"top_k"`
	// EffectiveDepth is how many candidates each channel fetched.
	EffectiveDepth int    `json:"effective_depth"`
	RerankStatus   string `json:"rerank_status,omitempty"`
	// Results is the final top-K. Pool is the fused candidate list before any
	// rerank (at most 24), the same list the reranker would see.
	Results []Hit `json:"results"`
	Pool    []Hit `json:"pool"`
	// ErrorKind is set when the retrieval failed. It is a short code, never a
	// provider message.
	ErrorKind string `json:"error_kind,omitempty"`
	// EmbeddingCalls and RerankCalls are the real provider calls this record
	// caused; a cache hit costs none.
	EmbeddingCalls int     `json:"embedding_calls"`
	RerankCalls    int     `json:"rerank_calls"`
	ElapsedMs      float64 `json:"elapsed_ms"`
}

func hitsOf(results []rag.RetrievalResult) []Hit {
	hits := make([]Hit, len(results))
	for i, r := range results {
		hits[i] = Hit{
			Rank: i + 1, File: r.DocumentName, DocID: r.DocID, Version: r.DocVersion, ChunkIndex: r.ChunkIndex,
			Start: r.StartLine, End: r.EndLine, Score: r.Score, Distance: r.Distance, FusionScore: r.FusionScore,
			VectorRank: r.VectorRank, LexicalRank: r.LexicalRank, MatchKind: r.MatchKind,
			RerankScore: r.RerankScore, RerankRank: r.RerankRank,
		}
	}
	return hits
}

// errorKind reduces an error to a bounded code.
func errorKind(err error) string {
	if reason, ok := rag.RetrievalUnavailableReasonOf(err); ok {
		return "unavailable:" + string(reason)
	}
	switch {
	case errors.Is(err, ErrBudgetExhausted):
		return "budget_exhausted"
	case errors.Is(err, rag.ErrInvalidRetrievalRequest):
		return "invalid_request"
	case errors.Is(err, rag.ErrRetrievalFailed):
		return "retrieval_failed"
	default:
		return "failed"
	}
}

// ReadRecords parses a records.jsonl stream.
func ReadRecords(r io.Reader) ([]Record, error) {
	var records []Record
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for line := 1; scanner.Scan(); line++ {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("records line %d: %w", line, err)
		}
		records = append(records, rec)
	}
	return records, scanner.Err()
}
