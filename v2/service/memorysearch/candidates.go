// Package memorysearch is the candidate layer behind the Agent's memory_search
// tool.
//
// It is a new package rather than a function on the existing readers because
// the two are answering different questions. SimilarityRecall decides what may
// be injected into a System prompt without anybody asking for it, so its
// abstention gate trades recall for silence — and STABILIZE-031 measured what
// that trade costs in both directions. This layer decides what may be *shown to
// the Agent that asked for it*, which is a weaker claim: a card the Agent reads
// and ignores costs a few hundred tokens, while a memory that was never
// retrieved cannot be reconsidered at all.
//
// What does not weaken is the boundary. Redis is a candidate layer and MySQL is
// the fact source: every id a search channel proposes is re-read from MySQL and
// re-checked for owner, scope, status, confidence, expiry, supersede, embedding
// version and — for Episodes — the transitive source closure and the current
// session. A record that fails any of those is dropped and counted, never
// handed to the model with a caveat.
package memorysearch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"GopherAI/common/memorywindow"
	"GopherAI/common/rag"
	episodedao "GopherAI/dao/episode"
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	episodeservice "GopherAI/service/episode"
)

// Kind is which of the two long-term memory types a candidate is.
type Kind string

const (
	KindSemantic Kind = "semantic"
	KindEpisodic Kind = "episodic"
)

func (k Kind) Valid() bool { return k == KindSemantic || k == KindEpisodic }

// TemporalMode is the only time control the tool exposes.
//
// It is an enum rather than free text because the server must never infer a
// range from natural language: "上次" means one thing to a learner who studies
// daily and another to one who studies monthly, and a wrong guess silently
// changes which part of their history is reachable.
type TemporalMode string

const (
	TemporalAny    TemporalMode = "any"
	TemporalLatest TemporalMode = "latest"
	TemporalRange  TemporalMode = "range"
)

func (t TemporalMode) Valid() bool {
	return t == TemporalAny || t == TemporalLatest || t == TemporalRange
}

// Temporal is a validated time scope.
type Temporal struct {
	Mode TemporalMode
	From time.Time
	To   time.Time
}

// Status is the outcome the Agent sees.
type Status string

const (
	// StatusOK means the search ran and returned at least one card.
	StatusOK Status = "ok"
	// StatusNoEvidence means the search ran and found nothing. It is a real
	// answer, and the prompt requires the Agent to report it as "nothing
	// recorded", never as "you never said that".
	StatusNoEvidence Status = "no_evidence"
	// StatusUnavailable means the search could not run. It is deliberately
	// distinguishable from no_evidence: an outage that reads as an empty
	// memory teaches the model to assert a negative it has no basis for.
	StatusUnavailable Status = "unavailable"
)

// Reason codes are a closed vocabulary. They never carry user text, a database
// id, a vector or a provider error string.
const (
	ReasonNone                 = "none"
	ReasonNoCandidate          = "no_candidate"
	ReasonAllCandidatesDropped = "all_candidates_rejected"
	ReasonEmbeddingUnavailable = "embedding_unavailable"
	ReasonIndexUnavailable     = "index_unavailable"
	ReasonFactSourceFailure    = "fact_source_unavailable"
	ReasonCancelled            = "cancelled"
	ReasonDegradedTimeOnly     = "time_channel_only"
)

// Candidate is one record that survived the reload, in the shape the tool
// renders from. It carries no Redis score and no raw row.
type Candidate struct {
	Kind    Kind
	ID      string
	Version uint64
	Subject string
	Content string
	// OccurredAt is the Episode's own time. Semantic memory has no occurrence,
	// so it stays zero rather than borrowing an updated_at that would read to
	// the model as "when the learner said it".
	OccurredAt time.Time
	// EvidenceMessageIDs are the learner messages this Episode's structured
	// entries cite. They never leave the server as ids; they exist so the tool
	// can read bounded quotations without the model naming a message.
	EvidenceMessageIDs []uint
	// Channels records how the candidate was reached, for attribution only.
	Channels []string
	// rank is the fused rank inside its own kind, used for the interleave.
	rank int
}

