package fake

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"GopherAI/common/rag"
)

func embed(t *testing.T, e *Embedder, text string) []float32 {
	t.Helper()
	out, err := e.EmbedStrings(context.Background(), []string{text})
	if err != nil {
		t.Fatal(err)
	}
	vector := make([]float32, len(out[0]))
	for i, v := range out[0] {
		vector[i] = float32(v)
	}
	return vector
}

func TestTokensSplitWordsAndHan(t *testing.T) {
	got := strings.Join(Tokens("Grey Point, tide-3 潮汐表!"), "|")
	want := "grey|point|tide|3|潮|潮汐|汐|汐表|表"
	if got != want {
		t.Fatalf("Tokens = %s, want %s", got, want)
	}
	if len(Tokens("  ... ")) != 0 {
		t.Fatal("punctuation alone must give no tokens")
	}
}

func TestEmbedderIsDeterministicNormalisedAndFavoursSharedWords(t *testing.T) {
	e := NewEmbedder(64)
	a := embed(t, e, "the sandbar dries at low water")
	again := embed(t, e, "the sandbar dries at low water")
	for i := range a {
		if a[i] != again[i] {
			t.Fatal("same text gave different vectors")
		}
	}
	var norm float64
	for _, v := range a {
		norm += float64(v) * float64(v)
	}
	if math.Abs(norm-1) > 1e-5 {
		t.Fatalf("norm^2 = %v, want 1", norm)
	}
	related, _ := cosine(a, embed(t, e, "when does the sandbar dry"))
	unrelated, _ := cosine(a, embed(t, e, "grafting scions in january"))
	if related <= unrelated {
		t.Fatalf("shared words must score higher: related %v, unrelated %v", related, unrelated)
	}
	if empty := embed(t, e, "  "); empty[0] != 1 {
		t.Fatal("a text with no tokens must still embed to a usable vector")
	}
	if e.Dimension() != 64 || len(a) != 64 {
		t.Fatal("dimension")
	}
}

