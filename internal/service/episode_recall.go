package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// The episodic retriever. It routes one question through the read plan, runs
// the recall channels, fuses their candidates under the admission rule, and
// then re-reads every candidate through the store before anything can become
// evidence. The reload is not a formality: the derived view proposes, the
// authority decides.

// EpisodeRecallRequest is one recall call.
type EpisodeRecallRequest struct {
	UserID    string
	SessionID string
	Question  string
	Now       time.Time
	// CurrentSessionHasHistory decides the "继续上次" resumption branch.
	CurrentSessionHasHistory bool
}

// EpisodeAdmissionMode selects which fusion admission rule a recall runs. The
// two rules differ in exactly one clause, and keeping both named is what makes
// the difference attributable: the source project's shipped default admits a
// candidate on an exact topic hit or on lexical and vector agreeing at any
// rank, while its v3 experiment (D-156) additionally requires the vector hit to
// sit within a corroboration depth. The project's own note on the guard is
// explicit that adding it silently changed what `legacy` means, so the
// experiment stays an opt-in here and never becomes the default by passing.
type EpisodeAdmissionMode string

const (
	// EpisodeAdmissionLegacy is the shipped fusion: exact, or lexical+vector
	// agreeing, with no depth bound on the agreement.
	EpisodeAdmissionLegacy EpisodeAdmissionMode = "legacy"
	// EpisodeAdmissionV3 is the experimental fusion: the same rule, plus the
	// corroboration-depth bound on the vector channel's second opinion.
	EpisodeAdmissionV3 EpisodeAdmissionMode = "v3"
)

// NormalizeEpisodeAdmissionMode parses a configured mode, defaulting to the
// shipped rule and refusing an unknown value rather than guessing.
func NormalizeEpisodeAdmissionMode(value string) (EpisodeAdmissionMode, error) {
	switch strings.TrimSpace(value) {
	case "", string(EpisodeAdmissionLegacy):
		return EpisodeAdmissionLegacy, nil
	case string(EpisodeAdmissionV3):
		return EpisodeAdmissionV3, nil
	default:
		return "", fmt.Errorf("episode admission mode %q must be legacy or v3", value)
	}
}

// EpisodeRecallTrace says what a recall did. It names the plan and the channels
// so a miss can be told apart from a refusal.
type EpisodeRecallTrace struct {
	NeedEpisode  bool
	TemporalMode EpisodeTemporalMode
	EvidenceMode EpisodeEvidenceMode
	Abstained    bool
	Reason       string
	ReasonCodes  []string
	Channels     []string
}

// EpisodeRecallResult is one recall's outcome. Episodes are empty when the read
// abstained; Trace says which refusal produced the emptiness.
type EpisodeRecallResult struct {
	Episodes []domain.Episode
	Trace    EpisodeRecallTrace
}

// EpisodeRetriever recalls cross-session episodes for one user.
type EpisodeRetriever struct {
	repo     repository.Repository
	search   EpisodeVectorIndex
	embedder retrieval.Embedder
	identity string
	// admission picks the fusion admission rule. Its zero value is the shipped
	// rule; only a deployment that asked for the experiment gets the v3 bound.
	admission EpisodeAdmissionMode
}

// NewEpisodeRetriever wires the retriever.
func NewEpisodeRetriever(repo repository.Repository, search EpisodeVectorIndex, embedder retrieval.Embedder, identity string) (*EpisodeRetriever, error) {
	if repo == nil || search == nil {
		return nil, fmt.Errorf("episode retriever dependencies are required")
	}
	return &EpisodeRetriever{repo: repo, search: search, embedder: embedder, identity: identity,
		admission: EpisodeAdmissionLegacy}, nil
}

// UseAdmissionMode selects the fusion admission rule for this retriever.
func (r *EpisodeRetriever) UseAdmissionMode(mode EpisodeAdmissionMode) {
	if mode == EpisodeAdmissionLegacy || mode == EpisodeAdmissionV3 {
		r.admission = mode
	}
}

