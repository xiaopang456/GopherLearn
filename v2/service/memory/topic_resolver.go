package memory

import (
	"GopherAI/common/memoryrecall"
	"GopherAI/common/rag"
	"GopherAI/common/topicmatch"
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"fmt"
	"sort"
	"strings"
)

// TopicResolver answers one question and only one: which of this learner's
// topics is being asked about.
//
// It is deliberately not a memory retriever. AGENT-020's hybrid path injected
// whatever the vector index ranked first, which recalled more and was measured
// unsafe: an absolute cosine floor drifts upward as the memory pile grows (0.56
// at a hundred memories, 0.63 at five hundred), so the same query walks out
// from under any fixed threshold, and everything under it is personal history
// about a subject the learner did not raise. Here the index proposes a
// TopicKey, nothing else. The content still comes from the MySQL rows filed
// under that key, through the same active/scope/confidence/expiry predicate the
// lexical path uses, so a stale or wrongly built index can change which topic
// is chosen and can never change what a topic contains.
//
// Evidence is layered, strongest first:
//
//  1. authoritative — server-derived keys (active quiz, matched mastery, the
//     TopicKeys of episodes that already survived the MySQL reload)
//  2. lexical      — the learner reproduced enough of the topic's own name
//  3. semantic     — vector and BM25 over this user's active topic memories,
//     proposing keys only, and only when the first two layers found nothing
type TopicResolver struct {
	catalogue TopicKeyCatalogue
	index     MemoryVectorSearcher
	embedder  QueryEmbedder
	loader    MemoryLoader
	config    TopicResolverConfig
}

// TopicKeyCatalogue is the MySQL side: the topics this user actually has.
type TopicKeyCatalogue interface {
	ListTopicKeys(context.Context, int64, int) ([]memorydao.TopicKeyRow, error)
}

type TopicResolverConfig struct {
	// CandidateDepth is how deep each channel searches before aggregation.
	CandidateDepth int
	// CatalogueLimit bounds the MySQL topic listing.
	CatalogueLimit int
	// MaxProposed caps how many semantic candidates are reported. Only the
	// first is ever accepted; the rest exist so the diagnostics can show what
	// the gate turned down.
	MaxProposed int
	// MinStandout and MinMargin are the frozen development gate. They are
	// relative quantities on purpose: both terms move together as the memory
	// pile grows, which an absolute cosine floor does not.
	MinStandout float64
	MinMargin   float64
	// MinConfidence repeats the recall predicate on the authoritative rows.
	MinConfidence float64
}

// DefaultTopicResolverConfig carries only the structural bounds.
//
// MinStandout and MinMargin are deliberately left at zero: the STABILIZE-030
// development gate failed at every point of the frozen grid, so there is no
// validated value to ship. A number left in a Default*Config() is a
// production default whatever the comment above it says — that is exactly how
// BUG-AGENT-140 happened — so the semantic constructor demands both explicitly
// and refuses to build without them.
func DefaultTopicResolverConfig() TopicResolverConfig {
	return TopicResolverConfig{
		CandidateDepth: 20, CatalogueLimit: 200, MaxProposed: 3, MinConfidence: 0.6,
	}
}

func (c TopicResolverConfig) Validate() error {
	if c.CandidateDepth < 1 || c.CandidateDepth > 100 {
		return fmt.Errorf("topic resolver candidate depth must be between 1 and 100")
	}
	if c.CatalogueLimit < 1 || c.CatalogueLimit > 200 {
		return fmt.Errorf("topic resolver catalogue limit must be between 1 and 200")
	}
	if c.MaxProposed < 1 || c.MaxProposed > 5 {
		return fmt.Errorf("topic resolver may propose between 1 and 5 keys")
	}
	if c.MinConfidence < 0 || c.MinConfidence > 1 {
		return fmt.Errorf("topic resolver confidence floor must be between 0 and 1")
	}
	return nil
}

