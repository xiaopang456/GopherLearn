package knowledge

import (
	"GopherAI/config"
	daoknowledge "GopherAI/dao/knowledge"
	"GopherAI/model"
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
)

type DocumentRepository interface {
	Create(context.Context, *model.KnowledgeDocument) error
	CreateWithinLimits(context.Context, *model.KnowledgeDocument, int, int64) error
	GetByIDAndUserID(context.Context, string, int64) (*model.KnowledgeDocument, error)
	ListByUserID(context.Context, int64) ([]model.KnowledgeDocument, error)
	FindActiveByName(context.Context, int64, string) (*model.KnowledgeDocument, error)
	FindActiveByHash(context.Context, int64, string) (*model.KnowledgeDocument, error)
	UsageByUserID(context.Context, int64) (daoknowledge.Usage, error)
	RetryFailed(context.Context, string, int64) error
	StageUpdate(context.Context, string, int64, int, daoknowledge.DocumentUpdate, int64) (*model.KnowledgeDocument, error)
	MarkDeleting(context.Context, string, int64) (*model.KnowledgeDocument, error)
	Reindex(context.Context, string, int64) error
}

var (
	ErrInvalidDocument      = errors.New("invalid knowledge document")
	ErrEmptyDocument        = errors.New("knowledge document is empty")
	ErrFileTooLarge         = errors.New("knowledge document exceeds file size limit")
	ErrUnsupportedFile      = errors.New("knowledge document type is not supported")
	ErrDocumentLimit        = errors.New("knowledge document count limit reached")
	ErrStorageLimit         = errors.New("knowledge storage limit exceeded")
	ErrDocumentNameExists   = errors.New("knowledge document name already exists")
	ErrDocumentHashExists   = errors.New("knowledge document content already exists")
	ErrDocumentNotRetryable = errors.New("knowledge document is not in failed status")
	ErrDocumentBusy         = errors.New("knowledge document is currently busy")
	ErrDocumentChanged      = errors.New("knowledge document changed concurrently")
)

type DocumentService struct {
	repository DocumentRepository
	storage    FileStorage
	limits     config.KnowledgeConfig
	newID      func() string
	// changed is the optional change notifier (UseChangeNotifier). It is only a hint that a
	// document is waiting in "pending"; the durable row is what the pipeline acts on.
	changed atomic.Pointer[func()]
}

// UseChangeNotifier registers a hook that is called after a write that leaves a document waiting
// for processing has succeeded: a created upload, a staged update with new content, a retry or a
// reindex. It is not called for deletes, for an update whose content did not change, or for any
// failed write. A nil hook removes it, and a service without one behaves exactly as before.
//
// The hook must not block; a panic in it is recovered here, because the write it follows has
// already committed and its result must not turn into an error for the caller.
func (s *DocumentService) UseChangeNotifier(hook func()) {
	if hook == nil {
		s.changed.Store(nil)
		return
	}
	s.changed.Store(&hook)
}

func (s *DocumentService) notifyChanged() {
	hook := s.changed.Load()
	if hook == nil {
		return
	}
	defer func() { _ = recover() }()
	(*hook)()
}

func NewDocumentService(repository DocumentRepository, storage FileStorage, limits config.KnowledgeConfig) *DocumentService {
	return &DocumentService{
		repository: repository,
		storage:    storage,
		limits:     limits,
		newID:      func() string { return uuid.NewString() },
	}
}

