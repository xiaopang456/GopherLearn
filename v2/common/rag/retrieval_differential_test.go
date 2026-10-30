package rag

// TestRetrieveMatchesLegacy runs the frozen pre-refactor Retrieve
// (retrieval_legacy_test.go) and the live Retrieve on the same generated inputs
// and requires every observable effect to be identical: the results (nil and
// empty are different), the error text and every sentinel it matches, every call
// to the loader, the embedder, both searches and the reranker, and the metric
// counters. The inputs come from a fixed seed, so a failure is reproducible.
//
// It exists to prove the E2 refactor; see the rules in retrieval_legacy_test.go
// for what to do when the default behaviour is changed on purpose.

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
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

func TestRetrieveMatchesLegacy(t *testing.T) {
	r := rand.New(rand.NewSource(differentialSeed))
	outcomes := map[string]int{}
	for index := 0; index < differentialCases; index++ {
		scenario := randomRetrievalScenario(r, index)
		legacy := runRetrievalScenario(t, scenario, legacyRetrieve)
		current := runRetrievalScenario(t, scenario, currentRetrieve)
		if !reflect.DeepEqual(legacy, current) {
			legacyJSON, _ := jsonIndent(legacy.golden("legacy"))
			currentJSON, _ := jsonIndent(current.golden("current"))
			t.Fatalf("Retrieve differs from the frozen legacy copy on %s\n--- legacy\n%s\n--- current\n%s",
				describeScenario(scenario), truncateForReport(legacyJSON), truncateForReport(currentJSON))
		}
		switch {
		case legacy.Err != "":
			outcomes["error"]++
		case len(legacy.Results) == 0:
			outcomes["empty"]++
		default:
			outcomes["results"]++
		}
		if len(legacy.RerankCalls) > 0 {
			outcomes["reranked"]++
		}
	}
	// The generator must keep reaching every kind of outcome; otherwise the test
	// would pass without comparing anything interesting.
	for _, outcome := range []string{"error", "empty", "results", "reranked"} {
		if outcomes[outcome] < 100 {
			t.Errorf("only %d of %d generated cases ended as %q; the generator no longer covers it", outcomes[outcome], differentialCases, outcome)
		}
	}
}
