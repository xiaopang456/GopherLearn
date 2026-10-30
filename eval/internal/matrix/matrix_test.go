package matrix

import (
	"strings"
	"testing"

	"GopherAI/common/rag"

	"github.com/xiaopang456/GopherLearn/eval/internal/config"
)

func defaultMatrix() config.Matrix { return config.Default().Matrix }

func ids(cells []Cell) string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = c.ID()
	}
	return strings.Join(out, " ")
}

func TestExpandDefaultMatrix(t *testing.T) {
	cells := Expand(defaultMatrix())
	// 3 plain methods x 5 topK, plus hybrid+rerank under the production rule at
	// all 5, plus its forced rule at the two values above 8.
	if len(cells) != 3*5+5+2 {
		t.Fatalf("%d cells: %s", len(cells), ids(cells))
	}
	want := "vector/k=3 vector/k=5 vector/k=8 vector/k=12 vector/k=20 " +
		"keyword/k=3 keyword/k=5 keyword/k=8 keyword/k=12 keyword/k=20 " +
		"hybrid/k=3 hybrid/k=5 hybrid/k=8 hybrid/k=12 hybrid/k=20 " +
		"hybrid+rerank/production/k=3 hybrid+rerank/production/k=5 hybrid+rerank/production/k=8 " +
		"hybrid+rerank/production/k=12 hybrid+rerank/forced/k=12 hybrid+rerank/production/k=20 hybrid+rerank/forced/k=20"
	if got := ids(cells); got != want {
		t.Fatalf("cells:\n got %s\nwant %s", got, want)
	}
}

func TestExpandSortsTopKAndKeepsMethodOrder(t *testing.T) {
	cells := Expand(config.Matrix{Methods: []string{config.MethodHybrid, config.MethodVector}, TopK: []int{20, 3, 8}})
	if got := ids(cells); got != "hybrid/k=3 hybrid/k=8 hybrid/k=20 vector/k=3 vector/k=8 vector/k=20" {
		t.Fatalf("cells = %s", got)
	}
}

func TestExpandHasNoForcedRowAtOrBelowTheProductionLimit(t *testing.T) {
	cells := Expand(config.Matrix{Methods: []string{config.MethodHybridRerank}, TopK: []int{1, 8}})
	if got := ids(cells); got != "hybrid+rerank/production/k=1 hybrid+rerank/production/k=8" {
		t.Fatalf("cells = %s: at k <= 8 the forced row is the production row and must not run twice", got)
	}
}

func TestOptionsRealiseEachCell(t *testing.T) {
	cases := map[Cell]rag.RetrieveOptions{
		{Method: config.MethodVector, TopK: 5}:                             {TopK: 5, Mode: rag.RetrievalModeVector, Rerank: rag.RerankForceOff, WantCandidates: true},
		{Method: config.MethodKeyword, TopK: 5}:                            {TopK: 5, Mode: rag.RetrievalModeKeyword, Rerank: rag.RerankForceOff, WantCandidates: true},
		{Method: config.MethodHybrid, TopK: 5}:                             {TopK: 5, Mode: rag.RetrievalModeHybrid, Rerank: rag.RerankForceOff, WantCandidates: true},
		{Method: config.MethodHybridRerank, Rule: RuleProduction, TopK: 5}: {TopK: 5, Mode: rag.RetrievalModeHybrid, Rerank: rag.RerankDefault, WantCandidates: true},
		{Method: config.MethodHybridRerank, Rule: RuleForced, TopK: 12}:    {TopK: 12, Mode: rag.RetrievalModeHybrid, Rerank: rag.RerankForceOn, WantCandidates: true},
	}
	for cell, want := range cases {
		if got := cell.Options(); got != want {
			t.Errorf("%s: options = %+v, want %+v", cell.ID(), got, want)
		}
	}
}

func TestUsesRerankerFollowsTheProductionRule(t *testing.T) {
	for cell, want := range map[Cell]bool{
		{Method: config.MethodHybrid, TopK: 3}:                             false,
		{Method: config.MethodVector, TopK: 3}:                             false,
		{Method: config.MethodHybridRerank, Rule: RuleProduction, TopK: 8}: true,
		{Method: config.MethodHybridRerank, Rule: RuleProduction, TopK: 9}: false,
		{Method: config.MethodHybridRerank, Rule: RuleForced, TopK: 24}:    true,
	} {
		if got := cell.UsesReranker(); got != want {
			t.Errorf("%s: UsesReranker = %v, want %v", cell.ID(), got, want)
		}
	}
}

func TestNeedsFlags(t *testing.T) {
	keywordOnly := Expand(config.Matrix{Methods: []string{config.MethodKeyword}, TopK: []int{3}})
	if NeedsEmbedding(keywordOnly) || NeedsReranker(keywordOnly) {
		t.Error("a keyword-only matrix needs neither the embedder nor the reranker")
	}
	hybridOnly := Expand(config.Matrix{Methods: []string{config.MethodHybrid}, TopK: []int{3}})
	if !NeedsEmbedding(hybridOnly) || NeedsReranker(hybridOnly) {
		t.Error("hybrid needs the embedder but not the reranker")
	}
	aboveLimit := Expand(config.Matrix{Methods: []string{config.MethodHybridRerank}, TopK: []int{3}})
	if !NeedsReranker(aboveLimit) {
		t.Error("hybrid+rerank at k=3 needs the reranker")
	}
}