// ValidateSemanticGate is checked only by the semantic constructor, because
// only that mode can use these numbers.
//
// Below one the "best" candidate is worse than the average of its own batch,
// which cannot be evidence of anything; the upper bounds are typo guards.
//
// A margin of exactly zero is allowed *here* and refused by configuration. The
// distinction is deliberate: only the configuration layer can tell "the operator
// chose zero" from "nobody filled the field in", because only it knows what an
// absent TOML key looks like. Refusing zero in the library as well would also
// forbid the widest-gate recording an offline sweep needs, which is a legitimate
// measurement rather than a production setting.
func (c TopicResolverConfig) ValidateSemanticGate() error {
	if c.MinStandout < 1 || c.MinStandout > 3 {
		return fmt.Errorf("semantic topic gate needs an explicit standout between 1 and 3, got %v", c.MinStandout)
	}
	if c.MinMargin < 0 || c.MinMargin > 0.5 {
		return fmt.Errorf("semantic topic gate margin must be between 0 and 0.5, got %v", c.MinMargin)
	}
	return nil
}

// TopicKeyCandidate is one aggregated semantic candidate. It is reported even
// when the gate rejects it, so the offline sweep can re-apply any gate in the
// frozen grid without searching again.
type TopicKeyCandidate struct {
	TopicKey        string  `json:"topic_key"`
	BestVectorRank  int     `json:"best_vector_rank"`
	BestLexicalRank int     `json:"best_lexical_rank"`
	BestSimilarity  float64 `json:"best_similarity"`
	InCatalogue     bool    `json:"in_catalogue"`
}

// TopicResolution is what the resolver decided plus everything it saw.
type TopicResolution struct {
	// Keys are the accepted TopicKeys, authoritative and lexical first.
	Keys []string `json:"keys"`
	// SemanticKey is the single key the semantic layer contributed, if any.
	SemanticKey string `json:"semantic_key,omitempty"`
	// Candidates are the semantic layer's aggregated proposals, ranked.
	Candidates []TopicKeyCandidate `json:"candidates,omitempty"`
	// MeanSimilarity is the mean over the retrieved vector candidate set, the
	// denominator of the standout ratio.
	MeanSimilarity float64  `json:"mean_similarity,omitempty"`
	Standout       float64  `json:"standout,omitempty"`
	Margin         float64  `json:"margin,omitempty"`
	Agreement      bool     `json:"agreement,omitempty"`
	ReasonCodes    []string `json:"reason_codes,omitempty"`
	EmbeddingCalls int      `json:"embedding_calls,omitempty"`
}

type TopicResolveRequest struct {
	UserID int64
	// Question is the learner's raw text. It is never expanded: expansion costs
	// a model call on the request path and invents subjects the learner did not
	// mention, and EXP-005 measured its planner variant at a 1.6s P95 for a
	// benefit that did not survive.
	Question string
	// AuthoritativeKeys are server-derived. They are accepted without any
	// semantic confirmation because the server, not a ranker, produced them.
	AuthoritativeKeys []string
	// Signals are what the read plan already derived from this question: the
	// de-shelled topic terms and the strong identifiers the learner spelled
	// out. They are passed in rather than re-derived so the resolver and the
	// plan cannot disagree about the same sentence.
	//
	// A nil value means the caller did not compute them, and the two rules that
	// depend on them are skipped rather than applied to an empty guess.
	Signals *TopicSignals
	Cycle   *memoryrecall.Cycle
}

// TopicSignals mirrors the read plan's derived signals. It is a plain value so
// that this package does not depend on the episode package.
type TopicSignals struct {
	TopicTerms  []string
	Identifiers []string
}

