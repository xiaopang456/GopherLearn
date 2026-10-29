package memoryplan

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
	episodeservice "GopherAI/service/episode"
)

// CandidateKind separates the two memory types all the way to the Verifier, so
// a field reference can be checked against the thing it actually addresses.
type CandidateKind string

const (
	CandidateSemantic CandidateKind = "semantic"
	CandidateEpisode  CandidateKind = "episode"
)

// Candidate is one record that survived the reload, with the body the Verifier
// will judge.
type Candidate struct {
	ID   string
	Kind CandidateKind
	// Channels records how this candidate was reached, for attribution. A
	// candidate found only by the query channel and one found by an explicit id
	// are different evidence about whether the Planner needed the query at all.
	Channels []string

	Memory  model.LongTermMemory
	Episode model.EpisodeMemory
}

// TopicKey is the subject the candidate is filed under, for diagnostics.
func (c Candidate) TopicKey() string {
	if c.Kind == CandidateSemantic {
		return c.Memory.TopicKey
	}
	if len(c.Episode.TopicKeys) > 0 {
		return c.Episode.TopicKeys[0]
	}
	return ""
}

// EpisodeSearcher and EpisodeLoader are the two halves of the episode channel.
// They are the production types, not reimplementations: the reload that decides
// what is eligible has to be the same one the rest of the system uses.
type EpisodeSearcher = episodeservice.EpisodeSearcher

type EpisodeLoader interface {
	LoadCandidates(context.Context, int64, string, []string) ([]model.EpisodeMemory, error)
	ListByTime(context.Context, int64, string, time.Time, time.Time, int) ([]model.EpisodeMemory, error)
}

// QueryEmbedder turns the plan's episode query into a vector.
type QueryEmbedder interface {
	EmbedQuery(context.Context, string) ([]float32, error)
}

// TopicMemoryReader reloads topic memories by key.
type TopicMemoryReader interface {
	ListByTopicKey(context.Context, int64, model.MemoryScope, string, float64, int) ([]model.LongTermMemory, error)
}

// CandidateRequest is what one turn asks for.
type CandidateRequest struct {
	UserID        int64
	SessionID     string
	Question      string
	Now           time.Time
	Plan          MemoryReadPlan
	Catalogue     MemoryCatalogueSnapshot
	CandidateDeep int
}

// CandidateSet is the pool plus how it was reached.
type CandidateSet struct {
	Semantic []Candidate
	Episodes []Candidate

	ExplicitHits int
	ExactHits    int
	QueryHits    int
	LatestHits   int
	// ReloadRejections counts candidates the search channels proposed and the
	// MySQL reload refused. It is the number that says whether the Redis view
	// was stale, and it is reported separately from "the planner asked for
	// nothing".
	ReloadRejections int
	// Reasons are non-sensitive codes. They never carry user text.
	Reasons []string
}

// CandidateBuilder turns a validated plan into a reloaded candidate pool.
type CandidateBuilder struct {
	memories TopicMemoryReader
	search   EpisodeSearcher
	loader   EpisodeLoader
	embedder QueryEmbedder

	minConfidence float64
}

func NewCandidateBuilder(memories TopicMemoryReader, search EpisodeSearcher, loader EpisodeLoader,
	embedder QueryEmbedder, minConfidence float64) (*CandidateBuilder, error) {
	if memories == nil || loader == nil {
		return nil, fmt.Errorf("candidate builder needs a memory reader and an episode loader")
	}
	if minConfidence < 0 || minConfidence > 1 {
		return nil, fmt.Errorf("candidate minimum confidence must be between 0 and 1")
	}
	return &CandidateBuilder{memories: memories, search: search, loader: loader,
		embedder: embedder, minConfidence: minConfidence}, nil
}

// Build assembles the pool.
//
// The order of the four episode channels is fixed and the union is taken by
// candidate id, so the pool a question produces does not depend on which
// channel happened to be fast. That matters twice: the Verifier's "keep the
// candidate order inside a relation" rule is only meaningful if the order is
// stable, and the two experiment arms have to be able to consume the same pool.
func (b *CandidateBuilder) Build(ctx context.Context, request CandidateRequest) (CandidateSet, error) {
	set := CandidateSet{Semantic: []Candidate{}, Episodes: []Candidate{}, Reasons: []string{}}
	if request.UserID <= 0 || strings.TrimSpace(request.SessionID) == "" {
		return set, fmt.Errorf("candidate build needs an owner and the current session")
	}
	if request.Plan.Decision == DecisionNone {
		set.Reasons = append(set.Reasons, "plan_none")
		return set, nil
	}

	if request.Plan.Decision.needsSemantic() {
		if err := b.buildSemantic(ctx, request, &set); err != nil {
			return set, err
		}
	}
	if request.Plan.Decision.needsEpisodic() {
		if err := b.buildEpisodes(ctx, request, &set); err != nil {
			return set, err
		}
	}
	return set, nil
}

