package knowledge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// The lifecycle half of MemoryStore. It mirrors v2's dao/knowledge/document.go
// transitions one-for-one: staged rows carry no active version, a claim is a
// compare-and-swap on (id, user, version, expected status), success publishes the
// version it was claimed for, and a superseded version is queued for cleanup rather
// than dropped inside the transition that superseded it.
//
// Everything here assumes one rule the whole file depends on: a document is visible
// to reads, and occupies uniqueness and quota, exactly while it is not deleting or
// deleted. That is v2's `active_name IS NOT NULL` predicate, written as a status test
// because the target has no generated column to null out.

var _ Lifecycle = (*MemoryStore)(nil)

func (s *MemoryStore) StageDocument(ctx context.Context, userID string, document *domain.Document, chunks []domain.DocumentChunk) (*domain.Document, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if document == nil || len(chunks) == 0 || strings.TrimSpace(userID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	staged := *document
	staged.Title = strings.TrimSpace(staged.Title)
	if staged.Title == "" || !staged.Format.IsValid() || !ValidDocumentMetadata(staged.ContentHash, staged.FileSize) {
		return nil, repository.ErrInvalidArgument
	}
	if staged.ID == "" {
		staged.ID = s.nextID("document")
	}
	staged.Version = 1
	staged.ActiveVersion = 0
	staged.ChunkCount = 0
	staged.IndexStatus = domain.DocumentStatusPending
	staged.ErrorMessage = ""
	staged.CleanupVersion, staged.CleanupAttempts, staged.CleanupAfter = 0, 0, time.Time{}
	staged.CreatedAt, staged.UpdatedAt = nonZero(staged.CreatedAt), nonZero(staged.UpdatedAt)

	normalized, err := normalizeChunks(staged.ID, staged.CollectionID, staged.Version, chunks)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.documents[staged.ID]; exists {
		return nil, repository.ErrConflict
	}
	if _, exists := s.ownedCollectionLocked(userID, staged.CollectionID); !exists {
		return nil, repository.ErrNotFound
	}
	if err := s.checkUniquenessLocked(userID, staged, ""); err != nil {
		return nil, err
	}
	if err := s.limits.CheckCreation(s.usageLocked(userID), positiveSize(staged.FileSize)); err != nil {
		return nil, err
	}
	s.documents[staged.ID] = staged
	s.documentOrder = append(s.documentOrder, staged.ID)
	s.chunks[staged.ID] = normalized
	return cloneDocument(staged), nil
}

func (s *MemoryStore) StageReplacement(ctx context.Context, userID, documentID string, expectedVersion int,
	replacement DocumentReplacement) (*domain.Document, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := validateReplacement(userID, documentID, replacement); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stageReplacementLocked(userID, documentID, expectedVersion, replacement)
}

// stageReplacementLocked advances one document under the store lock. expectation is
// StageOntoCurrentVersion only for the composition inside this package, which decides the
// next version from the same locked read it writes with and therefore has no earlier
// observation to be stale about. Any other value is compared before anything else is
// judged, in v2's order (dao StageUpdate: version, then status, then hash).
func (s *MemoryStore) stageReplacementLocked(userID, documentID string, expectation int,
	replacement DocumentReplacement) (*domain.Document, error) {
	title := strings.TrimSpace(replacement.Title)
	current, exists := s.ownedDocumentLocked(userID, documentID)
	if !exists {
		return nil, repository.ErrNotFound
	}
	if expectation != StageOntoCurrentVersion && current.Version != expectation {
		return nil, fmt.Errorf("%w: document %s is at version %d, the request was built on %d",
			ErrDocumentChanged, documentID, current.Version, expectation)
	}
	// v2 refuses an update to anything that is not ready (service/knowledge/
	// document.go:149-151). Without that refusal a rewrite could be staged on top of
	// a pass that is still running, or on top of a delete that is being carried out.
	if current.IndexStatus != domain.DocumentStatusReady {
		return nil, ErrDocumentBusy
	}
	// Unchanged is only allowed to short-circuit a healthy document, and only against
	// the version that is actually answering queries - not against a staged one that
	// never published. Comparing against Version instead strands a document whose
	// index rows were never written, because the retry would look like a no-op.
	if replacement.ContentHash != "" && replacement.ContentHash == current.ContentHash &&
		current.ActiveVersion == current.Version {
		return cloneDocument(current), ErrDocumentUnchanged
	}
	staged := current
	staged.Title = firstNonEmpty(title, current.Title)
	staged.Format = replacement.Format
	staged.Version = current.Version + 1
	staged.ActiveVersion = current.ActiveVersion
	staged.ChunkCount = current.ChunkCount
	staged.ContentHash = replacement.ContentHash
	if replacement.FileSize > 0 {
		staged.FileSize = replacement.FileSize
	}
	staged.IndexStatus = domain.DocumentStatusPending
	staged.ErrorMessage = ""
	staged.CleanupVersion, staged.CleanupAttempts, staged.CleanupAfter = 0, 0, time.Time{}
	staged.UpdatedAt = time.Now().UTC()

	normalized, err := normalizeChunks(staged.ID, staged.CollectionID, staged.Version, replacement.Chunks)
	if err != nil {
		return nil, err
	}
	if err := s.checkUniquenessLocked(userID, staged, current.ID); err != nil {
		return nil, err
	}
	if err := s.limits.CheckReplacement(s.usageLocked(userID), positiveSize(replacement.FileSize), positiveSize(current.FileSize)); err != nil {
		return nil, err
	}
	// The previous version's chunks stay: they are what keeps answering until this
	// one publishes, and what cleanup has to remove afterwards.
	s.documents[staged.ID] = staged
	s.chunks[staged.ID] = append(s.chunks[staged.ID], normalized...)
	return cloneDocument(staged), nil
}

// validateReplacement is the half of the check that needs no lock: an id, a format and a
// chunk set that could not be stored are refused before any file is named after them.
func validateReplacement(userID, documentID string, replacement DocumentReplacement) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(documentID) == "" || len(replacement.Chunks) == 0 {
		return repository.ErrInvalidArgument
	}
	if !replacement.Format.IsValid() || !ValidDocumentMetadata(replacement.ContentHash, replacement.FileSize) {
		return repository.ErrInvalidArgument
	}
	if replacement.Title != "" && strings.TrimSpace(replacement.Title) == "" {
		return repository.ErrInvalidArgument
	}
	return nil
}

