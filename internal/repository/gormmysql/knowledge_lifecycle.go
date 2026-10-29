package gormmysql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// The lifecycle half of the GORM store mirrors v2's dao/knowledge/document.go
// transitions: pending -> indexing -> ready|failed, every move a compare-and-swap on
// (id, version, expected status) scoped to the calling user, an active version only
// success advances, a superseded version queued for cleanup rather than dropped inside
// the transition that superseded it, and a startup pass that returns rows stuck in
// indexing to the queue.

var _ knowledge.Lifecycle = (*KnowledgeRepository)(nil)

// ownedBy scopes a single-table knowledge_documents statement to one user. Ownership
// lives on the collection, so this is a subquery rather than a JOIN repeated in every
// update and delete.
func ownedBy(userID string) (string, []any) {
	return "collection_id IN (SELECT id FROM knowledge_collections WHERE user_id = ?)", []any{userID}
}

// servedVersion is the Go twin of servingVersionSQL and of
// domain.Document.ServingVersion: one rule, three places it has to agree with.
func (r DocumentRecord) servedVersion() int {
	if r.IndexStatus == "" {
		return r.Version
	}
	return r.ActiveVersion
}

func (r *KnowledgeRepository) StageDocument(ctx context.Context, userID string, document *domain.Document, chunks []domain.DocumentChunk) (*domain.Document, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || document == nil || len(chunks) == 0 || strings.TrimSpace(userID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	// Chunk content is validated before anything is written: an empty chunk would be
	// stored, counted and then invisible to retrieval, which is a document that
	// reports success and answers nothing.
	if err := validateChunkSet(chunks); err != nil {
		return nil, err
	}
	record := DocumentRecord{
		ID:           document.ID,
		CollectionID: strings.TrimSpace(document.CollectionID),
		Title:        strings.TrimSpace(document.Title),
		Format:       string(document.Format),
	}
	if validateTitle(record.Title) != nil || !knowledge.ValidDocumentMetadata(document.ContentHash, document.FileSize) {
		return nil, repository.ErrInvalidArgument
	}
	if record.CollectionID == "" || !domain.DocumentFormat(record.Format).IsValid() {
		return nil, repository.ErrInvalidArgument
	}
	if record.ID == "" {
		record.ID = newID("document")
	}
	// Staged, not published: no active version, so nothing of it answers queries yet.
	record.Version, record.ActiveVersion = 1, 0
	record.ChunkCount, record.ErrorMessage = 0, ""
	record.IndexStatus = string(domain.DocumentStatusPending)
	record.CleanupVersion, record.CleanupAttempts, record.CleanupAfter = 0, 0, nil
	record.ContentHash, record.FileSize = documentMetadata(document.ContentHash, document.FileSize)
	record.CreatedAt, record.UpdatedAt = clockTime(document.CreatedAt), clockTime(document.UpdatedAt)
	chunkRecords := chunkRecordsFor(record.ID, record.CollectionID, record.Version, chunks)

	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := lockUserRow(tx, userID); err != nil {
			return err
		}
		if err := requireOwnedCollection(tx, userID, record.CollectionID); err != nil {
			return err
		}
		if err := requireUniqueDocument(tx, userID, record, ""); err != nil {
			return err
		}
		usage, err := usageForUser(tx, userID)
		if err != nil {
			return err
		}
		if err := r.limits.CheckCreation(usage, positiveBytes(record.FileSize)); err != nil {
			return err
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return tx.CreateInBatches(&chunkRecords, 100).Error
	})
	if err := translate(err); err != nil {
		return nil, err
	}
	staged := record.toDomain()
	return &staged, nil
}

func (r *KnowledgeRepository) StageReplacement(ctx context.Context, userID, documentID string, expectedVersion int,
	replacement knowledge.DocumentReplacement) (*domain.Document, error) {
	return r.stageReplacementTx(ctx, userID, documentID, expectedVersion, replacement)
}