func (b *CandidateBuilder) buildSemantic(ctx context.Context, request CandidateRequest, set *CandidateSet) error {
	seen := make(map[string]struct{})
	for _, topicKey := range request.Plan.TopicKeys {
		if len(set.Semantic) >= MaxTopicMemories {
			set.Reasons = append(set.Reasons, "semantic_cap_reached")
			break
		}
		rows, err := b.memories.ListByTopicKey(ctx, request.UserID, model.MemoryScopeTopic, topicKey,
			b.minConfidence, MaxPerTopicMemory)
		if err != nil {
			return fmt.Errorf("reload topic memories for %q: %w", topicKey, err)
		}
		taken := 0
		for _, memory := range rows {
			if taken >= MaxPerTopicMemory || len(set.Semantic) >= MaxTopicMemories {
				break
			}
			if _, duplicate := seen[memory.ID]; duplicate {
				continue
			}
			// The reload is an assertion, not a filter. A row that reaches here
			// having failed it means the query predicate and this predicate
			// disagree, and the safe reading of that is to drop the row and
			// count it, never to pass it on because the SQL said so.
			if !eligibleMemory(memory, request.UserID, request.Now, b.minConfidence) || memory.TopicKey != topicKey {
				set.ReloadRejections++
				continue
			}
			seen[memory.ID] = struct{}{}
			set.Semantic = append(set.Semantic, Candidate{
				ID: memory.ID, Kind: CandidateSemantic, Channels: []string{"plan_topic"}, Memory: memory,
			})
			taken++
		}
	}
	return nil
}

