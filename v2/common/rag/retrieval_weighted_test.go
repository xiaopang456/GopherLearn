package rag

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

func chunkRow(id, content string) RedisChunkSearchResult {
	return RedisChunkSearchResult{RedisKey: id, ChunkID: id, DocID: "doc", DocVersion: 1, RawContent: content, Distance: 0.1}
}

func rows(prefix string, from, to int) []RedisChunkSearchResult {
	var out []RedisChunkSearchResult
	for i := from; i < to; i++ {
		out = append(out, chunkRow(fmt.Sprintf("%s%d", prefix, i), "text "+prefix))
	}
	return out
}

func ids(results []RedisChunkSearchResult) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.ChunkID
	}
	return out
}

func TestFuseWeightedRRFWithWeightOneIsFuseRRF(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	pick := func(n int) []RedisChunkSearchResult {
		perm := r.Perm(40)
		var out []RedisChunkSearchResult
		for _, p := range perm[:n] {
			out = append(out, chunkRow(fmt.Sprintf("c%d", p), "x"))
		}
		return out
	}
	for i := 0; i < 300; i++ {
		vector, lexical := pick(r.Intn(31)), pick(r.Intn(31))
		topK := 1 + r.Intn(45)
		want := FuseRRF(vector, lexical, topK)
		got := FuseWeightedRRF(vector, lexical, topK, 1)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("weight 1 differs from FuseRRF on case %d", i)
		}
	}
}

// The failure the weight exists for: a chunk found by both channels at middling ranks used to
// outscore the vector channel's first choices.
func TestFuseWeightedRRFKeepsTheVectorFirstChoicesAhead(t *testing.T) {
	vector := rows("v", 0, 12)
	agreed := vector[9]                                                     // vector rank 10
	lexical := append([]RedisChunkSearchResult{agreed}, rows("l", 0, 5)...) // keyword rank 1, then keyword-only chunks
	equal := ids(FuseRRF(vector, lexical, 3))
	if equal[0] != agreed.ChunkID {
		t.Fatalf("setup: the agreed chunk should lead under equal weights, got %v", equal)
	}
	weighted := ids(FuseWeightedRRF(vector, lexical, 3, DefaultLexicalWeight))
	// The first two vector choices stay in front; the keyword channel still lifts the chunk it
	// agrees with from vector rank 10 to third, and does not put it first.
	if !reflect.DeepEqual(weighted, []string{"v0", "v1", "v9"}) {
		t.Fatalf("top 3 at weight %g = %v, want [v0 v1 v9]", DefaultLexicalWeight, weighted)
	}
	// Keyword-only chunks stay behind every vector candidate.
	all := ids(FuseWeightedRRF(vector, lexical, 30, DefaultLexicalWeight))
	position := func(id string) int {
		for i, v := range all {
			if v == id {
				return i
			}
		}
		return -1
	}
	if position(agreed.ChunkID) != 2 {
		t.Fatalf("the agreed chunk did not move up: position %d in %v", position(agreed.ChunkID), all)
	}
	for _, only := range []string{"l0", "l1", "l2", "l3", "l4"} {
		if position(only) < len(vector) {
			t.Fatalf("keyword-only chunk %s outranks a vector candidate: %v", only, all)
		}
	}
}

func TestQueryIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"Prompt chaining 这种工作流，主要在拿什么换什么？", nil}, // plain words are not identifiers
		{"appendfsync everysec 后台 fsync 耗时", nil},
		{"AOF 重写和 RDB 快照有什么区别", []string{"AOF", "RDB"}},
		{"context.WithTimeout 取消的时机", []string{"context.WithTimeout"}},
		{"GetOrCreateAIHelper 为什么要加 RLock？", []string{"GetOrCreateAIHelper", "RLock"}},
		{"配置项 max_tool_calls 默认是多少", []string{"max_tool_calls"}},
		{"Go 1.21 的 slog 和 etcd-v3 有什么关系", []string{"1.21", "etcd-v3"}},
		{"the how what and is of", nil},
		{"ab 与 XY 以及 ReAct 的区别", []string{"ReAct"}}, // too short; a two-capital start needs three characters
		{"ReAct ReAct RLock", []string{"ReAct", "RLock"}},
		{"-leading... trailing-- _x_", nil},
	} {
		if got := QueryIdentifiers(tc.query); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("QueryIdentifiers(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestReserveIdentifierSlot(t *testing.T) {
	fused := []RedisChunkSearchResult{
		chunkRow("a", "ordinary text"), chunkRow("b", "more text"), chunkRow("c", "still text"),
		chunkRow("d", "mentions RLock once"), chunkRow("e", "RLock and GetOrCreateAIHelper together"),
	}
	query := "GetOrCreateAIHelper 为什么要加 RLock？"

	got := ids(ReserveIdentifierSlot(query, fused, 3))
	if !reflect.DeepEqual(got, []string{"a", "b", "e"}) {
		t.Fatalf("the chunk with the most identifiers should take the last place, got %v", got)
	}
	// Already present in the top K (chunk d names RLock): nothing changes.
	if got := ids(ReserveIdentifierSlot(query, fused, 4)); !reflect.DeepEqual(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("K=4: got %v", got)
	}
	present := append([]RedisChunkSearchResult{chunkRow("p", "has RLock inside")}, fused...)
	if got := ids(ReserveIdentifierSlot(query, present, 3)); !reflect.DeepEqual(got, []string{"p", "a", "b"}) {
		t.Fatalf("a top K that already has a matching chunk must not change, got %v", got)
	}
	// K < 3, no identifier in the question, no candidate that contains one.
	if got := ids(ReserveIdentifierSlot(query, fused, 2)); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("K=2 must not reserve a slot, got %v", got)
	}
	if got := ids(ReserveIdentifierSlot("Prompt chaining 是什么", fused, 3)); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("a question without identifiers must not reserve a slot, got %v", got)
	}
	if got := ids(ReserveIdentifierSlot("sync.Mutex 的用法", fused, 3)); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("no candidate contains the identifier, got %v", got)
	}
	// Verbatim and case-sensitive.
	if got := ids(ReserveIdentifierSlot("rlock 与 GETORCREATE_X", fused, 3)); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("a different spelling must not match, got %v", got)
	}
	// Shorter lists than K, and the input is never modified.
	before := ids(fused)
	if got := ReserveIdentifierSlot(query, fused[:2], 5); len(got) != 2 {
		t.Fatalf("fewer candidates than K: got %d", len(got))
	}
	ReserveIdentifierSlot(query, fused, 3)
	if !reflect.DeepEqual(before, ids(fused)) {
		t.Fatal("the fused list was modified")
	}
}

func weightedService(t *testing.T, vector, lexical []RedisChunkSearchResult, reranker Reranker) *RAGService {
	t.Helper()
	searcher := &retrievalFakeSearcher{results: vector, textResults: lexical}
	service, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, searcher, func(context.Context, int64) ([]ActiveDocument, error) {
		return []ActiveDocument{{DocID: "doc", Version: 1, OriginalName: "note.md"}}, nil
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if reranker != nil {
		if err := service.UseReranker(context.Background(), reranker); err != nil {
			t.Fatal(err)
		}
	}
	return service
}

func TestRetrieveWithoutRerankUsesTheKeywordWeightButTheRerankPoolKeepsEqualWeights(t *testing.T) {
	vector := rows("v", 0, 12)
	lexical := append([]RedisChunkSearchResult{vector[9]}, rows("l", 0, 5)...)
	ask := func(opts RetrieveOptions) []RetrievalResult {
		service := weightedService(t, vector, lexical, nil)
		opts.TopK = 3
		outcome, err := service.RetrieveWithOptions(context.Background(), 73, "question", opts)
		if err != nil {
			t.Fatal(err)
		}
		return outcome.Results
	}
	chunkIDsOf := func(rs []RetrievalResult) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = r.ChunkID
		}
		return out
	}
	if got := chunkIDsOf(ask(RetrieveOptions{})); !reflect.DeepEqual(got, []string{"v0", "v1", "v9"}) {
		t.Fatalf("default order = %v, want [v0 v1 v9]", got)
	}
	if got := chunkIDsOf(ask(RetrieveOptions{LexicalWeight: 1})); got[0] != "v9" {
		t.Fatalf("weight 1 must be the original equal-weight order, got %v", got)
	}
	if got := chunkIDsOf(ask(RetrieveOptions{LexicalWeight: DefaultLexicalWeight})); !reflect.DeepEqual(got, []string{"v0", "v1", "v9"}) {
		t.Fatalf("an explicit default weight must equal the default, got %v", got)
	}

	// The reranker is given the same passages in the same order whatever the weight is, and the
	// results after a rerank do not depend on it either.
	passagesFor := func(weight float64) ([]string, []string) {
		ranker := &scriptedReranker{steps: []rerankStep{{scores: reverseScores(24)}}}
		service := weightedService(t, vector, lexical, &recordingPassages{inner: ranker})
		outcome, err := service.RetrieveWithOptions(context.Background(), 73, "question", RetrieveOptions{TopK: 3, LexicalWeight: weight, WantCandidates: true})
		if err != nil {
			t.Fatal(err)
		}
		var pool []string
		for _, c := range outcome.Candidates {
			pool = append(pool, c.ChunkID)
		}
		return pool, chunkIDsOf(outcome.Results)
	}
	poolDefault, resultsDefault := passagesFor(0)
	poolEqual, resultsEqual := passagesFor(1)
	if !reflect.DeepEqual(poolDefault, poolEqual) || !reflect.DeepEqual(resultsDefault, resultsEqual) {
		t.Fatalf("the keyword weight changed the rerank pool or the reranked results:\npool %v vs %v\nresults %v vs %v", poolDefault, poolEqual, resultsDefault, resultsEqual)
	}
}

