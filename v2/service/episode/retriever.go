package episode

import (
	"GopherAI/common/memoryrecall"
	"GopherAI/common/observability"
	"GopherAI/common/rag"
	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

type EpisodeSearcher interface {
	SearchByVector(context.Context, int64, string, []float32, int) ([]episodedao.EpisodeSearchResult, error)
	SearchByText(context.Context, int64, string, []string, int) ([]episodedao.EpisodeSearchResult, error)
	SearchExactTopics(context.Context, int64, string, []string, int) ([]episodedao.EpisodeSearchResult, error)
}
type EpisodeLoader interface {
	LoadCandidates(context.Context, int64, string, []string) ([]model.EpisodeMemory, error)
	ListByTime(context.Context, int64, string, time.Time, time.Time, int) ([]model.EpisodeMemory, error)
}
type EpisodeQueryEmbedder interface {
	EmbedQuery(context.Context, string) ([]float32, error)
}

type RecallRequest struct {
	UserID                   int64
	SessionID, Question      string
	CurrentSessionHasHistory bool
	Now                      time.Time
	// ProposedTopicKeys are TopicKeys a resolver already confirmed for this
	// user against the MySQL catalogue. They widen the exact channel; they
	// never bypass the reload that follows it.
	ProposedTopicKeys []string
	// Cycle is the request-scoped coordination point. When it is present the
	// query vector computed here is the same one the TopicKey resolver uses,
	// so a mixed question costs one embedding call rather than two.
	Cycle *memoryrecall.Cycle
}
type CandidateTrace struct {
	EpisodeID, Channels                string
	VectorRank, LexicalRank, ExactRank int
}
type ShadowTrace struct {
	Intent          QueryIntent
	TemporalMode    TemporalMode
	EvidenceMode    EvidenceMode
	Abstained       bool
	Reason          string
	ReasonCodes     []string
	Candidates      []CandidateTrace
	TopicKeys       []string
	ElapsedMS       int64
	EstimatedTokens int
}
type RecallResult struct {
	Episodes []model.EpisodeMemory
	Trace    ShadowTrace
}

type RetrieverConfig struct{ CandidateDepth, MaxResults int }

func DefaultRetrieverConfig() RetrieverConfig {
	return RetrieverConfig{CandidateDepth: 20, MaxResults: 5}
}

type Retriever struct {
	router   *QueryRouter
	search   EpisodeSearcher
	loader   EpisodeLoader
	embedder EpisodeQueryEmbedder
	config   RetrieverConfig
}

func NewRetriever(router *QueryRouter, search EpisodeSearcher, loader EpisodeLoader, embedder EpisodeQueryEmbedder, config RetrieverConfig) (*Retriever, error) {
	if router == nil || search == nil || loader == nil || config.CandidateDepth < config.MaxResults || config.MaxResults < 1 || config.MaxResults > 5 {
		return nil, fmt.Errorf("invalid episode retriever configuration")
	}
	return &Retriever{router: router, search: search, loader: loader, embedder: embedder, config: config}, nil
}

func (r *Retriever) Recall(ctx context.Context, request RecallRequest) (result RecallResult, err error) {
	started := time.Now()
	plan := r.router.Plan(RouteRequest{
		Question: request.Question, Now: request.Now,
		CurrentSessionHasHistory: request.CurrentSessionHasHistory,
		ProposedTopicKeys:        request.ProposedTopicKeys,
	})
	result = RecallResult{Episodes: []model.EpisodeMemory{}, Trace: ShadowTrace{
		Intent: legacyIntent(plan), TemporalMode: plan.TemporalMode, EvidenceMode: plan.EvidenceMode,
		Reason: plan.Reason(), ReasonCodes: plan.ReasonCodes,
	}}
	defer func() {
		duration := time.Since(started)
		result.Trace.ElapsedMS = duration.Milliseconds()
		keys := make([]string, 0, len(result.Episodes))
		for _, episode := range result.Episodes {
			result.Trace.EstimatedTokens += rag.EstimateTokens(episode.SearchText)
			keys = append(keys, episode.TopicKeys...)
		}
		result.Trace.TopicKeys = keys
		// The keys of the episodes that survived the reload are authoritative
		// for the topic resolver that runs after this one.
		request.Cycle.SetEpisodeTopicKeys(keys)
		channels := make([]string, 0, len(result.Trace.Candidates))
		for _, candidate := range result.Trace.Candidates {
			channels = append(channels, candidate.Channels)
		}
		reason := result.Trace.Reason
		if err != nil {
			reason = "provider_error"
		}
		observability.Memory.Retrieval(string(result.Trace.Intent), channels, len(result.Episodes), reason, duration)
	}()
	if request.UserID <= 0 || request.SessionID == "" {
		return result, fmt.Errorf("episode recall identity required")
	}
	if !plan.NeedEpisode {
		result.Trace.Abstained = true
		return result, nil
	}
	if plan.TemporalMode == TemporalLatestAny || plan.TemporalMode == TemporalRange {
		return r.recallByTime(ctx, request, plan, result)
	}
	return r.recallByRelevance(ctx, request, plan, result)
}

// recallByTime answers the two questions whose primary key is time: the most
// recent other session, and an explicit date or period.
//
// The evidence filter is applied after the reload, which is why the fetch is
// deepened when one is active: the newest session need not be the newest
// session that recorded a decision.
func (r *Retriever) recallByTime(ctx context.Context, request RecallRequest, plan MemoryReadPlan, result RecallResult) (RecallResult, error) {
	limit := r.config.MaxResults
	switch {
	case plan.TemporalMode == TemporalLatestAny && plan.EvidenceMode == EvidenceAny:
		limit = 1
	case plan.TemporalMode == TemporalLatestAny:
		limit = r.config.CandidateDepth
	case plan.HasTopicConstraint():
		limit = r.config.CandidateDepth
	}
	rows, err := r.loader.ListByTime(ctx, request.UserID, request.SessionID, plan.From, plan.To, limit)
	if err != nil {
		return result, err
	}
	rows = filterByEvidence(rows, plan.EvidenceMode)
	if plan.TemporalMode == TemporalRange {
		rows = narrowRangeByTopic(rows, plan)
	}
	if plan.TemporalMode == TemporalLatestAny && len(rows) > 1 {
		rows = rows[:1]
	}
	if len(rows) > r.config.MaxResults {
		rows = rows[:r.config.MaxResults]
	}
	result.Episodes = rows
	result.Trace.Abstained = len(rows) == 0
	for _, row := range rows {
		result.Trace.Candidates = append(result.Trace.Candidates, CandidateTrace{EpisodeID: row.ID, Channels: "mysql_time"})
	}
	return result, nil
}

// narrowRangeByTopic applies the topic constraint a dated question may also
// carry.
//
// An explicitly named topic is a hard filter: "2026-08-25 关于迁移定了什么" is
// wrong to answer with that day's other session. A topic merely inferred from
// the remaining words is a preference: the words left over from "那天最后是
// 怎么收的" are not a subject, and treating them as one would empty a range
// query that the date alone had already answered correctly.
func narrowRangeByTopic(rows []model.EpisodeMemory, plan MemoryReadPlan) []model.EpisodeMemory {
	if len(rows) == 0 || !plan.HasTopicConstraint() {
		return rows
	}
	keys := append(append([]string{}, plan.ExactTopicKeys...), plan.ProposedTopicKeys...)
	matched := make([]model.EpisodeMemory, 0, len(rows))
	for _, row := range rows {
		if episodeMatchesTopic(row, keys) || episodeMatchesTerms(row, plan.TopicTerms) {
			matched = append(matched, row)
		}
	}
	if len(matched) > 0 {
		return matched
	}
	if plan.RequireExactTopic || len(plan.ExactIdentifiers) > 0 {
		return nil
	}
	return rows
}

// recallByRelevance is the topic-constrained path, including "last time, about
// X". Exact topics and strong identifiers are preferred but never final: an
// exact miss records its reason and continues into the constrained fallback
// instead of ending the read, which is where five of the twenty episodic cases
// used to stop.
func (r *Retriever) recallByRelevance(ctx context.Context, request RecallRequest, plan MemoryReadPlan, result RecallResult) (RecallResult, error) {
	topicKeys := append(append([]string{}, plan.ExactTopicKeys...), plan.ProposedTopicKeys...)
	exact, exactErr := r.search.SearchExactTopics(ctx, request.UserID, request.SessionID, topicKeys, r.config.CandidateDepth)
	if len(plan.ExactIdentifiers) > 0 {
		identifierHits, identifierErr := r.search.SearchByText(ctx, request.UserID, request.SessionID, plan.ExactIdentifiers, r.config.CandidateDepth)
		exact = mergeSearchResults(exact, identifierHits)
		if exactErr == nil {
			exactErr = identifierErr
		}
	}
	lexical, lexicalErr := r.search.SearchByText(ctx, request.UserID, request.SessionID, plan.TopicTerms, r.config.CandidateDepth)
	if exactErr != nil && lexicalErr != nil {
		result.Trace.Abstained = true
		result.Trace.Reason = "redis_unavailable"
		result.Trace.ReasonCodes = append(result.Trace.ReasonCodes, "redis_unavailable")
		return result, nil
	}
	if len(exact) == 0 && plan.RequireExactTopic {
		result.Trace.ReasonCodes = append(result.Trace.ReasonCodes, "exact_topic_miss_fallback")
	}

	// Both channels run before fusion, because the fallback admits a candidate
	// only on an exact tag or on lexical and vector agreeing. Running one of
	// them and not the other does not save a call; it makes the surviving
	// channel dead weight, since a single-channel hit can never satisfy the
	// second half of the rule.
	var vectors []episodedao.EpisodeSearchResult
	if r.embedder != nil {
		vector, embedErr := request.Cycle.QueryVector(ctx, request.Question, r.embedder.EmbedQuery)
		if embedErr != nil {
			result.Trace.ReasonCodes = append(result.Trace.ReasonCodes, "embedding_unavailable")
		} else {
			vectors, _ = r.search.SearchByVector(ctx, request.UserID, request.SessionID, vector, r.config.CandidateDepth)
		}
	}
	ranked, traces := eligibleRRF(exact, lexical, vectors, r.config.CandidateDepth, r.corroborationDepth())
	result.Trace.Candidates = traces
	if len(ranked) == 0 {
		result.Trace.Abstained = true
		result.Trace.Reason = "no_eligible_candidate"
		result.Trace.ReasonCodes = append(result.Trace.ReasonCodes, "no_eligible_candidate")
		return result, nil
	}
	rows, err := r.loader.LoadCandidates(ctx, request.UserID, request.SessionID, ranked)
	if err != nil {
		return result, err
	}
	rows = filterByEvidence(rows, plan.EvidenceMode)
	if plan.TemporalMode == TemporalLatestMatching {
		// Recency ranks the relevant candidates; it does not select them. The
		// old router had this the other way round, which is how "上次关于 X"
		// returned whichever session happened to be newest.
		rows = sortByRecency(rows)
	}
	if len(rows) > r.config.MaxResults {
		rows = rows[:r.config.MaxResults]
	}
	result.Episodes = rows
	result.Trace.Abstained = len(rows) == 0
	if result.Trace.Abstained {
		result.Trace.Reason = "no_eligible_candidate"
		result.Trace.ReasonCodes = append(result.Trace.ReasonCodes, "mysql_reload_rejected")
	} else if len(exact) > 0 {
		result.Trace.ReasonCodes = append(result.Trace.ReasonCodes, "exact_topic_hit")
	} else {
		result.Trace.ReasonCodes = append(result.Trace.ReasonCodes, "lexical_vector_hit")
	}
	return result, nil
}

func legacyIntent(plan MemoryReadPlan) QueryIntent {
	switch {
	case !plan.NeedEpisode:
		return IntentNone
	case plan.TemporalMode == TemporalLatestAny:
		return IntentLastSession
	case plan.TemporalMode == TemporalRange:
		return IntentTimeRange
	case plan.EvidenceMode == EvidenceDecision:
		return IntentDecisionHistory
	default:
		return IntentTopicHistory
	}
}

// filterByEvidence applies the requested evidence type against the structured
// projection rather than against the search text. "决定" is a word that occurs
// in episodes that recorded no decision, and an episode that recorded one does
// not have to contain the word.
func filterByEvidence(rows []model.EpisodeMemory, mode EvidenceMode) []model.EpisodeMemory {
	if mode == EvidenceAny {
		return rows
	}
	kept := make([]model.EpisodeMemory, 0, len(rows))
	for _, row := range rows {
		switch mode {
		case EvidenceDecision:
			if len(row.Document.Decisions) == 0 {
				continue
			}
		case EvidenceCorrection:
			if len(row.Document.UserCorrections) == 0 {
				continue
			}
		case EvidenceOpenLoop:
			if len(row.Document.OpenLoops) == 0 {
				continue
			}
		}
		kept = append(kept, row)
	}
	return kept
}

func sortByRecency(rows []model.EpisodeMemory) []model.EpisodeMemory {
	ordered := append([]model.EpisodeMemory(nil), rows...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].LastMessageAt.Equal(ordered[j].LastMessageAt) {
			return ordered[i].LastMessageAt.After(ordered[j].LastMessageAt)
		}
		return ordered[i].ID < ordered[j].ID
	})
	return ordered
}

