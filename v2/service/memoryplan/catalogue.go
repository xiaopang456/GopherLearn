package memoryplan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"GopherAI/common/rag"
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
)

// CatalogueSchemaVersion pins the snapshot contract.
const CatalogueSchemaVersion = "memory.catalogue.v1"

// The catalogue bounds. Topic groups match the existing TopicCatalogue limit;
// episodes and tokens are this task's own.
//
// Overflow is made explicit rather than hidden by a bigger window. A catalogue
// that silently truncated would let "the planner did not ask for it" and "the
// planner was never shown it" become the same observation, and those are
// different failures with different fixes.
const (
	MaxCatalogueTopicGroups = 100
	MaxCatalogueEpisodes    = 120
	MaxCatalogueTokens      = 12000
)

// TopicEntry is one topic the learner has notes under. It carries subjects, not
// bodies: a catalogue that shipped full text would be the whole memory store in
// the prompt, and the budget the design refuses to raise is the reason there are
// two calls rather than one.
type TopicEntry struct {
	TopicKey        string   `json:"topic_key"`
	Subjects        []string `json:"subjects"`
	MemoryIDs       []string `json:"memory_ids"`
	LatestUpdatedAt string   `json:"latest_updated_at"`
}

// EpisodeEntry is one past session's projection, in metadata only.
type EpisodeEntry struct {
	EpisodeID     string   `json:"episode_id"`
	OccurredAt    string   `json:"occurred_at"`
	Title         string   `json:"title"`
	TopicKeys     []string `json:"topic_keys"`
	EvidenceKinds []string `json:"evidence_kinds"`
}

// CatalogueLimits is what the snapshot was bounded by, echoed into the prompt so
// the record of a run says what the model could have seen.
type CatalogueLimits struct {
	TopicGroups     int `json:"topic_groups"`
	Episodes        int `json:"episodes"`
	CatalogueTokens int `json:"catalogue_tokens"`
}

// MemoryCatalogueSnapshot is the read-only projection the Planner sees.
type MemoryCatalogueSnapshot struct {
	SchemaVersion    string          `json:"schema_version"`
	CatalogueVersion string          `json:"catalogue_version"`
	TopicComplete    bool            `json:"topic_catalogue_complete"`
	EpisodeComplete  bool            `json:"episode_catalogue_complete"`
	Topics           []TopicEntry    `json:"topics"`
	Episodes         []EpisodeEntry  `json:"episodes"`
	Limits           CatalogueLimits `json:"limits"`
	// OldestVisible says how far back the catalogue reaches when it overflowed.
	// A truncated catalogue that did not say where it stopped would be
	// indistinguishable from a complete one that found nothing older.
	OldestVisible string `json:"oldest_visible,omitempty"`
}

// Index is what the plan validator checks a plan against.
func (s MemoryCatalogueSnapshot) Index() CatalogueIndex {
	index := CatalogueIndex{
		TopicKeys:  make(map[string]struct{}, len(s.Topics)),
		EpisodeIDs: make(map[string]struct{}, len(s.Episodes)),
	}
	for _, topic := range s.Topics {
		index.TopicKeys[topic.TopicKey] = struct{}{}
	}
	for _, episode := range s.Episodes {
		index.EpisodeIDs[episode.EpisodeID] = struct{}{}
	}
	return index
}

// Render is the exact text the Planner is given.
func (s MemoryCatalogueSnapshot) Render() (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("render memory catalogue: %w", err)
	}
	return string(data), nil
}

// Tokens estimates the catalogue's prompt cost with the same estimator the
// context builder budgets against.
func (s MemoryCatalogueSnapshot) Tokens() int {
	rendered, err := s.Render()
	if err != nil {
		return 0
	}
	return rag.EstimateTokens(rendered)
}

// CatalogueTopicReader is the MySQL side of the topic half.
type CatalogueTopicReader interface {
	ListTopicKeys(context.Context, int64, int) ([]memorydao.TopicKeyRow, error)
	ListByTopicKey(context.Context, int64, model.MemoryScope, string, float64, int) ([]model.LongTermMemory, error)
}

