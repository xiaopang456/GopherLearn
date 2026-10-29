package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// The indexer publishes the derived retrieval view after MySQL has committed.
// Failure is recorded on that exact Episode version and never turns a
// successful projection into another model retry: the card is durable, and the
// view can always be rebuilt from the authority.

// EpisodeIndexDocument is one card's derived-view payload.
type EpisodeIndexDocument struct {
	EpisodeID     string
	UserID        string
	SessionID     string
	Status        string
	LastMessageAt int64 // unix seconds, sortable
	Title         string
	TopicKeys     []string
	SearchText    string
	Identity      string
	Vector        []float32
}

// EpisodeVectorIndex is the retrieval view behind episode recall. Two
// implementations exist: the Redis-backed one for semantic deployments, and a
// process-local one for the deterministic mode, whose content dies with the
// process and is warmed from the authority at startup.
type EpisodeVectorIndex interface {
	EnsureIndex(ctx context.Context) error
	Store(ctx context.Context, document EpisodeIndexDocument) error
	Delete(ctx context.Context, userID string, episodeIDs []string) error
	SearchByVector(ctx context.Context, userID, currentSession, identity string, vector []float32, topK int) ([]EpisodeSearchHit, error)
	SearchByText(ctx context.Context, userID, currentSession, identity string, terms []string, topK int) ([]EpisodeSearchHit, error)
	SearchExactTopics(ctx context.Context, userID, currentSession, identity string, topics []string, topK int) ([]EpisodeSearchHit, error)
	CountUser(ctx context.Context, userID string) (int, error)
	ResetUser(ctx context.Context, userID string) error
}

// EpisodeSearchHit is one hit from any of the three recall channels.
type EpisodeSearchHit struct {
	EpisodeID     string
	LastMessageAt int64
	Score         float64
	Distance      float64
	Rank          int
}

// EpisodeIndexSource is the authority-side seam the indexer records status on.
type EpisodeIndexSource interface {
	CommitEpisodeIndexStatus(ctx context.Context, userID, episodeID string, version uint64, status domain.EpisodeIndexStatus) error
	ListActiveEpisodesForReindex(ctx context.Context, userID, afterID string, limit int) ([]domain.Episode, error)
	ListAllActiveEpisodes(ctx context.Context, afterID string, limit int) ([]domain.Episode, error)
}

// EpisodeIndexer embeds and retrieves the derived view.
type EpisodeIndexer struct {
	index    EpisodeVectorIndex
	source   EpisodeIndexSource
	embedder retrieval.Embedder
	identity string
}

// NewEpisodeIndexer wires the indexer. The embedder identity is part of every
// stored document and every query filter: a model change is an explicit
// re-index, never a silent accuracy drop.
func NewEpisodeIndexer(index EpisodeVectorIndex, source EpisodeIndexSource, embedder retrieval.Embedder, identity string) (*EpisodeIndexer, error) {
	if index == nil || source == nil || embedder == nil {
		return nil, fmt.Errorf("episode indexer dependencies are required")
	}
	return &EpisodeIndexer{index: index, source: source, embedder: embedder, identity: identity}, nil
}