// Recall answers one question. An unrelated question is not forced to use
// history: the plan refuses personal-history reads outright, and a history
// read with no eligible candidate abstains with a named reason instead of
// returning a nearest neighbour.
func (r *EpisodeRetriever) Recall(ctx context.Context, request EpisodeRecallRequest) (EpisodeRecallResult, error) {
	result := EpisodeRecallResult{Episodes: []domain.Episode{}}
	if strings.TrimSpace(request.UserID) == "" || strings.TrimSpace(request.SessionID) == "" {
		return result, fmt.Errorf("episode recall identity required")
	}
	plan := BuildEpisodeReadPlan(EpisodeRouteRequest{
		Question: request.Question, Now: request.Now,
		CurrentSessionHasHistory: request.CurrentSessionHasHistory,
	})
	result.Trace.NeedEpisode = plan.NeedEpisode
	result.Trace.TemporalMode = plan.TemporalMode
	result.Trace.EvidenceMode = plan.EvidenceMode
	result.Trace.Reason = plan.Reason()
	result.Trace.ReasonCodes = plan.ReasonCodes
	if !plan.NeedEpisode {
		result.Trace.Abstained = true
		return result, nil
	}
	if plan.TemporalMode == EpisodeTemporalLatestAny || plan.TemporalMode == EpisodeTemporalRange {
		return r.recallByTime(ctx, request, plan, result)
	}
	return r.recallByRelevance(ctx, request, plan, result)
}

// recallByTime answers the two questions whose primary key is time: the most
// recent other session, and an explicit date or period. The evidence filter is
// applied after the reload, which is why the fetch is deepened when one is
// active: the newest session need not be the newest session that recorded a
// decision.
func (r *EpisodeRetriever) recallByTime(ctx context.Context, request EpisodeRecallRequest, plan EpisodeReadPlan, result EpisodeRecallResult) (EpisodeRecallResult, error) {
	limit := episodeMaxResults
	switch {
	case plan.TemporalMode == EpisodeTemporalLatestAny && plan.EvidenceMode == EpisodeEvidenceAny:
		limit = 1
	case plan.TemporalMode == EpisodeTemporalLatestAny:
		limit = episodeCandidateDepth
	case plan.HasTopicConstraint():
		limit = episodeCandidateDepth
	}
	rows, err := r.repo.ListEpisodesByTime(ctx, request.UserID, request.SessionID, plan.From, plan.To, limit)
	if err != nil {
		return result, err
	}
	rows = filterEpisodesByEvidence(rows, plan.EvidenceMode)
	if plan.TemporalMode == EpisodeTemporalRange {
		rows = narrowEpisodeRangeByTopic(rows, plan)
	}
	if plan.TemporalMode == EpisodeTemporalLatestAny && len(rows) > 1 {
		rows = rows[:1]
	}
	if len(rows) > episodeMaxResults {
		rows = rows[:episodeMaxResults]
	}
	result.Episodes = rows
	result.Trace.Abstained = len(rows) == 0
	result.Trace.Channels = append(result.Trace.Channels, "store_time")
	return result, nil
}