// stageReplacementTx is v2's StageUpdate. expectation is knowledge.StageOntoCurrentVersion
// only for this package's own composition, which has no observation to be stale about
// because it decides the next version inside the same transaction that locks the row.
func (r *KnowledgeRepository) stageReplacementTx(ctx context.Context, userID, documentID string,
	expectation int, replacement knowledge.DocumentReplacement) (*domain.Document, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(documentID) == "" || len(replacement.Chunks) == 0 {
		return nil, repository.ErrInvalidArgument
	}
	if !replacement.Format.IsValid() || !knowledge.ValidDocumentMetadata(replacement.ContentHash, replacement.FileSize) {
		return nil, repository.ErrInvalidArgument
	}
	title := strings.TrimSpace(replacement.Title)
	if replacement.Title != "" && title == "" {
		return nil, repository.ErrInvalidArgument
	}
	if err := validateChunkSet(replacement.Chunks); err != nil {
		return nil, err
	}
	var staged DocumentRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		// Lock, then read, then decide: see lockUserRow.
		if err := lockUserRow(tx, userID); err != nil {
			return err
		}
		var current DocumentRecord
		err := tx.Table("knowledge_documents").
			Joins("JOIN knowledge_collections ON knowledge_collections.id = knowledge_documents.collection_id").
			Where("knowledge_documents.id = ? AND knowledge_collections.user_id = ?", documentID, userID).
			Take(&current).Error
		if err != nil {
			return err
		}
		// v2's ErrDocumentChanged check (dao/knowledge/document.go:56-58), in the same
		// position: after the row is locked, before the status and hash judgements. A
		// request that names a source file for one version must not be renumbered onto
		// the next one - that file belongs to a version somebody else already committed.
		if expectation != knowledge.StageOntoCurrentVersion && current.Version != expectation {
			return fmt.Errorf("%w: document %s is at version %d, the request was built on %d",
				knowledge.ErrDocumentChanged, documentID, current.Version, expectation)
		}
		if normalizeStatus(current) != domain.DocumentStatusReady {
			return knowledge.ErrDocumentBusy
		}
		// Unchanged short-circuits only a healthy document measured against the
		// version that is actually answering. Comparing against the staged version
		// would let a rewrite whose index rows were never written be reported as a
		// no-op forever.
		if replacement.ContentHash != "" && replacement.ContentHash == dereferenceString(current.ContentHash) &&
			current.ActiveVersion == current.Version {
			staged = current
			return knowledge.ErrDocumentUnchanged
		}
		candidate := current
		candidate.Title = firstNonEmpty(title, current.Title)
		candidate.Format = string(replacement.Format)
		candidate.Version = current.Version + 1
		candidate.ChunkCount = 0
		candidate.IndexStatus = string(domain.DocumentStatusPending)
		candidate.ErrorMessage = ""
		candidate.CleanupVersion, candidate.CleanupAttempts, candidate.CleanupAfter = 0, 0, nil
		candidate.ContentHash, candidate.FileSize = documentMetadata(replacement.ContentHash, replacement.FileSize)
		candidate.UpdatedAt = time.Now().UTC()

		if err := requireUniqueDocument(tx, userID, candidate, current.ID); err != nil {
			return err
		}
		usage, err := usageForUser(tx, userID)
		if err != nil {
			return err
		}
		if err := r.limits.CheckReplacement(usage, positiveBytes(candidate.FileSize), positiveBytes(current.FileSize)); err != nil {
			return err
		}
		// The previous version's chunk rows stay where they are: they are what keeps
		// answering until this version publishes, and what cleanup later removes.
		// The row keeps saying which version is serving while the new one is staged -
		// for a legacy row that is its own Version rather than the 0 the column holds,
		// and recording it here is the only moment that fact is still knowable.
		// CompleteIndexing needs it to queue the superseded version for cleanup;
		// without it a legacy document's old rows stay in the table with nothing
		// scheduled to remove them.
		if err := tx.Model(&DocumentRecord{}).Where("knowledge_documents.id = ?", current.ID).Updates(map[string]any{
			"title": candidate.Title, "format": candidate.Format, "version": candidate.Version,
			"active_version": current.servedVersion(),
			"chunk_count":    0, "index_status": candidate.IndexStatus, "error_message": "",
			"content_hash": candidate.ContentHash, "file_size": candidate.FileSize,
			"cleanup_version": 0, "cleanup_attempts": 0, "cleanup_after": nil,
			"updated_at": candidate.UpdatedAt,
		}).Error; err != nil {
			return err
		}
		staged = candidate
		return tx.CreateInBatches(chunkRecordsFor(current.ID, current.CollectionID, candidate.Version, replacement.Chunks), 100).Error
	})
	if err := translate(err); err != nil {
		if errors.Is(err, knowledge.ErrDocumentUnchanged) {
			unchanged := staged.toDomain()
			return &unchanged, knowledge.ErrDocumentUnchanged
		}
		return nil, err
	}
	published := staged.toDomain()
	return &published, nil
}

