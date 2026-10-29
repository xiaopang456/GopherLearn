package memory

import (
	"GopherAI/common/rag"
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"fmt"
	"sort"
	"strings"
)

// AGENT-020 replaces tag matching as the recall channel for `topic_note`.
//
// EXP-002B B1 measured the shipped tag path at hit@5 0.000 on a corpus built
// from the user's real notes — below the 0.052 of a control that ignores the
// question entirely. The replacement is the retrieval shape the knowledge base
// has used since RAG-010 (vector plus lexical, fused by reciprocal rank), with
// two additions B1 found necessary:
//
//   - query expansion, the only change measured to raise recall@20 (0.662 to
//     0.740) rather than merely reorder what was already retrieved;
//   - a cosine floor, because a ranker always returns a top result and nothing
//     in the old path ever asked whether the best match was good enough. All 43
//     unanswerable queries were answered with five irrelevant memories.
//
// Tags are not deleted. They remain on the row and stay useful as a filter;
// what they stop being is the way candidates are found.

type MemoryVectorSearcher interface {
	SearchByVector(ctx context.Context, userID int64, vector []float32, topK int) ([]memorydao.MemorySearchResult, error)
	SearchByText(ctx context.Context, userID int64, terms []string, topK int) ([]memorydao.MemorySearchResult, error)
}

type QueryEmbedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// QueryExpander turns a question into extra retrieval terms. It costs one model
// call on the request path, so every failure mode falls back to the raw
// question rather than failing recall.
type QueryExpander interface {
	Expand(ctx context.Context, question string) (string, error)
}

type MemoryLoader interface {
	LoadByIDs(ctx context.Context, userID int64, memoryIDs []string) ([]model.LongTermMemory, error)
}

// AbstentionRule selects how the recall path decides it has found nothing.
//
// EXP-002B shipped AbstainByAbsoluteCosine and the blind set refuted it: the
// rule lost 13 answerable queries to win 1, p = 0.0018. The cause was measured
// afterwards and is a property of the quantity, not of the value chosen for it.
// Cosine similarity to the best match drifts upward as the memory pile grows -
// the same queries score a median of 0.56 against a pool of 100 memories and
// 0.63 against 539 - and that drift is as large as the entire gap between
// answerable and unanswerable queries. A user accumulating memories therefore
// walks out from under any fixed threshold.
//
// AbstainByStandout asks a question that does not drift: is the best candidate
// clearly better than the others retrieved alongside it? Measured as area under
// the ROC curve on the same data, it separates answerable from unanswerable at
// 0.925 against 0.849 for the absolute rule, while its own drift across pool
// sizes is 4.2% against 11.5%.
type AbstentionRule string

const (
	AbstainByAbsoluteCosine AbstentionRule = "absolute_cosine"
	AbstainByStandout       AbstentionRule = "standout_ratio"
)

type SimilarityConfig struct {
	// CandidateDepth is how deep each channel searches before fusion. B1
	// measured recall@20 at 0.740 against hit@5 of 0.584, so a depth well
	// above the result count is where the headroom is.
	CandidateDepth int
	MaxResults     int
	// MinSimilarity is the abstention floor, calibrated in B1 on the training
	// split and validated on development: at 0.535 it removed 70% of the false
	// returns with no loss of hit@5 on either split.
	MinSimilarity float64
	// Rule selects which abstention question is asked. The absolute rule is kept
	// so the two can be compared on one corpus rather than across reports.
	Rule AbstentionRule
	// MinStandout is the floor for the standout rule: the best candidate's
	// similarity divided by the mean similarity of the whole retrieved
	// candidate set. A value of 1 means the best match is merely average.
	//
	// Calibration is pending on the v4 training split; until then the shipped
	// default keeps the absolute rule, because an uncalibrated new gate is
	// worse than a known-imperfect old one.
	MinStandout float64
	// LexicalRescue keeps a result when the lexical channel independently
	// agrees with the semantic one. The blind run showed the gate is computed
	// on the semantic channel but applied to the fused output, so a query the
	// keyword side would have answered can be discarded on a weak vector score:
	// 5 of the 13 losses were of that kind.
	LexicalRescue bool
	MinConfidence float64
}