// Index publishes the derived view for one card. A non-active card's view is
// deleted rather than stored: the view exists for recall, and recall only ever
// answers from active rows.
func (i *EpisodeIndexer) Index(ctx context.Context, episode *domain.Episode) error {
	if episode == nil {
		return nil
	}
	if episode.Status != domain.EpisodeStatusActive {
		return i.index.Delete(ctx, episode.UserID, []string{episode.ID})
	}
	vectors, err := i.embedder.Embed(ctx, []string{episode.SearchText})
	if err == nil && len(vectors) != 1 {
		err = fmt.Errorf("episode embedder returned %d vectors", len(vectors))
	}
	if err == nil && len(vectors[0]) != i.embedder.Dimensions() {
		err = fmt.Errorf("episode embedder returned %d dimensions, want %d", len(vectors[0]), i.embedder.Dimensions())
	}
	if err == nil {
		err = i.index.Store(ctx, EpisodeIndexDocument{
			EpisodeID:     episode.ID,
			UserID:        episode.UserID,
			SessionID:     episode.SessionID,
			Status:        string(episode.Status),
			LastMessageAt: episode.LastMessageAt.UTC().Unix(),
			Title:         episode.Title,
			TopicKeys:     episode.TopicKeys,
			SearchText:    episode.SearchText,
			Identity:      i.identity,
			Vector:        vectors[0],
		})
	}
	status := domain.EpisodeIndexIndexed
	if err != nil {
		status = domain.EpisodeIndexFailed
	}
	// The status is recorded even when the store call failed, so the row
	// honestly says its view is missing instead of silently claiming otherwise.
	statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if statusErr := i.source.CommitEpisodeIndexStatus(statusCtx, episode.UserID, episode.ID, episode.Version, status); statusErr != nil {
		if err != nil {
			return fmt.Errorf("index episode: %v; mark %s: %w", err, status, statusErr)
		}
		return fmt.Errorf("mark episode %s: %w", status, statusErr)
	}
	if err != nil {
		return err
	}
	episode.IndexStatus = status
	return nil
}

