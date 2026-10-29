package retrieval

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// scriptScorer reports fixed measurements for known chunk ids and 0 for the
// rest, admitting only at or above its own cut. It lets the fusion tests state
// exact reciprocal-rank values and lets a test vary the admission rule without
// depending on what BM25 or cosine happens to produce.
type scriptScorer struct {
	name   string
	cut    float64
	scores map[string]float64
}

func (s scriptScorer) Name() string { return s.name }

func (s scriptScorer) Admit(score float64) bool { return isFinite(score) && score >= s.cut }

func (s scriptScorer) Score(_ context.Context, _ string, candidates []domain.DocumentChunk) ([]Scored, error) {
	ranked := make([]Scored, 0, len(candidates))
	for _, candidate := range candidates {
		ranked = append(ranked, Scored{Chunk: candidate, Score: s.scores[candidate.ID]})
	}
	return ranked, nil
}

// positionalEmbedder hands back pre-set vectors in input order: index 0 is the
// query and the rest are the candidates. That fixes the cosine values a test
// asserts on instead of inheriting whatever the placeholder embedding yields.
type positionalEmbedder struct {
	vectors [][]float32
}

func (e *positionalEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if len(texts) != len(e.vectors) {
		return nil, errors.New("positionalEmbedder: wanted " + itoaTest(len(e.vectors)) + " texts, got " + itoaTest(len(texts)))
	}
	return e.vectors, nil
}

func (e *positionalEmbedder) Dimensions() int { return len(e.vectors[0]) }

func (e *positionalEmbedder) Model() string { return "positional-test-embedder" }

func rrf(rank int) float64 { return 1 / float64(DefaultRRFK+rank) }

// TestBM25AdmitsOnlyRealTermOverlap pins the lexical cut at its zero point: the
// absence of any shared term is a measurement of 0, and 0 is never evidence.
func TestBM25AdmitsOnlyRealTermOverlap(t *testing.T) {
	scorer := NewLexicalScorer(BM25Options{})
	if !scorer.Admit(1e-9) {
		t.Fatal("a chunk sharing one term with the query must be admitted")
	}
	for _, rejected := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if scorer.Admit(rejected) {
			t.Fatalf("Admit(%v) = true, want false", rejected)
		}
	}
	scored, err := scorer.Score(context.Background(), "quantum chromodynamics", corpus)
	if err != nil {
		t.Fatal(err)
	}
	if len(scored) != len(corpus) {
		t.Fatalf("Score reported %d candidates, want one measurement per candidate", len(scored))
	}
	for _, item := range scored {
		if item.Score != 0 {
			t.Fatalf("%q scored %v on a query that shares no term with it", item.Chunk.ID, item.Score)
		}
		if scorer.Admit(item.Score) {
			t.Fatalf("%q was admitted with a score of %v", item.Chunk.ID, item.Score)
		}
	}
}

// TestZeroScoreChunksCannotEnterTopK is the regression this whole change exists
// for. Every candidate scores 0 on an unrelated question, and the old fusion
// loop took the head of that ranking anyway, so chunks were cited in stored
// order and id order - the tie-break silently became the answer.
func TestZeroScoreChunksCannotEnterTopK(t *testing.T) {
	silent := []domain.DocumentChunk{
		chunk("a1", "col-a", "doc-1", 1, "Redis 持久化：AOF 以追加写记录每条写命令。"),
		chunk("a2", "col-a", "doc-1", 2, "RDB 在 fork 出的子进程里生成内存快照。"),
	}
	pipeline := NewPipeline(&fixedSource{chunks: silent}, []Scorer{NewLexicalScorer(BM25Options{})}, Options{TopK: 5})
	results, err := pipeline.Search(context.Background(), Query{UserID: "user-1", Text: "quantum chromodynamics", CollectionIDs: []string{"col-a"}})
	if !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("error = %v, want ErrNoCandidates", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %v, want none: a non-empty collection is not evidence", idsOf(results))
	}
}

