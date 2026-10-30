package rag

// Tests for RetrieveWithOptions. They use the recording fakes of
// retrieval_harness_test.go, so they check what the service asks of its
// dependencies and what it returns, not retrieval quality on a real index.

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

func optionsScenario(name string, mutate func(*retrievalScenario)) retrievalScenario {
	sc := retrievalScenario{
		Name: name, UserID: 73, Query: "如何设置 TTL", TopK: 5, Active: scenarioActive(3),
		Vector: scenarioRows(0, 60, 3, rowFlags{}), Lexical: scenarioRows(20, 60, 3, rowFlags{}),
	}
	if mutate != nil {
		mutate(&sc)
	}
	return sc
}

// runWithOptions runs one scenario through RetrieveWithOptions. The scenario's
// TopK is used; every other option comes from opts.
func runWithOptions(t testing.TB, sc retrievalScenario, opts RetrieveOptions) (retrievalRecord, RetrieveOutcome) {
	t.Helper()
	var outcome RetrieveOutcome
	record := runRetrievalScenario(t, sc, func(s *RAGService, ctx context.Context, userID int64, query string, topK int) ([]RetrievalResult, error) {
		opts.TopK = topK
		var err error
		outcome, err = s.RetrieveWithOptions(ctx, userID, query, opts)
		return outcome.Results, err
	})
	return record, outcome
}

func chunkIDs(results []RetrievalResult) []string {
	ids := make([]string, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.ChunkID)
	}
	return ids
}

func rowIDs(first, count int) []string {
	ids := make([]string, 0, count)
	for n := first; n < first+count; n++ {
		ids = append(ids, fmt.Sprintf("chunk-%d", n))
	}
	return ids
}

func hasKind(record retrievalRecord, kind string) bool {
	for _, have := range record.ErrKinds {
		if have == kind {
			return true
		}
	}
	return false
}

// RetrieveWithOptions with only TopK set must be indistinguishable from
// Retrieve: same results, same error, same calls to every dependency, same
// metric counters. It is checked on the fixed scenarios and on the generated
// ones used for the differential test.
func TestRetrieveWithZeroOptionsMatchesRetrieve(t *testing.T) {
	compare := func(sc retrievalScenario) {
		t.Helper()
		want := runRetrievalScenario(t, sc, currentRetrieve)
		got, outcome := runWithOptions(t, sc, RetrieveOptions{})
		if !reflect.DeepEqual(want, got) {
			wantJSON, _ := jsonIndent(want.golden("Retrieve"))
			gotJSON, _ := jsonIndent(got.golden("RetrieveWithOptions"))
			t.Fatalf("RetrieveWithOptions differs from Retrieve on %s\n--- Retrieve\n%s\n--- RetrieveWithOptions\n%s",
				describeScenario(sc), truncateForReport(wantJSON), truncateForReport(gotJSON))
		}
		if want.Err != "" && (outcome.Results != nil || outcome.Candidates != nil || outcome.RerankStatus != "" || outcome.EffectiveCandidateDepth != 0) {
			t.Fatalf("a failed retrieval must return the zero outcome, got %+v on %s", outcome, describeScenario(sc))
		}
	}
	for _, scenario := range goldenScenarios() {
		compare(scenario)
	}
	r := rand.New(rand.NewSource(differentialSeed))
	for index := 0; index < differentialCases; index++ {
		compare(randomRetrievalScenario(r, index))
	}
}

func TestRetrieveWithOptionsVectorMode(t *testing.T) {
	record, outcome := runWithOptions(t, optionsScenario("vector", nil), RetrieveOptions{Mode: RetrievalModeVector})
	if record.Err != "" {
		t.Fatal(record.Err)
	}
	if len(record.LexicalCalls) != 0 || len(record.VectorCalls) != 1 || len(record.EmbedCalls) != 1 {
		t.Fatalf("vector mode calls = embed %d vector %d lexical %d, want 1/1/0", len(record.EmbedCalls), len(record.VectorCalls), len(record.LexicalCalls))
	}
	if !reflect.DeepEqual(chunkIDs(outcome.Results), rowIDs(0, 5)) {
		t.Fatalf("vector results = %v, want the first five vector rows", chunkIDs(outcome.Results))
	}
	for _, result := range outcome.Results {
		if result.MatchKind != "vector" || result.LexicalRank != 0 || result.VectorRank == 0 {
			t.Fatalf("vector-only result carries a keyword signal: %+v", result)
		}
	}
	if outcome.EffectiveCandidateDepth != 30 {
		t.Fatalf("depth = %d, want 30", outcome.EffectiveCandidateDepth)
	}
}

