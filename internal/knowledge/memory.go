package knowledge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// MemoryStore is a concurrency-safe Store for tests and database-free local
// development. It keeps only the current version of a document's chunks, so a
// replaced document cannot be answered out of a retired chunk.
type MemoryStore struct {
	mu sync.RWMutex

	collections     map[string]domain.Collection
	collectionOrder []string
	documents       map[string]domain.Document
	documentOrder   []string
	chunks          map[string][]domain.DocumentChunk
	limits          Limits
	sequence        uint64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		collections: make(map[string]domain.Collection),
		documents:   make(map[string]domain.Document),
		chunks:      make(map[string][]domain.DocumentChunk),
		limits:      DefaultLimits(),
	}
}

// WithLimits replaces the per-user ceilings. It exists so a test can reach a
// quota boundary with three documents instead of a hundred; production takes the
// defaults.
func (s *MemoryStore) WithLimits(limits Limits) *MemoryStore {
	s.limits = limits
	return s
}

var _ Store = (*MemoryStore)(nil)

func (s *MemoryStore) CreateCollection(ctx context.Context, collection *domain.Collection) (*domain.Collection, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if collection == nil {
		return nil, repository.ErrInvalidArgument
	}
	stored := *collection
	stored.UserID = strings.TrimSpace(stored.UserID)
	stored.Title = strings.TrimSpace(stored.Title)
	if stored.UserID == "" || stored.Title == "" || len([]rune(stored.Title)) > 200 {
		return nil, repository.ErrInvalidArgument
	}
	if stored.ID == "" {
		stored.ID = s.nextID("collection")
	}
	stored.CreatedAt, stored.UpdatedAt = nonZero(stored.CreatedAt), nonZero(stored.UpdatedAt)
	if stored.UpdatedAt.IsZero() {
		stored.UpdatedAt = stored.CreatedAt
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.collections[stored.ID]; exists {
		return nil, repository.ErrConflict
	}
	s.collections[stored.ID] = stored
	s.collectionOrder = append(s.collectionOrder, stored.ID)
	return cloneCollection(stored), nil
}

func (s *MemoryStore) ListCollections(ctx context.Context, userID string) ([]domain.Collection, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]domain.Collection, 0)
	for _, id := range s.collectionOrder {
		if collection, exists := s.collections[id]; exists && collection.UserID == userID {
			result = append(result, collection)
		}
	}
	return result, nil
}

func (s *MemoryStore) GetCollection(ctx context.Context, userID, collectionID string) (*domain.Collection, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(collectionID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	collection, exists := s.ownedCollectionLocked(userID, collectionID)
	if !exists {
		return nil, repository.ErrNotFound
	}
	return cloneCollection(collection), nil
}

func (s *MemoryStore) CreateDocument(ctx context.Context, userID string, document *domain.Document, chunks []domain.DocumentChunk) (*domain.Document, error) {
	staged, err := s.StageDocument(ctx, userID, document, chunks)
	if err != nil {
		return nil, err
	}
	return s.publishStaged(ctx, userID, staged)
}

// publishStaged drives the lifecycle a deployment with no external index would run
// asynchronously: claim, then succeed. It exists so the synchronous entry points stay
// a thin composition over the same state machine instead of a second implementation -
// the two cannot disagree about what "published" means.
func (s *MemoryStore) publishStaged(ctx context.Context, userID string, staged *domain.Document) (*domain.Document, error) {
	claimed, err := s.ClaimForIndexing(ctx, userID, staged.ID, staged.Version)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, ErrIllegalTransition
	}
	if err := s.CompleteIndexing(ctx, userID, staged.ID, staged.Version, IndexOutcome{
		Status: domain.DocumentStatusReady, ChunkCount: len(s.storedChunkCount(staged.ID, staged.Version)),
	}); err != nil {
		return nil, err
	}
	published, err := s.GetDocument(ctx, userID, staged.ID)
	if err != nil {
		return nil, err
	}
	// A first upload publishes cleanly; a rewrite that superseded an older active
	// version lands in cleanup_pending, and the synchronous path finishes that work
	// itself because nothing else is going to.
	if published.IndexStatus == domain.DocumentStatusCleanupPending {
		if err := s.runCleanup(ctx, userID, published); err != nil {
			return nil, err
		}
		return s.GetDocument(ctx, userID, staged.ID)
	}
	return published, nil
}

// runCleanup carries out recorded cleanup for one document, the way a cleanup worker
// would, and leaves the record in place when it cannot.
func (s *MemoryStore) runCleanup(ctx context.Context, userID string, current *domain.Document) error {
	now := time.Now().UTC()
	claimed, err := s.ClaimCleanup(ctx, userID, current.ID, now, now.Add(time.Minute))
	if err != nil || !claimed {
		return err
	}
	return s.CompleteCleanup(ctx, userID, current.ID)
}

