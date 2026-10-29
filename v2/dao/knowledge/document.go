package knowledge

import (
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrActiveNameConflict   = errors.New("active document name already exists")
	ErrActiveHashConflict   = errors.New("active document content already exists")
	ErrDocumentNotFound     = errors.New("knowledge document not found")
	ErrInvalidStatus        = errors.New("invalid knowledge document status")
	ErrDocumentLimit        = errors.New("knowledge document count limit reached")
	ErrStorageLimit         = errors.New("knowledge storage limit exceeded")
	ErrIndexTransition      = errors.New("knowledge document index status transition rejected")
	ErrDocumentNotRetryable = errors.New("knowledge document is not failed and cannot be retried")
	ErrDocumentBusy         = errors.New("knowledge document is busy")
	ErrDocumentChanged      = errors.New("knowledge document changed concurrently")
	ErrDocumentUnchanged    = errors.New("knowledge document content is unchanged")
)

type DocumentUpdate struct {
	OriginalName string
	StoragePath  string
	FileType     string
	ContentHash  string
	FileSize     int64
}

// StageUpdate atomically installs a new source version while preserving the
// old ActiveVersion. Queries can therefore keep using the old Redis chunks
// until CompleteIndexing switches the active version.
func (r *Repository) StageUpdate(ctx context.Context, id string, userID int64, expectedVersion int, update DocumentUpdate, maxTotalBytes int64) (*model.KnowledgeDocument, error) {
	var staged model.KnowledgeDocument
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").First(&user, userID).Error; err != nil {
			return err
		}
		var current model.KnowledgeDocument
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ? AND active_name IS NOT NULL", id, userID).First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDocumentNotFound
			}
			return err
		}
		if current.Version != expectedVersion {
			return ErrDocumentChanged
		}
		if current.IndexStatus != model.KnowledgeStatusReady {
			return ErrDocumentBusy
		}
		if current.ContentHash == update.ContentHash {
			return ErrDocumentUnchanged
		}
		usage, err := NewRepository(tx).UsageByUserID(ctx, userID)
		if err != nil {
			return err
		}
		if usage.TotalBytes-current.FileSize+update.FileSize > maxTotalBytes {
			return ErrStorageLimit
		}
		activeName, activeHash := update.OriginalName, update.ContentHash
		updates := map[string]any{
			"original_name": update.OriginalName, "active_name": &activeName,
			"storage_path": update.StoragePath, "file_type": update.FileType,
			"content_hash": update.ContentHash, "active_hash": &activeHash,
			"file_size": update.FileSize, "version": expectedVersion + 1,
			"index_status": model.KnowledgeStatusPending, "error_message": "",
			"cleanup_version": current.ActiveVersion, "cleanup_storage_path": current.StoragePath,
			"cleanup_attempts": 0, "cleanup_after": nil,
		}
		if err := tx.Model(&current).Updates(updates).Error; err != nil {
			return classifyConflict(err, update.OriginalName, update.ContentHash)
		}
		return tx.Where("id = ? AND user_id = ?", id, userID).First(&staged).Error
	})
	return &staged, err
}

type Repository struct {
	db *gorm.DB
}

// CreateWithinLimits serializes uploads per user by locking the owning user
// row. This makes the final quota check and insert atomic under concurrency.
func (r *Repository) CreateWithinLimits(ctx context.Context, document *model.KnowledgeDocument, maxDocuments int, maxTotalBytes int64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").First(&user, document.UserID).Error; err != nil {
			return err
		}
		txRepository := NewRepository(tx)
		usage, err := txRepository.UsageByUserID(ctx, document.UserID)
		if err != nil {
			return err
		}
		if usage.DocumentCount >= int64(maxDocuments) {
			return ErrDocumentLimit
		}
		if usage.TotalBytes+document.FileSize > maxTotalBytes {
			return ErrStorageLimit
		}
		return txRepository.Create(ctx, document)
	})
}

