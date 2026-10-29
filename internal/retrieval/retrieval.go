// Package retrieval ranks document chunks for a grounded Chat turn. The public
// shape is deliberately small: a query, a closed set of collections, and ranked
// results. Scoring, admission, fusion and the candidate limit stay inside so the
// HTTP contract never has to publish an internal score.
//
// Retrieval is two distinct stages and the boundary is part of the design:
//
//  1. Admission belongs to each scorer. A scorer decides which of the candidates
//     it looked at are evidence at all, using a cut that only makes sense for its
//     own scale.
//  2. Fusion belongs to the pipeline. Reciprocal rank fusion reads positions in a
//     ranking and nothing else; it never compares two raw scores and therefore
//     never needs a threshold that would have to fit both BM25 and cosine.
//
// The stages stay separate because a single threshold cannot: BM25 reports an
// absolute term-overlap magnitude while cosine reports an angle. Collapsing them
// is what makes an unrelated question cite a chunk it has nothing to do with.
package retrieval

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

var (
	// ErrNoCandidates reports that the turn cannot be grounded: the scope held
	// nothing to search, or no scorer admitted anything from it. Both mean the
	// same thing to the caller, which is that there is no evidence to cite.
	ErrNoCandidates = errors.New("retrieval: no candidates in scope")
	// ErrEmptyQuery reports a query with no searchable text.
	ErrEmptyQuery = errors.New("retrieval: query is empty")
	// ErrNoPrincipal reports a search with no owner, which would make scope
	// authorization impossible.
	ErrNoPrincipal = errors.New("retrieval: search requires a user scope")
	// ErrInvalidScore reports a scorer that returned NaN or Inf. That is a broken
	// scorer, not a chunk that failed to match, so it surfaces as an error instead
	// of quietly dropping one candidate.
	ErrInvalidScore = errors.New("retrieval: scorer returned a non-finite score")

	// errNoEmbedder and errEmbedderShape report a misconfigured vector scorer;
	// both are startup-wiring faults, not query outcomes.
	errNoEmbedder    = errors.New("retrieval: vector scorer has no embedder")
	errEmbedderShape = errors.New("retrieval: embedder returned the wrong vector count")
	errNoContext     = errors.New("retrieval: context is required")
)

const (
	// DefaultTopK bounds how many chunks ground one answer.
	DefaultTopK = 5
	// DefaultCandidateLimit bounds the pool each scorer ranks. Without a cap a
	// large collection would make every turn linear in stored chunks.
	DefaultCandidateLimit = 500
	// DefaultRRFK is the standard smoothing constant for reciprocal rank fusion.
	DefaultRRFK = 60
)

// Source supplies the chunks a query may search, already limited to the
// collections the caller is allowed to read.
type Source interface {
	// Chunks returns the retrievable chunks of the given collections. The caller
	// identity is an explicit argument so scope authorization cannot be lost in
	// a context value.
	Chunks(ctx context.Context, userID string, collectionIDs []string) ([]domain.DocumentChunk, error)
}

// Scored is one scorer's measurement for a chunk. It is a raw reading, not a
// verdict: whether the reading counts as evidence is what Admit decides.
type Scored struct {
	Chunk domain.DocumentChunk
	Score float64
}

// Scorer ranks candidates by one signal. Keeping scorers separate is what lets a
// lexical-only deployment and a lexical-plus-vector deployment run the same
// fusion code.
//
// Admission is part of the interface on purpose. A scorer must state, in its own
// units, where "this chunk is not evidence" begins, so the pipeline can fuse a
// ranking without ever holding a score threshold of its own. Making the method
// required means a new scorer cannot forget the question by accident.
type Scorer interface {
	Name() string
	// Score returns a measurement for every candidate it was handed, including
	// the ones it will not admit, so a caller can see what the signal said.
	Score(ctx context.Context, query string, candidates []domain.DocumentChunk) ([]Scored, error)
	// Admit reports whether a score from this scorer's own Score is high enough
	// to be treated as evidence. Implementations must reject non-finite values.
	Admit(score float64) bool
}

// Query is one retrieval request.
type Query struct {
	// UserID is the owner whose collections are being searched. A scorer must not
	// see chunks this user does not own.
	UserID        string
	Text          string
	CollectionIDs []string
	TopK          int
}

// Result is a retrieved chunk with its fused score.
type Result struct {
	Chunk domain.DocumentChunk
	// Score is the fused reciprocal-rank value. It orders results and is ranked on
	// its own; it is comparable across runs of the same inputs, which is what the
	// recomputation tests assert. It says nothing about how well the chunk
	// answers the question, which is why it never leaves the server.
	Score float64
	// Contributors names the scorers that admitted and ranked this chunk, for
	// debugging why a chunk was chosen without exposing numbers to clients.
	Contributors []string
}

// Retriever is the interface the answering strategy depends on.
type Retriever interface {
	Search(ctx context.Context, query Query) ([]Result, error)
}

