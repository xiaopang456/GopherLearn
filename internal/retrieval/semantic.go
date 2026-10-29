package retrieval

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// The semantic half of retrieval is separated from the deterministic half by one
// question: where do the vectors live?
//
// VectorScorer answers "nowhere" - it embeds the query and every candidate on each
// turn, which is correct, cheap at test scale, and impossible at real corpus scale.
// A persisted VectorIndex answers "once, at index time", so a query embeds one text
// and asks for neighbours. Both produce the same Scored shape, which is why the
// fusion in fuse.go is shared and the deterministic path did not change to
// accommodate this one.
//
// Hybrid is a Retriever, so every consumer - Knowledge Q&A, the agent's
// knowledge_search tool, and learning evidence selection - keeps calling the same
// interface it always did. Nothing downstream learns that the candidate source got
// smarter, and that is the property that makes the swap safe.

const (
	// DefaultRecallLimit is how many neighbours the index is asked for before
	// admission. It is wider than TopK on purpose: the cut is a judgement, and a
	// cut applied to a list that was already truncated to the answer cannot be
	// re-tuned without re-querying.
	DefaultRecallLimit = 24
)

// ErrVectorIdentity reports vectors that were produced by a different model, width
// or text version than the one asking. It is a re-index condition, not a query
// outcome, and treating it as "nothing matched" would hide it until someone compared
// two answer sets and asked why retrieval quietly got worse.
var ErrVectorIdentity = errors.New("retrieval: vector index was built by a different embedder")

// VectorEntry is one chunk with the vector that represents it.
type VectorEntry struct {
	Chunk  domain.DocumentChunk
	Vector []float32
	// Identity is the fingerprint of the embedder that produced this vector. It
	// travels per entry rather than per call because an index can legitimately hold
	// vectors written before a model change, and those must stop being recalled.
	Identity string
}

// VectorHit is one index answer: which chunk, and how close.
type VectorHit struct {
	ChunkID string
	// Similarity is already converted to "higher is better" by the adapter.
	Similarity float64
}

// VectorSearch is one neighbour query.
type VectorSearch struct {
	UserID        string
	CollectionIDs []string
	Vector        []float32
	// Identity is the fingerprint of the embedder asking. An adapter that stores it
	// per document must refuse to answer across identities rather than compare
	// incompatible vectors.
	Identity string
	Limit    int
}

// VectorIndex stores and searches pre-computed chunk vectors. Implementations own
// their own schema; this package only ever sees entries and hits, so no vector
// database SDK reaches into retrieval.
type VectorIndex interface {
	// Upsert writes or replaces the vectors for the given chunks. It must be
	// idempotent per chunk id: a re-index of unchanged text is normal.
	Upsert(ctx context.Context, userID string, entries []VectorEntry) error
	// DeleteDocument removes every version of one document's vectors.
	DeleteDocument(ctx context.Context, userID string, documentID string) error
	// DeleteVersions removes the superseded versions of one document, keeping the
	// version that is now current.
	DeleteVersions(ctx context.Context, userID string, documentID string, keepVersion int) error
	// Search returns neighbours ordered by similarity, best first.
	Search(ctx context.Context, query VectorSearch) ([]VectorHit, error)
}

// SimilarityFromCosineDistance converts a RediSearch COSINE distance into the
// "higher is better" scale the rest of the package ranks on.
//
// The original project defaulted a missing distance to 1.0, which reads as "maximally
// distant" and quietly drops the hit. Here a missing field is an error at the
// adapter instead: a hit with no score is a schema problem, not evidence about the
// chunk.
func SimilarityFromCosineDistance(distance float64) float64 {
	value := 1 - distance
	if value < -1 {
		return -1
	}
	if value > 1 {
		return 1
	}
	return value
}

// EmbeddingInput is the exact text a chunk's vector was made from. Indexing and
// querying must both go through it: the shape of a stored vector is meaningless
// unless the text that produced it is reproducible.
//
// It matches what VectorScorer scores, so a corpus indexed by one can be queried by
// the other.
func EmbeddingInput(chunk domain.DocumentChunk) string {
	return chunk.Text + "\n" + chunk.Section
}