func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// Create deliberately writes only legacy document fields. Candidate metadata
// is persisted by ExecutionStore after an explicit migration, never by uploads.
func (r *Repository) Create(ctx context.Context, document *model.KnowledgeDocument) error {
	if document == nil {
		return errors.New("knowledge document is nil")
	}
	document.PrepareForCreate()
	if !document.IndexStatus.Valid() {
		return ErrInvalidStatus
	}
	if err := r.db.WithContext(ctx).Omit("ProcessingJobID", "ActiveExecutionID", "ActivePipelineFingerprint").Create(document).Error; err != nil {
		return classifyConflict(err, document.OriginalName, document.ContentHash)
	}
	return nil
}

func classifyConflict(err error, name, hash string) error {
	var mysqlErr *mysqldriver.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		if strings.Contains(mysqlErr.Message, "uidx_knowledge_active_hash") {
			return fmt.Errorf("%w: %s", ErrActiveHashConflict, hash)
		}
		return fmt.Errorf("%w: %s", ErrActiveNameConflict, name)
	}
	return err
}

func (r *Repository) GetByIDAndUserID(ctx context.Context, id string, userID int64) (*model.KnowledgeDocument, error) {
	var document model.KnowledgeDocument
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ? AND active_name IS NOT NULL", id, userID).First(&document).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrDocumentNotFound
	}
	return &document, err
}

func (r *Repository) ListByUserID(ctx context.Context, userID int64) ([]model.KnowledgeDocument, error) {
	var documents []model.KnowledgeDocument
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND active_name IS NOT NULL", userID).
		Order("updated_at DESC").
		Find(&documents).Error
	return documents, err
}

func (r *Repository) FindActiveByName(ctx context.Context, userID int64, name string) (*model.KnowledgeDocument, error) {
	var document model.KnowledgeDocument
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND active_name = ?", userID, name).
		First(&document).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrDocumentNotFound
	}
	return &document, err
}

func (r *Repository) FindActiveByHash(ctx context.Context, userID int64, hash string) (*model.KnowledgeDocument, error) {
	var document model.KnowledgeDocument
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND active_hash = ?", userID, hash).
		First(&document).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrDocumentNotFound
	}
	return &document, err
}

type Usage struct {
	DocumentCount int64
	TotalBytes    int64
}

type ActiveDocumentVersion struct {
	ID            string
	ActiveVersion int
	OriginalName  string
}

// ListActiveVersionsByUserID is the retrieval-side allow-list used by
// RAG-010. It deliberately includes a document whose new Version failed while
// its older ActiveVersion is still valid, and excludes logical deletions.
func (r *Repository) ListActiveVersionsByUserID(ctx context.Context, userID int64) ([]ActiveDocumentVersion, error) {
	var versions []ActiveDocumentVersion
	err := r.db.WithContext(ctx).Model(&model.KnowledgeDocument{}).
		Select("id, active_version, original_name").
		Where("user_id = ? AND active_name IS NOT NULL AND active_version > 0", userID).
		Find(&versions).Error
	return versions, err
}

func (r *Repository) UsageByUserID(ctx context.Context, userID int64) (Usage, error) {
	var usage Usage
	err := r.db.WithContext(ctx).Model(&model.KnowledgeDocument{}).
		Select("COUNT(*) AS document_count, COALESCE(SUM(file_size), 0) AS total_bytes").
		Where("user_id = ? AND active_name IS NOT NULL", userID).
		Scan(&usage).Error
	return usage, err
}

type IndexUpdate struct {
	Status        model.KnowledgeDocumentStatus
	ChunkCount    int
	ActiveVersion int
	ErrorMessage  string
}

