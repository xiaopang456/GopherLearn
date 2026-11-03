package rag

// The generated inputs the options tests compare against the default retrieval.
//
// This file used to hold TestRetrieveMatchesLegacy, which ran a frozen copy of the pre-refactor
// Retrieve (retrieval_legacy_test.go) side by side with the live one. A failed reranker no longer
// takes the retrieval down - the default retrieval keeps the fused order and marks the results -
// so the frozen copy stopped describing the default and was removed together with that test, as
// the rules in the removed file asked. The golden scenarios (retrieval_golden_test.go) keep
// guarding the new default; what is left here is the seeded scenario generator, which
// TestRetrieveWithZeroOptionsMatchesRetrieve still uses.

import (
	"context"
	"fmt"
	"math/rand"
)

const (
	differentialSeed  = 20261030
	differentialCases = 2000
)

var differentialQueries = []string{"如何设置 TTL", "Redis 持久化 AOF", "  padded query  ", "q", "GetOrCreateAIHelper RLock", "中文 and English 混合"}

func randomTopK(r *rand.Rand) int {
	switch roll := r.Intn(100); {
	case roll < 55:
		return 1 + r.Intn(30)
	case roll < 70:
		return 31 + r.Intn(70)
	case roll < 80:
		return []int{0, -1, 101, 500}[r.Intn(4)]
	default:
		return []int{1, 3, 5, 8, 9, 24, 25, 100}[r.Intn(8)]
	}
}

func randomRowCount(r *rand.Rand) int {
	switch r.Intn(4) {
	case 0:
		return 0
	case 1:
		return r.Intn(8)
	case 2:
		return r.Intn(60)
	default:
		return r.Intn(121)
	}
}

func randomRetrievalScenario(r *rand.Rand, index int) retrievalScenario {
	docs := r.Intn(5)
	rowDocs := docs
	if rowDocs == 0 || r.Intn(30) == 0 {
		rowDocs++ // rows may name a document that is not active
	}
	var flags rowFlags
	if r.Intn(5) == 0 {
		flags.noKeyEvery = 2 + r.Intn(5)
	}
	if r.Intn(5) == 0 {
		flags.noEmbedEvery = 2 + r.Intn(5)
	}
	if r.Intn(40) == 0 {
		flags.emptyContent = true
	}
	if r.Intn(25) == 0 {
		flags.staleEvery = 2 + r.Intn(9)
	}
	vectorCount, lexicalCount := randomRowCount(r), randomRowCount(r)
	offset := 0
	if vectorCount > 0 {
		offset = r.Intn(vectorCount + 1)
	}
	sc := retrievalScenario{
		Name: fmt.Sprintf("random-%04d", index), UserID: 73, Query: differentialQueries[r.Intn(len(differentialQueries))],
		TopK: randomTopK(r), Active: scenarioActive(docs),
		Vector: scenarioRows(0, vectorCount, rowDocs, flags), Lexical: scenarioRows(offset, lexicalCount, rowDocs, flags),
	}
	if r.Intn(8) == 0 && len(sc.Lexical) > 3 { // the same chunk twice in one channel
		sc.Lexical = append(sc.Lexical, sc.Lexical[:3]...)
	}
	switch roll := r.Intn(100); {
	case roll < 3:
		sc.Embed = embedError
	case roll < 5:
		sc.Embed = embedCanceled
	case roll < 8:
		sc.Embed = embedWrongCount
	case roll < 11:
		sc.Embed = embedWrongDim
	case roll < 14:
		sc.Embed = embedNaN
	}
	switch roll := r.Intn(100); {
	case roll < 3:
		sc.VectorErr = errScenarioVector
	case roll < 4:
		sc.VectorErr = fmt.Errorf("vector wrapped: %w", context.Canceled)
	}
	switch roll := r.Intn(100); {
	case roll < 3:
		sc.LexicalErr = errScenarioLexical
	case roll < 4:
		sc.LexicalErr = fmt.Errorf("lexical wrapped: %w", context.Canceled)
	}
	if r.Intn(100) < 45 {
		modes := []rerankMode{rerankReverse, rerankTie, rerankMixed, rerankUnavailable, rerankFailure, rerankShort, rerankBadIndex, rerankDupIndex, rerankNaN}
		sc.Rerank = modes[r.Intn(len(modes))]
	}
	sc.PreCancel = r.Intn(100) < 3
	sc.NilContext = !sc.PreCancel && r.Intn(100) < 2
	if r.Intn(100) < 2 {
		sc.ActiveErr = errScenarioLoad
	}
	if r.Intn(100) < 2 {
		sc.UserID = 0
	}
	return sc
}