func TestEmbedderCountsCallsAndInjectsFailure(t *testing.T) {
	e := NewEmbedder(8)
	if _, err := e.EmbedStrings(context.Background(), []string{"a", "b", "c"}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("provider down")
	e.Fail(boom)
	if _, err := e.EmbedStrings(context.Background(), []string{"d"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	e.Fail(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.EmbedStrings(ctx, []string{"e"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if e.Calls() != 3 || e.Texts() != 5 {
		t.Fatalf("calls %d texts %d, want 3 and 5 (failed calls count: they were paid for)", e.Calls(), e.Texts())
	}
}

func TestNewEmbedderRejectsTinyDimensions(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("dimension 1 must panic")
		}
	}()
	NewEmbedder(1)
}

// ---- backend ---------------------------------------------------------------

func chunk(doc string, version, index int, raw string) rag.Chunk {
	return rag.Chunk{
		ID: doc + "-c" + string(rune('0'+index)), Index: index, DocID: doc, DocVersion: version,
		SourceFile: doc + ".md", TitlePath: []string{"Top", "Sub"}, StartLine: index*10 + 1, EndLine: index*10 + 9,
		RawContent: raw, EmbeddingContent: raw, ContentHash: "h",
	}
}

func store(t *testing.T, b *Backend, e *Embedder, user int64, chunks ...rag.Chunk) {
	t.Helper()
	embedded := make([]rag.EmbeddedChunk, len(chunks))
	for i, c := range chunks {
		embedded[i] = rag.EmbeddedChunk{UserID: user, Chunk: c, Vector: embed(t, e, c.EmbeddingContent), EmbeddingInputHash: "ih", EmbeddingFingerprint: "fp"}
	}
	if err := b.Store(context.Background(), embedded); err != nil {
		t.Fatal(err)
	}
}

func TestBackendVectorSearchRanksByCosineDistanceWithinTheActiveSet(t *testing.T) {
	e := NewEmbedder(64)
	b := NewBackend(64)
	store(t, b, e, 1, chunk("tides", 1, 0, "the sandbar dries at low water"), chunk("tides", 1, 1, "spring tides follow the new moon"))
	store(t, b, e, 1, chunk("graft", 1, 0, "graft scions in april"))
	store(t, b, e, 2, chunk("other", 1, 0, "the sandbar dries at low water")) // another tenant
	active := map[string]int{"tides": 1, "graft": 1, "other": 1}

	results, err := b.SearchActiveByUserVector(context.Background(), 1, embed(t, e, "when does the sandbar dry at low water"), 10, active)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("%d results; the other tenant's chunk must not appear", len(results))
	}
	if results[0].ChunkID != "tides-c0" {
		t.Fatalf("best = %s, want tides-c0", results[0].ChunkID)
	}
	for i := 1; i < len(results); i++ {
		if results[i].Distance < results[i-1].Distance {
			t.Fatal("distances must be ascending")
		}
	}
	if results[0].Distance < 0 || results[0].Distance > 1 {
		t.Fatalf("distance %v is not 1-cosine of near vectors", results[0].Distance)
	}
	if results[0].TitlePath != "Top > Sub" || results[0].StartLine != 1 || results[0].EndLine != 9 || results[0].SourceFile != "tides.md" {
		t.Fatalf("metadata not carried: %+v", results[0])
	}
	if !strings.HasSuffix(results[0].RedisKey, ":tides:1:tides-c0") {
		t.Fatalf("redis key = %s", results[0].RedisKey)
	}

	top1, _ := b.SearchActiveByUserVector(context.Background(), 1, embed(t, e, "sandbar"), 1, active)
	if len(top1) != 1 {
		t.Fatalf("topK 1 returned %d", len(top1))
	}
}

func TestBackendActiveVersionFilterHidesStaleVersionsAndUnlistedDocuments(t *testing.T) {
	e := NewEmbedder(64)
	b := NewBackend(64)
	store(t, b, e, 1, chunk("d", 1, 0, "old text about tides"))
	store(t, b, e, 1, chunk("d", 2, 0, "new text about tides"))
	store(t, b, e, 1, chunk("hidden", 1, 0, "hidden text about tides"))
	query := embed(t, e, "text about tides")

	got, _ := b.SearchActiveByUserVector(context.Background(), 1, query, 10, map[string]int{"d": 2})
	if len(got) != 1 || got[0].DocVersion != 2 {
		t.Fatalf("results = %+v, want only d@2", got)
	}
	none, err := b.SearchActiveByUserVector(context.Background(), 1, query, 10, map[string]int{})
	if err != nil || len(none) != 0 {
		t.Fatalf("an empty allow-list must return nothing: %v %v", none, err)
	}
	text, _ := b.SearchActiveByUserText(context.Background(), 1, "tides", 10, map[string]int{"d": 1})
	if len(text) != 1 || text[0].DocVersion != 1 {
		t.Fatalf("text search must honour the version filter too: %+v", text)
	}
}

func TestBackendTextSearchRanksByTermFrequencyAndRarity(t *testing.T) {
	e := NewEmbedder(64)
	b := NewBackend(64)
	store(t, b, e, 1,
		chunk("a", 1, 0, "lantern lantern lantern wick"),
		chunk("a", 1, 1, "lantern glass"),
		chunk("a", 1, 2, "orchard grafting"),
	)
	active := map[string]int{"a": 1}
	got, err := b.SearchActiveByUserText(context.Background(), 1, "lantern wick", 10, active)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ChunkID != "a-c0" || got[1].ChunkID != "a-c1" {
		t.Fatalf("results = %+v, want a-c0 then a-c1 (the orchard chunk matches nothing)", got)
	}
	if got[0].LexicalScore <= got[1].LexicalScore || got[1].LexicalScore <= 0 {
		t.Fatalf("scores %v %v", got[0].LexicalScore, got[1].LexicalScore)
	}
	if empty, _ := b.SearchActiveByUserText(context.Background(), 1, "the of", 10, active); len(empty) != 0 {
		t.Fatalf("a query without usable terms must return nothing: %+v", empty)
	}
	if top, _ := b.SearchActiveByUserText(context.Background(), 1, "lantern", 1, active); len(top) != 1 {
		t.Fatalf("topK 1 returned %d", len(top))
	}
}

func TestBackendStoreReplacesAVersionAndValidates(t *testing.T) {
	e := NewEmbedder(64)
	b := NewBackend(64)
	store(t, b, e, 1, chunk("d", 1, 0, "one"), chunk("d", 1, 1, "two"), chunk("d", 1, 2, "three"))
	store(t, b, e, 1, chunk("d", 1, 0, "one again"))
	if b.Len() != 1 {
		t.Fatalf("storing a version must replace it, %d chunks remain", b.Len())
	}
	store(t, b, e, 1, chunk("d", 2, 0, "version two"))
	if b.Len() != 2 {
		t.Fatalf("another version must coexist, %d chunks", b.Len())
	}

	bad := rag.EmbeddedChunk{UserID: 1, Chunk: chunk("d", 3, 0, "x"), Vector: make([]float32, 5)}
	if err := b.Store(context.Background(), []rag.EmbeddedChunk{bad}); err == nil {
		t.Fatal("a vector of the wrong dimension must be refused")
	}
	if err := b.Store(context.Background(), []rag.EmbeddedChunk{{Chunk: chunk("d", 3, 0, "x"), Vector: make([]float32, 64)}}); err == nil {
		t.Fatal("a chunk without a user must be refused")
	}
	if b.Len() != 2 {
		t.Fatal("a refused store must not change the index")
	}
	if err := b.Store(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	loaded, err := b.LoadDocumentVersionEmbeddings(context.Background(), 1, "d", 1)
	if err != nil || len(loaded) != 1 || loaded[0].ChunkID != "d-c0" || loaded[0].EmbeddingContent != "one again" || len(loaded[0].Vector) != 64 {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
	if other, _ := b.LoadDocumentVersionEmbeddings(context.Background(), 2, "d", 1); len(other) != 0 {
		t.Fatal("LoadDocumentVersionEmbeddings leaked across tenants")
	}
}

func TestBackendStoresACopyOfTheVector(t *testing.T) {
	e := NewEmbedder(64)
	b := NewBackend(64)
	vector := embed(t, e, "hello")
	embedded := rag.EmbeddedChunk{UserID: 1, Chunk: chunk("d", 1, 0, "hello"), Vector: vector}
	if err := b.Store(context.Background(), []rag.EmbeddedChunk{embedded}); err != nil {
		t.Fatal(err)
	}
	vector[0] = 99
	if b.All()[0].Vector[0] == 99 {
		t.Fatal("the index shares memory with the caller's vector")
	}
}

func TestBackendRecordsSearchesAndInjectsFailures(t *testing.T) {
	e := NewEmbedder(64)
	b := NewBackend(64)
	store(t, b, e, 1, chunk("d", 1, 0, "tides"))
	active := map[string]int{"d": 1}
	ctx := context.Background()
	_, _ = b.SearchActiveByUserVector(ctx, 1, embed(t, e, "tides"), 30, active)
	_, _ = b.SearchActiveByUserText(ctx, 1, "tides", 48, active)
	got := b.Searches()
	if len(got) != 2 || got[0] != (Search{"vector", 30}) || got[1] != (Search{"text", 48}) {
		t.Fatalf("searches = %+v", got)
	}

	boom := errors.New("redis down")
	b.FailVectorSearch(boom)
	if _, err := b.SearchActiveByUserVector(ctx, 1, embed(t, e, "x"), 5, active); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	b.FailVectorSearch(nil)
	b.FailTextSearch(boom)
	if _, err := b.SearchActiveByUserText(ctx, 1, "tides", 5, active); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	b.FailTextSearch(nil)
	b.FailStore(boom)
	if err := b.Store(ctx, []rag.EmbeddedChunk{{UserID: 1, Chunk: chunk("d", 9, 0, "x"), Vector: make([]float32, 64)}}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	b.FailStore(nil)
	b.FailEnsureIndex(boom)
	if err := b.EnsureIndex(ctx); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if b.StoreCalls() != 2 {
		t.Fatalf("store calls = %d", b.StoreCalls())
	}
}

func TestBackendRejectsInvalidSearches(t *testing.T) {
	b := NewBackend(8)
	ctx := context.Background()
	active := map[string]int{"d": 1}
	if _, err := b.SearchActiveByUserVector(ctx, 0, make([]float32, 8), 5, active); err == nil {
		t.Error("user 0 must be refused")
	}
	if _, err := b.SearchActiveByUserVector(ctx, 1, make([]float32, 3), 5, active); err == nil {
		t.Error("a wrong-dimension query vector must be refused")
	}
	if _, err := b.SearchActiveByUserVector(ctx, 1, make([]float32, 8), 0, active); err == nil {
		t.Error("topK 0 must be refused")
	}
	if _, err := b.SearchActiveByUserText(ctx, 1, "tides", 0, active); err == nil {
		t.Error("topK 0 must be refused")
	}
}

// ---- reranker --------------------------------------------------------------

func TestRerankerOrdersByOverlapAndCountsCalls(t *testing.T) {
	r := NewReranker()
	scores, err := r.Rerank(context.Background(), "sandbar low water", []string{"graft scions in april", "the sandbar dries at low water", "spring tides"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 3 {
		t.Fatalf("%d scores, want one per passage", len(scores))
	}
	if scores[0].Index != 1 {
		t.Fatalf("best passage = %d, want 1", scores[0].Index)
	}
	for i := 1; i < len(scores); i++ {
		if scores[i].Score > scores[i-1].Score {
			t.Fatal("scores must come best first")
		}
	}
	seen := map[int]bool{}
	for _, s := range scores {
		seen[s.Index] = true
	}
	if len(seen) != 3 {
		t.Fatal("every passage index must appear exactly once")
	}
	if r.Calls() != 1 || r.Passages() != 3 {
		t.Fatalf("calls %d passages %d", r.Calls(), r.Passages())
	}
}

func TestRerankerEnforcesTheCandidatePoolAndInjectsFailure(t *testing.T) {
	r := NewReranker()
	ctx := context.Background()
	many := make([]string, rag.RerankCandidateK+1)
	for i := range many {
		many[i] = "passage"
	}
	if _, err := r.Rerank(ctx, "q", many); err == nil {
		t.Fatal("more than RerankCandidateK passages must be refused")
	}
	if _, err := r.Rerank(ctx, "q", many[:rag.RerankCandidateK]); err != nil {
		t.Fatalf("exactly RerankCandidateK passages must work: %v", err)
	}
	if _, err := r.Rerank(ctx, "q", nil); err == nil {
		t.Fatal("no passages must be refused")
	}

	r.FailRerank(Unavailable())
	if _, err := r.Rerank(ctx, "q", many[:2]); !errors.Is(err, rag.ErrRerankUnavailable) {
		t.Fatalf("err = %v, want it to wrap rag.ErrRerankUnavailable", err)
	}
	r.FailRerank(nil)

	boom := errors.New("bad gateway")
	r.FailHealth(boom)
	if err := r.Health(ctx); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	r.FailHealth(nil)
	if err := r.Health(ctx); err != nil || r.HealthCalls() != 2 {
		t.Fatalf("health err %v calls %d", err, r.HealthCalls())
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.Rerank(cancelled, "q", many[:2]); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