func (r *KnowledgeRepository) ClaimForIndexing(ctx context.Context, userID, documentID string, version int) (bool, error) {
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return false, repository.ErrInvalidArgument
	}
	scope, args := ownedBy(userID)
	bound := append([]any{documentID, version, string(domain.DocumentStatusPending)}, args...)
	result := r.db.WithContext(ctx).Model(&DocumentRecord{}).
		Where("id = ? AND version = ? AND index_status = ? AND "+scope, bound...).
		Updates(map[string]any{"index_status": string(domain.DocumentStatusIndexing), "error_message": ""})
	if result.Error != nil {
		return false, translate(result.Error)
	}
	return result.RowsAffected == 1, nil
}

func (r *KnowledgeRepository) CompleteIndexing(ctx context.Context, userID, documentID string, version int, outcome knowledge.IndexOutcome) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if r.db == nil || !outcome.Status.Valid() {
		return repository.ErrInvalidArgument
	}
	if outcome.Status == domain.DocumentStatusReady && outcome.ChunkCount <= 0 {
		return repository.ErrInvalidArgument
	}
	scope, args := ownedBy(userID)
	bound := append([]any{documentID, version, string(domain.DocumentStatusIndexing)}, args...)
	return translate(r.transaction(ctx, func(tx *gorm.DB) error {
		var current DocumentRecord
		err := tx.Table("knowledge_documents").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND version = ? AND index_status = ? AND "+scope, bound...).
			Take(&current).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return knowledge.ErrIllegalTransition
			}
			return err
		}
		updates := map[string]any{"error_message": outcome.ErrorMessage}
		switch outcome.Status {
		case domain.DocumentStatusReady:
			updates["index_status"] = string(domain.DocumentStatusReady)
			updates["chunk_count"] = outcome.ChunkCount
			updates["active_version"] = version
			updates["error_message"] = ""
			// The superseded version is whatever this row was serving, which for a row
			// written before the lifecycle columns existed is its own Version, not the
			// zero its active_version column holds. Reading the column instead would
			// publish the new version and leave the old rows in the table forever.
			superseded := current.servedVersion()
			if superseded > 0 && superseded != version {
				// Superseded, not deleted: dropping the older version's rows is
				// recorded work that can fail and be retried.
				updates["index_status"] = string(domain.DocumentStatusCleanupPending)
				updates["cleanup_version"] = superseded
				updates["cleanup_attempts"] = 0
				updates["cleanup_after"] = time.Now().UTC()
			}
		case domain.DocumentStatusFailed:
			// active_version is untouched, so the text that was answering before this
			// attempt keeps answering.
			updates["index_status"] = string(domain.DocumentStatusFailed)
		default:
			return repository.ErrInvalidArgument
		}
		updates["updated_at"] = time.Now().UTC()
		result := tx.Model(&DocumentRecord{}).Where("knowledge_documents.id = ?", current.ID).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return knowledge.ErrIllegalTransition
		}
		return nil
	}))
}

func (r *KnowledgeRepository) RequeueIndexing(ctx context.Context, userID, documentID string, version int) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return repository.ErrInvalidArgument
	}
	scope, args := ownedBy(userID)
	bound := append([]any{documentID, version, string(domain.DocumentStatusIndexing)}, args...)
	result := r.db.WithContext(ctx).Model(&DocumentRecord{}).
		Where("id = ? AND version = ? AND index_status = ? AND "+scope, bound...).
		Updates(map[string]any{"index_status": string(domain.DocumentStatusPending), "error_message": ""})
	if result.Error != nil {
		return translate(result.Error)
	}
	if result.RowsAffected != 1 {
		return knowledge.ErrIllegalTransition
	}
	return nil
}

