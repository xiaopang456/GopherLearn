package retrieval

import (
	"context"
	"hash/fnv"
	"math"
	"strconv"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// Embedder turns text into a vector. It is an interface precisely so that this
// package does not depend on any provider or vector store SDK.
type Embedder interface {
	// Embed returns exactly one vector per input text, in the same order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimensions() int
	// Model names what produced the vectors, so a persisted index can detect a
	// model change instead of comparing incompatible vectors.
	Model() string
}

// DeterministicEmbedder is feature hashing over Tokenize output: each term is
// hashed to a bucket with a signed weight, then the vector is L2 normalized.
//
// It is a placeholder with real mathematical behavior, not a language model. It
// has no semantics - it rewards shared surface tokens - so it is useful for
// exercising the pipeline and completely unsuitable for claiming retrieval
// quality. Swapping in a real model means implementing Embedder elsewhere.
type DeterministicEmbedder struct {
	dimensions int
}

func NewDeterministicEmbedder(dimensions int) *DeterministicEmbedder {
	if dimensions <= 0 {
		dimensions = 256
	}
	return &DeterministicEmbedder{dimensions: dimensions}
}

func (e *DeterministicEmbedder) Dimensions() int { return e.dimensions }

// Model names this implementation explicitly so nobody can mistake it for a
// trained embedding.
func (e *DeterministicEmbedder) Model() string {
	return "deterministic-feature-hash-v1:" + strconv.Itoa(e.dimensions)
}

func (e *DeterministicEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if ctx == nil {
		return nil, errNoContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vectors := make([][]float32, len(texts))
	for index, text := range texts {
		vectors[index] = e.embedOne(text)
	}
	return vectors, nil
}

func (e *DeterministicEmbedder) embedOne(text string) []float32 {
	vector := make([]float32, e.dimensions)
	terms := Tokenize(text)
	if len(terms) == 0 {
		return vector
	}
	counts := termFrequencies(terms)
	for term, count := range counts {
		bucket, sign := e.slot(term)
		// Sublinear term frequency keeps a repeated word from dominating a chunk.
		weight := float32(1+math.Log(float64(count))) * sign
		vector[bucket] += weight
	}
	return normalize(vector)
}

func (e *DeterministicEmbedder) slot(term string) (int, float32) {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(term))
	value := hash.Sum64()
	return int(value % uint64(e.dimensions)), signOf(value)
}

func signOf(value uint64) float32 {
	if value&(1<<63) != 0 {
		return 1
	}
	return -1
}

func normalize(vector []float32) []float32 {
	var sum float64
	for _, value := range vector {
		sum += float64(value) * float64(value)
	}
	if sum == 0 {
		return vector
	}
	scale := float32(1 / math.Sqrt(sum))
	for index := range vector {
		vector[index] *= scale
	}
	return vector
}

// CosineSimilarity returns the cosine of two vectors, clamped to [-1, 1]. Vectors
// of different lengths are not comparable and score 0.
func CosineSimilarity(left, right []float32) float64 {
	if len(left) != len(right) || len(left) == 0 {
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		a, b := float64(left[index]), float64(right[index])
		dot += a * b
		leftNorm += a * a
		rightNorm += b * b
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	value := dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
	return math.Max(-1, math.Min(1, value))
}

// DefaultMinCosineSimilarity is the conservative cosine cut for the deterministic
// feature-hash placeholder. The number comes from measurements of this embedder,
// not from taste: across the test corpora the most similar an unrelated question
// ever looked to a chunk was 0.19 (a four-word English question colliding into the
// buckets of a Chinese chunk), while a question that really shares content with a
// chunk scored 0.29 to 0.56. Cutting at zero would cite the collision; cutting at
// 0.25 keeps the genuine matches and drops the noise.
//
// The cut is deliberately high because this signal is supplementary: BM25 already
// carries every exact-term hit, and the placeholder has no semantics to add. A
// deployment that injects a trained Embedder gets a different, better-behaved
// scale and should set VectorOptions.MinSimilarity from measurements of that model
// instead of inheriting this number.
const DefaultMinCosineSimilarity = 0.25

// VectorOptions tunes the cosine cut. A zero value selects the default.
type VectorOptions struct {
	// MinSimilarity is the lowest cosine similarity still treated as evidence.
	// Any value <= 0, and any non-finite value, selects
	// DefaultMinCosineSimilarity instead: this field can raise the cut or pick
	// the default, but it cannot ask for a zero or negative threshold.
	MinSimilarity float64
}

// VectorScorer ranks candidates by cosine similarity to the query under an
// injected Embedder.
type VectorScorer struct {
	embedder      Embedder
	minSimilarity float64
}

func NewVectorScorer(embedder Embedder, options VectorOptions) *VectorScorer {
	cut := options.MinSimilarity
	if !(cut > 0) || !isFinite(cut) {
		cut = DefaultMinCosineSimilarity
	}
	return &VectorScorer{embedder: embedder, minSimilarity: cut}
}

// Name implements Scorer.
func (s *VectorScorer) Name() string {
	if s.embedder == nil {
		return "vector"
	}
	return "vector:" + s.embedder.Model()
}

// Admit implements Scorer. A negative cosine means the vectors point apart, and
// a similarity below the cut means the only thing they share is noise. Unlike
// BM25 there is no built-in zero point to lean on here, which is exactly why the
// rule lives on this scorer and not in the shared fusion code.
func (s *VectorScorer) Admit(score float64) bool {
	return isFinite(score) && score >= s.minSimilarity
}

// Score implements Scorer. It reports the similarity of every candidate,
// including the ones Admit rejects, so the measurement stays separate from the
// admission decision.
func (s *VectorScorer) Score(ctx context.Context, query string, candidates []domain.DocumentChunk) ([]Scored, error) {
	if s.embedder == nil {
		return nil, errNoEmbedder
	}
	texts := make([]string, 0, len(candidates)+1)
	texts = append(texts, query)
	for _, chunk := range candidates {
		texts = append(texts, chunk.Text+"\n"+chunk.Section)
	}
	vectors, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, errEmbedderShape
	}
	results := make([]Scored, 0, len(candidates))
	for index, chunk := range candidates {
		results = append(results, Scored{
			Chunk: chunk,
			Score: CosineSimilarity(vectors[0], vectors[index+1]),
		})
	}
	return results, nil
}