// TestNegativeAndNoiseVectorSimilarityIsNotAdmitted checks the vector cut in
// isolation: an opposite vector and a noise-level one just above zero are both
// not evidence, while a real match is. Only the last one may be cited.
func TestNegativeAndNoiseVectorSimilarityIsNotAdmitted(t *testing.T) {
	if CosineSimilarity([]float32{1, 0}, []float32{-1, 0}) >= 0 {
		t.Fatal("the test vectors no longer produce a negative cosine")
	}
	candidates := []domain.DocumentChunk{
		chunk("neg", "col-a", "doc-1", 1, "opposite"),
		chunk("noise", "col-a", "doc-1", 2, "almost orthogonal"),
		chunk("real", "col-a", "doc-1", 3, "aligned"),
	}
	embedder := &positionalEmbedder{vectors: [][]float32{
		{1, 0},
		{-1, 0},
		{0.1, 0.98},
		{0.35, 0.9},
	}}
	scorer := NewVectorScorer(embedder, VectorOptions{})
	scored, err := scorer.Score(context.Background(), "query", candidates)
	if err != nil {
		t.Fatal(err)
	}
	measured := map[string]float64{}
	wantAdmitted := map[string]bool{"neg": false, "noise": false, "real": true}
	for _, item := range scored {
		want, known := wantAdmitted[item.Chunk.ID]
		if !known {
			t.Fatalf("unexpected candidate %q", item.Chunk.ID)
		}
		if scorer.Admit(item.Score) != want {
			t.Fatalf("%q cosine %v admitted=%v, want %v", item.Chunk.ID, item.Score, !want, want)
		}
		measured[item.Chunk.ID] = item.Score
	}
	if !(measured["noise"] > 0) || measured["noise"] >= DefaultMinCosineSimilarity {
		t.Fatalf("noise cosine = %v, want a value above 0 but below the default cut", measured["noise"])
	}
	if measured["real"] <= DefaultMinCosineSimilarity {
		t.Fatalf("match cosine = %v, want a value above the default cut", measured["real"])
	}

	pipeline := NewPipeline(&fixedSource{chunks: candidates}, []Scorer{scorer}, Options{TopK: 5})
	results, err := pipeline.Search(context.Background(), Query{UserID: "user-1", Text: "query", CollectionIDs: []string{"col-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(results); len(got) != 1 || got[0] != "real" {
		t.Fatalf("results = %v, want only the cosine above the cut", got)
	}
	if results[0].Score != rrf(1) {
		t.Fatalf("fused score = %v, want a lone first rank of %v", results[0].Score, rrf(1))
	}
	// Relaxing the cut is configuration, not a different pipeline: the same
	// measurements now admit the noise-level chunk too.
	loose := NewPipeline(&fixedSource{chunks: candidates}, []Scorer{NewVectorScorer(embedder, VectorOptions{MinSimilarity: 0.05})}, Options{TopK: 5})
	relaxed, err := loose.Search(context.Background(), Query{UserID: "user-1", Text: "query", CollectionIDs: []string{"col-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(relaxed); len(got) != 2 || got[0] != "real" || got[1] != "noise" {
		t.Fatalf("with a 0.05 cut results = %v, want real then noise", got)
	}
}

// TestVectorCutIsConfigurableWithAConservativePlaceholderDefault checks both
// halves of the requirement: the deterministic embedder gets a default that is
// above measured collision noise and below a real match, and an operator can
// still choose the number.
func TestVectorCutIsConfigurableWithAConservativePlaceholderDefault(t *testing.T) {
	// The unrelated question in TestBM25AdmitsOnlyRealTermOverlap reaches about
	// 0.10 cosine from hash collisions alone; a question sharing real tokens
	// reaches about 0.40. A default outside that gap is not conservative.
	// 0.19 is the highest similarity an unrelated question reached on either test
	// corpus; 0.28 is the lowest a question with real shared content reached.
	if DefaultMinCosineSimilarity < 0.19 || DefaultMinCosineSimilarity > 0.28 {
		t.Fatalf("default cosine cut = %v, must sit above the measured collision noise and below the measured real matches", DefaultMinCosineSimilarity)
	}
	embedder := NewDeterministicEmbedder(64)
	if got := NewVectorScorer(embedder, VectorOptions{}).minSimilarity; got != DefaultMinCosineSimilarity {
		t.Fatalf("zero-value options selected cut %v, want %v", got, DefaultMinCosineSimilarity)
	}
	if got := NewVectorScorer(embedder, VectorOptions{MinSimilarity: 0.42}).minSimilarity; got != 0.42 {
		t.Fatalf("explicit cut = %v, want 0.42", got)
	}
	for _, unusable := range []float64{0, -0.2, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := NewVectorScorer(embedder, VectorOptions{MinSimilarity: unusable}).minSimilarity; got != DefaultMinCosineSimilarity {
			t.Fatalf("MinSimilarity(%v) produced cut %v, want the default", unusable, got)
		}
	}
}

// TestUnrelatedQuestionStillScoresZeroOnThePlaceholderEmbedder keeps the numbers
// quoted in the default's comment honest: the deterministic embedder does hand out
// non-zero similarity to a question that shares no term at all.
func TestUnrelatedQuestionStillScoresZeroOnThePlaceholderEmbedder(t *testing.T) {
	scorer := NewVectorScorer(NewDeterministicEmbedder(0), VectorOptions{})
	scored, err := scorer.Score(context.Background(), "quantum chromodynamics", corpus)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range scored {
		if scorer.Admit(item.Score) {
			t.Fatalf("%q was admitted at cosine %v for a question with no shared token", item.Chunk.ID, item.Score)
		}
	}
}

// TestPipelineFusesAroundAScorerThatAdmitsNothing covers the mixed case: one
// signal is useless on this query, the other is real, and fusion must still
// produce the real hit rather than failing or diluting it.
func TestPipelineFusesAroundAScorerThatAdmitsNothing(t *testing.T) {
	silent := scriptScorer{name: "silent", cut: 1, scores: map[string]float64{"c1": 0, "c2": 0, "c3": 0}}
	pipeline := NewPipeline(&fixedSource{chunks: corpus}, []Scorer{
		silent,
		NewLexicalScorer(BM25Options{}),
	}, Options{TopK: 2})
	results, err := pipeline.Search(context.Background(), Query{UserID: "user-1", Text: "Redis AOF 持久化", CollectionIDs: []string{"col-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].Chunk.ID != "c1" {
		t.Fatalf("results = %v, want the lexical hit first", idsOf(results))
	}
	if len(results[0].Contributors) != 1 || results[0].Contributors[0] != "lexical-bm25" {
		t.Fatalf("contributors = %v, want only the scorer that admitted a real hit", results[0].Contributors)
	}
	if results[0].Score != rrf(1) {
		t.Fatalf("fused score = %v, want a lone first rank of %v", results[0].Score, rrf(1))
	}
}

// TestAdmissionRuleLivesWithTheScorerNotTheFusion is the direct check on the
// separation: identical measurements fuse differently depending only on what the
// scorer reports about its own scale, so the pipeline cannot hold a threshold.
func TestAdmissionRuleLivesWithTheScorerNotTheFusion(t *testing.T) {
	scores := map[string]float64{"c1": 0, "c2": 0, "c3": 0}
	permissive := scriptScorer{name: "permissive", cut: math.Inf(-1), scores: scores}
	strict := scriptScorer{name: "strict", cut: 0.01, scores: scores}

	lenient := NewPipeline(&fixedSource{chunks: corpus}, []Scorer{permissive}, Options{TopK: 3})
	results, err := lenient.Search(context.Background(), Query{UserID: "user-1", Text: "quantum chromodynamics", CollectionIDs: []string{"col-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("a scorer that admits everything produced %v, want its whole head fused", idsOf(results))
	}

	guarded := NewPipeline(&fixedSource{chunks: corpus}, []Scorer{strict}, Options{TopK: 3})
	if _, err := guarded.Search(context.Background(), Query{UserID: "user-1", Text: "quantum chromodynamics", CollectionIDs: []string{"col-a"}}); !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("error = %v, want ErrNoCandidates from the same measurements under a stricter scorer", err)
	}
}

// TestAdmittedRankingsFuseWithExactReciprocalValues states the fusion arithmetic
// independently of any scorer implementation: rank counts admitted entries only,
// and the same chunk named by two scorers collects both contributions.
func TestAdmittedRankingsFuseWithExactReciprocalValues(t *testing.T) {
	first := scriptScorer{name: "one", cut: 1, scores: map[string]float64{"c1": 9, "c2": 5, "c3": 0}}
	second := scriptScorer{name: "two", cut: 1, scores: map[string]float64{"c1": 2, "c2": 8, "c3": 0}}
	pipeline := NewPipeline(&fixedSource{chunks: corpus}, []Scorer{first, second}, Options{TopK: 3})
	results, err := pipeline.Search(context.Background(), Query{UserID: "user-1", Text: "whatever", CollectionIDs: []string{"col-a"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, result := range results {
		got[result.Chunk.ID] = result.Score
	}
	if len(results) != 2 {
		t.Fatalf("results = %v, want the two admitted chunks only", idsOf(results))
	}
	// c1 is second of the two admitted in ranking one and first in ranking two;
	// c2 is first in one and second in the other, so they tie and stored order
	// breaks it. c3 scored 0 on both and must not appear at all.
	want := map[string]float64{
		"c1": rrf(2) + rrf(1),
		"c2": rrf(1) + rrf(2),
	}
	for id, value := range want {
		if math.Abs(got[id]-value) > 1e-12 {
			t.Fatalf("%q fused score = %v, want %v", id, got[id], value)
		}
	}
	if idsOf(results)[0] != "c1" || idsOf(results)[1] != "c2" {
		t.Fatalf("tie order = %v, want stored order c1 then c2", idsOf(results))
	}
	for _, result := range results {
		if len(result.Contributors) != 2 {
			t.Fatalf("%q contributors = %v, want both scorers", result.Chunk.ID, result.Contributors)
		}
	}
}

// TestPipelineRejectsNonFiniteScorerOutput keeps the rejection of NaN and Inf
// independent of where the bad value happens to sit in the ranking.
func TestPipelineRejectsNonFiniteScorerOutput(t *testing.T) {
	for name, scores := range map[string]map[string]float64{
		"head":              {"c1": math.NaN(), "c2": 5, "c3": 4},
		"beyond top-k":      {"c1": 5, "c2": 4, "c3": math.Inf(1)},
		"negative infinity": {"c1": 5, "c2": 4, "c3": math.Inf(-1)},
	} {
		pipeline := NewPipeline(&fixedSource{chunks: corpus[:3]}, []Scorer{scriptScorer{name: name, cut: 1, scores: scores}}, Options{TopK: 2})
		_, err := pipeline.Search(context.Background(), Query{UserID: "user-1", Text: "whatever", CollectionIDs: []string{"col-a"}})
		if !errors.Is(err, ErrInvalidScore) {
			t.Fatalf("%s: error = %v, want ErrInvalidScore", name, err)
		}
	}
}