// HybridOptions configures a semantic Retriever.
type HybridOptions struct {
	Source   Source
	Embedder Embedder
	Index    VectorIndex
	// Reranker is optional. A nil value keeps fused order, which is also what happens
	// at runtime when the reranker is unavailable.
	Reranker Reranker
	// Lexical is the term-overlap side of the fusion. Nil selects BM25 defaults, and
	// the assembly passes the same scorer the deterministic pipeline uses so the two
	// modes cannot drift apart on tuning alone.
	Lexical Scorer
	// MinSimilarity is the cosine cut for the vector ranking. Zero selects
	// DefaultMinCosineSimilarity; a real model needs its own measured number.
	MinSimilarity float64
	RRFK          int
	TopK          int
	// CandidateLimit bounds the pool the lexical side ranks, exactly as in Pipeline.
	CandidateLimit int
	// RecallLimit bounds the vector side. Zero selects DefaultRecallLimit.
	RecallLimit int
	// Identity is the embedder fingerprint stored with the vectors.
	Identity string
}

// Hybrid retrieves by persisted vector neighbours plus in-process lexical ranking,
// fuses them, and optionally reorders the head with a cross-encoder.
type Hybrid struct {
	source        Source
	embedder      Embedder
	index         VectorIndex
	reranker      Reranker
	lexical       Scorer
	minSimilarity float64
	rrK           int
	topK          int
	candidateLim  int
	recallLimit   int
	identity      string
}

var _ Retriever = (*Hybrid)(nil)

func NewHybrid(options HybridOptions) (*Hybrid, error) {
	if options.Source == nil {
		return nil, errors.New("retrieval: hybrid needs a chunk source")
	}
	if options.Embedder == nil {
		return nil, errors.New("retrieval: hybrid needs an embedder")
	}
	if options.Index == nil {
		return nil, errors.New("retrieval: hybrid needs a vector index")
	}
	cut := options.MinSimilarity
	if !(cut > 0) || !isFinite(cut) {
		cut = DefaultMinCosineSimilarity
	}
	topK := options.TopK
	if topK <= 0 {
		topK = DefaultTopK
	}
	recall := options.RecallLimit
	if recall <= 0 {
		recall = DefaultRecallLimit
	}
	if recall < topK {
		// Asking for fewer neighbours than the answer must contain cannot be fixed by
		// the cut later; it is a configuration mistake worth naming.
		return nil, fmt.Errorf("retrieval: recall limit %d is smaller than top-k %d", recall, topK)
	}
	lexical := options.Lexical
	if lexical == nil {
		lexical = NewLexicalScorer(BM25Options{})
	}
	candidateLimit := options.CandidateLimit
	if candidateLimit <= 0 {
		candidateLimit = DefaultCandidateLimit
	}
	rrK := options.RRFK
	if rrK <= 0 {
		rrK = DefaultRRFK
	}
	return &Hybrid{
		source: options.Source, embedder: options.Embedder, index: options.Index,
		reranker: options.Reranker, lexical: lexical,
		minSimilarity: cut, rrK: rrK, topK: topK,
		candidateLim: candidateLimit, recallLimit: recall, identity: options.Identity,
	}, nil
}

// VectorIdentity reports the fingerprint this retriever asks the index for.
func (h *Hybrid) VectorIdentity() string { return h.identity }

