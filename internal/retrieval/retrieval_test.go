package retrieval

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

type fixedSource struct {
	chunks  []domain.DocumentChunk
	err     error
	calls   int
	scopes  [][]string
	userIDs []string
}

func (s *fixedSource) Chunks(_ context.Context, userID string, collectionIDs []string) ([]domain.DocumentChunk, error) {
	s.calls++
	s.scopes = append(s.scopes, append([]string(nil), collectionIDs...))
	s.userIDs = append(s.userIDs, userID)
	if s.err != nil {
		return nil, s.err
	}
	return s.chunks, nil
}

func chunk(id, collection, document string, order int, text string) domain.DocumentChunk {
	return domain.DocumentChunk{ID: id, CollectionID: collection, DocumentID: document, Order: order, Text: text, Version: 1}
}

var corpus = []domain.DocumentChunk{
	chunk("c1", "col-a", "doc-1", 1, "Redis 持久化：AOF 以追加写记录每条写命令，fsync 策略可选 everysec。"),
	chunk("c2", "col-a", "doc-1", 2, "RDB 在 fork 出的子进程里生成内存快照，恢复速度快于重放日志。"),
	chunk("c3", "col-a", "doc-2", 3, "Kafka 的分区日志按 offset 顺序追加，消费者自己维护位置。"),
	chunk("c4", "col-b", "doc-3", 4, "分布式锁可以用 Redlock，但它的安全性存在争议。"),
}

func TestTokenizeHandlesCJKAndLatin(t *testing.T) {
	latin := Tokenize("Hello World fsync_everysec")
	if !reflect.DeepEqual(latin, []string{"hello", "world", "fsync_everysec"}) {
		t.Fatalf("latin tokens = %#v", latin)
	}
	cjk := Tokenize("持久化")
	// Bigrams carry the word meaning; lone characters are not terms, so a shared
	// character cannot pass for a shared word.
	want := []string{"持久", "久化"}
	if !reflect.DeepEqual(cjk, want) {
		t.Fatalf("cjk tokens = %#v, want %#v", cjk, want)
	}
	if lone := Tokenize("锁"); !reflect.DeepEqual(lone, []string{"锁"}) {
		t.Fatalf("a single-character run tokenized to %#v, must stay searchable", lone)
	}
	if overlap := Tokenize("量子"); containsAny(overlap, Tokenize("子进程")) {
		t.Fatal("量子 and 子进程 share a term, so a single character would pass as evidence")
	}
	if mixed := Tokenize("AOF 追加"); len(mixed) == 0 || mixed[0] != "aof" {
		t.Fatalf("mixed tokens = %#v", mixed)
	}
	if empty := Tokenize("  ,, -- "); len(empty) != 0 {
		t.Fatalf("punctuation produced tokens: %#v", empty)
	}
}

func containsAny(terms []string, others []string) bool {
	for _, term := range terms {
		for _, other := range others {
			if term == other {
				return true
			}
		}
	}
	return false
}

func TestLexicalScorerRanksByRelevance(t *testing.T) {
	scorer := NewLexicalScorer(BM25Options{})
	scored, err := scorer.Score(context.Background(), "Redis AOF 追加写", corpus)
	if err != nil {
		t.Fatal(err)
	}
	if len(scored) != len(corpus) {
		t.Fatalf("scored %d chunks, want one per candidate", len(scored))
	}
	order := orderDescending(scored)
	if order[0].Chunk.ID != "c1" {
		t.Fatalf("best lexical match = %q, want c1", order[0].Chunk.ID)
	}
	if order[0].Score <= 0 {
		t.Fatal("the best match scored zero")
	}
	for _, item := range order[1:] {
		if item.Chunk.ID == "c4" && item.Score > 0 {
			t.Fatalf("an unrelated chunk scored %v on a Redis query", item.Score)
		}
	}
}

func TestDeterministicEmbedderIsRepeatableAndNormalized(t *testing.T) {
	embedder := NewDeterministicEmbedder(0)
	if embedder.Dimensions() != 256 {
		t.Fatalf("dimensions = %d, want the default 256", embedder.Dimensions())
	}
	if !strings.Contains(embedder.Model(), "deterministic") {
		t.Fatalf("model name %q must say it is not a trained embedding", embedder.Model())
	}
	texts := []string{"Redis 持久化 AOF", "Redis 持久化 AOF", "完全无关的话题"}
	vectors, err := embedder.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != len(texts) {
		t.Fatalf("vectors = %d, want %d", len(vectors), len(texts))
	}
	if !reflect.DeepEqual(vectors[0], vectors[1]) {
		t.Fatal("the same text embedded twice produced different vectors")
	}
	similar := CosineSimilarity(vectors[0], vectors[1])
	unrelated := CosineSimilarity(vectors[0], vectors[2])
	if !(similar > 0.99) {
		t.Fatalf("identical texts scored %v, want ~1", similar)
	}
	if unrelated >= similar {
		t.Fatalf("unrelated text scored %v, must be below the identical pair", unrelated)
	}
	blank, err := embedder.Embed(context.Background(), []string{"   "})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range blank[0] {
		if value != 0 {
			t.Fatal("an empty text must embed to the zero vector")
		}
	}
}