// Result is one search.
type Result struct {
	Status     Status
	Reason     string
	Candidates []Candidate
	// Reserve is the records that reached a per-kind top list but lost the
	// final merge, in the order that merge would have taken them. It is what a
	// window policy may draw on; it is never part of Candidates, so a caller
	// that ignores it sees exactly the window this layer has always returned.
	//
	// It is computed on every mixed-kind call whether or not anything will use
	// it. A reserve derived only under an experimental policy would be a second
	// code path exercised only by the experiment, and the experiment's whole
	// claim is that candidate production did not change.
	Reserve []Candidate
	// Degraded says a channel was missing and the result is what the remaining
	// server-provable channel could show. It is never set to paper over a
	// vector outage with lexical matches.
	Degraded       bool
	EmbeddingCalls int
	Diagnostics    Diagnostics
}

// Diagnostics is the non-sensitive record of what the layer did.
type Diagnostics struct {
	SemanticCandidates int
	EpisodeCandidates  int
	ReloadRejections   int
	Channels           []string
	Reasons            []string
}

// Request is one validated tool call. The identities are server-owned: they
// arrive from the RunContext and can never be named by the model.
type Request struct {
	UserID    int64
	SessionID string
	Query     string
	Kinds     []Kind
	Temporal  Temporal
	TopK      int
	Now       time.Time
}

// Config bounds the layer. Every number here is part of the method's identity
// and is written into the freeze.
type Config struct {
	// CandidateDepth is how deep each channel searches before fusion.
	CandidateDepth int
	// MaxPerKind bounds each type's share before the merge.
	MaxPerKind int
	// MaxResults is the hard ceiling on the merged list, whatever top_k asks.
	MaxResults int
	// MinConfidence is the semantic eligibility floor. It is the same figure
	// the injected path uses, because it expresses whether the record is
	// trusted, not whether it is relevant.
	MinConfidence float64
}

func DefaultConfig() Config {
	return Config{CandidateDepth: 20, MaxPerKind: 4, MaxResults: 6, MinConfidence: 0.6}
}

func (c Config) Validate() error {
	if c.CandidateDepth < 5 || c.CandidateDepth > 100 {
		return fmt.Errorf("memory search candidate depth must be between 5 and 100")
	}
	if c.MaxPerKind < 1 || c.MaxPerKind > 6 {
		return fmt.Errorf("memory search per-kind cap must be between 1 and 6")
	}
	if c.MaxResults < 1 || c.MaxResults > 6 {
		return fmt.Errorf("memory search result cap must be between 1 and 6")
	}
	if c.MinConfidence < 0 || c.MinConfidence > 1 {
		return fmt.Errorf("memory search minimum confidence must be between 0 and 1")
	}
	return nil
}

// The narrow dependencies. Each one is the production type's own interface, so
// the reload that decides eligibility here is the reload the rest of the system
// performs.
type (
	// MemoryIndex is the Redis semantic candidate layer.
	MemoryIndex interface {
		SearchByVector(ctx context.Context, userID int64, vector []float32, topK int) ([]memorydao.MemorySearchResult, error)
		SearchByText(ctx context.Context, userID int64, terms []string, topK int) ([]memorydao.MemorySearchResult, error)
	}
	// MemoryLoader is the semantic fact source.
	MemoryLoader interface {
		LoadByIDs(ctx context.Context, userID int64, memoryIDs []string) ([]model.LongTermMemory, error)
	}
	// EpisodeSearcher is the Redis Episode candidate layer.
	EpisodeSearcher = episodeservice.EpisodeSearcher
	// EpisodeLoader is the Episode fact source, including the source closure.
	EpisodeLoader interface {
		LoadCandidates(context.Context, int64, string, []string) ([]model.EpisodeMemory, error)
		ListByTime(context.Context, int64, string, time.Time, time.Time, int) ([]model.EpisodeMemory, error)
	}
	// QueryEmbedder turns the tool's query into one vector.
	QueryEmbedder interface {
		EmbedQuery(ctx context.Context, text string) ([]float32, error)
	}
)

// Searcher is the whole layer.
type Searcher struct {
	memoryIndex  MemoryIndex
	memoryLoader MemoryLoader
	episodeIndex EpisodeSearcher
	episodeStore EpisodeLoader
	embedder     QueryEmbedder
	config       Config
}