func DefaultSimilarityConfig() SimilarityConfig {
	return SimilarityConfig{
		CandidateDepth: 20, MaxResults: 5,
		Rule: AbstainByAbsoluteCosine, MinSimilarity: 0.535,
		MinStandout: 0, LexicalRescue: false, MinConfidence: 0.6,
	}
}

func (c SimilarityConfig) Validate() error {
	if c.MaxResults < 1 || c.MaxResults > 5 {
		return fmt.Errorf("similarity max results must be between 1 and 5")
	}
	if c.CandidateDepth < c.MaxResults || c.CandidateDepth > 100 {
		return fmt.Errorf("similarity candidate depth must be between max results and 100")
	}
	if c.MinSimilarity < 0 || c.MinSimilarity > 1 || c.MinConfidence < 0 || c.MinConfidence > 1 {
		return fmt.Errorf("similarity thresholds must be between 0 and 1")
	}
	switch c.Rule {
	case AbstainByAbsoluteCosine:
	case AbstainByStandout:
		// A ratio below one would admit a best match that is worse than the
		// average of its own candidate set, which cannot be evidence of
		// anything. The upper bound is a typo guard: the observed separation
		// lives between roughly 1.2 and 1.6.
		if c.MinStandout < 1 || c.MinStandout > 3 {
			return fmt.Errorf("standout floor must be between 1 and 3")
		}
	default:
		return fmt.Errorf("unknown abstention rule %q", c.Rule)
	}
	return nil
}

// SimilarityRecall owns the replacement recall path. It is optional on the
// Retriever: when it is absent, or when Redis is unavailable, the retriever
// falls back to the tag path, which is worse but not zero.
type SimilarityRecall struct {
	index    MemoryVectorSearcher
	embedder QueryEmbedder
	expander QueryExpander
	loader   MemoryLoader
	config   SimilarityConfig
	stats    RecallDegradation
}

// RecallDegradation counts the silent failures. Both of the new dependencies
// degrade rather than error, which means without counters an outage looks
// exactly like a quiet day.
type RecallDegradation struct {
	ExpansionFailures int64
	IndexFailures     int64
	AbstainedByFloor  int64
}