func (r *Repository) UpdateIndex(ctx context.Context, id string, userID int64, update IndexUpdate) error {
	if !update.Status.Valid() {
		return ErrInvalidStatus
	}
	result := r.db.WithContext(ctx).Model(&model.KnowledgeDocument{}).
		Where("id = ? AND user_id = ?", id, userID).
		Updates(map[string]any{
			"index_status":   update.Status,
			"chunk_count":    update.ChunkCount,
			"active_version": update.ActiveVersion,
			"error_message":  update.ErrorMessage,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrDocumentNotFound
	}
	return nil
}

// ListPending returns persistent work candidates. ClaimForIndexing performs
// the authoritative compare-and-swap before any worker processes a candidate.
func (r *Repository) ListPending(ctx context.Context, limit int) ([]model.KnowledgeDocument, error) {
	if limit <= 0 {
		return nil, errors.New("pending document limit must be greater than zero")
	}
	var documents []model.KnowledgeDocument
	err := r.db.WithContext(ctx).
		Where("index_status = ? AND active_name IS NOT NULL", model.KnowledgeStatusPending).
		Order("updated_at ASC").Limit(limit).Find(&documents).Error
	return documents, err
}

func (r *Repository) ClaimForIndexing(ctx context.Context, id string, userID int64, version int) (bool, error) {
	result := r.db.WithContext(ctx).Model(&model.KnowledgeDocument{}).
		Where("id = ? AND user_id = ? AND version = ? AND index_status = ? AND active_name IS NOT NULL",
			id, userID, version, model.KnowledgeStatusPending).
		Updates(map[string]any{"index_status": model.KnowledgeStatusIndexing, "error_message": ""})
	return result.RowsAffected == 1, result.Error
}

func (r *Repository) CompleteIndexing(ctx context.Context, id string, userID int64, version int, status model.KnowledgeDocumentStatus, chunkCount int, errorMessage string) error {
	if status != model.KnowledgeStatusReady && status != model.KnowledgeStatusFailed {
		return ErrInvalidStatus
	}
	updates := map[string]any{"index_status": status, "error_message": errorMessage}
	if status == model.KnowledgeStatusReady {
		if chunkCount <= 0 {
			return errors.New("ready document chunk count must be greater than zero")
		}
		updates["chunk_count"] = chunkCount
		updates["active_version"] = version
		updates["error_message"] = ""
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current model.KnowledgeDocument
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ? AND version = ? AND index_status = ?", id, userID, version, model.KnowledgeStatusIndexing).
			First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrIndexTransition
			}
			return err
		}
		if status == model.KnowledgeStatusReady && current.ActiveVersion > 0 && current.ActiveVersion != version {
			updates["index_status"] = model.KnowledgeStatusCleanupPending
			now := time.Now()
			updates["cleanup_after"] = &now
		}
		result := tx.Model(&current).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrIndexTransition
		}
		return nil
	})
}

func (r *Repository) RequeueIndexing(ctx context.Context, id string, userID int64, version int) error {
	result := r.db.WithContext(ctx).Model(&model.KnowledgeDocument{}).
		Where("id = ? AND user_id = ? AND version = ? AND index_status = ?",
			id, userID, version, model.KnowledgeStatusIndexing).
		Updates(map[string]any{"index_status": model.KnowledgeStatusPending, "error_message": ""})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrIndexTransition
	}
	return nil
}

// RecoverInterruptedIndexing is a single-process crash recovery rule: no work
// from the previous process can still be running, so every indexing row is
// returned to the durable pending queue on startup.
func (r *Repository) RecoverInterruptedIndexing(ctx context.Context) (int64, error) {
	result := r.db.WithContext(ctx).Model(&model.KnowledgeDocument{}).
		Where("index_status = ? AND active_name IS NOT NULL", model.KnowledgeStatusIndexing).
		Updates(map[string]any{
			"index_status":  model.KnowledgeStatusPending,
			"error_message": "recovered after interrupted indexing",
		})
	return result.RowsAffected, result.Error
}