func episodeMatchesTerms(episode model.EpisodeMemory, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	haystack := strings.ToLower(episode.Title + " " + episode.SearchText + " " + strings.Join(episode.TopicKeys, " "))
	for _, term := range terms {
		if strings.Contains(haystack, strings.ToLower(strings.TrimSpace(term))) {
			return true
		}
	}
	return false
}

func episodeMatchesTopic(episode model.EpisodeMemory, keys []string) bool {
	for _, stored := range episode.TopicKeys {
		for _, key := range keys {
			if stored == key {
				return true
			}
		}
	}
	return false
}

func mergeSearchResults(left, right []episodedao.EpisodeSearchResult) []episodedao.EpisodeSearchResult {
	seen := map[string]struct{}{}
	result := make([]episodedao.EpisodeSearchResult, 0, len(left)+len(right))
	for _, items := range [][]episodedao.EpisodeSearchResult{left, right} {
		for _, item := range items {
			if _, ok := seen[item.EpisodeID]; ok {
				continue
			}
			seen[item.EpisodeID] = struct{}{}
			result = append(result, item)
		}
	}
	return result
}

type fusedEpisode struct {
	id                     string
	score                  float64
	exact, lexical, vector int
}

// corroborationDepth bounds how deep a vector hit may sit and still count as
// the lexical channel's second opinion.
//
// A nearest-neighbour search always returns its top K, and on a small index
// that is most of the corpus. Treating "the vector channel did not rule it out"
// as agreement would collapse the two-channel rule back into a lexical-only
// rule, which is how an unrelated session that happens to share one word gets
// admitted. A corroborating opinion has to be one of the channel's own better
// answers.
func (r *Retriever) corroborationDepth() int {
	depth := r.config.CandidateDepth / 2
	if depth < r.config.MaxResults {
		depth = r.config.MaxResults
	}
	return depth
}