func TestVectorScorerRanksByCosine(t *testing.T) {
	scorer := NewVectorScorer(NewDeterministicEmbedder(128), VectorOptions{})
	scored, err := scorer.Score(context.Background(), "Redis AOF 持久化", corpus)
	if err != nil {
		t.Fatal(err)
	}
	order := orderDescending(scored)
	if order[0].Chunk.ID != "c1" {
		t.Fatalf("best vector match = %q, want c1", order[0].Chunk.ID)
	}
	if !strings.HasPrefix(scorer.Name(), "vector:deterministic") {
		t.Fatalf("scorer name %q must name the embedding implementation", scorer.Name())
	}
	if _, err := NewVectorScorer(nil, VectorOptions{}).Score(context.Background(), "q", corpus); !errors.Is(err, errNoEmbedder) {
		t.Fatalf("error without an embedder = %v", err)
	}
}

// TestPipelineFusesRanksAndCutsToTopK pins the fusion arithmetic for a query with
// real lexical hits: admission changed which chunks reach the ranking, not the
// value a genuine hit earns there.
func TestPipelineFusesRanksAndCutsToTopK(t *testing.T) {
	source := &fixedSource{chunks: corpus}
	pipeline := NewPipeline(source, []Scorer{
		NewLexicalScorer(BM25Options{}),
		NewVectorScorer(NewDeterministicEmbedder(0), VectorOptions{}),
	}, Options{TopK: 2})

	results, err := pipeline.Search(context.Background(), Query{UserID: "user-1", Text: "Redis 持久化 快照 恢复", CollectionIDs: []string{"col-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %v, want the top-k cut of 2", idsOf(results))
	}
	if results[0].Chunk.ID != "c1" {
		t.Fatalf("first result = %q, want c1", results[0].Chunk.ID)
	}
	if results[0].Score <= results[1].Score {
		t.Fatalf("results are not ordered by fused score: %v then %v", results[0].Score, results[1].Score)
	}
	// Reciprocal rank fusion gives a chunk ranked highly by both scorers more
	// than the sum of either alone would: c1 is first in both rankings and c2
	// second in both.
	if results[0].Score != 2*rrf(1) {
		t.Fatalf("top score = %v, want both first ranks fused to %v", results[0].Score, 2*rrf(1))
	}
	if results[1].Score != 2*rrf(2) {
		t.Fatalf("second score = %v, want both second ranks fused to %v", results[1].Score, 2*rrf(2))
	}
	for index, result := range results {
		if len(result.Contributors) != 2 {
			t.Fatalf("result %d contributors = %v, want both scorers", index, result.Contributors)
		}
	}
	if source.calls != 1 || !reflect.DeepEqual(source.scopes[0], []string{"col-a"}) || source.userIDs[0] != "user-1" {
		t.Fatalf("scope was not passed through untouched: calls=%d scopes=%v users=%v", source.calls, source.scopes, source.userIDs)
	}
}

func TestPipelineIsReproducible(t *testing.T) {
	source := &fixedSource{chunks: corpus}
	pipeline := NewPipeline(source, []Scorer{
		NewLexicalScorer(BM25Options{}),
		NewVectorScorer(NewDeterministicEmbedder(64), VectorOptions{}),
	}, Options{TopK: 3})
	query := Query{UserID: "user-1", Text: "日志追加与快照恢复", CollectionIDs: []string{"col-a"}}

	first, err := pipeline.Search(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	for repeat := 0; repeat < 3; repeat++ {
		again, err := pipeline.Search(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(idsOf(first), idsOf(again)) {
			t.Fatalf("run %d produced %v, want the same order as %v", repeat, idsOf(again), idsOf(first))
		}
		for index := range first {
			if first[index].Score != again[index].Score {
				t.Fatalf("run %d changed the fused score of %q", repeat, first[index].Chunk.ID)
			}
		}
	}
}

// TestPipelineTieBreakIsStableAcrossMapOrders pins that two chunks with an equal
// fused score come back in stored order, not in Go map order.
func TestPipelineTieBreakIsStableAcrossMapOrders(t *testing.T) {
	identical := []domain.DocumentChunk{
		chunk("z9", "col-a", "doc-9", 9, "同一句话"),
		chunk("a1", "col-a", "doc-1", 1, "同一句话"),
	}
	query := Query{UserID: "user-1", Text: "同一句话", CollectionIDs: []string{"col-a"}}
	var seen [][]string
	for repeat := 0; repeat < 20; repeat++ {
		pipeline := NewPipeline(&fixedSource{chunks: identical}, []Scorer{NewLexicalScorer(BM25Options{})}, Options{TopK: 2})
		results, err := pipeline.Search(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, idsOf(results))
	}
	for _, ids := range seen[1:] {
		if !reflect.DeepEqual(ids, seen[0]) {
			t.Fatalf("tie order varied: %v vs %v", seen[0], ids)
		}
	}
	if seen[0][0] != "a1" {
		t.Fatalf("ties should resolve by stored order, got %v", seen[0])
	}
}

func TestPipelineRefusesUnscopedOrUnsearchableRequests(t *testing.T) {
	pipeline := NewPipeline(&fixedSource{chunks: corpus}, []Scorer{NewLexicalScorer(BM25Options{})}, Options{})
	cases := []struct {
		name  string
		query Query
		want  error
		calls int
	}{
		{"no principal", Query{Text: "Redis", CollectionIDs: []string{"col-a"}}, ErrNoPrincipal, 0},
		{"no collections", Query{UserID: "user-1", Text: "Redis"}, ErrNoCandidates, 0},
		{"blank query", Query{UserID: "user-1", Text: "   ", CollectionIDs: []string{"col-a"}}, ErrEmptyQuery, 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := &fixedSource{chunks: corpus}
			guarded := NewPipeline(source, []Scorer{NewLexicalScorer(BM25Options{})}, Options{})
			if _, err := guarded.Search(context.Background(), testCase.query); !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want %v", err, testCase.want)
			}
			if source.calls != 0 {
				t.Fatalf("the source was consulted %d times for a request that must be refused first", source.calls)
			}
		})
	}
	if _, err := pipeline.Search(nil, Query{UserID: "u", Text: "x", CollectionIDs: []string{"c"}}); err == nil {
		t.Fatal("a nil context was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pipeline.Search(ctx, Query{UserID: "u", Text: "x", CollectionIDs: []string{"c"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if _, err := NewPipeline(nil, nil, Options{}).Search(context.Background(), Query{UserID: "u", Text: "x", CollectionIDs: []string{"c"}}); err == nil {
		t.Fatal("an unconfigured pipeline reported no error")
	}
}

func TestPipelinePropagatesSourceFailure(t *testing.T) {
	boom := errors.New("store unavailable")
	pipeline := NewPipeline(&fixedSource{err: boom}, []Scorer{NewLexicalScorer(BM25Options{})}, Options{})
	if _, err := pipeline.Search(context.Background(), Query{UserID: "u", Text: "Redis", CollectionIDs: []string{"col-a"}}); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the store failure", err)
	}
}

func TestCandidateLimitIsDeterministicAndScopedFirst(t *testing.T) {
	var many []domain.DocumentChunk
	for index := 0; index < 50; index++ {
		many = append(many, chunk("c"+itoaTest(index), "col-a", "doc-1", index+1, "Redis 持久化 everysec"))
	}
	pipeline := NewPipeline(&fixedSource{chunks: many}, []Scorer{NewLexicalScorer(BM25Options{})}, Options{TopK: 3, CandidateLimit: 10})
	first, err := pipeline.Search(context.Background(), Query{UserID: "u", Text: "Redis", CollectionIDs: []string{"col-a"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := pipeline.Search(context.Background(), Query{UserID: "u", Text: "Redis", CollectionIDs: []string{"col-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(idsOf(first), idsOf(second)) {
		t.Fatalf("a capped candidate pool searched differently across runs: %v vs %v", idsOf(first), idsOf(second))
	}
	// The cap keeps the earliest positions, so the truncated pool always covers
	// the top of each document rather than an arbitrary slice of it.
	if len(first) == 0 || first[0].Chunk.ID != "c0" {
		t.Fatalf("capped pool started at %v, want the first stored chunks", idsOf(first))
	}
}

// TestScorerCannotInventCandidates pins the authorization invariant: a scorer
// that returns an id outside the authorized candidate set must be dropped rather
// than retrieved into the answer, and dropping it must not leave a foreign chunk
// behind in the result set either.
func TestScorerCannotInventCandidates(t *testing.T) {
	pipeline := NewPipeline(&fixedSource{chunks: corpus[:1]}, []Scorer{foreignScorer{}}, Options{TopK: 5})
	results, err := pipeline.Search(context.Background(), Query{UserID: "user-1", Text: "Redis", CollectionIDs: []string{"col-a"}})
	if !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("error = %v, want ErrNoCandidates: the only scored candidate was invented, so nothing may be cited", err)
	}
	for _, result := range results {
		if result.Chunk.CollectionID != "col-a" {
			t.Fatalf("a scorer smuggled in %q from collection %q", result.Chunk.ID, result.Chunk.CollectionID)
		}
	}
}

type foreignScorer struct{}

func (foreignScorer) Name() string { return "foreign" }

func (foreignScorer) Admit(score float64) bool { return isFinite(score) && score > 0 }

func (foreignScorer) Score(context.Context, string, []domain.DocumentChunk) ([]Scored, error) {
	return []Scored{{Chunk: chunk("evil", "col-b", "doc-x", 1, "别的用户的内容"), Score: 100}}, nil
}

func idsOf(results []Result) []string {
	ids := make([]string, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.Chunk.ID)
	}
	return ids
}

func itoaTest(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
