// Package fake holds the offline stand-ins the evaluation's tests run on: a
// deterministic embedder, an in-memory index that plays both the Redis chunk
// repository and the Redis searcher, and a deterministic reranker.
//
// They exist to prove plumbing: that every option reaches the retrieval call,
// that call counts match the plan, that records and metrics are computed
// correctly. Their rankings say nothing about how good retrieval is, and no
// result produced with them may be reported as a quality result.
package fake

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"sync"
	"unicode"

	"github.com/cloudwego/eino/components/embedding"
)

// Embedder is a feature-hashing embedder: each word (and each Han character and
// adjacent Han pair) is hashed to a signed bucket, and the vector is normalised.
// Texts that share words get a high cosine; the result is a pure function of
// the text and the dimension.
type Embedder struct {
	dim int

	mu    sync.Mutex
	calls int
	texts int
	err   error
}

var _ embedding.Embedder = (*Embedder)(nil)

// NewEmbedder returns an embedder producing vectors of the given dimension.
func NewEmbedder(dimension int) *Embedder {
	if dimension < 2 {
		panic("fake.NewEmbedder: dimension must be at least 2")
	}
	return &Embedder{dim: dimension}
}

// Dimension is the length of every vector.
func (e *Embedder) Dimension() int { return e.dim }

// Fail makes every later call return err; nil restores normal behaviour.
func (e *Embedder) Fail(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err = err
}

// Calls is the number of EmbedStrings calls, failed ones included.
func (e *Embedder) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// Texts is the total number of texts sent, failed calls included.
func (e *Embedder) Texts() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.texts
}

// EmbedStrings implements embedding.Embedder.
func (e *Embedder) EmbedStrings(ctx context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	e.mu.Lock()
	e.calls++
	e.texts += len(texts)
	failure := e.err
	e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if failure != nil {
		return nil, failure
	}
	out := make([][]float64, len(texts))
	for i, text := range texts {
		out[i] = embedText(text, e.dim)
	}
	return out, nil
}

func embedText(text string, dim int) []float64 {
	vector := make([]float64, dim)
	for _, token := range Tokens(text) {
		sum := fnv.New32a()
		_, _ = sum.Write([]byte(token))
		h := sum.Sum32()
		sign := 1.0
		if h>>16&1 == 1 {
			sign = -1
		}
		vector[int(h%uint32(dim))] += sign
	}
	var norm float64
	for _, v := range vector {
		norm += v * v
	}
	if norm == 0 {
		// Text with no tokens still needs a usable direction.
		vector[0] = 1
		return vector
	}
	norm = math.Sqrt(norm)
	for i := range vector {
		vector[i] /= norm
	}
	return vector
}

// Tokens splits text into lowercase words, plus every Han character and every
// adjacent Han pair. The fakes share it so their notions of "the same word"
// agree.
func Tokens(text string) []string {
	var tokens []string
	var word strings.Builder
	var han []rune
	flushWord := func() {
		if word.Len() > 0 {
			tokens = append(tokens, word.String())
			word.Reset()
		}
	}
	flushHan := func() {
		for i, r := range han {
			tokens = append(tokens, string(r))
			if i+1 < len(han) {
				tokens = append(tokens, string(han[i:i+2]))
			}
		}
		han = han[:0]
	}
	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			word.WriteRune(r)
		default:
			flushWord()
			flushHan()
		}
	}
	flushWord()
	flushHan()
	return tokens
}

// cosine returns the cosine similarity of two vectors of equal length. Both are
// expected to be unit length, but it divides by the norms so it does not depend
// on that.
func cosine(a []float32, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("vector dimensions differ: %d and %d", len(a), len(b))
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0, nil
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb)), nil
}