func eligibleRRF(exact, lexical, vector []episodedao.EpisodeSearchResult, max, corroboration int) ([]string, []CandidateTrace) {
	byID := map[string]*fusedEpisode{}
	add := func(items []episodedao.EpisodeSearchResult, channel string) {
		for idx, item := range items {
			if item.EmbeddingVersion != episodedao.CurrentEpisodeEmbeddingVersion {
				continue
			}
			entry := byID[item.EpisodeID]
			if entry == nil {
				entry = &fusedEpisode{id: item.EpisodeID}
				byID[item.EpisodeID] = entry
			}
			rank := idx + 1
			entry.score += 1 / float64(60+rank)
			switch channel {
			case "exact":
				entry.exact = rank
			case "lexical":
				entry.lexical = rank
			case "vector":
				entry.vector = rank
			}
		}
	}
	add(exact, "exact")
	add(lexical, "lexical")
	add(vector, "vector")
	items := make([]fusedEpisode, 0, len(byID))
	for _, entry := range byID {
		// A vector-only candidate is never admitted. Nearest-neighbour search
		// always returns a nearest neighbour, so "closest thing in the index"
		// is not evidence that the learner asked about it.
		if entry.exact > 0 || (entry.lexical > 0 && entry.vector > 0 && entry.vector <= corroboration) {
			items = append(items, *entry)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].exact > 0 && items[j].exact == 0 {
			return true
		}
		if items[j].exact > 0 && items[i].exact == 0 {
			return false
		}
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].id < items[j].id
	})
	if len(items) > max {
		items = items[:max]
	}
	ids := make([]string, 0, len(items))
	traces := make([]CandidateTrace, 0, len(items))
	for _, item := range items {
		channels := ""
		if item.exact > 0 {
			channels = "exact"
		}
		if item.lexical > 0 {
			if channels != "" {
				channels += "+"
			}
			channels += "lexical"
		}
		if item.vector > 0 {
			if channels != "" {
				channels += "+"
			}
			channels += "vector"
		}
		ids = append(ids, item.id)
		traces = append(traces, CandidateTrace{EpisodeID: item.id, Channels: channels, ExactRank: item.exact, LexicalRank: item.lexical, VectorRank: item.vector})
	}
	return ids, traces
}