// RecoverInterruptedIndexing is v2's startup pass (worker.go:84,
// dao/knowledge/document.go:323-331): nothing from a previous process can still be
// running, so every claimed row returns to the queue. A second run moves nothing, which
// is what makes it safe to call from more than one place.
func (r *KnowledgeRepository) RecoverInterruptedIndexing(ctx context.Context) (int, error) {
	if err := checkContext(ctx); err != nil {
		return 0, err
	}
	if r.db == nil {
		return 0, repository.ErrInvalidArgument
	}
	result := r.db.WithContext(ctx).Model(&DocumentRecord{}).
		Where("index_status = ?", string(domain.DocumentStatusIndexing)).
		Updates(map[string]any{
			"index_status":  string(domain.DocumentStatusPending),
			"error_message": "recovered after interrupted indexing",
		})
	if result.Error != nil {
		return 0, translate(result.Error)
	}
	return int(result.RowsAffected), nil
}

func (r *KnowledgeRepository) RetryFailed(ctx context.Context, userID, documentID string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return repository.ErrInvalidArgument
	}
	scope, args := ownedBy(userID)
	bound := append([]any{documentID, string(domain.DocumentStatusFailed)}, args...)
	result := r.db.WithContext(ctx).Model(&DocumentRecord{}).
		Where("id = ? AND index_status = ? AND "+scope, bound...).
		Updates(map[string]any{"index_status": string(domain.DocumentStatusPending), "error_message": ""})
	if result.Error != nil {
		return translate(result.Error)
	}
	if result.RowsAffected == 1 {
		return nil
	}
	if _, err := r.GetDocument(ctx, userID, documentID); err != nil {
		return err
	}
	return knowledge.ErrDocumentNotRetryable
}

// StagedChunks reads one version's rows back, scoped to documents the caller owns:
// an indexing pass must not be able to name somebody else's document id and receive
// its text.
func (r *KnowledgeRepository) StagedChunks(ctx context.Context, userID, documentID string, version int) ([]domain.DocumentChunk, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(documentID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	var records []DocumentChunkRecord
	err := r.db.WithContext(ctx).Table("knowledge_chunks").
		Joins("JOIN knowledge_collections ON knowledge_collections.id = knowledge_chunks.collection_id").
		Where("knowledge_chunks.document_id = ? AND knowledge_chunks.version = ? AND knowledge_collections.user_id = ?",
			documentID, version, userID).
		Order("knowledge_chunks.seq ASC").Find(&records).Error
	if err := translate(err); err != nil {
		return nil, err
	}
	chunks := make([]domain.DocumentChunk, 0, len(records))
	for _, record := range records {
		chunks = append(chunks, record.toDomain())
	}
	return chunks, nil
}

func (r *KnowledgeRepository) ListPending(ctx context.Context, limit int) ([]knowledge.QueuedDocument, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || limit <= 0 {
		return nil, repository.ErrInvalidArgument
	}
	var records []queuedRecord
	err := r.db.WithContext(ctx).Table("knowledge_documents").
		Select(queuedRecordSelect).
		Joins("JOIN knowledge_collections ON knowledge_collections.id = knowledge_documents.collection_id").
		Where("knowledge_documents.index_status = ?", string(domain.DocumentStatusPending)).
		Order("knowledge_documents.updated_at ASC, knowledge_documents.id ASC").Limit(limit).
		Find(&records).Error
	if err := translate(err); err != nil {
		return nil, err
	}
	return queuedFromRecords(records), nil
}

func (r *KnowledgeRepository) MarkDeleting(ctx context.Context, userID, documentID string) (*domain.Document, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	var marked DocumentRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		var current DocumentRecord
		err := tx.Table("knowledge_documents").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Joins("JOIN knowledge_collections ON knowledge_collections.id = knowledge_documents.collection_id").
			Where("knowledge_documents.id = ? AND knowledge_collections.user_id = ?", documentID, userID).
			Take(&current).Error
		if err != nil {
			return err
		}
		if normalizeStatus(current) == domain.DocumentStatusIndexing {
			return knowledge.ErrDocumentBusy
		}
		now := time.Now().UTC()
		result := tx.Model(&DocumentRecord{}).Where("knowledge_documents.id = ?", current.ID).Updates(map[string]any{
			"index_status": string(domain.DocumentStatusDeleting), "active_version": 0,
			// Zero together with a deleting status means "every version this document
			// ever had".
			"cleanup_version": 0, "cleanup_attempts": 0, "cleanup_after": now,
			"error_message": "", "updated_at": now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return repository.ErrNotFound
		}
		marked = current
		marked.IndexStatus = string(domain.DocumentStatusDeleting)
		marked.ActiveVersion = 0
		marked.CleanupAfter = &now
		return nil
	})
	if err := translate(err); err != nil {
		return nil, err
	}
	document := marked.toDomain()
	return &document, nil
}