func (s *MemoryStore) ClaimForIndexing(ctx context.Context, userID, documentID string, version int) (bool, error) {
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.ownedDocumentLocked(userID, documentID)
	if !exists || current.Version != version || current.IndexStatus != domain.DocumentStatusPending {
		return false, nil
	}
	current.IndexStatus = domain.DocumentStatusIndexing
	current.ErrorMessage = ""
	current.UpdatedAt = time.Now().UTC()
	s.documents[current.ID] = current
	return true, nil
}

func (s *MemoryStore) CompleteIndexing(ctx context.Context, userID, documentID string, version int, outcome IndexOutcome) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if !outcome.Status.Valid() {
		return repository.ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.ownedDocumentLocked(userID, documentID)
	if !exists || current.Version != version || current.IndexStatus != domain.DocumentStatusIndexing {
		return ErrIllegalTransition
	}
	switch outcome.Status {
	case domain.DocumentStatusReady:
		if outcome.ChunkCount <= 0 {
			return repository.ErrInvalidArgument
		}
		superseded := current.ActiveVersion
		current.ActiveVersion = version
		current.ChunkCount = outcome.ChunkCount
		current.ErrorMessage = ""
		current.IndexStatus = domain.DocumentStatusReady
		if superseded > 0 && superseded != version {
			// The older version is live no more, but its rows are still here: queue
			// their removal instead of deleting them inside this transition.
			current.IndexStatus = domain.DocumentStatusCleanupPending
			current.CleanupVersion = superseded
			current.CleanupAttempts = 0
			current.CleanupAfter = time.Now().UTC()
		}
	case domain.DocumentStatusFailed:
		// ActiveVersion is untouched, so the text that was answering before this
		// attempt keeps answering.
		current.IndexStatus = domain.DocumentStatusFailed
		current.ErrorMessage = outcome.ErrorMessage
	default:
		return repository.ErrInvalidArgument
	}
	current.UpdatedAt = time.Now().UTC()
	s.documents[current.ID] = current
	return nil
}

