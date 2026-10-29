package retrieval

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// The hybrids below are built from in-process fakes only: a chunk source, a scripted
// embedder, an index stub and a reranker stub. That is what makes this phase's claim
// verifiable without a Provider, without Redis and without a GPU: the contract of each
// seam is asserted here, and the real adapters are asserted against the same
// interfaces where they actually talk to a server.

type hybridIndexStub struct {
	hits       []VectorHit
	searches   int
	lastSearch VectorSearch
	upserts    []VectorEntry
	deletes    []string
	searchErr  error
}

func (s *hybridIndexStub) Upsert(_ context.Context, _ string, entries []VectorEntry) error {
	s.upserts = append(s.upserts, entries...)
	return nil
}

func (s *hybridIndexStub) DeleteDocument(_ context.Context, userID string, documentID string) error {
	s.deletes = append(s.deletes, "doc:"+userID+"/"+documentID)
	return nil
}

func (s *hybridIndexStub) DeleteVersions(_ context.Context, userID string, documentID string, keepVersion int) error {
	s.deletes = append(s.deletes, "versions:"+userID+"/"+documentID+"@keep"+strconv.Itoa(keepVersion))
	return nil
}

func (s *hybridIndexStub) Search(_ context.Context, query VectorSearch) ([]VectorHit, error) {
	s.searches++
	s.lastSearch = query
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	return s.hits, nil
}

type recordingEmbedder struct {
	vectors   [][]float32
	calls     int
	lastTexts []string
}

func (e *recordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	e.lastTexts = append([]string(nil), texts...)
	if len(texts) != len(e.vectors) {
		return nil, errors.New("recordingEmbedder: wanted " + strconv.Itoa(len(e.vectors)) + " texts, got " + strconv.Itoa(len(texts)))
	}
	return e.vectors, nil
}

func (e *recordingEmbedder) Dimensions() int { return len(e.vectors[0]) }
func (e *recordingEmbedder) Model() string   { return "hybrid-test-embedder" }

type stubReranker struct {
	hits        []RerankHit
	err         error
	calls       int
	lastQuery   string
	lastTexts   []string
	maxPassages int
}

func (r *stubReranker) Rerank(_ context.Context, query string, passages []string) ([]RerankHit, error) {
	r.calls++
	r.lastQuery = query
	r.lastTexts = append([]string(nil), passages...)
	if r.err != nil {
		return nil, r.err
	}
	return r.hits, nil
}

func (r *stubReranker) Model() string { return "stub-reranker" }

func TestNewHybridRequiresItsCollaborators(t *testing.T) {
	source := &fixedSource{}
	embedder := &recordingEmbedder{vectors: [][]float32{{1, 0}}}
	index := &hybridIndexStub{}
	if _, err := NewHybrid(HybridOptions{Embedder: embedder, Index: index}); err == nil {
		t.Error("missing source accepted")
	}
	if _, err := NewHybrid(HybridOptions{Source: source, Index: index}); err == nil {
		t.Error("missing embedder accepted")
	}
	if _, err := NewHybrid(HybridOptions{Source: source, Embedder: embedder}); err == nil {
		t.Error("missing index accepted")
	}
	if _, err := NewHybrid(HybridOptions{Source: source, Embedder: embedder, Index: index, TopK: 5, RecallLimit: 3}); err == nil {
		t.Error("a recall window narrower than the answer was accepted")
	}
}