func TestRetrieveWithOptionsKeywordModeNeverEmbeds(t *testing.T) {
	sc := optionsScenario("keyword", func(sc *retrievalScenario) { sc.Embed = embedError }) // an embedder that would fail if it were called
	record, outcome := runWithOptions(t, sc, RetrieveOptions{Mode: RetrievalModeKeyword})
	if record.Err != "" {
		t.Fatalf("keyword mode failed: %s", record.Err)
	}
	if len(record.EmbedCalls) != 0 || len(record.VectorCalls) != 0 || len(record.LexicalCalls) != 1 {
		t.Fatalf("keyword mode calls = embed %d vector %d lexical %d, want 0/0/1", len(record.EmbedCalls), len(record.VectorCalls), len(record.LexicalCalls))
	}
	if !reflect.DeepEqual(chunkIDs(outcome.Results), rowIDs(20, 5)) {
		t.Fatalf("keyword results = %v, want the first five lexical rows", chunkIDs(outcome.Results))
	}
	for _, result := range outcome.Results {
		if result.MatchKind != "lexical" || result.VectorRank != 0 || result.LexicalRank == 0 {
			t.Fatalf("keyword-only result carries a vector signal: %+v", result)
		}
	}
}

func TestRetrieveWithOptionsSingleChannelIgnoresTheOtherChannelsFailure(t *testing.T) {
	vectorOnly := optionsScenario("vector ignores lexical failure", func(sc *retrievalScenario) { sc.LexicalErr = errScenarioLexical })
	if record, _ := runWithOptions(t, vectorOnly, RetrieveOptions{Mode: RetrievalModeVector}); record.Err != "" {
		t.Fatalf("vector mode must not depend on the keyword search: %s", record.Err)
	}
	keywordOnly := optionsScenario("keyword ignores vector failure", func(sc *retrievalScenario) { sc.VectorErr, sc.Embed = errScenarioVector, embedError })
	if record, _ := runWithOptions(t, keywordOnly, RetrieveOptions{Mode: RetrievalModeKeyword}); record.Err != "" {
		t.Fatalf("keyword mode must not depend on the vector search or the embedder: %s", record.Err)
	}
	// The channel that does run still reports its own failure with its own reason.
	failing := optionsScenario("vector mode reports a vector failure", func(sc *retrievalScenario) { sc.VectorErr = errScenarioVector })
	record, _ := runWithOptions(t, failing, RetrieveOptions{Mode: RetrievalModeVector})
	if record.Reason != string(RetrievalReasonVector) {
		t.Fatalf("reason = %q, want %q", record.Reason, RetrievalReasonVector)
	}
}

func TestRetrieveWithOptionsRejectsBadOptions(t *testing.T) {
	cases := []struct {
		name string
		opts RetrieveOptions
		sc   func(*retrievalScenario)
	}{
		{"unknown mode", RetrieveOptions{Mode: "semantic"}, nil},
		{"unknown rerank policy", RetrieveOptions{Rerank: RerankPolicy(7)}, nil},
		{"negative depth", RetrieveOptions{CandidateDepth: -1}, nil},
		{"depth above 100", RetrieveOptions{CandidateDepth: 101}, nil},
		{"forced rerank without a reranker", RetrieveOptions{Rerank: RerankForceOn}, nil},
		{"forced rerank above the candidate pool", RetrieveOptions{Rerank: RerankForceOn}, func(sc *retrievalScenario) { sc.Rerank, sc.TopK = rerankReverse, RerankCandidateK+1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record, outcome := runWithOptions(t, optionsScenario(tc.name, tc.sc), tc.opts)
			if !hasKind(record, "invalid_request") {
				t.Fatalf("error kinds = %v (%q), want invalid_request", record.ErrKinds, record.Err)
			}
			if len(record.Loads)+len(record.EmbedCalls)+len(record.VectorCalls)+len(record.LexicalCalls)+len(record.RerankCalls) != 0 {
				t.Fatalf("a rejected request must not touch any dependency: %+v", record)
			}
			if !reflect.DeepEqual(outcome, RetrieveOutcome{}) {
				t.Fatalf("outcome = %+v, want zero", outcome)
			}
		})
	}
}