func (s *MemoryStore) RequeueIndexing(ctx context.Context, userID, documentID string, version int) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.ownedDocumentLocked(userID, documentID)
	if !exists || current.Version != version || current.IndexStatus != domain.DocumentStatusIndexing {
		return ErrIllegalTransition
	}
	current.IndexStatus = domain.DocumentStatusPending
	current.ErrorMessage = ""
	current.UpdatedAt = time.Now().UTC()
	s.documents[current.ID] = current
	return nil
}

// RecoverInterruptedIndexing is the startup pass v2 runs before any worker starts
// (service/knowledge/worker.go:84, dao/knowledge/document.go:323-331): nothing from
// the previous process can still be running, so every claimed row goes back to the
// queue. Running it again moves nothing, which is what makes it safe to call from
// more than one place.
func (s *MemoryStore) RecoverInterruptedIndexing(ctx context.Context) (int, error) {
	if err := checkContext(ctx); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recovered := 0
	for id, current := range s.documents {
		if current.IndexStatus != domain.DocumentStatusIndexing {
			continue
		}
		current.IndexStatus = domain.DocumentStatusPending
		current.ErrorMessage = "recovered after interrupted indexing"
		current.UpdatedAt = time.Now().UTC()
		s.documents[id] = current
		recovered++
	}
	return recovered, nil
}

func (s *MemoryStore) RetryFailed(ctx context.Context, userID, documentID string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.ownedDocumentLocked(userID, documentID)
	if !exists {
		return repository.ErrNotFound
	}
	if current.IndexStatus != domain.DocumentStatusFailed {
		return ErrDocumentNotRetryable
	}
	current.IndexStatus = domain.DocumentStatusPending
	current.ErrorMessage = ""
	current.UpdatedAt = time.Now().UTC()
	s.documents[current.ID] = current
	return nil
}