// narrowEpisodeRangeByTopic applies the topic constraint a dated question may
// also carry. An explicitly named topic is a hard filter: "2026-08-25 关于迁移
// 定了什么" is wrong to answer with that day's other session. A topic merely
// inferred from the remaining words is a preference: the words left over from
// "那天最后是怎么收的" are not a subject, and treating them as one would empty a
// range query that the date alone had already answered correctly.
func narrowEpisodeRangeByTopic(rows []domain.Episode, plan EpisodeReadPlan) []domain.Episode {
	if len(rows) == 0 || !plan.HasTopicConstraint() {
		return rows
	}
	keys := append([]string{}, plan.ExactTopicKeys...)
	matched := make([]domain.Episode, 0, len(rows))
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
// instead of ending the read.
func (r *EpisodeRetriever) recallByRelevance(ctx context.Context, request EpisodeRecallRequest, plan EpisodeReadPlan, result EpisodeRecallResult) (EpisodeRecallResult, error) {
	exact, exactErr := r.search.SearchExactTopics(ctx, request.UserID, request.SessionID, r.identity, plan.ExactTopicKeys, episodeCandidateDepth)
	if len(plan.ExactIdentifiers) > 0 {
		identifierHits, identifierErr := r.search.SearchByText(ctx, request.UserID, request.SessionID, r.identity, plan.ExactIdentifiers, episodeCandidateDepth)
		exact = mergeEpisodeHits(exact, identifierHits)
		if exactErr == nil {
			exactErr = identifierErr
		}
	}
	lexical, lexicalErr := r.search.SearchByText(ctx, request.UserID, request.SessionID, r.identity, plan.TopicTerms, episodeCandidateDepth)
	if exactErr != nil && lexicalErr != nil {
		result.Trace.Abstained = true
		result.Trace.Reason = "index_unavailable"
		result.Trace.ReasonCodes = append(result.Trace.ReasonCodes, "index_unavailable")
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
	var vectors []EpisodeSearchHit
	if r.embedder != nil {
		vector, embedErr := r.embedder.Embed(ctx, []string{request.Question})
		if embedErr != nil || len(vector) != 1 {
			result.Trace.ReasonCodes = append(result.Trace.ReasonCodes, "embedding_unavailable")
		} else {
			vectors, _ = r.search.SearchByVector(ctx, request.UserID, request.SessionID, r.identity, vector[0], episodeCandidateDepth)
		}
	}
	ranked, channels := r.fuseChannels(exact, lexical, vectors)
	result.Trace.Channels = channels
	if len(ranked) == 0 {
		result.Trace.Abstained = true
		result.Trace.Reason = "no_eligible_candidate"
		result.Trace.ReasonCodes = append(result.Trace.ReasonCodes, "no_eligible_candidate")
		return result, nil
	}
	rows, err := r.repo.LoadEpisodeCandidates(ctx, request.UserID, request.SessionID, ranked)
	if err != nil {
		return result, err
	}
	rows = filterEpisodesByEvidence(rows, plan.EvidenceMode)
	if plan.TemporalMode == EpisodeTemporalLatestMatching {
		// Recency ranks the relevant candidates; it does not select them. The
		// old router had this the other way round, which is how "上次关于 X"
		// returned whichever session happened to be newest.
		rows = sortEpisodesByRecency(rows)
	}
	if len(rows) > episodeMaxResults {
		rows = rows[:episodeMaxResults]
	}
	result.Episodes = rows
	result.Trace.Abstained = len(rows) == 0
	if result.Trace.Abstained {
		result.Trace.Reason = "no_eligible_candidate"
		result.Trace.ReasonCodes = append(result.Trace.ReasonCodes, "store_reload_rejected")
	} else if len(exact) > 0 {
		result.Trace.ReasonCodes = append(result.Trace.ReasonCodes, "exact_topic_hit")
	} else {
		result.Trace.ReasonCodes = append(result.Trace.ReasonCodes, "lexical_vector_hit")
	}
	return result, nil
}

// filterEpisodesByEvidence applies the requested evidence type against the
// structured projection rather than against the search text. "决定" is a word
// that occurs in episodes that recorded no decision, and an episode that
// recorded one does not have to contain the word.
func filterEpisodesByEvidence(rows []domain.Episode, mode EpisodeEvidenceMode) []domain.Episode {
	if mode == EpisodeEvidenceAny {
		return rows
	}
	kept := make([]domain.Episode, 0, len(rows))
	for _, row := range rows {
		switch mode {
		case EpisodeEvidenceDecision:
			if len(row.Document.Decisions) == 0 {
				continue
			}
		case EpisodeEvidenceCorrection:
			if len(row.Document.UserCorrections) == 0 {
				continue
			}
		case EpisodeEvidenceOpenLoop:
			if len(row.Document.OpenLoops) == 0 {
				continue
			}
		}
		kept = append(kept, row)
	}
	return kept
}

func sortEpisodesByRecency(rows []domain.Episode) []domain.Episode {
	ordered := append([]domain.Episode(nil), rows...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].LastMessageAt.Equal(ordered[j].LastMessageAt) {
			return ordered[i].LastMessageAt.After(ordered[j].LastMessageAt)
		}
		return ordered[i].ID < ordered[j].ID
	})
	return ordered
}