func (r *KnowledgeRepository) ListDueCleanup(ctx context.Context, now time.Time, limit int) ([]knowledge.QueuedDocument, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || limit <= 0 {
		return nil, repository.ErrInvalidArgument
	}
	var records []queuedRecord
	err := r.db.WithContext(ctx).Table("knowledge_documents").
		Select(queuedRecordSelect).
		Joins("JOIN knowledge_collections ON knowledge_collections.id = knowledge_documents.collection_id").
		Where("knowledge_documents.index_status IN (?, ?)",
			string(domain.DocumentStatusCleanupPending), string(domain.DocumentStatusDeleting)).
		Where("knowledge_documents.cleanup_after IS NULL OR knowledge_documents.cleanup_after <= ?", now.UTC()).
		Order("knowledge_documents.updated_at ASC, knowledge_documents.id ASC").Limit(limit).
		Find(&records).Error
	if err := translate(err); err != nil {
		return nil, err
	}
	return queuedFromRecords(records), nil
}

func (r *KnowledgeRepository) ClaimCleanup(ctx context.Context, userID, documentID string, now, leaseUntil time.Time) (bool, error) {
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return false, repository.ErrInvalidArgument
	}
	scope, args := ownedBy(userID)
	bound := append([]any{documentID, string(domain.DocumentStatusCleanupPending), string(domain.DocumentStatusDeleting), now.UTC()}, args...)
	result := r.db.WithContext(ctx).Model(&DocumentRecord{}).
		Where("id = ? AND index_status IN (?, ?) AND (cleanup_after IS NULL OR cleanup_after <= ?) AND "+scope, bound...).
		Update("cleanup_after", leaseUntil.UTC())
	if result.Error != nil {
		return false, translate(result.Error)
	}
	return result.RowsAffected == 1, nil
}

// CompleteCleanup finishes recorded work. A superseded version loses its own chunk
// rows and the document returns to ready; a deletion removes the document together
// with every version of its chunks, because those rows are keyed by document and would
// otherwise outlive the only thing that explains them.
func (r *KnowledgeRepository) CompleteCleanup(ctx context.Context, userID, documentID string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return repository.ErrInvalidArgument
	}
	scope, args := ownedBy(userID)
	bound := append([]any{documentID}, args...)
	return translate(r.transaction(ctx, func(tx *gorm.DB) error {
		var current DocumentRecord
		err := tx.Table("knowledge_documents").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND "+scope, bound...).
			Take(&current).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return knowledge.ErrIllegalTransition
			}
			return err
		}
		switch normalizeStatus(current) {
		case domain.DocumentStatusDeleting:
			if err := tx.Where("document_id = ?", current.ID).Delete(&DocumentChunkRecord{}).Error; err != nil {
				return err
			}
			result := tx.Where("knowledge_documents.id = ?", current.ID).Delete(&DocumentRecord{})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return knowledge.ErrIllegalTransition
			}
			return nil
		case domain.DocumentStatusCleanupPending:
			if current.CleanupVersion > 0 {
				if err := tx.Where("document_id = ? AND version = ?", current.ID, current.CleanupVersion).
					Delete(&DocumentChunkRecord{}).Error; err != nil {
					return err
				}
			}
			result := tx.Model(&DocumentRecord{}).Where("knowledge_documents.id = ?", current.ID).Updates(map[string]any{
				"index_status": string(domain.DocumentStatusReady), "cleanup_version": 0,
				"cleanup_attempts": 0, "cleanup_after": nil, "error_message": "",
				"updated_at": time.Now().UTC(),
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return knowledge.ErrIllegalTransition
			}
			return nil
		default:
			return knowledge.ErrIllegalTransition
		}
	}))
}