// CatalogueEpisodeReader is the MySQL side of the episode half.
//
// It is ListByTime rather than a bespoke query on purpose: that method already
// excludes the current session, already applies the transitive source closure,
// and already orders by LastMessageAt then id. Writing a second query here would
// mean two definitions of "which episodes exist", and the one in the catalogue
// would be the one nobody re-checked.
type CatalogueEpisodeReader interface {
	ListByTime(context.Context, int64, string, time.Time, time.Time, int) ([]model.EpisodeMemory, error)
}

// CatalogueBuilder derives the snapshot from MySQL.
type CatalogueBuilder struct {
	topics        CatalogueTopicReader
	episodes      CatalogueEpisodeReader
	minConfidence float64
}

func NewCatalogueBuilder(topics CatalogueTopicReader, episodes CatalogueEpisodeReader, minConfidence float64) (*CatalogueBuilder, error) {
	if topics == nil || episodes == nil {
		return nil, fmt.Errorf("catalogue builder needs a topic and an episode reader")
	}
	if minConfidence < 0 || minConfidence > 1 {
		return nil, fmt.Errorf("catalogue minimum confidence must be between 0 and 1")
	}
	return &CatalogueBuilder{topics: topics, episodes: episodes, minConfidence: minConfidence}, nil
}

// Build reads the learner's catalogue.
//
// Everything it lists comes from a MySQL query whose predicate already excludes
// other accounts, archived, deleted, superseded and expired rows, and whose
// episode half already applies the source closure. The catalogue is therefore
// not a second definition of what is valid; it is a projection of the one that
// already exists, which is why the reload after the plan can be an assertion
// rather than a filter.
func (b *CatalogueBuilder) Build(ctx context.Context, userID int64, currentSession string, now time.Time) (MemoryCatalogueSnapshot, error) {
	if userID <= 0 || strings.TrimSpace(currentSession) == "" {
		return MemoryCatalogueSnapshot{}, fmt.Errorf("catalogue needs an owner and the current session")
	}
	snapshot := MemoryCatalogueSnapshot{
		SchemaVersion: CatalogueSchemaVersion,
		TopicComplete: true, EpisodeComplete: true,
		Topics: []TopicEntry{}, Episodes: []EpisodeEntry{},
		Limits: CatalogueLimits{
			TopicGroups: MaxCatalogueTopicGroups, Episodes: MaxCatalogueEpisodes,
			CatalogueTokens: MaxCatalogueTokens,
		},
	}

	// One extra row is fetched on purpose: it is how "exactly at the limit" and
	// "more than the limit" are told apart without a second count query.
	rows, err := b.topics.ListTopicKeys(ctx, userID, MaxCatalogueTopicGroups+1)
	if err != nil {
		return MemoryCatalogueSnapshot{}, fmt.Errorf("list catalogue topics: %w", err)
	}
	if len(rows) > MaxCatalogueTopicGroups {
		snapshot.TopicComplete = false
		rows = rows[:MaxCatalogueTopicGroups]
	}
	for _, row := range rows {
		memories, err := b.topics.ListByTopicKey(ctx, userID, model.MemoryScopeTopic, row.TopicKey,
			b.minConfidence, MaxPerTopicMemory)
		if err != nil {
			return MemoryCatalogueSnapshot{}, fmt.Errorf("list catalogue topic %q: %w", row.TopicKey, err)
		}
		entry := TopicEntry{TopicKey: row.TopicKey}
		var latest time.Time
		for _, memory := range memories {
			if !eligibleMemory(memory, userID, now, b.minConfidence) {
				continue
			}
			entry.Subjects = append(entry.Subjects, strings.TrimSpace(memory.Subject))
			entry.MemoryIDs = append(entry.MemoryIDs, memory.ID)
			if memory.UpdatedAt.After(latest) {
				latest = memory.UpdatedAt
			}
		}
		if len(entry.MemoryIDs) == 0 {
			continue
		}
		if !latest.IsZero() {
			entry.LatestUpdatedAt = latest.UTC().Format(time.RFC3339)
		}
		snapshot.Topics = append(snapshot.Topics, entry)
	}

	episodes, err := b.episodes.ListByTime(ctx, userID, currentSession, time.Time{}, time.Time{}, MaxCatalogueEpisodes+1)
	if err != nil {
		return MemoryCatalogueSnapshot{}, fmt.Errorf("list catalogue episodes: %w", err)
	}
	if len(episodes) > MaxCatalogueEpisodes {
		snapshot.EpisodeComplete = false
		episodes = episodes[:MaxCatalogueEpisodes]
	}
	for _, episode := range episodes {
		snapshot.Episodes = append(snapshot.Episodes, EpisodeEntry{
			EpisodeID: episode.ID, OccurredAt: episode.LastMessageAt.UTC().Format(time.RFC3339),
			Title: strings.TrimSpace(episode.Title), TopicKeys: append([]string(nil), episode.TopicKeys...),
			EvidenceKinds: evidenceKindsOf(episode),
		})
	}
	if !snapshot.EpisodeComplete && len(snapshot.Episodes) > 0 {
		snapshot.OldestVisible = snapshot.Episodes[len(snapshot.Episodes)-1].OccurredAt
	}

	// Token overflow drops whole episodes from the tail, oldest first, because
	// the episode half is the one ordered by time and therefore the one where
	// "the rest is older than this" is a true statement.
	for snapshot.Tokens() > MaxCatalogueTokens && len(snapshot.Episodes) > 0 {
		snapshot.EpisodeComplete = false
		snapshot.OldestVisible = snapshot.Episodes[len(snapshot.Episodes)-1].OccurredAt
		snapshot.Episodes = snapshot.Episodes[:len(snapshot.Episodes)-1]
	}
	for snapshot.Tokens() > MaxCatalogueTokens && len(snapshot.Topics) > 0 {
		snapshot.TopicComplete = false
		snapshot.Topics = snapshot.Topics[:len(snapshot.Topics)-1]
	}
	snapshot.CatalogueVersion = catalogueVersion(snapshot)
	return snapshot, nil
}

