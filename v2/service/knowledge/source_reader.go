package knowledge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

var (
	ErrDocumentVersionNotActive = errors.New("knowledge document version is not active")
	ErrDocumentSourceInvalid    = errors.New("knowledge document source is invalid")
)

// ActiveDocumentSource is an immutable source snapshot selected by the
// document's currently searchable version. StoragePath is deliberately not
// exposed to Agent tools or model-visible output.
type ActiveDocumentSource struct {
	DocumentID   string
	Version      int
	DocumentName string
	FileType     string
	Content      string
}

// ReadActiveSource resolves the source file that corresponds to the exact
// active version seen by retrieval. During a staged update, StoragePath points
// at the new version while CleanupStoragePath still holds the old searchable
// version, so choosing StoragePath unconditionally would mix two versions.
func (s *DocumentService) ReadActiveSource(ctx context.Context, userID int64, documentID string, version int) (ActiveDocumentSource, error) {
	if s == nil || s.repository == nil || s.storage == nil || userID <= 0 || strings.TrimSpace(documentID) == "" || version <= 0 {
		return ActiveDocumentSource{}, ErrInvalidDocument
	}
	if err := contextError(ctx); err != nil {
		return ActiveDocumentSource{}, err
	}
	document, err := s.repository.GetByIDAndUserID(ctx, documentID, userID)
	if err != nil {
		return ActiveDocumentSource{}, err
	}
	if document.ActiveVersion != version {
		return ActiveDocumentSource{}, ErrDocumentVersionNotActive
	}
	storagePath, err := activeSourcePath(document.Version, document.ActiveVersion, document.StoragePath, document.CleanupVersion, document.CleanupStoragePath)
	if err != nil {
		return ActiveDocumentSource{}, err
	}
	source, err := s.storage.Open(userID, document.ID, storagePath)
	if err != nil {
		return ActiveDocumentSource{}, fmt.Errorf("open active document source: %w", err)
	}
	defer source.Close()
	content, err := io.ReadAll(io.LimitReader(source, s.limits.MaxFileSizeBytes+1))
	if err != nil {
		return ActiveDocumentSource{}, fmt.Errorf("read active document source: %w", err)
	}
	if int64(len(content)) > s.limits.MaxFileSizeBytes {
		return ActiveDocumentSource{}, ErrDocumentSourceInvalid
	}
	if err := contextError(ctx); err != nil {
		return ActiveDocumentSource{}, err
	}
	fileType := strings.TrimPrefix(strings.ToLower(filepath.Ext(storagePath)), ".")
	if fileType != "md" && fileType != "txt" {
		return ActiveDocumentSource{}, ErrDocumentSourceInvalid
	}
	// Metadata size/hash describe StoragePath. They can only be verified when
	// the active source is the current version; staged updates no longer retain
	// the old source's hash in the document row.
	if document.Version == version && (int64(len(content)) != document.FileSize || ContentHash(content) != document.ContentHash) {
		return ActiveDocumentSource{}, ErrDocumentSourceInvalid
	}
	return ActiveDocumentSource{
		DocumentID: document.ID, Version: version, DocumentName: document.OriginalName,
		FileType: fileType, Content: string(NormalizeContent(content)),
	}, nil
}

func activeSourcePath(currentVersion, activeVersion int, currentPath string, cleanupVersion int, cleanupPath string) (string, error) {
	switch {
	case activeVersion <= 0:
		return "", ErrDocumentVersionNotActive
	case activeVersion == currentVersion && strings.TrimSpace(currentPath) != "":
		return currentPath, nil
	case activeVersion == cleanupVersion && strings.TrimSpace(cleanupPath) != "":
		return cleanupPath, nil
	default:
		return "", ErrDocumentSourceInvalid
	}
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