func reverseScores(n int) []RerankScore {
	out := make([]RerankScore, n)
	for i := range out {
		out[i] = RerankScore{Index: i, Score: float64(i)}
	}
	return out
}

// recordingPassages wraps a scripted reranker and answers with scores for exactly the passages it was given.
type recordingPassages struct{ inner *scriptedReranker }

func (r *recordingPassages) Health(ctx context.Context) error { return r.inner.Health(ctx) }

func (r *recordingPassages) Rerank(ctx context.Context, query string, passages []string) ([]RerankScore, error) {
	r.inner.steps = []rerankStep{{scores: reverseScores(len(passages))}}
	return r.inner.Rerank(ctx, query, passages)
}

func TestRetrieveReservesASlotForAnIdentifierAndFallsBackToTheSameOrder(t *testing.T) {
	vector := rows("v", 0, 12)
	lexical := []RedisChunkSearchResult{chunkRow("hit", "calls GetOrCreateAIHelper under RLock")}
	service := weightedService(t, vector, lexical, nil)
	got, err := service.Retrieve(context.Background(), 73, "GetOrCreateAIHelper 为什么要加锁", 5)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(got))
	for i, r := range got {
		ids[i] = r.ChunkID
	}
	if !reflect.DeepEqual(ids, []string{"v0", "v1", "v2", "v3", "hit"}) {
		t.Fatalf("the chunk that names the identifier must take the last place, got %v", ids)
	}
	// A question without an identifier keeps the plain weighted order.
	plain, err := service.Retrieve(context.Background(), 73, "为什么要加锁", 5)
	if err != nil {
		t.Fatal(err)
	}
	if plain[4].ChunkID != "v4" {
		t.Fatalf("no slot was due, got %v", plain[4].ChunkID)
	}
	// A failed reranker falls back to the same order the unreranked retrieval returns.
	failing := weightedService(t, vector, lexical, &scriptedReranker{steps: []rerankStep{{err: ErrRerankConfig}}})
	fell, err := failing.Retrieve(context.Background(), 73, "GetOrCreateAIHelper 为什么要加锁", 5)
	if err != nil {
		t.Fatal(err)
	}
	for i := range fell {
		if fell[i].ChunkID != got[i].ChunkID || !fell[i].RerankFellBack {
			t.Fatalf("fallback result %d = %+v, want %s marked", i, fell[i], got[i].ChunkID)
		}
	}
}

func TestRetrieveRejectsAKeywordWeightOutsideZeroToOne(t *testing.T) {
	service := weightedService(t, rows("v", 0, 3), nil, nil)
	for _, weight := range []float64{-0.1, 1.5} {
		if _, err := service.RetrieveWithOptions(context.Background(), 73, "question", RetrieveOptions{TopK: 3, LexicalWeight: weight}); err == nil {
			t.Fatalf("weight %g was accepted", weight)
		}
	}
}