// Reindex rebuilds one user's whole derived view. It is the repair path for a
// view that fell behind - an outage, a failed embed, an embedder change.
func (i *EpisodeIndexer) Reindex(ctx context.Context, userID string) (int, error) {
	if strings.TrimSpace(userID) == "" {
		return 0, fmt.Errorf("reindex requires user id")
	}
	if err := i.index.EnsureIndex(ctx); err != nil {
		return 0, err
	}
	var all []domain.Episode
	for after := ""; ; {
		batch, err := i.source.ListActiveEpisodesForReindex(ctx, userID, after, 100)
		if err != nil {
			return 0, err
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		after = batch[len(batch)-1].ID
	}
	if err := i.index.ResetUser(ctx, userID); err != nil {
		return 0, err
	}
	for index := range all {
		if err := i.Index(ctx, &all[index]); err != nil {
			return index, err
		}
	}
	stored, err := i.index.CountUser(ctx, userID)
	if err != nil {
		return len(all), err
	}
	if stored != len(all) {
		return len(all), fmt.Errorf("reindex stored %d view rows for %d active episodes", stored, len(all))
	}
	return len(all), nil
}

// WarmStart rebuilds the process-local view from the authority. The memory
// index's content dies with the process while the rows do not, so a
// deterministic deployment re-reads every active card at startup instead of
// serving recall from an empty view that looks healthy.
func (i *EpisodeIndexer) WarmStart(ctx context.Context) (int, error) {
	if err := i.index.EnsureIndex(ctx); err != nil {
		return 0, err
	}
	warmed := 0
	for after := ""; ; {
		batch, err := i.source.ListAllActiveEpisodes(ctx, after, 100)
		if err != nil {
			return warmed, err
		}
		if len(batch) == 0 {
			break
		}
		for index := range batch {
			if err := i.Index(ctx, &batch[index]); err != nil {
				return warmed, err
			}
			warmed++
		}
		after = batch[len(batch)-1].ID
	}
	return warmed, nil
}

// MemoryEpisodeVectorIndex is the process-local retrieval view used by the
// deterministic mode. Ranking is by surface-term overlap: the deterministic
// embedder carries no semantics, and this index exists to exercise the exact
// same recall rules as the Redis view (channels, fusion, reload) without a
// server, not to claim retrieval quality.
//
// All rules mirror the RediSearch index: owner and status filters, current-
// session exclusion, identity filtering, recency tie-breaks.
type MemoryEpisodeVectorIndex struct {
	mu   sync.RWMutex
	docs map[string]EpisodeIndexDocument
}

// NewMemoryEpisodeVectorIndex builds an empty process-local view.
func NewMemoryEpisodeVectorIndex() *MemoryEpisodeVectorIndex {
	return &MemoryEpisodeVectorIndex{docs: make(map[string]EpisodeIndexDocument)}
}

func memoryEpisodeKey(userID, episodeID string) string {
	return userID + "\x00" + episodeID
}

// EnsureIndex is a no-op: a process-local view has nothing to create.
func (m *MemoryEpisodeVectorIndex) EnsureIndex(_ context.Context) error { return nil }

// Store writes or overwrites one card's view row.
func (m *MemoryEpisodeVectorIndex) Store(_ context.Context, document EpisodeIndexDocument) error {
	if document.EpisodeID == "" || document.UserID == "" {
		return fmt.Errorf("invalid episode index document")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs[memoryEpisodeKey(document.UserID, document.EpisodeID)] = document
	return nil
}

// Delete drops the named cards' view rows.
func (m *MemoryEpisodeVectorIndex) Delete(_ context.Context, userID string, episodeIDs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range episodeIDs {
		delete(m.docs, memoryEpisodeKey(userID, id))
	}
	return nil
}

// CountUser reports how many view rows one user has.
func (m *MemoryEpisodeVectorIndex) CountUser(_ context.Context, userID string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for key := range m.docs {
		if strings.HasPrefix(key, userID+"\x00") {
			count++
		}
	}
	return count, nil
}

// ResetUser removes one user's whole view.
func (m *MemoryEpisodeVectorIndex) ResetUser(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.docs {
		if strings.HasPrefix(key, userID+"\x00") {
			delete(m.docs, key)
		}
	}
	return nil
}

// SearchByVector ranks by cosine similarity over the stored vectors.
func (m *MemoryEpisodeVectorIndex) SearchByVector(_ context.Context, userID, currentSession, identity string, vector []float32, topK int) ([]EpisodeSearchHit, error) {
	if topK <= 0 {
		return nil, fmt.Errorf("invalid episode vector query")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	type scored struct {
		hit   EpisodeSearchHit
		score float64
	}
	var matches []scored
	for _, doc := range m.docs {
		if !memoryEpisodeVisible(doc, userID, currentSession, identity) || len(doc.Vector) == 0 {
			continue
		}
		similarity := cosineSimilarity(vector, doc.Vector)
		if math.IsNaN(similarity) {
			continue
		}
		matches = append(matches, scored{hit: EpisodeSearchHit{
			EpisodeID: doc.EpisodeID, LastMessageAt: doc.LastMessageAt,
		}, score: similarity})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		if matches[i].hit.LastMessageAt != matches[j].hit.LastMessageAt {
			return matches[i].hit.LastMessageAt > matches[j].hit.LastMessageAt
		}
		return matches[i].hit.EpisodeID < matches[j].hit.EpisodeID
	})
	if len(matches) > topK {
		matches = matches[:topK]
	}
	hits := make([]EpisodeSearchHit, 0, len(matches))
	for index, item := range matches {
		item.hit.Distance = 1 - item.score
		item.hit.Rank = index + 1
		hits = append(hits, item.hit)
	}
	return hits, nil
}

// SearchByText ranks by the number of matched terms, with the same recency and
// id tie-breaks the RediSearch path uses.
func (m *MemoryEpisodeVectorIndex) SearchByText(_ context.Context, userID, currentSession, identity string, terms []string, topK int) ([]EpisodeSearchHit, error) {
	if topK <= 0 {
		return nil, fmt.Errorf("invalid episode text query")
	}
	cleanTerms := make([]string, 0, len(terms))
	for _, term := range terms {
		if trimmed := strings.TrimSpace(term); trimmed != "" {
			cleanTerms = append(cleanTerms, strings.ToLower(trimmed))
		}
	}
	if len(cleanTerms) == 0 {
		return []EpisodeSearchHit{}, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	type scored struct {
		hit   EpisodeSearchHit
		score float64
	}
	var matches []scored
	for _, doc := range m.docs {
		if !memoryEpisodeVisible(doc, userID, currentSession, identity) {
			continue
		}
		haystack := strings.ToLower(doc.Title + " " + doc.SearchText + " " + strings.Join(doc.TopicKeys, " "))
		matched := 0
		for _, term := range cleanTerms {
			if strings.Contains(haystack, term) {
				matched++
			}
		}
		if matched == 0 {
			continue
		}
		matches = append(matches, scored{hit: EpisodeSearchHit{
			EpisodeID: doc.EpisodeID, LastMessageAt: doc.LastMessageAt,
		}, score: float64(matched)})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		if matches[i].hit.LastMessageAt != matches[j].hit.LastMessageAt {
			return matches[i].hit.LastMessageAt > matches[j].hit.LastMessageAt
		}
		return matches[i].hit.EpisodeID < matches[j].hit.EpisodeID
	})
	if len(matches) > topK {
		matches = matches[:topK]
	}
	hits := make([]EpisodeSearchHit, 0, len(matches))
	for index, item := range matches {
		item.hit.Score = item.score
		item.hit.Rank = index + 1
		hits = append(hits, item.hit)
	}
	return hits, nil
}

// SearchExactTopics matches normalized topic keys exactly, newest first.
func (m *MemoryEpisodeVectorIndex) SearchExactTopics(_ context.Context, userID, currentSession, identity string, topics []string, topK int) ([]EpisodeSearchHit, error) {
	if topK <= 0 {
		return nil, fmt.Errorf("invalid episode topic query")
	}
	wanted := make(map[string]struct{}, len(topics))
	for _, topic := range topics {
		if trimmed := strings.TrimSpace(topic); trimmed != "" {
			wanted[trimmed] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return []EpisodeSearchHit{}, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var matches []EpisodeSearchHit
	for _, doc := range m.docs {
		if !memoryEpisodeVisible(doc, userID, currentSession, identity) {
			continue
		}
		hit := false
		for _, key := range doc.TopicKeys {
			if _, ok := wanted[key]; ok {
				hit = true
				break
			}
		}
		if hit {
			matches = append(matches, EpisodeSearchHit{EpisodeID: doc.EpisodeID, LastMessageAt: doc.LastMessageAt})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].LastMessageAt != matches[j].LastMessageAt {
			return matches[i].LastMessageAt > matches[j].LastMessageAt
		}
		return matches[i].EpisodeID < matches[j].EpisodeID
	})
	if len(matches) > topK {
		matches = matches[:topK]
	}
	for index := range matches {
		matches[index].Rank = index + 1
	}
	return matches, nil
}

func memoryEpisodeVisible(doc EpisodeIndexDocument, userID, currentSession, identity string) bool {
	if doc.UserID != userID || doc.Status != string(domain.EpisodeStatusActive) || doc.SessionID == currentSession {
		return false
	}
	if identity != "" && doc.Identity != identity {
		return false
	}
	return true
}

func cosineSimilarity(left, right []float32) float64 {
	if len(left) != len(right) {
		return math.NaN()
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		dot += float64(left[index]) * float64(right[index])
		leftNorm += float64(left[index]) * float64(left[index])
		rightNorm += float64(right[index]) * float64(right[index])
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}

// repositoryEpisodeIndexSource adapts the full repository to the indexer's
// authority seam.
type repositoryEpisodeIndexSource struct {
	repo repository.Repository
}

// NewRepositoryEpisodeIndexSource exposes the authority seam so the reindex
// tool can rebuild the view without building the whole system - a reindex needs
// the embedder and the view, never the projector or the verifier.
func NewRepositoryEpisodeIndexSource(repo repository.Repository) EpisodeIndexSource {
	return repositoryEpisodeIndexSource{repo: repo}
}

func (s repositoryEpisodeIndexSource) CommitEpisodeIndexStatus(ctx context.Context, userID, episodeID string, version uint64, status domain.EpisodeIndexStatus) error {
	return s.repo.CommitEpisodeIndexStatus(ctx, userID, episodeID, version, status)
}

func (s repositoryEpisodeIndexSource) ListActiveEpisodesForReindex(ctx context.Context, userID, afterID string, limit int) ([]domain.Episode, error) {
	return s.repo.ListActiveEpisodesForReindex(ctx, userID, afterID, limit)
}

func (s repositoryEpisodeIndexSource) ListAllActiveEpisodes(ctx context.Context, afterID string, limit int) ([]domain.Episode, error) {
	return s.repo.ListAllActiveEpisodes(ctx, afterID, limit)
}