func (s *MemoryStore) StagedChunks(ctx context.Context, userID, documentID string, version int) ([]domain.DocumentChunk, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(documentID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	current, exists := s.ownedDocumentLocked(userID, documentID)
	if !exists {
		return nil, repository.ErrNotFound
	}
	found := make([]domain.DocumentChunk, 0, len(s.chunks[documentID]))
	for _, chunk := range s.chunks[documentID] {
		if chunk.Version == version {
			found = append(found, chunk)
		}
	}
	_ = current
	return found, nil
}

func (s *MemoryStore) ListPending(ctx context.Context, limit int) ([]QueuedDocument, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, repository.ErrInvalidArgument
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	found := make([]QueuedDocument, 0, limit)
	// documentOrder is creation order, which is the queue order v2 gets from
	// ORDER BY updated_at ASC: oldest staged work first.
	for _, id := range s.documentOrder {
		current, exists := s.documents[id]
		if !exists || current.IndexStatus != domain.DocumentStatusPending {
			continue
		}
		owner, known := s.collectionOwnerLocked(current.CollectionID)
		if !known {
			continue
		}
		found = append(found, QueuedDocument{UserID: owner, Document: current})
		if len(found) == limit {
			break
		}
	}
	return found, nil
}

// collectionOwnerLocked answers the queue's one cross-user question. A document whose
// collection is gone has no owner to claim it under, so it leaves the queue rather than
// joining it with an empty user - a work item that can never be claimed would be picked
// up forever and never finished.
func (s *MemoryStore) collectionOwnerLocked(collectionID string) (string, bool) {
	collection, exists := s.collections[collectionID]
	if !exists || collection.UserID == "" {
		return "", false
	}
	return collection.UserID, true
}

func (s *MemoryStore) MarkDeleting(ctx context.Context, userID, documentID string) (*domain.Document, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.ownedDocumentLocked(userID, documentID)
	if !exists {
		return nil, repository.ErrNotFound
	}
	if current.IndexStatus == domain.DocumentStatusIndexing {
		return nil, ErrDocumentBusy
	}
	current.IndexStatus = domain.DocumentStatusDeleting
	current.ActiveVersion = 0
	// Zero with a deleting status means "every version this document ever had".
	current.CleanupVersion = 0
	current.CleanupAttempts = 0
	current.CleanupAfter = time.Now().UTC()
	current.UpdatedAt = time.Now().UTC()
	s.documents[current.ID] = current
	// The name and the content hash are released the moment it stops being visible,
	// which is what lets the same notes be uploaded again straight away.
	return cloneDocument(current), nil
}

func (s *MemoryStore) ListDueCleanup(ctx context.Context, now time.Time, limit int) ([]QueuedDocument, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, repository.ErrInvalidArgument
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	found := make([]QueuedDocument, 0, limit)
	for _, id := range s.documentOrder {
		current, exists := s.documents[id]
		if !exists || !cleanupPending(current) {
			continue
		}
		if !current.CleanupAfter.IsZero() && current.CleanupAfter.After(now) {
			continue
		}
		owner, known := s.collectionOwnerLocked(current.CollectionID)
		if !known {
			continue
		}
		found = append(found, QueuedDocument{UserID: owner, Document: current})
		if len(found) == limit {
			break
		}
	}
	return found, nil
}

func (s *MemoryStore) ClaimCleanup(ctx context.Context, userID, documentID string, now, leaseUntil time.Time) (bool, error) {
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.ownedDocumentLocked(userID, documentID)
	if !exists || !cleanupPending(current) {
		return false, nil
	}
	if !current.CleanupAfter.IsZero() && current.CleanupAfter.After(now) {
		return false, nil
	}
	current.CleanupAfter = leaseUntil
	s.documents[current.ID] = current
	return true, nil
}

func (s *MemoryStore) CompleteCleanup(ctx context.Context, userID, documentID string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.ownedDocumentLocked(userID, documentID)
	if !exists || !cleanupPending(current) {
		return ErrIllegalTransition
	}
	if current.IndexStatus == domain.DocumentStatusDeleting {
		s.removeDocumentLocked(current.ID)
		return nil
	}
	dropped := current.CleanupVersion
	remaining := make([]domain.DocumentChunk, 0, len(s.chunks[current.ID]))
	for _, chunk := range s.chunks[current.ID] {
		if chunk.Version != dropped {
			remaining = append(remaining, chunk)
		}
	}
	s.chunks[current.ID] = remaining
	current.IndexStatus = domain.DocumentStatusReady
	current.CleanupVersion, current.CleanupAttempts, current.CleanupAfter = 0, 0, time.Time{}
	current.ErrorMessage = ""
	current.UpdatedAt = time.Now().UTC()
	s.documents[current.ID] = current
	return nil
}

func (s *MemoryStore) FailCleanup(ctx context.Context, userID, documentID string, message string, retryAt time.Time) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.ownedDocumentLocked(userID, documentID)
	if !exists || !cleanupPending(current) {
		return ErrIllegalTransition
	}
	current.CleanupAttempts++
	current.CleanupAfter = retryAt
	current.ErrorMessage = message
	current.UpdatedAt = time.Now().UTC()
	s.documents[current.ID] = current
	return nil
}

// removeDocumentLocked drops the document and every version of its chunks.
func (s *MemoryStore) removeDocumentLocked(documentID string) {
	delete(s.documents, documentID)
	delete(s.chunks, documentID)
	for index, storedID := range s.documentOrder {
		if storedID == documentID {
			s.documentOrder = append(s.documentOrder[:index], s.documentOrder[index+1:]...)
			break
		}
	}
}

// cleanupPending is the queue v2 keeps in index_status: a superseded version waiting
// to be dropped, or a deletion waiting for its rows to go.
func cleanupPending(current domain.Document) bool {
	return current.IndexStatus == domain.DocumentStatusCleanupPending || current.IndexStatus == domain.DocumentStatusDeleting
}

// liveDocument reports the rows that are still somebody's knowledge: v2's
// active_name IS NOT NULL.
func liveDocument(current domain.Document) bool {
	return current.IndexStatus != domain.DocumentStatusDeleting && current.IndexStatus != domain.DocumentStatusDeleted
}

func firstNonEmpty(preferred, fallback string) string {
	if preferred != "" {
		return preferred
	}
	return fallback
}