// Search embeds the query once, recalls neighbours, ranks the authorized scope
// lexically, fuses the two, and reorders the head when a reranker is configured.
//
// The error contract matches Pipeline: an empty scope, an empty query or a turn where
// nothing is admitted all surface as errors, because each of them means the caller
// asked for grounding that cannot be honored. A citation invented from an
// out-of-scope hit is prevented here rather than downstream: index hits only become
// results if the same chunk id is present in the authorized scope.
func (h *Hybrid) Search(ctx context.Context, query Query) ([]Result, error) {
	if ctx == nil {
		return nil, errNoContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h == nil {
		return nil, errors.New("retrieval: hybrid is not configured")
	}
	if strings.TrimSpace(query.UserID) == "" {
		return nil, ErrNoPrincipal
	}
	if len(query.CollectionIDs) == 0 {
		return nil, ErrNoCandidates
	}
	if isEmptyText(query.Text) {
		return nil, ErrEmptyQuery
	}
	topK := query.TopK
	if topK <= 0 {
		topK = h.topK
	}

	candidates, err := h.source.Chunks(ctx, query.UserID, query.CollectionIDs)
	if err != nil {
		return nil, err
	}
	candidates = selectCandidates(candidates, h.candidateLim)
	if len(candidates) == 0 {
		return nil, ErrNoCandidates
	}
	byID := make(map[string]domain.DocumentChunk, len(candidates))
	for _, chunk := range candidates {
		byID[chunk.ID] = chunk
	}

	vectors, err := h.embedder.Embed(ctx, []string{query.Text})
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 || len(vectors[0]) != h.embedder.Dimensions() {
		return nil, fmt.Errorf("%w: query embedding count %d", errEmbedderShape, len(vectors))
	}
	hits, err := h.index.Search(ctx, VectorSearch{
		UserID: query.UserID, CollectionIDs: query.CollectionIDs, Vector: vectors[0],
		Identity: h.identity, Limit: max(h.recallLimit, topK),
	})
	if err != nil {
		return nil, err
	}
	vectorRanked := make([]Scored, 0, len(hits))
	for _, hit := range hits {
		chunk, known := byID[hit.ChunkID]
		if !known {
			continue
		}
		vectorRanked = append(vectorRanked, Scored{Chunk: chunk, Score: hit.Similarity})
	}

	rankings := []ranking{{
		name:   "vector:" + h.embedder.Model(),
		scored: vectorRanked,
		admit: func(score float64) bool {
			return isFinite(score) && score >= h.minSimilarity
		},
	}}
	lexicalRanked, err := h.lexical.Score(ctx, query.Text, candidates)
	if err != nil {
		return nil, err
	}
	if err := checkScores(lexicalRanked); err != nil {
		return nil, err
	}
	rankings = append(rankings, ranking{name: h.lexical.Name(), scored: lexicalRanked, admit: h.lexical.Admit})

	results, admitted := fuse(h.rrK, topK, byID, rankings)
	if admitted == 0 {
		return nil, ErrNoCandidates
	}
	ordered := sortedCapped(results, topK)
	if h.reranker == nil {
		return ordered, nil
	}
	return h.rerank(ctx, query.Text, ordered)
}

// rerank reorders the fused head. An unavailable reranker leaves the fused order
// untouched and says so, because the alternative - failing a learner's turn because an
// optional second stage is down - would make the system less available than the sum
// of its parts.
func (h *Hybrid) rerank(ctx context.Context, query string, fused []Result) ([]Result, error) {
	limit := len(fused)
	if capacity, ok := h.reranker.(interface{ MaxPassages() int }); ok && capacity.MaxPassages() < limit {
		limit = capacity.MaxPassages()
	}
	passages := make([]string, 0, limit)
	for _, result := range fused[:limit] {
		passages = append(passages, EmbeddingInput(result.Chunk))
	}
	hits, err := h.reranker.Rerank(ctx, query, passages)
	if errors.Is(err, ErrRerankUnavailable) {
		marked := make([]Result, len(fused))
		copy(marked, fused)
		for index := range marked {
			marked[index].Contributors = append(marked[index].Contributors, "rerank:unavailable")
		}
		return marked, nil
	}
	if err != nil {
		return nil, err
	}
	reordered := make([]Result, 0, len(hits))
	seen := make(map[string]struct{}, len(hits))
	for rank, hit := range hits {
		result := fused[hit.Index]
		if _, taken := seen[result.Chunk.ID]; taken {
			continue
		}
		seen[result.Chunk.ID] = struct{}{}
		result.Score = 1 / float64(rank+1)
		result.Contributors = append(append([]string{}, result.Contributors...), "rerank:"+h.reranker.Model()+":"+strconv.Itoa(rank+1))
		reordered = append(reordered, result)
	}
	// Anything the rerank window never covered keeps its place behind the reordered
	// head rather than disappearing: the fused list may be longer than the reranker's
	// passage budget.
	for _, result := range fused[limit:] {
		if _, taken := seen[result.Chunk.ID]; taken {
			continue
		}
		seen[result.Chunk.ID] = struct{}{}
		reordered = append(reordered, result)
	}
	return reordered, nil
}