func TestRetrieveWithOptionsRerankPolicy(t *testing.T) {
	withReranker := func(topK int, mode rerankMode) retrievalScenario {
		return optionsScenario(fmt.Sprintf("rerank topk=%d", topK), func(sc *retrievalScenario) { sc.Rerank, sc.TopK = mode, topK })
	}
	t.Run("default follows the production rule", func(t *testing.T) {
		for _, tc := range []struct {
			topK   int
			status RerankStatus
			calls  int
		}{{1, RerankApplied, 1}, {8, RerankApplied, 1}, {9, RerankBypassed, 0}, {12, RerankBypassed, 0}, {30, RerankBypassed, 0}} {
			record, outcome := runWithOptions(t, withReranker(tc.topK, rerankReverse), RetrieveOptions{})
			if outcome.RerankStatus != tc.status || len(record.RerankCalls) != tc.calls || len(outcome.Results) != tc.topK {
				t.Fatalf("topK=%d: status %q calls %d results %d, want %q %d %d", tc.topK, outcome.RerankStatus, len(record.RerankCalls), len(outcome.Results), tc.status, tc.calls, tc.topK)
			}
		}
	})
	t.Run("force on reranks above the production threshold", func(t *testing.T) {
		for _, topK := range []int{9, 12, 20, RerankCandidateK} {
			record, outcome := runWithOptions(t, withReranker(topK, rerankReverse), RetrieveOptions{Rerank: RerankForceOn})
			if record.Err != "" || outcome.RerankStatus != RerankApplied || len(record.RerankCalls) != 1 {
				t.Fatalf("topK=%d: err %q status %q calls %d", topK, record.Err, outcome.RerankStatus, len(record.RerankCalls))
			}
			if got := len(record.RerankCalls[0].Passages); got != RerankCandidateK {
				t.Fatalf("topK=%d: reranker saw %d passages, want the pool of %d", topK, got, RerankCandidateK)
			}
			if len(outcome.Results) != topK || outcome.EffectiveCandidateDepth != 48 {
				t.Fatalf("topK=%d: %d results at depth %d, want %d at 48", topK, len(outcome.Results), outcome.EffectiveCandidateDepth, topK)
			}
			if !outcome.Results[0].RerankApplied || outcome.Results[0].RerankRank != 1 {
				t.Fatalf("topK=%d: first result is not marked reranked: %+v", topK, outcome.Results[0])
			}
		}
	})
	t.Run("force off bypasses the reranker and uses the no-reranker depth", func(t *testing.T) {
		record, outcome := runWithOptions(t, withReranker(5, rerankReverse), RetrieveOptions{Rerank: RerankForceOff})
		if outcome.RerankStatus != RerankBypassed || len(record.RerankCalls) != 0 || outcome.EffectiveCandidateDepth != 30 {
			t.Fatalf("status %q calls %d depth %d, want bypassed 0 30", outcome.RerankStatus, len(record.RerankCalls), outcome.EffectiveCandidateDepth)
		}
		if record.Counters["knowledge_rerank_total{status=bypassed}"] != 1 {
			t.Fatalf("a bypassed rerank must be counted like in production: %v", record.Counters)
		}
		// It must be the same retrieval as a deployment that has no reranker at all.
		plainRecord, plainOutcome := runWithOptions(t, withReranker(5, rerankNone), RetrieveOptions{})
		if !reflect.DeepEqual(outcome.Results, plainOutcome.Results) || !reflect.DeepEqual(record.VectorCalls, plainRecord.VectorCalls) || !reflect.DeepEqual(record.LexicalCalls, plainRecord.LexicalCalls) {
			t.Fatalf("force-off with a reranker differs from having no reranker")
		}
		if plainOutcome.RerankStatus != RerankNotAttached {
			t.Fatalf("status without a reranker = %q", plainOutcome.RerankStatus)
		}
	})
	t.Run("an unavailable reranker falls back to the fused order", func(t *testing.T) {
		record, outcome := runWithOptions(t, withReranker(5, rerankUnavailable), RetrieveOptions{})
		if record.Err != "" || outcome.RerankStatus != RerankFallback || len(outcome.Results) != 5 {
			t.Fatalf("err %q status %q results %d", record.Err, outcome.RerankStatus, len(outcome.Results))
		}
		for _, result := range outcome.Results {
			if result.RerankApplied {
				t.Fatalf("a fallback result is marked reranked: %+v", result)
			}
		}
	})
}