func episodeMatchesTerms(episode domain.Episode, terms []string) bool {
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

func episodeMatchesTopic(episode domain.Episode, keys []string) bool {
	for _, stored := range episode.TopicKeys {
		for _, key := range keys {
			if stored == key {
				return true
			}
		}
	}
	return false
}

func mergeEpisodeHits(left, right []EpisodeSearchHit) []EpisodeSearchHit {
	seen := map[string]struct{}{}
	result := make([]EpisodeSearchHit, 0, len(left)+len(right))
	for _, items := range [][]EpisodeSearchHit{left, right} {
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

// episodeCorroborationDepth bounds how deep a vector hit may sit and still
// count as the lexical channel's second opinion. A nearest-neighbour search
// always returns its top K, and on a small index that is most of the corpus.
// Treating "the vector channel did not rule it out" as agreement would collapse
// the two-channel rule back into a lexical-only rule, which is how an unrelated
// session that happens to share one word gets admitted. A corroborating opinion
// has to be one of the channel's own better answers.
func episodeCorroborationDepth() int {
	depth := episodeCandidateDepth / 2
	if depth < episodeMaxResults {
		depth = episodeMaxResults
	}
	return depth
}

// fuseChannels fuses the three channels under the retriever's admission mode.
// The mode is read here, once, so the two rules cannot drift apart by one call
// site passing a different bound.
func (r *EpisodeRetriever) fuseChannels(exact, lexical, vector []EpisodeSearchHit) ([]string, []string) {
	if r.admission == EpisodeAdmissionV3 {
		return eligibleEpisodeRRF(exact, lexical, vector, episodeCandidateDepth, episodeCorroborationDepth())
	}
	return legacyEligibleEpisodeRRF(exact, lexical, vector, episodeCandidateDepth)
}

// legacyEligibleEpisodeRRF is the shipped fusion. It has no corroboration-depth
// bound: that guard is the v3 experiment's change, and adding it here would
// silently move what the shipped default means - the source project states the
// same rule for the same reason. A candidate enters on an exact topic hit, or
// on lexical and vector agreeing at any rank; a vector-only candidate is never
// admitted, because nearest-neighbour search always returns a neighbour and
// "closest thing in the index" is not evidence the learner asked about it.
func legacyEligibleEpisodeRRF(exact, lexical, vector []EpisodeSearchHit, max int) ([]string, []string) {
	items := fuseEpisodeHits(exact, lexical, vector, func(entry *fusedEpisode) bool {
		return entry.exact > 0 || (entry.lexical > 0 && entry.vector > 0)
	})
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
	channels := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.id)
		channels = append(channels, episodeChannelLabel(item))
	}
	return ids, channels
}

// eligibleEpisodeRRF fuses the three channels with reciprocal-rank fusion and
// applies the v3 admission rule: a vector-only candidate is never admitted, and
// the lexical channel's second opinion must sit within the corroboration depth.
func eligibleEpisodeRRF(exact, lexical, vector []EpisodeSearchHit, max, corroboration int) ([]string, []string) {
	items := fuseEpisodeHits(exact, lexical, vector, func(entry *fusedEpisode) bool {
		return entry.exact > 0 || (entry.lexical > 0 && entry.vector > 0 && entry.vector <= corroboration)
	})
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
	channels := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.id)
		channels = append(channels, episodeChannelLabel(item))
	}
	return ids, channels
}

// fuseEpisodeHits gathers the three channels into fused candidates and keeps
// the ones the admission rule accepts.
func fuseEpisodeHits(exact, lexical, vector []EpisodeSearchHit, admit func(*fusedEpisode) bool) []fusedEpisode {
	byID := map[string]*fusedEpisode{}
	add := func(items []EpisodeSearchHit, channel string) {
		for idx, item := range items {
			entry := byID[item.EpisodeID]
			if entry == nil {
				entry = &fusedEpisode{id: item.EpisodeID}
				byID[item.EpisodeID] = entry
			}
			rank := idx + 1
			entry.score += 1 / float64(episodeRRFRankConstant+rank)
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
		if admit(entry) {
			items = append(items, *entry)
		}
	}
	return items
}

func episodeChannelLabel(item fusedEpisode) string {
	channelsForItem := ""
	if item.exact > 0 {
		channelsForItem = "exact"
	}
	if item.lexical > 0 {
		if channelsForItem != "" {
			channelsForItem += "+"
		}
		channelsForItem += "lexical"
	}
	if item.vector > 0 {
		if channelsForItem != "" {
			channelsForItem += "+"
		}
		channelsForItem += "vector"
	}
	return channelsForItem
}