func (s *MemoryStore) storedChunkCount(documentID string, version int) []domain.DocumentChunk {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var count []domain.DocumentChunk
	for _, chunk := range s.chunks[documentID] {
		if chunk.Version == version {
			count = append(count, chunk)
		}
	}
	return count
}

func (s *MemoryStore) GetDocument(ctx context.Context, userID, documentID string) (*domain.Document, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(documentID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	document, exists := s.ownedDocumentLocked(userID, documentID)
	if !exists || !liveDocument(document) {
		return nil, repository.ErrNotFound
	}
	return cloneDocument(document), nil
}

func (s *MemoryStore) ListDocuments(ctx context.Context, userID, collectionID string) ([]domain.Document, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(collectionID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, exists := s.ownedCollectionLocked(userID, collectionID); !exists {
		return nil, repository.ErrNotFound
	}
	documents := make([]domain.Document, 0)
	for _, documentID := range s.documentOrder {
		// A deletion that is still being carried out is already nobody's knowledge,
		// and it must not reappear in a listing while its rows are on their way out.
		if document, exists := s.documents[documentID]; exists &&
			document.CollectionID == collectionID && liveDocument(document) {
			documents = append(documents, *cloneDocument(document))
		}
	}
	return documents, nil
}

// DeleteDocument is the composition of the two lifecycle steps a caller has no
// reason to sequence by hand: make the document invisible and release its
// uniqueness, then carry out the removal that was recorded. A store whose second
// step can fail separately (an external index) exposes MarkDeleting and the cleanup
// queue instead, so the record survives the failure.
func (s *MemoryStore) DeleteDocument(ctx context.Context, userID, documentID string) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(documentID) == "" {
		return repository.ErrInvalidArgument
	}
	if _, err := s.MarkDeleting(ctx, userID, documentID); err != nil {
		return err
	}
	return s.CompleteCleanup(ctx, userID, documentID)
}

func (s *MemoryStore) ReplaceDocumentContent(ctx context.Context, userID, documentID string, replacement DocumentReplacement) (*domain.Document, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := validateReplacement(userID, documentID, replacement); err != nil {
		return nil, err
	}
	s.mu.Lock()
	// No expectation: this composition decides the next version from the same locked
	// read it writes with, so there is no earlier observation that could be stale. A
	// caller that read first - the source path names a file for the version it chose -
	// has to go through StageReplacement and pass what it saw, where a moved row is
	// refused instead of being quietly renumbered.
	staged, err := s.stageReplacementLocked(userID, documentID, StageOntoCurrentVersion, replacement)
	s.mu.Unlock()
	if err != nil {
		// ErrDocumentUnchanged travels on to the caller on purpose: "nothing to do"
		// is a decision the caller has to be able to see, because the caller is the
		// one that knows whether an external index still needs writing.
		return staged, err
	}
	published, err := s.publishStaged(ctx, userID, staged)
	if err != nil {
		return nil, err
	}
	return published, nil
}

func (s *MemoryStore) UsageByUser(ctx context.Context, userID string) (Usage, error) {
	if err := checkContext(ctx); err != nil {
		return Usage{}, err
	}
	if strings.TrimSpace(userID) == "" {
		return Usage{}, repository.ErrInvalidArgument
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.usageLocked(userID), nil
}

// usageLocked adds up what one user occupies across every collection they own.
// Rows whose size was never recorded are counted in Unmeasured instead of being
// read as zero bytes, so a quota computed over a partially measured table stays
// visibly approximate rather than looking exact.
func (s *MemoryStore) usageLocked(userID string) Usage {
	var usage Usage
	for _, documentID := range s.documentOrder {
		document, exists := s.documents[documentID]
		if !exists {
			continue
		}
		if _, owned := s.ownedCollectionLocked(userID, document.CollectionID); !owned {
			continue
		}
		// Quotas are spent by documents that are still somebody's knowledge. A
		// deletion in progress has already released its name and hash, so it must
		// release its bytes and its slot too.
		if !liveDocument(document) {
			continue
		}
		usage.DocumentCount++
		if document.FileSize > 0 {
			usage.TotalBytes += document.FileSize
			continue
		}
		usage.Unmeasured++
	}
	return usage
}

// checkUniquenessLocked refuses a title or a recorded content hash that another live
// document of this user already holds. exceptID is the document being rewritten,
// which must not collide with itself - its own name is about to be replaced, not
// taken twice.
func (s *MemoryStore) checkUniquenessLocked(userID string, candidate domain.Document, exceptID string) error {
	for _, documentID := range s.documentOrder {
		document, exists := s.documents[documentID]
		if !exists || document.ID == candidate.ID || document.ID == exceptID || !liveDocument(document) {
			continue
		}
		if _, owned := s.ownedCollectionLocked(userID, document.CollectionID); !owned {
			continue
		}
		if candidate.ContentHash != "" && document.ContentHash == candidate.ContentHash {
			return ErrDocumentContentDuplicate
		}
		if document.Title == candidate.Title {
			return ErrDocumentNameTaken
		}
	}
	return nil
}

func positiveSize(size int64) int64 {
	if size < 0 {
		return 0
	}
	return size
}

func (s *MemoryStore) ChunksFor(ctx context.Context, userID string, collectionIDs []string) ([]domain.DocumentChunk, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureReadableLocked(userID, collectionIDs); err != nil {
		return nil, err
	}

	wanted := make(map[string]bool, len(collectionIDs))
	for _, id := range collectionIDs {
		wanted[id] = true
	}
	result := make([]domain.DocumentChunk, 0)
	for _, documentID := range s.documentOrder {
		document, exists := s.documents[documentID]
		if !exists || !wanted[document.CollectionID] || !document.Retrievable() {
			continue
		}
		// Only the version that actually published answers queries. A staged or
		// failed version has rows here too, and serving them would let a rewrite
		// that never indexed replace the text that was working.
		for _, chunk := range s.chunks[documentID] {
			if chunk.Version == document.ServingVersion() {
				result = append(result, chunk)
			}
		}
	}
	return result, nil
}

func (s *MemoryStore) DocumentTitles(ctx context.Context, userID string, documentIDs []string) (map[string]string, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	titles := make(map[string]string, len(documentIDs))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, documentID := range documentIDs {
		if document, exists := s.ownedDocumentLocked(userID, documentID); exists && liveDocument(document) {
			titles[documentID] = document.Title
		}
	}
	return titles, nil
}

func (s *MemoryStore) EnsureReadable(ctx context.Context, userID string, collectionIDs []string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(userID) == "" {
		return repository.ErrInvalidArgument
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ensureReadableLocked(userID, collectionIDs)
}

func (s *MemoryStore) ensureReadableLocked(userID string, collectionIDs []string) error {
	for _, collectionID := range collectionIDs {
		if _, exists := s.ownedCollectionLocked(userID, collectionID); !exists {
			return repository.ErrNotFound
		}
	}
	return nil
}

func (s *MemoryStore) ownedCollectionLocked(userID, collectionID string) (domain.Collection, bool) {
	collection, exists := s.collections[strings.TrimSpace(collectionID)]
	if !exists || userID == "" || collection.UserID != userID {
		return domain.Collection{}, false
	}
	return collection, true
}

func (s *MemoryStore) ownedDocumentLocked(userID, documentID string) (domain.Document, bool) {
	document, exists := s.documents[documentID]
	if !exists {
		return domain.Document{}, false
	}
	if _, owned := s.ownedCollectionLocked(userID, document.CollectionID); !owned {
		return domain.Document{}, false
	}
	return document, true
}

func (s *MemoryStore) nextID(kind string) string {
	sequence := atomic.AddUint64(&s.sequence, 1)
	return fmt.Sprintf("%s-%016x-%04x", kind, time.Now().UTC().UnixNano(), sequence&0xffff)
}

// normalizeChunks makes the stored ordering and identity of a chunk set the
// store's decision rather than the caller's, so a document's chunks always read
// back in sequence with the version they belong to.
func normalizeChunks(documentID, collectionID string, version int, chunks []domain.DocumentChunk) ([]domain.DocumentChunk, error) {
	result := make([]domain.DocumentChunk, 0, len(chunks))
	for index, chunk := range chunks {
		if strings.TrimSpace(chunk.Text) == "" {
			return nil, repository.ErrInvalidArgument
		}
		// Chunk identity is the store's decision so ids stay unique per document
		// version and never collide across a rewrite.
		chunk.ID = strings.TrimSpace(chunk.ID)
		if chunk.ID == "" {
			chunk.ID = fmt.Sprintf("%s-v%d-c%d", documentID, version, index+1)
		}
		chunk.DocumentID = documentID
		chunk.CollectionID = collectionID
		chunk.Version = version
		chunk.Order = index + 1
		if chunk.CreatedAt.IsZero() {
			chunk.CreatedAt = time.Now().UTC()
		}
		result = append(result, chunk)
	}
	sort.SliceStable(result, func(left, right int) bool { return result[left].Order < result[right].Order })
	return result, nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return repository.ErrInvalidArgument
	}
	return ctx.Err()
}

func nonZero(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func cloneCollection(collection domain.Collection) *domain.Collection {
	copy := collection
	return &copy
}

func cloneDocument(document domain.Document) *domain.Document {
	copy := document
	return &copy
}