// catalogueVersion is the fingerprint of the ordered metadata.
//
// It covers the ids, the order and the completeness flags but no body text, so
// it can be logged and cached on. Identical data must produce an identical
// version, which is what lets a cache key say "this user, this catalogue" and
// what lets two arms prove they planned over the same thing.
func catalogueVersion(snapshot MemoryCatalogueSnapshot) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "v=%s|tc=%t|ec=%t|", snapshot.SchemaVersion, snapshot.TopicComplete, snapshot.EpisodeComplete)
	for _, topic := range snapshot.Topics {
		fmt.Fprintf(&builder, "t:%s:%s:%s|", topic.TopicKey, strings.Join(topic.MemoryIDs, ","), topic.LatestUpdatedAt)
	}
	for _, episode := range snapshot.Episodes {
		fmt.Fprintf(&builder, "e:%s:%s:%s|", episode.EpisodeID, episode.OccurredAt,
			strings.Join(episode.EvidenceKinds, ","))
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return "sha256-" + hex.EncodeToString(sum[:])
}

func evidenceKindsOf(episode model.EpisodeMemory) []string {
	kinds := make([]string, 0, 4)
	if strings.TrimSpace(episode.Document.Synopsis) != "" {
		kinds = append(kinds, string(EvidenceSynopsis))
	}
	if len(episode.Document.Decisions) > 0 {
		kinds = append(kinds, string(EvidenceDecision))
	}
	if len(episode.Document.OpenLoops) > 0 {
		kinds = append(kinds, string(EvidenceOpenLoop))
	}
	if len(episode.Document.UserCorrections) > 0 {
		kinds = append(kinds, string(EvidenceCorrection))
	}
	sort.Strings(kinds)
	return kinds
}

// eligibleMemory repeats the production recall predicate in the process.
//
// The SQL already applies it; this is the assertion that the row in hand is the
// row the predicate described. A fixture or a migration that produced a row the
// query should never have returned fails here rather than reaching a prompt.
func eligibleMemory(memory model.LongTermMemory, userID int64, now time.Time, minConfidence float64) bool {
	return memory.UserID == userID &&
		memory.Status == model.LongTermMemoryActive &&
		memory.Scope == model.MemoryScopeTopic &&
		memory.Confidence >= minConfidence &&
		!memory.ExpiredAt(now) &&
		strings.TrimSpace(memory.TopicKey) != ""
}