// The premise behind the evaluation's rerank cache: with a forced rerank the
// reranker sees the same pool whatever TopK is, so one call serves every TopK,
// and a smaller TopK is a prefix of a larger one.
func TestForcedRerankPoolDoesNotDependOnTopK(t *testing.T) {
	var pools [][]string
	var firstResults [][]string
	for _, topK := range []int{3, 8, 12, 20} {
		sc := optionsScenario("pool", func(sc *retrievalScenario) { sc.Rerank, sc.TopK = rerankReverse, topK })
		record, outcome := runWithOptions(t, sc, RetrieveOptions{Rerank: RerankForceOn})
		if record.Err != "" || len(record.RerankCalls) != 1 {
			t.Fatalf("topK=%d: err %q rerank calls %d", topK, record.Err, len(record.RerankCalls))
		}
		pools = append(pools, record.RerankCalls[0].Passages)
		firstResults = append(firstResults, chunkIDs(outcome.Results))
	}
	for index := 1; index < len(pools); index++ {
		if !reflect.DeepEqual(pools[0], pools[index]) {
			t.Fatalf("the reranker saw a different pool for another TopK")
		}
	}
	for index := 1; index < len(firstResults); index++ {
		shorter, longer := firstResults[index-1], firstResults[index]
		if !reflect.DeepEqual(shorter, longer[:len(shorter)]) {
			t.Fatalf("results of a smaller TopK are not a prefix of a larger one: %v vs %v", shorter, longer)
		}
	}
}

func TestRetrieveWithOptionsCandidateDepth(t *testing.T) {
	record, outcome := runWithOptions(t, optionsScenario("depth", nil), RetrieveOptions{CandidateDepth: 17})
	if record.VectorCalls[0].TopK != 17 || record.LexicalCalls[0].TopK != 17 || outcome.EffectiveCandidateDepth != 17 {
		t.Fatalf("depth 17 reached vector %d lexical %d, outcome %d", record.VectorCalls[0].TopK, record.LexicalCalls[0].TopK, outcome.EffectiveCandidateDepth)
	}
	forced := optionsScenario("depth forced rerank", func(sc *retrievalScenario) { sc.Rerank, sc.TopK = rerankReverse, 12 })
	record, outcome = runWithOptions(t, forced, RetrieveOptions{Rerank: RerankForceOn, CandidateDepth: 40})
	if outcome.EffectiveCandidateDepth != 40 || record.VectorCalls[0].TopK != 40 {
		t.Fatalf("an explicit depth must win over the rerank depth of 48: %d", outcome.EffectiveCandidateDepth)
	}
	_, outcome = runWithOptions(t, optionsScenario("depth default", nil), RetrieveOptions{})
	if outcome.EffectiveCandidateDepth != 30 {
		t.Fatalf("default depth = %d, want 30", outcome.EffectiveCandidateDepth)
	}
	_, outcome = runWithOptions(t, optionsScenario("depth default large", func(sc *retrievalScenario) { sc.TopK = 30 }), RetrieveOptions{})
	if outcome.EffectiveCandidateDepth != 100 { // 30*6 = 180, capped
		t.Fatalf("default depth for topK 30 = %d, want 100", outcome.EffectiveCandidateDepth)
	}
}