func TestHybridDropsIndexHitsOutsideTheAuthorizedScope(t *testing.T) {
	// A stale or compromised index can name a chunk the caller may not read. Only ids
	// present in the authorized scope become results, so an invented id can never reach
	// a citation list.
	chunkA := chunk("chunk-a", "col-1", "doc-1", 1, "读写锁允许并发读取，写锁独占")
	source := &fixedSource{chunks: []domain.DocumentChunk{chunkA}}
	embedder := &recordingEmbedder{vectors: [][]float32{{1, 0}}}
	index := &hybridIndexStub{hits: []VectorHit{{ChunkID: "chunk-a", Similarity: 0.8}, {ChunkID: "chunk-other-users", Similarity: 0.99}}}
	hybrid, err := NewHybrid(HybridOptions{Source: source, Embedder: embedder, Index: index, MinSimilarity: 0.2})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	results, err := hybrid.Search(context.Background(), Query{UserID: "user-1", Text: "读写锁", CollectionIDs: []string{"col-1"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, result := range results {
		if result.Chunk.ID == "chunk-other-users" {
			t.Fatal("an out-of-scope chunk id reached the results")
		}
	}
	if len(results) != 1 || results[0].Chunk.ID != "chunk-a" {
		t.Fatalf("results = %v, want only the authorized chunk", resultIDs(results))
	}

	// A ghost-only index can still be rescued by the lexical side, so this case removes
	// the lexical overlap: nothing in scope matches, and inventing a citation must fail.
	ghostOnly := &hybridIndexStub{hits: []VectorHit{{ChunkID: "ghost", Similarity: 0.99}}}
	hybridOnly, err := NewHybrid(HybridOptions{Source: source, Embedder: embedder, Index: ghostOnly, MinSimilarity: 0.2})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if _, err := hybridOnly.Search(context.Background(), Query{
		UserID: "user-1", Text: "completely unrelated latin question", CollectionIDs: []string{"col-1"},
	}); !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("error = %v, want ErrNoCandidates rather than a fabricated citation", err)
	}
}

func TestHybridRefusesToCiteBelowTheVectorCutWhenLexicalAgreesWithNothing(t *testing.T) {
	// The original deployment returned up to top-k neighbours unconditionally. Clean's
	// contract is the opposite: a weak neighbour is not evidence, and answering from it
	// is how "I did not find this" becomes a confident fabrication.
	source := &fixedSource{chunks: []domain.DocumentChunk{chunk("chunk-a", "col-1", "doc-1", 1, "读写锁允许并发读取，写锁独占")}}
	embedder := &recordingEmbedder{vectors: [][]float32{{1, 0}}}
	index := &hybridIndexStub{hits: []VectorHit{{ChunkID: "chunk-a", Similarity: 0.11}}}
	hybrid, err := NewHybrid(HybridOptions{Source: source, Embedder: embedder, Index: index, MinSimilarity: 0.25})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if _, err := hybrid.Search(context.Background(), Query{UserID: "user-1", Text: "unrelated english question", CollectionIDs: []string{"col-1"}}); !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("error = %v, want ErrNoCandidates", err)
	}
}

func TestHybridEmbedsTheQueryOnceAndCarriesTheIdentity(t *testing.T) {
	source := &fixedSource{chunks: []domain.DocumentChunk{
		chunk("chunk-a", "col-1", "doc-1", 1, "无缓冲 channel 的收发必须会合"),
		chunk("chunk-b", "col-1", "doc-1", 2, "有缓冲 channel 只是推迟阻塞"),
	}}
	embedder := &recordingEmbedder{vectors: [][]float32{{1, 0}}}
	index := &hybridIndexStub{hits: []VectorHit{{ChunkID: "chunk-a", Similarity: 0.7}, {ChunkID: "chunk-b", Similarity: 0.6}}}
	hybrid, err := NewHybrid(HybridOptions{
		Source: source, Embedder: embedder, Index: index, MinSimilarity: 0.2, Identity: "abc123", TopK: 2,
	})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if _, err := hybrid.Search(context.Background(), Query{UserID: "user-9", Text: "channel 缓冲", CollectionIDs: []string{"col-1"}}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	// One call, one text: the whole point of a persisted index is that the corpus is
	// not re-embedded on every question.
	if embedder.calls != 1 || len(embedder.lastTexts) != 1 || embedder.lastTexts[0] != "channel 缓冲" {
		t.Fatalf("embedder saw %d calls with %d texts, want one call carrying only the query", embedder.calls, len(embedder.lastTexts))
	}
	if index.searches != 1 || index.lastSearch.UserID != "user-9" || index.lastSearch.Identity != "abc123" {
		t.Fatalf("index search = %+v, want the owner and the embedder identity to travel", index.lastSearch)
	}
	if index.lastSearch.Limit < 2 {
		t.Fatalf("recall limit = %d, want at least the answer width", index.lastSearch.Limit)
	}
}

func TestHybridFusesExactlyTheWayPipelineDoes(t *testing.T) {
	// The two modes must not disagree about arithmetic. With the same corpus, the same
	// query vector and an index whose hits are the same similarities VectorScorer
	// computes, the fused results have to match chunk for chunk and score for score.
	corpus := []domain.DocumentChunk{
		chunk("chunk-a", "col-1", "doc-1", 1, "读写锁 RWMutex 允许并发读取，写锁独占"),
		chunk("chunk-b", "col-1", "doc-1", 2, "WaitGroup 的 Add 必须在 Go 之前调用"),
		chunk("chunk-c", "col-1", "doc-1", 3, "context 的 cancel 必须调用，否则子节点挂在树上"),
	}
	query := "读写锁 读取 独占"
	vectors := [][]float32{{1, 0, 0, 0}, {1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}}

	source := &fixedSource{chunks: corpus}
	sharedEmbedder := &positionalEmbedder{vectors: vectors}
	pipeline := NewPipeline(source, []Scorer{
		NewLexicalScorer(BM25Options{}),
		NewVectorScorer(sharedEmbedder, VectorOptions{MinSimilarity: 0.2}),
	}, Options{TopK: 3})
	fromPipeline, err := pipeline.Search(context.Background(), Query{UserID: "user-1", Text: query, CollectionIDs: []string{"col-1"}})
	if err != nil {
		t.Fatalf("pipeline search: %v", err)
	}

	// The same three cosines, as an index would report them: query vector {1,0,0,0}
	// against the chunk vectors above gives 1, 0, 0.
	hybridEmbedder := &recordingEmbedder{vectors: [][]float32{{1, 0, 0, 0}}}
	hybridIndex := &hybridIndexStub{hits: []VectorHit{
		{ChunkID: "chunk-a", Similarity: 1}, {ChunkID: "chunk-b", Similarity: 0}, {ChunkID: "chunk-c", Similarity: 0},
	}}
	hybrid, err := NewHybrid(HybridOptions{
		Source: source, Embedder: hybridEmbedder, Index: hybridIndex,
		Lexical: NewLexicalScorer(BM25Options{}), MinSimilarity: 0.2, TopK: 3, RecallLimit: 6,
	})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	fromHybrid, err := hybrid.Search(context.Background(), Query{UserID: "user-1", Text: query, CollectionIDs: []string{"col-1"}})
	if err != nil {
		t.Fatalf("hybrid search: %v", err)
	}
	if len(fromPipeline) != len(fromHybrid) {
		t.Fatalf("widths differ: %v vs %v", resultIDs(fromPipeline), resultIDs(fromHybrid))
	}
	for index := range fromPipeline {
		if fromPipeline[index].Chunk.ID != fromHybrid[index].Chunk.ID {
			t.Fatalf("position %d: pipeline chose %s, hybrid chose %s", index, fromPipeline[index].Chunk.ID, fromHybrid[index].Chunk.ID)
		}
		if fromPipeline[index].Score != fromHybrid[index].Score {
			t.Fatalf("position %d: pipeline score %v, hybrid score %v", index, fromPipeline[index].Score, fromHybrid[index].Score)
		}
	}
	// chunk-a is first in both rankings, so its fused score is the sum of two rank-one
	// slots. Asserting the exact value is what keeps the shared fusion honest.
	if fromPipeline[0].Score != rrf(1)+rrf(1) {
		t.Fatalf("the shared fusion is no longer the pinned reciprocal rank: %v", fromPipeline[0].Score)
	}
}

func TestHybridReranksTheHeadAndSaysSo(t *testing.T) {
	source := &fixedSource{chunks: []domain.DocumentChunk{
		chunk("chunk-a", "col-1", "doc-1", 1, "读多写少用 RWMutex"),
		chunk("chunk-b", "col-1", "doc-1", 2, "写多读少退回 Mutex"),
	}}
	embedder := &recordingEmbedder{vectors: [][]float32{{1, 0}}}
	index := &hybridIndexStub{hits: []VectorHit{{ChunkID: "chunk-a", Similarity: 0.9}, {ChunkID: "chunk-b", Similarity: 0.8}}}
	// The second passage is the better answer to the question, and it is last in fused
	// order, so a pass that reorders is distinguishable from one that did nothing.
	reranker := &stubReranker{hits: []RerankHit{{Index: 1, Score: 0.95}, {Index: 0, Score: 0.10}}}
	hybrid, err := NewHybrid(HybridOptions{
		Source: source, Embedder: embedder, Index: index, Reranker: reranker, MinSimilarity: 0.2, TopK: 2,
	})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	results, err := hybrid.Search(context.Background(), Query{UserID: "user-1", Text: "锁", CollectionIDs: []string{"col-1"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if reranker.calls != 1 {
		t.Fatalf("reranker saw %d calls, want 1", reranker.calls)
	}
	if reranker.lastQuery != "锁" || len(reranker.lastTexts) != 2 {
		t.Fatalf("reranker received %d passages for query %q", len(reranker.lastTexts), reranker.lastQuery)
	}
	if reranker.lastTexts[0] != EmbeddingInput(source.chunks[0]) {
		t.Fatalf("passage 0 is not the text the vector was built from; index and rerank would disagree about what is being judged")
	}
	if results[0].Chunk.ID != "chunk-b" {
		t.Fatalf("order = %v, want the reranker's verdict to decide", resultIDs(results))
	}
	if !strings.Contains(strings.Join(results[0].Contributors, ","), "rerank:stub-reranker:1") {
		t.Fatalf("contributors = %v, want the rerank position recorded", results[0].Contributors)
	}
}

func TestHybridKeepsFusedOrderWhenTheRerankerIsDown(t *testing.T) {
	source := &fixedSource{chunks: []domain.DocumentChunk{
		chunk("chunk-a", "col-1", "doc-1", 1, "读多写少用 RWMutex"),
		chunk("chunk-b", "col-1", "doc-1", 2, "写多读少退回 Mutex"),
	}}
	embedder := &recordingEmbedder{vectors: [][]float32{{1, 0}}}
	index := &hybridIndexStub{hits: []VectorHit{{ChunkID: "chunk-a", Similarity: 0.9}, {ChunkID: "chunk-b", Similarity: 0.8}}}
	down := &stubReranker{err: fmt.Errorf("connection refused: %w", ErrRerankUnavailable)}
	hybrid, err := NewHybrid(HybridOptions{
		Source: source, Embedder: embedder, Index: index, Reranker: down, MinSimilarity: 0.2, TopK: 2,
	})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	results, err := hybrid.Search(context.Background(), Query{UserID: "user-1", Text: "锁", CollectionIDs: []string{"col-1"}})
	if err != nil {
		t.Fatalf("an unavailable optional stage must not fail the turn: %v", err)
	}
	if results[0].Chunk.ID != "chunk-a" {
		t.Fatalf("order = %v, want the fused order preserved", resultIDs(results))
	}
	if !strings.Contains(strings.Join(results[0].Contributors, ","), "rerank:unavailable") {
		t.Fatalf("contributors = %v, want the skipped rerank to be visible", results[0].Contributors)
	}
}

func TestHybridPropagatesARerankerThatBrokeTheContract(t *testing.T) {
	// Distinct from an outage: a service answering with a malformed body is a
	// misconfiguration, and quietly ranking without it would keep a broken deployment
	// serving answers nobody checked.
	source := &fixedSource{chunks: []domain.DocumentChunk{chunk("chunk-a", "col-1", "doc-1", 1, "读多写少用 RWMutex")}}
	embedder := &recordingEmbedder{vectors: [][]float32{{1, 0}}}
	index := &hybridIndexStub{hits: []VectorHit{{ChunkID: "chunk-a", Similarity: 0.9}}}
	hybrid, err := NewHybrid(HybridOptions{
		Source: source, Embedder: embedder, Index: index,
		Reranker:      &stubReranker{err: fmt.Errorf("scores disagree with passages: %w", ErrRerankShape)},
		MinSimilarity: 0.2,
	})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if _, err := hybrid.Search(context.Background(), Query{UserID: "user-1", Text: "锁", CollectionIDs: []string{"col-1"}}); !errors.Is(err, ErrRerankShape) {
		t.Fatalf("error = %v, want the protocol error to surface", err)
	}
}

func TestHybridRefusesUnscopedAndUnsearchableRequests(t *testing.T) {
	source := &fixedSource{chunks: []domain.DocumentChunk{chunk("chunk-a", "col-1", "doc-1", 1, "内容")}}
	embedder := &recordingEmbedder{vectors: [][]float32{{1, 0}}}
	hybrid, err := NewHybrid(HybridOptions{Source: source, Embedder: embedder, Index: &hybridIndexStub{}})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	ctx := context.Background()
	if _, err := hybrid.Search(ctx, Query{Text: "问题", CollectionIDs: []string{"col-1"}}); !errors.Is(err, ErrNoPrincipal) {
		t.Errorf("no owner: error = %v, want ErrNoPrincipal", err)
	}
	if _, err := hybrid.Search(ctx, Query{UserID: "user-1", Text: "问题"}); !errors.Is(err, ErrNoCandidates) {
		t.Errorf("no collections: error = %v, want ErrNoCandidates", err)
	}
	if _, err := hybrid.Search(ctx, Query{UserID: "user-1", Text: "   ", CollectionIDs: []string{"col-1"}}); !errors.Is(err, ErrEmptyQuery) {
		t.Errorf("blank query: error = %v, want ErrEmptyQuery", err)
	}
	if embedder.calls != 0 {
		t.Fatalf("embedder saw %d calls, want none: a rejected query must not cost a request", embedder.calls)
	}
}

func TestEmbeddingInputIsWhatVectorScorerEmbeds(t *testing.T) {
	// If these two diverge, a corpus indexed through one is queried through the other
	// and every similarity is quietly wrong. The scorer builds
	// query-then-candidates, so candidate 1 must equal EmbeddingInput(candidate 0).
	item := domain.DocumentChunk{ID: "chunk-a", Text: "读写锁允许并发读取", Section: "RWMutex"}
	recorder := &recordingEmbedder{vectors: [][]float32{{1, 0}, {0, 1}}}
	if _, err := NewVectorScorer(recorder, VectorOptions{}).Score(context.Background(), "查询", []domain.DocumentChunk{item}); err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(recorder.lastTexts) != 2 || recorder.lastTexts[1] != EmbeddingInput(item) {
		t.Fatalf("scorer embedded %q, want %q", recorder.lastTexts[1:], EmbeddingInput(item))
	}
}

func TestSimilarityFromCosineDistanceIsClamped(t *testing.T) {
	cases := []struct{ distance, want float64 }{
		{0, 1}, {1, 0}, {2, -1}, {0.25, 0.75}, {-0.5, 1},
	}
	for _, item := range cases {
		if got := SimilarityFromCosineDistance(item.distance); got != item.want {
			t.Errorf("distance %v mapped to %v, want %v", item.distance, got, item.want)
		}
	}
}

func TestHybridReportsAnIndexFailureRatherThanAnEmptyAnswer(t *testing.T) {
	source := &fixedSource{chunks: []domain.DocumentChunk{chunk("chunk-a", "col-1", "doc-1", 1, "内容")}}
	embedder := &recordingEmbedder{vectors: [][]float32{{1, 0}}}
	index := &hybridIndexStub{searchErr: errors.New("index down")}
	hybrid, err := NewHybrid(HybridOptions{Source: source, Embedder: embedder, Index: index})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if _, err := hybrid.Search(context.Background(), Query{UserID: "user-1", Text: "内容", CollectionIDs: []string{"col-1"}}); err == nil || !strings.Contains(err.Error(), "index down") {
		t.Fatalf("error = %v, want the index failure to surface", err)
	}
}

func resultIDs(results []Result) []string {
	ids := make([]string, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.Chunk.ID)
	}
	return ids
}