func (s *DocumentService) CreateDocument(ctx context.Context, userID int64, originalName string, source io.Reader) (*model.KnowledgeDocument, error) {
	originalName = strings.TrimSpace(originalName)
	if userID <= 0 || originalName == "" || len(originalName) > 255 || filepath.Base(originalName) != originalName || source == nil {
		return nil, ErrInvalidDocument
	}
	extension := strings.ToLower(filepath.Ext(originalName))
	if extension != ".md" && extension != ".txt" {
		return nil, ErrUnsupportedFile
	}
	content, err := io.ReadAll(io.LimitReader(source, s.limits.MaxFileSizeBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) == 0 {
		return nil, ErrEmptyDocument
	}
	if int64(len(content)) > s.limits.MaxFileSizeBytes {
		return nil, ErrFileTooLarge
	}
	usage, err := s.repository.UsageByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if usage.DocumentCount >= int64(s.limits.MaxDocumentsPerUser) {
		return nil, ErrDocumentLimit
	}
	if usage.TotalBytes+int64(len(content)) > s.limits.MaxTotalSizeBytes {
		return nil, ErrStorageLimit
	}
	if _, err := s.repository.FindActiveByName(ctx, userID, originalName); err == nil {
		return nil, ErrDocumentNameExists
	} else if !errors.Is(err, daoknowledge.ErrDocumentNotFound) {
		return nil, err
	}
	hash := ContentHash(content)
	if _, err := s.repository.FindActiveByHash(ctx, userID, hash); err == nil {
		return nil, ErrDocumentHashExists
	} else if !errors.Is(err, daoknowledge.ErrDocumentNotFound) {
		return nil, err
	}
	document, err := s.CreatePending(ctx, userID, originalName, strings.TrimPrefix(extension, "."), bytes.NewReader(content))
	if errors.Is(err, daoknowledge.ErrActiveNameConflict) {
		return nil, ErrDocumentNameExists
	}
	if errors.Is(err, daoknowledge.ErrActiveHashConflict) {
		return nil, ErrDocumentHashExists
	}
	return document, err
}

func (s *DocumentService) ListDocuments(ctx context.Context, userID int64) ([]model.KnowledgeDocument, error) {
	if userID <= 0 {
		return nil, ErrInvalidDocument
	}
	return s.repository.ListByUserID(ctx, userID)
}

func (s *DocumentService) GetDocument(ctx context.Context, userID int64, documentID string) (*model.KnowledgeDocument, error) {
	if userID <= 0 || documentID == "" {
		return nil, ErrInvalidDocument
	}
	return s.repository.GetByIDAndUserID(ctx, documentID, userID)
}

func (s *DocumentService) RetryDocument(ctx context.Context, userID int64, documentID string) (*model.KnowledgeDocument, error) {
	if userID <= 0 || documentID == "" {
		return nil, ErrInvalidDocument
	}
	if err := s.repository.RetryFailed(ctx, documentID, userID); err != nil {
		if errors.Is(err, daoknowledge.ErrDocumentNotRetryable) {
			return nil, ErrDocumentNotRetryable
		}
		return nil, err
	}
	s.notifyChanged()
	return s.repository.GetByIDAndUserID(ctx, documentID, userID)
}

// UpdateDocument stages a new immutable source version. unchanged=true means
// the uploaded bytes match the current content and no file, version, or
// embedding task was created.
func (s *DocumentService) UpdateDocument(ctx context.Context, userID int64, documentID, originalName string, source io.Reader) (*model.KnowledgeDocument, bool, error) {
	if userID <= 0 || documentID == "" {
		return nil, false, ErrInvalidDocument
	}
	current, err := s.repository.GetByIDAndUserID(ctx, documentID, userID)
	if err != nil {
		return nil, false, err
	}
	if current.IndexStatus != model.KnowledgeStatusReady {
		return nil, false, ErrDocumentBusy
	}
	originalName = strings.TrimSpace(originalName)
	if originalName == "" || len(originalName) > 255 || filepath.Base(originalName) != originalName || source == nil {
		return nil, false, ErrInvalidDocument
	}
	extension := strings.ToLower(filepath.Ext(originalName))
	if extension != ".md" && extension != ".txt" {
		return nil, false, ErrUnsupportedFile
	}
	content, err := io.ReadAll(io.LimitReader(source, s.limits.MaxFileSizeBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(content) == 0 {
		return nil, false, ErrEmptyDocument
	}
	if int64(len(content)) > s.limits.MaxFileSizeBytes {
		return nil, false, ErrFileTooLarge
	}
	hash := ContentHash(content)
	if hash == current.ContentHash {
		return current, true, nil
	}
	stored, err := s.storage.SaveTemp(ctx, userID, documentID, bytes.NewReader(content))
	if err != nil {
		return nil, false, err
	}
	fileType := strings.TrimPrefix(extension, ".")
	newVersion := current.Version + 1
	storagePath, err := s.storage.Publish(stored.TempPath, userID, documentID, newVersion, fileType)
	if err != nil {
		return nil, false, errors.Join(err, s.storage.DiscardTemp(stored.TempPath, userID, documentID))
	}
	staged, err := s.repository.StageUpdate(ctx, documentID, userID, current.Version, daoknowledge.DocumentUpdate{
		OriginalName: originalName, StoragePath: storagePath, FileType: fileType,
		ContentHash: stored.ContentHash, FileSize: stored.FileSize,
	}, s.limits.MaxTotalSizeBytes)
	if err != nil {
		cleanupErr := s.storage.Delete(userID, documentID, storagePath)
		switch {
		case errors.Is(err, daoknowledge.ErrDocumentUnchanged):
			latest, getErr := s.repository.GetByIDAndUserID(ctx, documentID, userID)
			return latest, true, errors.Join(getErr, cleanupErr)
		case errors.Is(err, daoknowledge.ErrStorageLimit):
			return nil, false, errors.Join(ErrStorageLimit, cleanupErr)
		case errors.Is(err, daoknowledge.ErrActiveNameConflict):
			return nil, false, errors.Join(ErrDocumentNameExists, cleanupErr)
		case errors.Is(err, daoknowledge.ErrActiveHashConflict):
			return nil, false, errors.Join(ErrDocumentHashExists, cleanupErr)
		case errors.Is(err, daoknowledge.ErrDocumentBusy):
			return nil, false, errors.Join(ErrDocumentBusy, cleanupErr)
		case errors.Is(err, daoknowledge.ErrDocumentChanged):
			return nil, false, errors.Join(ErrDocumentChanged, cleanupErr)
		default:
			return nil, false, errors.Join(err, cleanupErr)
		}
	}
	s.notifyChanged()
	return staged, false, nil
}

func (s *DocumentService) DeleteDocument(ctx context.Context, userID int64, documentID string) error {
	if userID <= 0 || documentID == "" {
		return ErrInvalidDocument
	}
	_, err := s.repository.MarkDeleting(ctx, documentID, userID)
	if errors.Is(err, daoknowledge.ErrDocumentBusy) {
		return ErrDocumentBusy
	}
	return err
}

func (s *DocumentService) ReindexDocument(ctx context.Context, userID int64, documentID string) (*model.KnowledgeDocument, error) {
	if userID <= 0 || documentID == "" {
		return nil, ErrInvalidDocument
	}
	if err := s.repository.Reindex(ctx, documentID, userID); err != nil {
		if errors.Is(err, daoknowledge.ErrDocumentBusy) {
			return nil, ErrDocumentBusy
		}
		return nil, err
	}
	s.notifyChanged()
	return s.repository.GetByIDAndUserID(ctx, documentID, userID)
}

// CreatePending saves the source before creating metadata. Therefore a file
// failure cannot leave a database row, and a database failure triggers file
// cleanup. Validation of product limits belongs to the upload task (RAG-004).
func (s *DocumentService) CreatePending(
	ctx context.Context,
	userID int64,
	originalName string,
	fileType string,
	source io.Reader,
) (*model.KnowledgeDocument, error) {
	if userID <= 0 || strings.TrimSpace(originalName) == "" || source == nil {
		return nil, errors.New("invalid knowledge document input")
	}
	documentID := s.newID()
	stored, err := s.storage.SaveTemp(ctx, userID, documentID, source)
	if err != nil {
		return nil, err
	}
	storagePath, err := s.storage.Publish(stored.TempPath, userID, documentID, 1, fileType)
	if err != nil {
		cleanupErr := s.storage.DiscardTemp(stored.TempPath, userID, documentID)
		return nil, errors.Join(err, cleanupErr)
	}
	document := &model.KnowledgeDocument{
		ID:           documentID,
		UserID:       userID,
		OriginalName: originalName,
		StoragePath:  storagePath,
		FileType:     strings.ToLower(strings.TrimPrefix(fileType, ".")),
		ContentHash:  stored.ContentHash,
		FileSize:     stored.FileSize,
		Version:      1,
		IndexStatus:  model.KnowledgeStatusPending,
	}
	if err := s.repository.CreateWithinLimits(ctx, document, s.limits.MaxDocumentsPerUser, s.limits.MaxTotalSizeBytes); err != nil {
		cleanupErr := s.storage.Delete(userID, documentID, storagePath)
		if errors.Is(err, daoknowledge.ErrDocumentLimit) {
			return nil, errors.Join(ErrDocumentLimit, cleanupErr)
		}
		if errors.Is(err, daoknowledge.ErrStorageLimit) {
			return nil, errors.Join(ErrStorageLimit, cleanupErr)
		}
		return nil, errors.Join(err, cleanupErr)
	}
	s.notifyChanged()
	return document, nil
}
