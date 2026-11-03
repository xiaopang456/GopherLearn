// Package matrix expands the configured methods and TopK values into the cells a
// run executes, and maps each cell to the retrieval options that realise it.
package matrix

import (
	"fmt"
	"sort"

	"GopherAI/common/rag"

	"github.com/xiaopang456/GopherLearn/eval/internal/config"
)

// Rule says which reranking rule a hybrid+rerank cell follows.
const (
	// RuleNone marks the cells that never rerank.
	RuleNone = ""
	// RuleProduction is the product's rule: rerank only when TopK <= 8. Above that
	// the retrieval is bypassed, so the cell reports what production would return.
	RuleProduction = "production"
	// RuleForced reranks at every TopK up to the reranker's pool of 24. It exists
	// to answer "would reranking help at larger K", which the product never asks.
	RuleForced = "forced"
)

// Cell is one (method, rule, TopK) combination.
type Cell struct {
	Method string
	Rule   string
	TopK   int
}

// ID is the stable name of the cell in records and reports.
func (c Cell) ID() string {
	if c.Rule == RuleNone {
		return fmt.Sprintf("%s/k=%d", c.Method, c.TopK)
	}
	return fmt.Sprintf("%s/%s/k=%d", c.Method, c.Rule, c.TopK)
}

// Options returns the retrieval options that run the cell.
//
// Cells that must not rerank force the rerank off, so attaching a reranker for
// the hybrid+rerank cells cannot change the other cells' results. Every cell asks
// for its fused candidate pool, which the candidate-coverage metric needs.
func (c Cell) Options() rag.RetrieveOptions {
	opts := rag.RetrieveOptions{TopK: c.TopK, WantCandidates: true, Rerank: rag.RerankForceOff}
	switch c.Method {
	case config.MethodVector:
		opts.Mode = rag.RetrievalModeVector
	case config.MethodKeyword:
		opts.Mode = rag.RetrievalModeKeyword
	case config.MethodHybrid:
		opts.Mode = rag.RetrievalModeHybrid
	case config.MethodHybridEqual:
		opts.Mode = rag.RetrievalModeHybrid
		opts.LexicalWeight = 1
	case config.MethodHybridRerank, config.MethodVectorRerank, config.MethodHybridEqualRerank:
		opts.Mode = rag.RetrievalModeHybrid
		if c.Method == config.MethodVectorRerank {
			opts.Mode = rag.RetrievalModeVector
		}
		if c.Method == config.MethodHybridEqualRerank {
			opts.LexicalWeight = 1
		}
		if c.Rule == RuleForced {
			opts.Rerank = rag.RerankForceOn
		} else {
			opts.Rerank = rag.RerankDefault
		}
	}
	return opts
}

// IsRerankMethod reports whether a method reranks (hybrid+rerank, vector+rerank or hybrid-equal+rerank).
func IsRerankMethod(method string) bool {
	return method == config.MethodHybridRerank || method == config.MethodVectorRerank || method == config.MethodHybridEqualRerank
}

// RerankPools is how many differently built candidate pools the matrix sends to the
// reranker for one question: one per rerank method that can actually call it. The
// rerank result is cached by (question, pool), so each pool is one paid call per question.
// hybrid-equal+rerank builds the same pool as hybrid+rerank, so it is not a pool of its own.
func RerankPools(cells []Cell) int {
	methods := map[string]bool{}
	for _, c := range cells {
		if c.UsesReranker() {
			method := c.Method
			if method == config.MethodHybridEqualRerank {
				method = config.MethodHybridRerank
			}
			methods[method] = true
		}
	}
	return len(methods)
}

// UsesReranker reports whether the cell can call the reranker.
func (c Cell) UsesReranker() bool {
	return IsRerankMethod(c.Method) && (c.Rule == RuleForced || c.TopK <= rag.DefaultRerankMaxTopK)
}

// Expand lists the cells in a fixed order: methods as configured, TopK ascending.
//
// hybrid+rerank appears under the production rule at every TopK, and under the
// forced rule only above rag.DefaultRerankMaxTopK: at or below it the two rules
// run the same retrieval, so the forced row there is the production row and is
// not run twice.
func Expand(m config.Matrix) []Cell {
	topK := append([]int(nil), m.TopK...)
	sortInts(topK)
	var cells []Cell
	for _, method := range m.Methods {
		for _, k := range topK {
			if !IsRerankMethod(method) {
				cells = append(cells, Cell{Method: method, Rule: RuleNone, TopK: k})
				continue
			}
			cells = append(cells, Cell{Method: method, Rule: RuleProduction, TopK: k})
			if k > rag.DefaultRerankMaxTopK {
				cells = append(cells, Cell{Method: method, Rule: RuleForced, TopK: k})
			}
		}
	}
	return cells
}

func sortInts(values []int) { sort.Ints(values) }

// NeedsEmbedding reports whether any cell embeds the query.
func NeedsEmbedding(cells []Cell) bool {
	for _, c := range cells {
		if c.Method != config.MethodKeyword {
			return true
		}
	}
	return false
}

// NeedsReranker reports whether any cell can call the reranker.
func NeedsReranker(cells []Cell) bool {
	for _, c := range cells {
		if c.UsesReranker() {
			return true
		}
	}
	return false
}