// NewLexicalTopicResolver builds the resolver for episode_v3_lexical.
//
// It takes no index, no embedder and no loader, so the semantic layer is not
// constructed rather than constructed and skipped. The design is explicit
// about the difference: something that exists gets called eventually.
func NewLexicalTopicResolver(catalogue TopicKeyCatalogue, config TopicResolverConfig) (*TopicResolver, error) {
	if catalogue == nil {
		return nil, fmt.Errorf("topic resolver requires a MySQL topic catalogue")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &TopicResolver{catalogue: catalogue, config: config}, nil
}

// NewSemanticTopicResolver builds the resolver for episode_v3_semantic. Every
// dependency and both gate numbers are required; there is no default.
func NewSemanticTopicResolver(catalogue TopicKeyCatalogue, index MemoryVectorSearcher, embedder QueryEmbedder, loader MemoryLoader, config TopicResolverConfig) (*TopicResolver, error) {
	if catalogue == nil {
		return nil, fmt.Errorf("topic resolver requires a MySQL topic catalogue")
	}
	if index == nil || embedder == nil || loader == nil {
		return nil, fmt.Errorf("semantic topic resolver requires an index, an embedder and a loader")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if err := config.ValidateSemanticGate(); err != nil {
		return nil, err
	}
	return &TopicResolver{catalogue: catalogue, index: index, embedder: embedder, loader: loader, config: config}, nil
}

// Resolve runs the three layers in order and stops as soon as one covers the
// question.
//
// It never returns an error: a resolver failure means "no implicit topic", not
// "no answer". Degrading to the lexical result is the whole reason the semantic
// layer is allowed to exist at all.
func (r *TopicResolver) Resolve(ctx context.Context, request TopicResolveRequest) TopicResolution {
	resolution := TopicResolution{}
	if r == nil || request.UserID <= 0 || strings.TrimSpace(request.Question) == "" {
		return resolution
	}

	authoritative := normalizeStrongTopicKeys(append(append([]string{}, request.AuthoritativeKeys...), request.Cycle.EpisodeTopicKeys()...))
	rows, err := r.catalogue.ListTopicKeys(ctx, request.UserID, r.config.CatalogueLimit)
	if err != nil {
		resolution.ReasonCodes = append(resolution.ReasonCodes, "topic_catalogue_unavailable")
		resolution.Keys = authoritative
		return resolution
	}
	catalogue := make(map[string]memorydao.TopicKeyRow, len(rows))
	for _, row := range rows {
		catalogue[row.TopicKey] = row
	}

	keys := append([]string{}, authoritative...)
	if len(authoritative) > 0 {
		resolution.ReasonCodes = append(resolution.ReasonCodes, "strong_topic")
	}
	prepared := topicmatch.NewQuestion(request.Question)
	lexicalFound := false
	for _, row := range rows {
		if matchesTopic(prepared, row.TopicKey, row.Topic) {
			keys = append(keys, row.TopicKey)
			lexicalFound = true
		}
	}
	if lexicalFound {
		resolution.ReasonCodes = append(resolution.ReasonCodes, "lexical_topic")
	}
	keys = normalizeStrongTopicKeys(keys)
	resolution.Keys = keys
	if len(keys) > 0 {
		// Layer three is skipped entirely once the question is covered. That is
		// what keeps the added embedding call off every turn whose topic the
		// server or the learner's own wording already named.
		return resolution
	}

	// A question with no lexical content has no subject to resolve. Asking the
	// provider anyway would spend a call to rank the whole index against a
	// greeting.
	if len(rag.ExtractLexicalTerms(request.Question)) == 0 {
		resolution.ReasonCodes = append(resolution.ReasonCodes, "no_topic_signal")
		return resolution
	}
	// STABILIZE-031 5.3: a question that is all shell names no subject either.
	// "上次聊了什么" used to reach the semantic layer, buy an embedding and
	// possibly propose a key — that is the system deciding a topic the learner
	// never mentioned.
	if request.Signals != nil && len(request.Signals.TopicTerms) == 0 {
		resolution.ReasonCodes = append(resolution.ReasonCodes, "no_topic_after_shell_strip")
		return resolution
	}
	if reused, ok := request.Cycle.ProposedTopicKeys(); ok {
		// The semantic layer already ran for this request. Running it again
		// would ask the same provider the same question and reach the same
		// answer at twice the cost.
		resolution.ReasonCodes = append(resolution.ReasonCodes, "semantic_topic_reused")
		resolution.Keys = reused
		return resolution
	}
	semantic := r.resolveSemantic(ctx, request, catalogue)
	semantic.ReasonCodes = append(resolution.ReasonCodes, semantic.ReasonCodes...)
	request.Cycle.SetProposedTopicKeys(semantic.Keys)
	return semantic
}

// identifierNegativeEvidence is STABILIZE-031 5.2.
//
// standout and margin both ask "how good is this candidate relative to the
// batch it arrived with". This asks something else entirely: the learner
// spelled out a name — is it in this candidate at all? Writing "PostgreSQL"
// is a strong positive statement about what is wanted; a topic that does not
// contain it is not that thing however high it ranks. It introduces no
// threshold, which is why it can sit beside two relative gates without
// reintroducing the absolute-cosine problem.
//
// Its reach is bounded by what counts as a strong identifier: "UDP" is three
// characters and does not qualify, so that shape is not protected here.
// Widening the identifier bar is a separate change that needs its own
// measurement.
func identifierNegativeEvidence(signals *TopicSignals, key string, row memorydao.TopicKeyRow) bool {
	if signals == nil || len(signals.Identifiers) == 0 {
		return false
	}
	haystack := strings.ToLower(key + " " + row.Topic)
	for _, identifier := range signals.Identifiers {
		if strings.Contains(haystack, strings.ToLower(strings.TrimSpace(identifier))) {
			return false
		}
	}
	return true
}

func (r *TopicResolver) resolveSemantic(ctx context.Context, request TopicResolveRequest, catalogue map[string]memorydao.TopicKeyRow) TopicResolution {
	resolution := TopicResolution{}
	if r.index == nil || r.embedder == nil || r.loader == nil {
		resolution.ReasonCodes = append(resolution.ReasonCodes, "semantic_topic_disabled")
		return resolution
	}
	vector, err := request.Cycle.QueryVector(ctx, request.Question, r.embedder.EmbedQuery)
	if err != nil {
		resolution.ReasonCodes = append(resolution.ReasonCodes, "embedding_unavailable")
		return resolution
	}
	semantic, err := r.index.SearchByVector(ctx, request.UserID, vector, r.config.CandidateDepth)
	if err != nil {
		resolution.ReasonCodes = append(resolution.ReasonCodes, "redis_unavailable")
		return resolution
	}
	if len(semantic) == 0 {
		resolution.ReasonCodes = append(resolution.ReasonCodes, "no_topic_candidate")
		return resolution
	}
	lexical, lexicalErr := r.index.SearchByText(ctx, request.UserID, rag.ExtractLexicalTerms(request.Question), r.config.CandidateDepth)
	if lexicalErr != nil {
		// One channel down means no agreement route, not no answer: the
		// standout gate can still decide on its own.
		resolution.ReasonCodes = append(resolution.ReasonCodes, "lexical_channel_unavailable")
		lexical = nil
	}

	ids := make([]string, 0, len(semantic)+len(lexical))
	seen := make(map[string]struct{}, len(semantic)+len(lexical))
	for _, group := range [][]memorydao.MemorySearchResult{semantic, lexical} {
		for _, hit := range group {
			if _, duplicate := seen[hit.MemoryID]; duplicate {
				continue
			}
			seen[hit.MemoryID] = struct{}{}
			ids = append(ids, hit.MemoryID)
		}
	}
	memories, err := r.loader.LoadByIDs(ctx, request.UserID, ids)
	if err != nil {
		resolution.ReasonCodes = append(resolution.ReasonCodes, "mysql_reload_rejected")
		return resolution
	}
	// Second gate on the authoritative rows. RediSearch already filtered on
	// user, status and scope; repeating it here is what stops a stale or
	// wrongly built index from proposing another account's topic.
	topicOf := make(map[string]string, len(memories))
	for _, memory := range memories {
		if memory.UserID != request.UserID || memory.Status != model.LongTermMemoryActive ||
			memory.Scope != model.MemoryScopeTopic || memory.Confidence < r.config.MinConfidence {
			continue
		}
		if key := strings.TrimSpace(memory.TopicKey); key != "" {
			topicOf[memory.ID] = key
		}
	}

	similarity := make(map[string]float64, len(semantic))
	total := 0.0
	for _, hit := range semantic {
		similarity[hit.MemoryID] = hit.Similarity()
		total += hit.Similarity()
	}
	resolution.MeanSimilarity = total / float64(len(semantic))

	// A topic scores by the best rank any one of its notes reached. Summing
	// would let a topic with many mediocre notes outrank one whose single note
	// matched exactly, which is the aggregation that sank the topic-centroid
	// method in EXP-002B.
	byKey := map[string]*TopicKeyCandidate{}
	entry := func(key string) *TopicKeyCandidate {
		if existing, ok := byKey[key]; ok {
			return existing
		}
		_, inCatalogue := catalogue[key]
		created := &TopicKeyCandidate{TopicKey: key, InCatalogue: inCatalogue}
		byKey[key] = created
		return created
	}
	for index, hit := range semantic {
		key, ok := topicOf[hit.MemoryID]
		if !ok {
			continue
		}
		item := entry(key)
		if item.BestVectorRank == 0 || index+1 < item.BestVectorRank {
			item.BestVectorRank = index + 1
			item.BestSimilarity = similarity[hit.MemoryID]
		}
	}
	for index, hit := range lexical {
		key, ok := topicOf[hit.MemoryID]
		if !ok {
			continue
		}
		item := entry(key)
		if item.BestLexicalRank == 0 || index+1 < item.BestLexicalRank {
			item.BestLexicalRank = index + 1
		}
	}

	ranked := make([]TopicKeyCandidate, 0, len(byKey))
	for _, item := range byKey {
		if item.BestVectorRank == 0 {
			continue
		}
		ranked = append(ranked, *item)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].BestVectorRank != ranked[j].BestVectorRank {
			return ranked[i].BestVectorRank < ranked[j].BestVectorRank
		}
		return ranked[i].TopicKey < ranked[j].TopicKey
	})
	if len(ranked) > r.config.MaxProposed {
		ranked = ranked[:r.config.MaxProposed]
	}
	resolution.Candidates = ranked
	if len(ranked) == 0 {
		resolution.ReasonCodes = append(resolution.ReasonCodes, "no_topic_candidate")
		return resolution
	}

	best := ranked[0]
	bestLexical := bestLexicalKey(byKey)
	resolution.Agreement = bestLexical != "" && bestLexical == best.TopicKey
	resolution.Standout = 0
	if resolution.MeanSimilarity > 0 {
		resolution.Standout = best.BestSimilarity / resolution.MeanSimilarity
	}
	// Margin is the gap to the next *topic*, not to the next memory: two notes
	// under one key are not competitors. With a single candidate there is
	// nothing to be ambiguous with, so the margin stays zero and the gate skips
	// it rather than falling back to an absolute similarity, which is the
	// quantity that drifts with pool size.
	if len(ranked) > 1 {
		resolution.Margin = best.BestSimilarity - ranked[1].BestSimilarity
	}

	accepted, reason := AcceptSemanticTopic(resolution, r.config.MinStandout, r.config.MinMargin)
	resolution.ReasonCodes = append(resolution.ReasonCodes, reason)
	if accepted && identifierNegativeEvidence(request.Signals, best.TopicKey, catalogue[best.TopicKey]) {
		resolution.ReasonCodes = append(resolution.ReasonCodes, "identifier_not_in_topic_abstain")
		return resolution
	}
	if !accepted || !best.InCatalogue {
		if accepted && !best.InCatalogue {
			// The index proposed a key MySQL no longer lists as active. The
			// authoritative catalogue wins.
			resolution.ReasonCodes = append(resolution.ReasonCodes, "topic_not_in_catalogue")
		}
		return resolution
	}
	resolution.SemanticKey = best.TopicKey
	resolution.Keys = []string{best.TopicKey}
	return resolution
}