func (r *KnowledgeRepository) FailCleanup(ctx context.Context, userID, documentID string, message string, retryAt time.Time) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return repository.ErrInvalidArgument
	}
	scope, args := ownedBy(userID)
	bound := append([]any{documentID, string(domain.DocumentStatusCleanupPending), string(domain.DocumentStatusDeleting)}, args...)
	result := r.db.WithContext(ctx).Model(&DocumentRecord{}).
		Where("id = ? AND index_status IN (?, ?) AND "+scope, bound...).
		Updates(map[string]any{
			"cleanup_attempts": gorm.Expr("cleanup_attempts + 1"),
			"cleanup_after":    retryAt.UTC(),
			"error_message":    message,
		})
	if result.Error != nil {
		return translate(result.Error)
	}
	if result.RowsAffected != 1 {
		return knowledge.ErrIllegalTransition
	}
	return nil
}

// publishStaged is the synchronous drive for a store with nothing external to build:
// claim, then report success. It runs over the same transitions the staged methods
// expose, so the two cannot disagree about what published means.
func (r *KnowledgeRepository) publishStaged(ctx context.Context, userID string, staged *domain.Document) (*domain.Document, error) {
	claimed, err := r.ClaimForIndexing(ctx, userID, staged.ID, staged.Version)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, knowledge.ErrIllegalTransition
	}
	count, err := r.countVersionChunks(ctx, staged.ID, staged.Version)
	if err != nil {
		return nil, err
	}
	if err := r.CompleteIndexing(ctx, userID, staged.ID, staged.Version, knowledge.IndexOutcome{
		Status: domain.DocumentStatusReady, ChunkCount: count,
	}); err != nil {
		return nil, err
	}
	published, err := r.GetDocument(ctx, userID, staged.ID)
	if err != nil {
		return nil, err
	}
	// This path has no worker, so it carries out the cleanup it just queued; a store
	// driven by a worker leaves it in the queue instead.
	if published.IndexStatus == domain.DocumentStatusCleanupPending {
		now := time.Now().UTC()
		claimed, claimErr := r.ClaimCleanup(ctx, userID, published.ID, now, now.Add(time.Minute))
		if claimErr != nil {
			return nil, claimErr
		}
		if !claimed {
			return published, nil
		}
		if err := r.CompleteCleanup(ctx, userID, published.ID); err != nil {
			return nil, err
		}
		return r.GetDocument(ctx, userID, staged.ID)
	}
	return published, nil
}

// countVersionChunks reads one version's stored rows directly, because ChunksFor
// serves the active version and a staged row does not have one yet.
func (r *KnowledgeRepository) countVersionChunks(ctx context.Context, documentID string, version int) (int, error) {
	var rows int64
	if err := r.db.WithContext(ctx).Table("knowledge_chunks").
		Where("document_id = ? AND version = ?", documentID, version).Count(&rows).Error; err != nil {
		return 0, translate(err)
	}
	return int(rows), nil
}

func documentsFromRecords(records []DocumentRecord) []domain.Document {
	documents := make([]domain.Document, 0, len(records))
	for _, record := range records {
		documents = append(documents, record.toDomain())
	}
	return documents
}

// queuedRecord is a document row plus the one column no document read is allowed to
// carry: its owner. Ownership lives on the collection, so the two queue queries join to
// get it, and they use an INNER join because a row nobody owns could be claimed by
// nobody - it would come back to the queue forever instead of being worked.
type queuedRecord struct {
	DocumentRecord
	UserID string `gorm:"column:queue_owner_id"`
}

const queuedRecordSelect = "knowledge_documents.*, knowledge_collections.user_id AS queue_owner_id"

func queuedFromRecords(records []queuedRecord) []knowledge.QueuedDocument {
	queued := make([]knowledge.QueuedDocument, 0, len(records))
	for _, record := range records {
		queued = append(queued, knowledge.QueuedDocument{UserID: record.UserID, Document: record.toDomain()})
	}
	return queued
}

// normalizeStatus reads an untracked row the way the synchronous path left it:
// published, at its own version, because that is what every row written before the
// lifecycle columns existed looks like.
func normalizeStatus(record DocumentRecord) domain.DocumentIndexStatus {
	if record.IndexStatus == "" {
		return domain.DocumentStatusReady
	}
	return domain.DocumentIndexStatus(record.IndexStatus)
}

func firstNonEmpty(preferred, fallback string) string {
	if preferred != "" {
		return preferred
	}
	return fallback
}