func TestRetrieveWithOptionsCandidatePool(t *testing.T) {
	t.Run("not requested", func(t *testing.T) {
		_, outcome := runWithOptions(t, optionsScenario("pool off", nil), RetrieveOptions{})
		if outcome.Candidates != nil {
			t.Fatalf("candidates returned without being asked for: %d", len(outcome.Candidates))
		}
	})
	t.Run("without a reranker it is the top of the fused list", func(t *testing.T) {
		sc := optionsScenario("pool plain", nil)
		_, outcome := runWithOptions(t, sc, RetrieveOptions{WantCandidates: true})
		fused := FuseRRF(truncateRows(sc.Vector, 30), truncateRows(sc.Lexical, 30), RerankCandidateK)
		want := make([]string, 0, len(fused))
		for _, candidate := range fused {
			want = append(want, candidate.ChunkID)
		}
		if len(outcome.Candidates) != RerankCandidateK || !reflect.DeepEqual(chunkIDs(outcome.Candidates), want) {
			t.Fatalf("pool = %v, want %v", chunkIDs(outcome.Candidates), want)
		}
		// The pool is the fused order, so the final top-K is its prefix.
		if !reflect.DeepEqual(chunkIDs(outcome.Results), want[:len(outcome.Results)]) {
			t.Fatalf("results %v are not the prefix of the pool %v", chunkIDs(outcome.Results), want)
		}
	})
	t.Run("with a reranker it is what the reranker was given", func(t *testing.T) {
		sc := optionsScenario("pool reranked", func(sc *retrievalScenario) { sc.Rerank, sc.TopK = rerankReverse, 5 })
		record, outcome := runWithOptions(t, sc, RetrieveOptions{WantCandidates: true})
		if len(outcome.Candidates) != RerankCandidateK || len(record.RerankCalls) != 1 {
			t.Fatalf("pool %d, rerank calls %d", len(outcome.Candidates), len(record.RerankCalls))
		}
		for index, candidate := range outcome.Candidates {
			if candidate.RerankApplied || candidate.RerankRank != 0 || candidate.RerankScore != 0 {
				t.Fatalf("a candidate before rerank carries rerank fields: %+v", candidate)
			}
			if record.RerankCalls[0].Passages[index] != candidate.EmbeddingContent {
				t.Fatalf("candidate %d is not the passage the reranker saw", index)
			}
		}
		inPool := map[string]bool{}
		for _, candidate := range outcome.Candidates {
			inPool[candidate.ChunkID] = true
		}
		for _, result := range outcome.Results {
			if !inPool[result.ChunkID] {
				t.Fatalf("result %s is not in the candidate pool", result.ChunkID)
			}
		}
	})
	t.Run("fewer candidates than the pool size", func(t *testing.T) {
		sc := optionsScenario("pool small", func(sc *retrievalScenario) {
			sc.Vector, sc.Lexical = scenarioRows(0, 5, 3, rowFlags{}), nil
		})
		_, outcome := runWithOptions(t, sc, RetrieveOptions{WantCandidates: true})
		if len(outcome.Candidates) != 5 {
			t.Fatalf("pool = %d, want 5", len(outcome.Candidates))
		}
	})
	t.Run("no active documents", func(t *testing.T) {
		sc := optionsScenario("pool empty", func(sc *retrievalScenario) { sc.Active = []ActiveDocument{} })
		_, outcome := runWithOptions(t, sc, RetrieveOptions{WantCandidates: true})
		if outcome.Results == nil || len(outcome.Results) != 0 || outcome.Candidates == nil || len(outcome.Candidates) != 0 {
			t.Fatalf("outcome = %+v, want empty non-nil slices", outcome)
		}
	})
	t.Run("a stale candidate in the pool fails the retrieval", func(t *testing.T) {
		sc := optionsScenario("pool stale", func(sc *retrievalScenario) { sc.Vector = scenarioRows(0, 60, 3, rowFlags{staleEvery: 7}) })
		record, _ := runWithOptions(t, sc, RetrieveOptions{WantCandidates: true})
		if !hasKind(record, "failed") {
			t.Fatalf("error kinds = %v", record.ErrKinds)
		}
	})
}

func TestRetrieveOptionsZeroValueAlwaysValidates(t *testing.T) {
	for _, attached := range []bool{false, true} {
		if err := (RetrieveOptions{TopK: 5}).validate(attached); err != nil {
			t.Fatalf("the zero options must validate (reranker attached=%t): %v", attached, err)
		}
	}
}