func NewSimilarityRecall(index MemoryVectorSearcher, embedder QueryEmbedder, expander QueryExpander, loader MemoryLoader, config SimilarityConfig) (*SimilarityRecall, error) {
	if index == nil || embedder == nil || loader == nil {
		return nil, fmt.Errorf("similarity recall requires an index, an embedder and a loader")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &SimilarityRecall{index: index, embedder: embedder, expander: expander, loader: loader, config: config}, nil
}

func (s *SimilarityRecall) Degradation() RecallDegradation { return s.stats }

// errIndexUnavailable signals the caller to fall back rather than to fail. A
// question the user asked should not error out because a cache is down.
var errIndexUnavailable = fmt.Errorf("memory similarity index unavailable")

// Recall returns ranked topic notes, or an empty slice when nothing clears the
// abstention floor. An empty result is a real answer here, not a failure.
func (s *SimilarityRecall) Recall(ctx context.Context, userID int64, question string) ([]model.LongTermMemory, error) {
	searchText := s.expandedQuestion(ctx, question)

	vector, err := s.embedder.EmbedQuery(ctx, searchText)
	if err != nil {
		s.stats.IndexFailures++
		return nil, errIndexUnavailable
	}
	semantic, err := s.index.SearchByVector(ctx, userID, vector, s.config.CandidateDepth)
	if err != nil {
		s.stats.IndexFailures++
		return nil, errIndexUnavailable
	}

	// Lexical search runs before the gate now, not after it. The blind run
	// showed why: with the gate first, a query the keyword channel could have
	// answered was discarded on a weak vector score alone.
	lexical, err := s.index.SearchByText(ctx, userID, rag.ExtractLexicalTerms(searchText), s.config.CandidateDepth)
	if err != nil {
		// The semantic channel already produced usable candidates, so a lexical
		// failure degrades quality rather than the answer.
		s.stats.IndexFailures++
		lexical = nil
	}

	if s.shouldAbstain(semantic, lexical) {
		s.stats.AbstainedByFloor++
		return nil, nil
	}

	ordered := fuseByRank(semantic, lexical, s.config.MaxResults)
	if len(ordered) == 0 {
		return nil, nil
	}
	memories, err := s.loader.LoadByIDs(ctx, userID, ordered)
	if err != nil {
		return nil, fmt.Errorf("load recalled memories: %w", err)
	}
	return s.order(memories, ordered, userID), nil
}

// shouldAbstain answers the only question the old path never asked: is the best
// candidate actually evidence of anything?
func (s *SimilarityRecall) shouldAbstain(semantic, lexical []memorydao.MemorySearchResult) bool {
	if len(semantic) == 0 {
		return true
	}
	// The lexical channel is an independent opinion. When it puts the same
	// memory first, two mechanisms that fail differently have agreed, which is
	// stronger evidence than either similarity value on its own.
	if s.config.LexicalRescue && len(lexical) > 0 && lexical[0].MemoryID == semantic[0].MemoryID {
		return false
	}
	if s.config.Rule == AbstainByStandout {
		return standoutRatio(semantic) < s.config.MinStandout
	}
	return semantic[0].Similarity() < s.config.MinSimilarity
}

// standoutRatio divides the best candidate's similarity by the mean over the
// retrieved set.
//
// Both terms move together when the corpus grows, so the ratio absorbs most of
// the drift that the numerator alone suffers. It does not remove it: the drift
// is an upward shift rather than a scaling, and only scaling cancels exactly.
// Measured across pool sizes the residual is 4.2% against 11.5% for the raw
// similarity, which is the whole claim - smaller, not zero.
//
// The mean is taken over the candidates actually returned rather than over the
// whole corpus. Using the corpus mean scores slightly better in the abstract
// but is not implementable: RediSearch returns the top K and nothing else, and
// scanning every memory to compute a gate would cost more than the retrieval it
// guards.
func standoutRatio(semantic []memorydao.MemorySearchResult) float64 {
	if len(semantic) == 0 {
		return 0
	}
	total := 0.0
	for _, hit := range semantic {
		total += hit.Similarity()
	}
	mean := total / float64(len(semantic))
	if mean <= 0 {
		return 0
	}
	return semantic[0].Similarity() / mean
}

// CandidateTopics is what retrieval is for after AGENT-021.
//
// It returns topic keys, ordered by how much of the candidate set falls under
// each, and it is used to shorten the catalogue offered to the model — not to
// decide anything. The distinction matters because it changes what the method
// has to be good at. Picking the one right memory demanded precision and
// measured 0.16 on a blind set; naming which topics are worth showing demands
// recall, and the same engine put the right topic in its top five 72% of the
// time.
//
// A learner with few topics does not need this at all: the whole catalogue
// fits in the prompt, and a shortlist could only lose the right answer. Call it
// when the list has outgrown its budget, not by default.
func (s *SimilarityRecall) CandidateTopics(ctx context.Context, userID int64, question string, limit int) ([]string, error) {
	if limit < 1 {
		return nil, fmt.Errorf("candidate topic limit must be positive")
	}
	searchText := s.expandedQuestion(ctx, question)
	vector, err := s.embedder.EmbedQuery(ctx, searchText)
	if err != nil {
		s.stats.IndexFailures++
		return nil, errIndexUnavailable
	}
	semantic, err := s.index.SearchByVector(ctx, userID, vector, s.config.CandidateDepth)
	if err != nil {
		s.stats.IndexFailures++
		return nil, errIndexUnavailable
	}
	lexical, err := s.index.SearchByText(ctx, userID, rag.ExtractLexicalTerms(searchText), s.config.CandidateDepth)
	if err != nil {
		s.stats.IndexFailures++
		lexical = nil
	}

	// No abstention gate here. Dropping a topic from a shortlist is not the
	// same act as answering with nothing: the model still sees the rest of the
	// catalogue and can decline on its own, so a gate would only remove options
	// it might have wanted.
	ranked := fuseByRank(semantic, lexical, s.config.CandidateDepth)
	if len(ranked) == 0 {
		return nil, nil
	}
	memories, err := s.loader.LoadByIDs(ctx, userID, ranked)
	if err != nil {
		return nil, fmt.Errorf("load candidate memories: %w", err)
	}

	// A topic is scored by the best rank any of its notes reached. Summing
	// instead would let a topic with many mediocre notes outrank one whose
	// single note matched exactly, which is the failure that sank the
	// topic-centroid method in EXP-002B: aggregating over a topic's contents
	// flattens the very signal that identifies it.
	byID := make(map[string]int, len(ranked))
	for position, id := range ranked {
		byID[id] = position
	}
	best := map[string]int{}
	for _, memory := range memories {
		key := strings.TrimSpace(memory.TopicKey)
		if key == "" || memory.UserID != userID {
			continue
		}
		position, ok := byID[memory.ID]
		if !ok {
			continue
		}
		if current, seen := best[key]; !seen || position < current {
			best[key] = position
		}
	}
	keys := make([]string, 0, len(best))
	for key := range best {
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if best[keys[i]] != best[keys[j]] {
			return best[keys[i]] < best[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > limit {
		keys = keys[:limit]
	}
	return keys, nil
}

// expandedQuestion never returns an error: expansion is an enhancement on the
// request path, and a provider hiccup must not cost the user their recall.
func (s *SimilarityRecall) expandedQuestion(ctx context.Context, question string) string {
	if s.expander == nil {
		return question
	}
	expansion, err := s.expander.Expand(ctx, question)
	if err != nil || strings.TrimSpace(expansion) == "" {
		s.stats.ExpansionFailures++
		return question
	}
	// The original question stays in front. Expansion is there to add recall,
	// not to replace what the user actually asked.
	return question + " " + expansion
}

// order re-applies the guarantees the index cannot make.
//
// The rank from fusion decides which memories are selected, because that is
// what B1 measured. Within the selected set, a memory the user confirmed is
// presented first: AGENT-012 protects user corrections from being overwritten,
// and it would be inconsistent to then bury them behind an inference.
func (s *SimilarityRecall) order(memories []model.LongTermMemory, ranked []string, userID int64) []model.LongTermMemory {
	position := make(map[string]int, len(ranked))
	for index, id := range ranked {
		position[id] = index
	}
	kept := make([]model.LongTermMemory, 0, len(memories))
	for _, memory := range memories {
		// Second gate. RediSearch already filtered on user, status and type;
		// this repeats the check on the authoritative rows, so an index that is
		// stale or wrongly built cannot leak another user's memory.
		if memory.UserID != userID || memory.Status != model.LongTermMemoryActive ||
			memory.Scope != model.MemoryScopeTopic || memory.Confidence < s.config.MinConfidence {
			continue
		}
		if _, selected := position[memory.ID]; !selected {
			continue
		}
		kept = append(kept, memory)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].UserConfirmed != kept[j].UserConfirmed {
			return kept[i].UserConfirmed
		}
		return position[kept[i].ID] < position[kept[j].ID]
	})
	return kept
}

// fuseByRank combines the two channels by reciprocal rank.
//
// The knowledge base side does the same thing in rag.FuseRRF with the same
// constant, but that function is typed to chunk results and reusing it here
// would mean either converting types back and forth or widening a component
// that has been stable since RAG-010. The rule it encodes is one line: never
// add a cosine distance to a BM25 score, because they are not on the same
// scale and the sum is not explainable afterwards.
const rrfRankConstant = 60.0

func fuseByRank(semantic, lexical []memorydao.MemorySearchResult, limit int) []string {
	score := make(map[string]float64, len(semantic)+len(lexical))
	for _, hit := range semantic {
		score[hit.MemoryID] += 1 / (rrfRankConstant + float64(hit.Rank+1))
	}
	for _, hit := range lexical {
		score[hit.MemoryID] += 1 / (rrfRankConstant + float64(hit.Rank+1))
	}
	ids := make([]string, 0, len(score))
	for id := range score {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		if score[ids[i]] != score[ids[j]] {
			return score[ids[i]] > score[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids
}