func (b *CandidateBuilder) buildEpisodes(ctx context.Context, request CandidateRequest, set *CandidateSet) error {
	depth := request.CandidateDeep
	if depth <= 0 {
		depth = 20
	}
	ordered := make([]string, 0, MaxEpisodes)
	channels := map[string][]string{}
	add := func(ids []string, channel string) {
		for _, id := range ids {
			if _, known := channels[id]; !known {
				ordered = append(ordered, id)
			}
			channels[id] = appendOnce(channels[id], channel)
		}
	}

	add(request.Plan.EpisodeIDs, "plan_explicit")
	set.ExplicitHits = len(request.Plan.EpisodeIDs)

	if b.search != nil && len(request.Plan.TopicKeys) > 0 {
		hits, err := b.search.SearchExactTopics(ctx, request.UserID, request.SessionID, request.Plan.TopicKeys, depth)
		if err != nil {
			set.Reasons = append(set.Reasons, "exact_channel_unavailable")
		} else {
			ids := currentVersionIDs(hits)
			set.ExactHits = len(ids)
			add(ids, "plan_topic_exact")
		}
	}

	query := strings.TrimSpace(request.Plan.EpisodeQuery)
	if b.search != nil && query != "" {
		lexical, err := b.search.SearchByText(ctx, request.UserID, request.SessionID, queryTerms(query), depth)
		if err != nil {
			set.Reasons = append(set.Reasons, "lexical_channel_unavailable")
		} else {
			ids := currentVersionIDs(lexical)
			set.QueryHits += len(ids)
			add(ids, "plan_query_lexical")
		}
		if b.embedder != nil {
			vector, embedErr := b.embedder.EmbedQuery(ctx, query)
			if embedErr != nil {
				set.Reasons = append(set.Reasons, "embedding_unavailable")
			} else if hits, searchErr := b.search.SearchByVector(ctx, request.UserID, request.SessionID, vector, depth); searchErr != nil {
				set.Reasons = append(set.Reasons, "vector_channel_unavailable")
			} else {
				ids := currentVersionIDs(hits)
				set.QueryHits += len(ids)
				add(ids, "plan_query_vector")
			}
		}
	}

	// The time channel is a MySQL read, so it works when Redis does not. It is
	// last because it is the least specific: it answers "which session was most
	// recent", and a plan that named an episode outright has already said more
	// than that.
	if request.Plan.TimeScope.Kind != TimeAny {
		from, to, err := request.Plan.TimeScope.Range()
		if err != nil {
			return err
		}
		limit := depth
		if request.Plan.TimeScope.Kind == TimeLatest {
			limit = MaxEpisodes
		}
		rows, listErr := b.loader.ListByTime(ctx, request.UserID, request.SessionID, from, to, limit)
		if listErr != nil {
			return fmt.Errorf("list episodes by time: %w", listErr)
		}
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		set.LatestHits = len(ids)
		add(ids, "plan_time")
	}

	if len(ordered) == 0 {
		set.Reasons = append(set.Reasons, "no_episode_candidate")
		return nil
	}
	if len(ordered) > MaxEpisodes {
		ordered = ordered[:MaxEpisodes]
		set.Reasons = append(set.Reasons, "episode_cap_reached")
	}

	// One reload for the whole pool. LoadCandidates re-reads MySQL, requires the
	// row to be this user's, active, indexed and not the current session, and
	// applies the transitive source closure — which is the check a Redis hit
	// cannot make, because Redis still holds the entry of an episode whose
	// source session was deleted a second ago.
	rows, err := b.loader.LoadCandidates(ctx, request.UserID, request.SessionID, ordered)
	if err != nil {
		return fmt.Errorf("reload episode candidates: %w", err)
	}
	survived := make(map[string]model.EpisodeMemory, len(rows))
	for _, row := range rows {
		survived[row.ID] = row
	}
	for _, id := range ordered {
		row, ok := survived[id]
		if !ok {
			set.ReloadRejections++
			continue
		}
		if row.UserID != request.UserID || row.SessionID == request.SessionID ||
			row.Status != model.EpisodeStatusActive {
			// Unreachable through the production loader, which is why it is
			// checked: if it ever becomes reachable, this is a dropped row and
			// a counter rather than a foreign record in a prompt.
			set.ReloadRejections++
			continue
		}
		if !matchesEvidenceKinds(row, request.Plan) {
			set.ReloadRejections++
			continue
		}
		set.Episodes = append(set.Episodes, Candidate{
			ID: row.ID, Kind: CandidateEpisode, Channels: channels[id], Episode: row,
		})
	}
	if len(set.Episodes) == 0 {
		set.Reasons = append(set.Reasons, "all_episode_candidates_rejected")
	}
	return nil
}

// matchesEvidenceKinds applies the plan's filter against the structured
// projection rather than the search text. "决定" is a word that appears in
// sessions that recorded no decision, and a session that recorded one need not
// contain the word.
func matchesEvidenceKinds(episode model.EpisodeMemory, plan MemoryReadPlan) bool {
	if len(plan.EvidenceKinds) == 0 {
		return true
	}
	for _, kind := range plan.EvidenceKinds {
		switch kind {
		case EvidenceSynopsis:
			if strings.TrimSpace(episode.Document.Synopsis) != "" {
				return true
			}
		case EvidenceDecision:
			if len(episode.Document.Decisions) > 0 {
				return true
			}
		case EvidenceOpenLoop:
			if len(episode.Document.OpenLoops) > 0 {
				return true
			}
		case EvidenceCorrection:
			if len(episode.Document.UserCorrections) > 0 {
				return true
			}
		}
	}
	return false
}

// currentVersionIDs drops index entries written by a superseded embedding
// version, which are stale by construction.
func currentVersionIDs(hits []episodedao.EpisodeSearchResult) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		if hit.EmbeddingVersion != episodedao.CurrentEpisodeEmbeddingVersion {
			continue
		}
		ids = append(ids, hit.EpisodeID)
	}
	return ids
}

// queryTerms splits the plan's query into search terms. It reuses the episode
// service's own signal extraction so that the lexical channel sees the same kind
// of input it sees in production.
func queryTerms(query string) []string {
	signals := episodeservice.TopicSignalsFor(query)
	terms := append([]string(nil), signals.TopicTerms...)
	terms = append(terms, signals.Identifiers...)
	if len(terms) == 0 {
		terms = strings.Fields(query)
	}
	sort.Strings(terms)
	return terms
}

func appendOnce(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