func NewSearcher(memoryIndex MemoryIndex, memoryLoader MemoryLoader, episodeIndex EpisodeSearcher,
	episodeStore EpisodeLoader, embedder QueryEmbedder, config Config) (*Searcher, error) {
	if memoryIndex == nil || memoryLoader == nil || episodeIndex == nil || episodeStore == nil || embedder == nil {
		return nil, fmt.Errorf("memory search needs both indexes, both fact sources and an embedder")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Searcher{
		memoryIndex: memoryIndex, memoryLoader: memoryLoader, episodeIndex: episodeIndex,
		episodeStore: episodeStore, embedder: embedder, config: config,
	}, nil
}

// Search runs one call.
//
// The query is embedded at most once and the vector is shared by both paths. A
// mixed question that needed semantic notes and an Episode used to be the case
// where a request quietly paid twice; here it cannot, because there is one
// vector and both channels are handed it.
func (s *Searcher) Search(ctx context.Context, request Request) (Result, error) {
	result := Result{Status: StatusNoEvidence, Reason: ReasonNoCandidate, Candidates: []Candidate{}}
	if request.UserID <= 0 || strings.TrimSpace(request.SessionID) == "" {
		return result, fmt.Errorf("memory search needs a trusted owner and the current session")
	}
	if strings.TrimSpace(request.Query) == "" {
		return result, fmt.Errorf("memory search needs a query")
	}
	if err := ctx.Err(); err != nil {
		result.Status, result.Reason = StatusUnavailable, ReasonCancelled
		return result, nil
	}
	kinds := normalizeKinds(request.Kinds)
	topK := request.TopK
	if topK <= 0 || topK > s.config.MaxResults {
		topK = s.config.MaxResults
	}
	if request.Now.IsZero() {
		request.Now = time.Now()
	}

	// One embedding for the whole call. Its failure is recorded rather than
	// retried: a provider that just refused this text will refuse it again
	// inside the same turn, and the second attempt only spends latency.
	vector, embedErr := s.embedder.EmbedQuery(ctx, request.Query)
	if embedErr == nil {
		result.EmbeddingCalls = 1
	} else {
		result.EmbeddingCalls = 1
		result.Diagnostics.Reasons = appendOnce(result.Diagnostics.Reasons, ReasonEmbeddingUnavailable)
		if ctx.Err() != nil {
			result.Status, result.Reason = StatusUnavailable, ReasonCancelled
			return result, nil
		}
	}
	vectorAvailable := embedErr == nil

	terms := rag.ExtractLexicalTerms(request.Query)
	var semantic, episodic []Candidate
	semanticFailed, episodeFailed := false, false

	if kinds[KindSemantic] {
		found, failed, err := s.searchSemantic(ctx, request, vector, vectorAvailable, terms, &result)
		if err != nil {
			return result, err
		}
		semantic, semanticFailed = found, failed
	}
	if kinds[KindEpisodic] {
		found, failed, err := s.searchEpisodic(ctx, request, vector, vectorAvailable, terms, &result)
		if err != nil {
			return result, err
		}
		episodic, episodeFailed = found, failed
	}
	if ctx.Err() != nil {
		result.Status, result.Reason = StatusUnavailable, ReasonCancelled
		result.Candidates = []Candidate{}
		return result, nil
	}

	result.Diagnostics.SemanticCandidates = len(semantic)
	result.Diagnostics.EpisodeCandidates = len(episodic)

	// Every requested path failed, so the search did not happen. Reporting that
	// as "nothing recorded" is the one degradation this layer refuses: an
	// outage that reads as an empty memory is how a model learns to assert a
	// negative about a learner's history.
	requested := 0
	failedPaths := 0
	if kinds[KindSemantic] {
		requested++
		if semanticFailed {
			failedPaths++
		}
	}
	if kinds[KindEpisodic] {
		requested++
		if episodeFailed {
			failedPaths++
		}
	}
	if requested > 0 && failedPaths == requested && len(semantic) == 0 && len(episodic) == 0 {
		result.Status = StatusUnavailable
		result.Reason = firstReason(result.Diagnostics.Reasons, ReasonIndexUnavailable)
		result.Candidates = []Candidate{}
		return result, nil
	}

	merged := interleave(semantic, episodic, topK)
	if len(merged) == 0 {
		result.Status = StatusNoEvidence
		result.Reason = ReasonNoCandidate
		if result.Diagnostics.ReloadRejections > 0 {
			result.Reason = ReasonAllCandidatesDropped
		}
		return result, nil
	}
	result.Candidates = merged
	result.Reserve = reserve(semantic, episodic, merged)
	result.Status = StatusOK
	result.Reason = ReasonNone
	if failedPaths > 0 || !vectorAvailable {
		result.Degraded = true
	}
	return result, nil
}

func (s *Searcher) searchSemantic(ctx context.Context, request Request, vector []float32,
	vectorAvailable bool, terms []string, result *Result) ([]Candidate, bool, error) {
	var vectorHits, lexicalHits []memorydao.MemorySearchResult
	vectorOK, lexicalOK := false, false
	if vectorAvailable {
		hits, err := s.memoryIndex.SearchByVector(ctx, request.UserID, vector, s.config.CandidateDepth)
		if err != nil {
			result.Diagnostics.Reasons = appendOnce(result.Diagnostics.Reasons, "semantic_vector_unavailable")
		} else {
			vectorHits, vectorOK = hits, true
			result.Diagnostics.Channels = appendOnce(result.Diagnostics.Channels, "semantic_vector")
		}
	}
	if len(terms) > 0 {
		hits, err := s.memoryIndex.SearchByText(ctx, request.UserID, terms, s.config.CandidateDepth)
		if err != nil {
			result.Diagnostics.Reasons = appendOnce(result.Diagnostics.Reasons, "semantic_lexical_unavailable")
		} else {
			lexicalHits, lexicalOK = hits, true
			result.Diagnostics.Channels = appendOnce(result.Diagnostics.Channels, "semantic_lexical")
		}
	}
	// A lexical channel on its own is not a semantic search. BM25 over a
	// personal note collection returns the neighbouring subject as readily as
	// the right one, and STABILIZE-031 spent a blind run establishing that
	// widening recall while losing the vector's discrimination trades silence
	// for noise. Without the vector this path returns nothing rather than
	// something.
	if !vectorOK {
		return nil, true, nil
	}
	if !lexicalOK {
		result.Diagnostics.Reasons = appendOnce(result.Diagnostics.Reasons, "semantic_lexical_missing")
	}

	ordered, channels := fuseMemoryHits(vectorHits, lexicalHits, s.config.CandidateDepth)
	if len(ordered) == 0 {
		return nil, false, nil
	}
	rows, err := s.memoryLoader.LoadByIDs(ctx, request.UserID, ordered)
	if err != nil {
		if ctx.Err() != nil {
			return nil, true, nil
		}
		result.Diagnostics.Reasons = appendOnce(result.Diagnostics.Reasons, ReasonFactSourceFailure)
		return nil, true, nil
	}
	survived := make(map[string]model.LongTermMemory, len(rows))
	for _, row := range rows {
		survived[row.ID] = row
	}
	out := make([]Candidate, 0, s.config.MaxPerKind)
	for rank, id := range ordered {
		if len(out) >= s.config.MaxPerKind {
			break
		}
		row, ok := survived[id]
		if !ok {
			// The index proposed a row the fact source will not return:
			// archived, superseded, deleted, expired or another account's.
			result.Diagnostics.ReloadRejections++
			continue
		}
		if !eligibleMemory(row, request.UserID, request.Now, s.config.MinConfidence) {
			result.Diagnostics.ReloadRejections++
			continue
		}
		out = append(out, Candidate{
			Kind: KindSemantic, ID: row.ID, Version: model.SemanticMemoryVersion(row),
			Subject: strings.TrimSpace(row.Subject), Content: strings.TrimSpace(row.Content),
			Channels: channels[id], rank: rank,
		})
	}
	return out, false, nil
}

func (s *Searcher) searchEpisodic(ctx context.Context, request Request, vector []float32,
	vectorAvailable bool, terms []string, result *Result) ([]Candidate, bool, error) {
	var vectorHits, lexicalHits []episodedao.EpisodeSearchResult
	vectorOK := false
	if vectorAvailable {
		hits, err := s.episodeIndex.SearchByVector(ctx, request.UserID, request.SessionID, vector, s.config.CandidateDepth)
		if err != nil {
			result.Diagnostics.Reasons = appendOnce(result.Diagnostics.Reasons, "episode_vector_unavailable")
		} else {
			vectorHits, vectorOK = hits, true
			result.Diagnostics.Channels = appendOnce(result.Diagnostics.Channels, "episode_vector")
		}
	}
	if len(terms) > 0 {
		hits, err := s.episodeIndex.SearchByText(ctx, request.UserID, request.SessionID, terms, s.config.CandidateDepth)
		if err != nil {
			result.Diagnostics.Reasons = appendOnce(result.Diagnostics.Reasons, "episode_lexical_unavailable")
		} else {
			lexicalHits = hits
			result.Diagnostics.Channels = appendOnce(result.Diagnostics.Channels, "episode_lexical")
		}
	}

	ordered, channels := fuseEpisodeHits(vectorHits, lexicalHits, s.config.CandidateDepth, vectorOK)

	// The time channel is a MySQL read, so it survives a Redis outage. It runs
	// only when the tool asked for it in so many words: "nothing matched, so
	// here is the most recent session" is the fabrication this design forbids
	// by name.
	timeOnly := false
	if request.Temporal.Mode == TemporalLatest || request.Temporal.Mode == TemporalRange {
		from, to := request.Temporal.From, request.Temporal.To
		limit := s.config.CandidateDepth
		if request.Temporal.Mode == TemporalLatest {
			limit = s.config.MaxPerKind
		}
		rows, err := s.episodeStore.ListByTime(ctx, request.UserID, request.SessionID, from, to, limit)
		if err != nil {
			result.Diagnostics.Reasons = appendOnce(result.Diagnostics.Reasons, ReasonFactSourceFailure)
		} else {
			result.Diagnostics.Channels = appendOnce(result.Diagnostics.Channels, "episode_time")
			ids := make([]string, 0, len(rows))
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			ordered, channels = appendChannel(ordered, channels, ids, "episode_time")
			timeOnly = !vectorOK
		}
	}
	if !vectorOK && !timeOnly {
		// Same rule as the semantic path: no vector, no lexical-only answer.
		return nil, true, nil
	}
	if len(ordered) == 0 {
		return nil, false, nil
	}
	if len(ordered) > s.config.CandidateDepth {
		ordered = ordered[:s.config.CandidateDepth]
	}
	rows, err := s.episodeStore.LoadCandidates(ctx, request.UserID, request.SessionID, ordered)
	if err != nil {
		if ctx.Err() != nil {
			return nil, true, nil
		}
		result.Diagnostics.Reasons = appendOnce(result.Diagnostics.Reasons, ReasonFactSourceFailure)
		return nil, true, nil
	}
	if timeOnly {
		result.Diagnostics.Reasons = appendOnce(result.Diagnostics.Reasons, ReasonDegradedTimeOnly)
	}
	survived := make(map[string]model.EpisodeMemory, len(rows))
	for _, row := range rows {
		survived[row.ID] = row
	}
	out := make([]Candidate, 0, s.config.MaxPerKind)
	for rank, id := range ordered {
		if len(out) >= s.config.MaxPerKind {
			break
		}
		row, ok := survived[id]
		if !ok {
			result.Diagnostics.ReloadRejections++
			continue
		}
		// LoadCandidates already applies owner, status, index state, current
		// session and the transitive source closure. Repeating the parts that
		// can be repeated here is not redundancy: if the loader ever changes,
		// this is a dropped row and a counter rather than a foreign record in
		// front of the model.
		if row.UserID != request.UserID || row.SessionID == request.SessionID ||
			row.Status != model.EpisodeStatusActive {
			result.Diagnostics.ReloadRejections++
			continue
		}
		content := RenderEpisodeContent(row)
		if strings.TrimSpace(content) == "" {
			result.Diagnostics.ReloadRejections++
			continue
		}
		out = append(out, Candidate{
			Kind: KindEpisodic, ID: row.ID, Version: row.Version,
			Subject: strings.TrimSpace(row.Title), Content: content,
			OccurredAt: row.LastMessageAt.UTC(), EvidenceMessageIDs: EpisodeEvidenceIDs(row),
			Channels: channels[id], rank: rank,
		})
	}
	return out, false, nil
}

// eligibleMemory is the semantic re-check. It is the same predicate the planned
// path applies, expressed once here so that a change to what "in force" means
// cannot reach one reader and miss the other.
func eligibleMemory(memory model.LongTermMemory, userID int64, now time.Time, minConfidence float64) bool {
	return memory.UserID == userID &&
		memory.Status == model.LongTermMemoryActive &&
		memory.Scope == model.MemoryScopeTopic &&
		memory.Confidence >= minConfidence &&
		!memory.ExpiredAt(now) &&
		strings.TrimSpace(memory.Subject) != "" &&
		strings.TrimSpace(memory.Content) != ""
}

// RenderEpisodeContent turns the structured projection into the bounded text a
// card carries. Source handles are deliberately absent: they are a write-time
// audit trail the model cannot act on.
// RenderEpisodeContent is the body an Episode contributes to an observation.
//
// It is exported because the frozen-trace ablation has to render the same text
// production renders. A second copy of this function in an evaluator would make
// the ablation a comparison of two renderers rather than of two windows.
func RenderEpisodeContent(episode model.EpisodeMemory) string {
	var out strings.Builder
	if synopsis := strings.TrimSpace(episode.Document.Synopsis); synopsis != "" {
		out.WriteString(synopsis)
	}
	write := func(label string, entries []model.EpisodeEvidence) {
		for _, entry := range entries {
			text := strings.TrimSpace(entry.Text)
			if text == "" {
				continue
			}
			if out.Len() > 0 {
				out.WriteString("\n")
			}
			fmt.Fprintf(&out, "%s: %s", label, text)
		}
	}
	write("decision", episode.Document.Decisions)
	write("open_loop", episode.Document.OpenLoops)
	write("correction", episode.Document.UserCorrections)
	return out.String()
}

// EpisodeEvidenceIDs is the learner-message set behind an Episode, exported for
// the same reason as RenderEpisodeContent.
func EpisodeEvidenceIDs(episode model.EpisodeMemory) []uint {
	seen := make(map[uint]struct{}, 12)
	ids := make([]uint, 0, 12)
	for _, entries := range [][]model.EpisodeEvidence{
		episode.Document.Decisions, episode.Document.OpenLoops, episode.Document.UserCorrections,
	} {
		for _, entry := range entries {
			for _, id := range entry.EvidenceMessageIDs {
				if id == 0 {
					continue
				}
				if _, duplicate := seen[id]; duplicate {
					continue
				}
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// rrfRankConstant is the fusion constant, fixed at the value both the knowledge
// base and the memory recall path already use. It is not tuned here: a constant
// that differs per call site makes two rankings incomparable for no gain.
const rrfRankConstant = 60.0

func fuseMemoryHits(vector, lexical []memorydao.MemorySearchResult, limit int) ([]string, map[string][]string) {
	score := make(map[string]float64, len(vector)+len(lexical))
	channels := make(map[string][]string, len(vector)+len(lexical))
	for _, hit := range vector {
		score[hit.MemoryID] += 1 / (rrfRankConstant + float64(hit.Rank+1))
		channels[hit.MemoryID] = appendOnce(channels[hit.MemoryID], "semantic_vector")
	}
	for _, hit := range lexical {
		score[hit.MemoryID] += 1 / (rrfRankConstant + float64(hit.Rank+1))
		channels[hit.MemoryID] = appendOnce(channels[hit.MemoryID], "semantic_lexical")
	}
	return rankByScore(score, limit), channels
}

func fuseEpisodeHits(vector, lexical []episodedao.EpisodeSearchResult, limit int, vectorOK bool) ([]string, map[string][]string) {
	score := make(map[string]float64, len(vector)+len(lexical))
	channels := make(map[string][]string, len(vector)+len(lexical))
	if vectorOK {
		for _, hit := range currentVersionHits(vector) {
			score[hit.EpisodeID] += 1 / (rrfRankConstant + float64(hit.Rank+1))
			channels[hit.EpisodeID] = appendOnce(channels[hit.EpisodeID], "episode_vector")
		}
		for _, hit := range currentVersionHits(lexical) {
			score[hit.EpisodeID] += 1 / (rrfRankConstant + float64(hit.Rank+1))
			channels[hit.EpisodeID] = appendOnce(channels[hit.EpisodeID], "episode_lexical")
		}
	}
	return rankByScore(score, limit), channels
}

// currentVersionHits drops entries written under a superseded embedding
// version. They are stale by construction: the vector they were ranked by does
// not live in the same space as the query's.
func currentVersionHits(hits []episodedao.EpisodeSearchResult) []episodedao.EpisodeSearchResult {
	out := make([]episodedao.EpisodeSearchResult, 0, len(hits))
	for _, hit := range hits {
		if hit.EmbeddingVersion != episodedao.CurrentEpisodeEmbeddingVersion {
			continue
		}
		out = append(out, hit)
	}
	return out
}

func rankByScore(score map[string]float64, limit int) []string {
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
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids
}

func appendChannel(ordered []string, channels map[string][]string, ids []string, channel string) ([]string, map[string][]string) {
	if channels == nil {
		channels = map[string][]string{}
	}
	for _, id := range ids {
		if _, known := channels[id]; !known {
			ordered = append(ordered, id)
		}
		channels[id] = appendOnce(channels[id], channel)
	}
	return ordered, channels
}

// mergeKey is a candidate's identity for merging: two rows that differ only by
// version are the same record seen twice, and the window must show it once.
func mergeKey(candidate Candidate) string {
	return fmt.Sprintf("%s|%s|%d", candidate.Kind, candidate.ID, candidate.Version)
}

// interleave merges the two kinds.
//
// The ordering rule itself lives in memorywindow, because the offline window
// experiments have to merge the same candidates the same way; this function is
// the projection between production rows and that rule.
func interleave(semantic, episodic []Candidate, topK int) []Candidate {
	byKey := make(map[string]Candidate, len(semantic)+len(episodic))
	project := func(in []Candidate) []memorywindow.Ranked {
		out := make([]memorywindow.Ranked, 0, len(in))
		for _, candidate := range in {
			key := mergeKey(candidate)
			byKey[key] = candidate
			out = append(out, memorywindow.Ranked{Key: key, Rank: candidate.rank})
		}
		return out
	}
	merged := memorywindow.Interleave(project(semantic), project(episodic), topK)
	out := make([]Candidate, 0, len(merged))
	for _, item := range merged {
		out = append(out, byKey[item.Key])
	}
	return out
}

// reserve is what the final merge dropped, in merge order.
//
// It is empty unless both kinds contributed. A single-kind call has nothing to
// have been squeezed out by: its window is short because that kind ran out of
// eligible records, and handing back more of the same kind would be a wider
// search rather than a preserved one.
func reserve(semantic, episodic, final []Candidate) []Candidate {
	if len(semantic) == 0 || len(episodic) == 0 {
		return nil
	}
	byKey := make(map[string]Candidate, len(semantic)+len(episodic))
	project := func(in []Candidate) []memorywindow.Ranked {
		out := make([]memorywindow.Ranked, 0, len(in))
		for _, candidate := range in {
			key := mergeKey(candidate)
			byKey[key] = candidate
			out = append(out, memorywindow.Ranked{Key: key, Rank: candidate.rank})
		}
		return out
	}
	held := make([]string, 0, len(final))
	for _, candidate := range final {
		held = append(held, mergeKey(candidate))
	}
	kept := memorywindow.Reserve(project(semantic), project(episodic), held,
		memorywindow.TokenBoundedMaxCards)
	if len(kept) == 0 {
		return nil
	}
	out := make([]Candidate, 0, len(kept))
	for _, item := range kept {
		out = append(out, byKey[item.Key])
	}
	return out
}

func normalizeKinds(kinds []Kind) map[Kind]bool {
	out := map[Kind]bool{}
	for _, kind := range kinds {
		if kind.Valid() {
			out[kind] = true
		}
	}
	if len(out) == 0 {
		out[KindSemantic] = true
		out[KindEpisodic] = true
	}
	return out
}

func appendOnce(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func firstReason(reasons []string, fallback string) string {
	for _, reason := range reasons {
		switch reason {
		case ReasonEmbeddingUnavailable, ReasonFactSourceFailure:
			return reason
		}
	}
	if len(reasons) > 0 {
		return fallback
	}
	return fallback
}

// ErrUnknownKind is returned by ParseKind so a tool can refuse an out-of-range
// value with a stable message instead of a database-shaped one.
var ErrUnknownKind = errors.New("memory kind must be semantic or episodic")

func ParseKind(value string) (Kind, error) {
	kind := Kind(strings.ToLower(strings.TrimSpace(value)))
	if !kind.Valid() {
		return "", ErrUnknownKind
	}
	return kind, nil
}