// AcceptSemanticTopic is the frozen gate, exported so the offline sweep applies
// exactly the rule the service applies rather than a copy of it.
//
// Two independent routes, both relative: the two channels naming the same topic
// is agreement between mechanisms that fail differently, and standout plus
// margin ask whether the best candidate stands out from the batch it arrived
// with rather than whether it cleared a number chosen against a different
// corpus size.
func AcceptSemanticTopic(resolution TopicResolution, minStandout, minMargin float64) (bool, string) {
	if len(resolution.Candidates) == 0 {
		return false, "no_topic_candidate"
	}
	if resolution.Agreement {
		return true, "semantic_topic_agreement"
	}
	if resolution.Standout >= minStandout && (len(resolution.Candidates) == 1 || resolution.Margin >= minMargin) {
		return true, "semantic_topic_standout"
	}
	return false, "topic_ambiguous_abstain"
}

func bestLexicalKey(byKey map[string]*TopicKeyCandidate) string {
	best, bestRank := "", 0
	for key, item := range byKey {
		if item.BestLexicalRank == 0 {
			continue
		}
		if bestRank == 0 || item.BestLexicalRank < bestRank || (item.BestLexicalRank == bestRank && key < best) {
			best, bestRank = key, item.BestLexicalRank
		}
	}
	return best
}