// Pipeline is a Retriever that runs candidate selection, each scorer with its
// own admission rule, rank fusion over the admitted head of each ranking, and the
// top-k cut in that order.
type Pipeline struct {
	source         Source
	scorers        []Scorer
	rrK            int
	topK           int
	candidateLimit int
}

// Options tunes a Pipeline. Zero values select the defaults.
type Options struct {
	RRFK           int
	TopK           int
	CandidateLimit int
}

func NewPipeline(source Source, scorers []Scorer, opts Options) *Pipeline {
	if opts.RRFK <= 0 {
		opts.RRFK = DefaultRRFK
	}
	if opts.TopK <= 0 {
		opts.TopK = DefaultTopK
	}
	if opts.CandidateLimit <= 0 {
		opts.CandidateLimit = DefaultCandidateLimit
	}
	return &Pipeline{source: source, scorers: scorers, rrK: opts.RRFK, topK: opts.TopK, candidateLimit: opts.CandidateLimit}
}

var _ Retriever = (*Pipeline)(nil)

// Search admits candidates per scorer, fuses the admitted rankings, and returns
// at most TopK chunks in fused order. An empty scope or an empty query is an
// error rather than a silent zero-result answer, because both mean the caller
// asked for grounding it cannot honor. The same is true when every scorer
// declines every chunk: citing something anyway would turn "I did not find this"
// into a fabricated answer.
func (p *Pipeline) Search(ctx context.Context, query Query) ([]Result, error) {
	if ctx == nil {
		return nil, errors.New("retrieval: context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || p.source == nil || len(p.scorers) == 0 {
		return nil, errors.New("retrieval: pipeline is not configured")
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

	candidates, err := p.source.Chunks(ctx, query.UserID, query.CollectionIDs)
	if err != nil {
		return nil, err
	}
	candidates = selectCandidates(candidates, p.candidateLimit)
	if len(candidates) == 0 {
		return nil, ErrNoCandidates
	}

	topK := query.TopK
	if topK <= 0 {
		topK = p.topK
	}
	byID := make(map[string]domain.DocumentChunk, len(candidates))
	for _, chunk := range candidates {
		byID[chunk.ID] = chunk
	}

	rankings := make([]ranking, 0, len(p.scorers))
	for _, scorer := range p.scorers {
		ranked, err := scorer.Score(ctx, query.Text, candidates)
		if err != nil {
			return nil, err
		}
		if err := checkScores(ranked); err != nil {
			return nil, err
		}
		rankings = append(rankings, ranking{name: scorer.Name(), scored: ranked, admit: scorer.Admit})
	}
	// Ranks are counted over admitted candidates only. A chunk the scorer rejected is
	// not a rank slot, so it cannot push a real match down the fusion the way it did
	// when every top-k entry was fused regardless.
	results, admitted := fuse(p.rrK, topK, byID, rankings)
	if admitted == 0 {
		return nil, ErrNoCandidates
	}
	return sortedCapped(results, topK), nil
}

// selectCandidates is the candidate-retrieval step: it keeps a deterministic
// prefix of the scope ordered by collection and position rather than an
// arbitrary subset, so a capped search still covers the documents from the top.
func selectCandidates(chunks []domain.DocumentChunk, limit int) []domain.DocumentChunk {
	ordered := make([]domain.DocumentChunk, len(chunks))
	copy(ordered, chunks)
	sort.SliceStable(ordered, func(left, right int) bool {
		a, b := ordered[left], ordered[right]
		if a.CollectionID != b.CollectionID {
			return a.CollectionID < b.CollectionID
		}
		if a.DocumentID != b.DocumentID {
			return a.DocumentID < b.DocumentID
		}
		return a.Order < b.Order
	})
	if limit > 0 && len(ordered) > limit {
		ordered = ordered[:limit]
	}
	return ordered
}

// checkScores rejects a scorer that produced a value no ranking can use. It runs
// over the whole output so the same scorer result cannot be an error or a
// success depending on where the top-k cut happened to fall.
func checkScores(ranked []Scored) error {
	for _, scored := range ranked {
		if !isFinite(scored.Score) {
			return ErrInvalidScore
		}
	}
	return nil
}

// isFinite reports whether a score can be ranked at all: NaN compares false
// against everything and would silently sort anywhere, and an infinite score
// would dominate a reciprocal-rank sum. The standard library spells this as two
// checks rather than offering IsFinite, so it is written down once here.
func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func orderDescending(ranked []Scored) []Scored {
	ordered := make([]Scored, len(ranked))
	copy(ordered, ranked)
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left].Score != ordered[right].Score {
			return ordered[left].Score > ordered[right].Score
		}
		if ordered[left].Chunk.Order != ordered[right].Chunk.Order {
			return ordered[left].Chunk.Order < ordered[right].Chunk.Order
		}
		return ordered[left].Chunk.ID < ordered[right].Chunk.ID
	})
	return ordered
}

func isEmptyText(value string) bool {
	for _, rune := range value {
		switch {
		case rune == ' ' || rune == '\t' || rune == '\n' || rune == '\r':
		default:
			return false
		}
	}
	return true
}