func (r *Repository) RetryFailed(ctx context.Context, id string, userID int64) error {
	result := r.db.WithContext(ctx).Model(&model.KnowledgeDocument{}).
		Where("id = ? AND user_id = ? AND index_status = ? AND active_name IS NOT NULL",
			id, userID, model.KnowledgeStatusFailed).
		Updates(map[string]any{
			"index_status":  model.KnowledgeStatusPending,
			"error_message": "",
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	if _, err := r.GetByIDAndUserID(ctx, id, userID); err != nil {
		return err
	}
	return ErrDocumentNotRetryable
}

func (r *Repository) Reindex(ctx context.Context, id string, userID int64) error {
	result := r.db.WithContext(ctx).Model(&model.KnowledgeDocument{}).
		Where("id = ? AND user_id = ? AND index_status = ? AND active_name IS NOT NULL", id, userID, model.KnowledgeStatusReady).
		Updates(map[string]any{"index_status": model.KnowledgeStatusPending, "error_message": ""})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	if _, err := r.GetByIDAndUserID(ctx, id, userID); err != nil {
		return err
	}
	return ErrDocumentBusy
}

func (r *Repository) MarkDeleting(ctx context.Context, id string, userID int64) (*model.KnowledgeDocument, error) {
	var document model.KnowledgeDocument
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ? AND active_name IS NOT NULL", id, userID).First(&document).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDocumentNotFound
			}
			return err
		}
		if document.IndexStatus == model.KnowledgeStatusIndexing {
			return ErrDocumentBusy
		}
		now := time.Now()
		return tx.Model(&document).Updates(map[string]any{
			"active_name": nil, "active_hash": nil,
			"index_status":     model.KnowledgeStatusDeleting,
			"cleanup_attempts": 0, "cleanup_after": &now, "error_message": "",
		}).Error
	})
	return &document, err
}

func (r *Repository) ListCleanupDue(ctx context.Context, now time.Time, limit int) ([]model.KnowledgeDocument, error) {
	if limit <= 0 {
		return nil, errors.New("cleanup document limit must be greater than zero")
	}
	var documents []model.KnowledgeDocument
	err := r.db.WithContext(ctx).
		Where("index_status IN ? AND (cleanup_after IS NULL OR cleanup_after <= ?)",
			[]model.KnowledgeDocumentStatus{model.KnowledgeStatusCleanupPending, model.KnowledgeStatusDeleting}, now).
		Order("updated_at ASC").Limit(limit).Find(&documents).Error
	return documents, err
}

func (r *Repository) ClaimCleanup(ctx context.Context, id string, userID int64, version int, now, leaseUntil time.Time) (bool, error) {
	result := r.db.WithContext(ctx).Model(&model.KnowledgeDocument{}).
		Where("id = ? AND user_id = ? AND version = ? AND index_status IN ? AND (cleanup_after IS NULL OR cleanup_after <= ?)",
			id, userID, version,
			[]model.KnowledgeDocumentStatus{model.KnowledgeStatusCleanupPending, model.KnowledgeStatusDeleting}, now).
		Update("cleanup_after", leaseUntil)
	return result.RowsAffected == 1, result.Error
}

func (r *Repository) CompleteCleanup(ctx context.Context, document model.KnowledgeDocument) error {
	updates := map[string]any{
		"cleanup_version": 0, "cleanup_storage_path": "", "cleanup_attempts": 0,
		"cleanup_after": nil, "error_message": "",
	}
	expectedStatus := document.IndexStatus
	if expectedStatus == model.KnowledgeStatusCleanupPending {
		updates["index_status"] = model.KnowledgeStatusReady
	} else if expectedStatus == model.KnowledgeStatusDeleting {
		updates["index_status"] = model.KnowledgeStatusDeleted
		updates["storage_path"] = ""
		updates["active_version"] = 0
		updates["chunk_count"] = 0
	} else {
		return ErrIndexTransition
	}
	result := r.db.WithContext(ctx).Model(&model.KnowledgeDocument{}).
		Where("id = ? AND user_id = ? AND version = ? AND index_status = ?", document.ID, document.UserID, document.Version, expectedStatus).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrIndexTransition
	}
	return nil
}

func (r *Repository) FailCleanup(ctx context.Context, document model.KnowledgeDocument, message string, retryAt time.Time) error {
	result := r.db.WithContext(ctx).Model(&model.KnowledgeDocument{}).
		Where("id = ? AND user_id = ? AND version = ? AND index_status = ?", document.ID, document.UserID, document.Version, document.IndexStatus).
		Updates(map[string]any{
			"cleanup_attempts": gorm.Expr("cleanup_attempts + 1"),
			"cleanup_after":    retryAt, "error_message": message,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrIndexTransition
	}
	return nil
}
