package conversation

// The knowledge seed is the immutable document metadata of one shared
// evaluation index. A run restores the rows into its own database and reads
// the files from the shared storage root; every file is checked against its
// recorded hash before the run may start. No live application database is
// ever a source of these rows.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"GopherAI/model"
	knowledge "GopherAI/service/knowledge"
	"gorm.io/gorm"
)

// KnowledgeSeed describes the documents of the shared evaluation index.
type KnowledgeSeed struct {
	UserID    int64               `json:"user_id"`
	UserName  string              `json:"user_name"`
	Documents []KnowledgeDocument `json:"documents"`
}

// KnowledgeDocument is the stored metadata of one seeded document. The
// fields the product keeps out of its own JSON representation are carried
// here explicitly, because the seed has to restore them.
type KnowledgeDocument struct {
	ID                        string    `json:"id"`
	UserID                    int64     `json:"user_id"`
	OriginalName              string    `json:"original_name"`
	ActiveName                string    `json:"active_name"`
	ActiveHash                string    `json:"active_hash"`
	StoragePath               string    `json:"storage_path"`
	FileType                  string    `json:"file_type"`
	ContentHash               string    `json:"content_hash"`
	FileSize                  int64     `json:"file_size"`
	Version                   int       `json:"version"`
	ActiveVersion             int       `json:"active_version"`
	ChunkCount                int       `json:"chunk_count"`
	IndexStatus               string    `json:"index_status"`
	ActivePipelineFingerprint string    `json:"active_pipeline_fingerprint"`
	ProcessingJobID           string    `json:"processing_job_id"`
	ActiveExecutionID         string    `json:"active_execution_id"`
	CleanupVersion            int       `json:"cleanup_version"`
	CleanupStoragePath        string    `json:"cleanup_storage_path"`
	CleanupAttempts           int       `json:"cleanup_attempts"`
	CreatedAt                 time.Time `json:"created_at"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

func (d KnowledgeDocument) row() model.KnowledgeDocument {
	activeName, activeHash := d.ActiveName, d.ActiveHash
	return model.KnowledgeDocument{
		ID:                        d.ID,
		UserID:                    d.UserID,
		OriginalName:              d.OriginalName,
		ActiveName:                &activeName,
		ActiveHash:                &activeHash,
		StoragePath:               d.StoragePath,
		FileType:                  d.FileType,
		ContentHash:               d.ContentHash,
		FileSize:                  d.FileSize,
		Version:                   d.Version,
		ActiveVersion:             d.ActiveVersion,
		ProcessingJobID:           d.ProcessingJobID,
		ActiveExecutionID:         d.ActiveExecutionID,
		ActivePipelineFingerprint: d.ActivePipelineFingerprint,
		ChunkCount:                d.ChunkCount,
		IndexStatus:               model.KnowledgeDocumentStatus(d.IndexStatus),
		CleanupVersion:            d.CleanupVersion,
		CleanupStoragePath:        d.CleanupStoragePath,
		CleanupAttempts:           d.CleanupAttempts,
		CreatedAt:                 d.CreatedAt,
		UpdatedAt:                 d.UpdatedAt,
	}
}

// documents returns the product rows a run restores.
func (s *KnowledgeSeed) documents() []model.KnowledgeDocument {
	if s == nil {
		return nil
	}
	rows := make([]model.KnowledgeDocument, 0, len(s.Documents))
	for _, d := range s.Documents {
		rows = append(rows, d.row())
	}
	return rows
}

// LoadKnowledgeSeed reads a seed file and rejects an incomplete one.
func LoadKnowledgeSeed(path string) (*KnowledgeSeed, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var seed KnowledgeSeed
	if err = json.Unmarshal(raw, &seed); err != nil {
		return nil, err
	}
	if seed.UserID <= 0 || seed.UserName == "" {
		return nil, fmt.Errorf("knowledge seed has no evaluation identity")
	}
	for _, d := range seed.Documents {
		if d.ID == "" || d.UserID != seed.UserID || d.StoragePath == "" || d.ContentHash == "" || d.FileSize <= 0 {
			return nil, fmt.Errorf("knowledge seed has an incomplete document")
		}
	}
	return &seed, nil
}

// WriteKnowledgeSeed writes a seed file and refuses to overwrite one.
func WriteKnowledgeSeed(path string, seed KnowledgeSeed) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(seed)
	return errors.Join(err, file.Close())
}

// DumpKnowledgeSeed reads the documents of one evaluation user and verifies
// every file against its recorded hash and size. A document whose file is
// missing or changed is refused: the seed would restore metadata that the
// shared index does not describe.
func DumpKnowledgeSeed(ctx context.Context, db *gorm.DB, userID int64, storageRoot string) (KnowledgeSeed, error) {
	var seed KnowledgeSeed
	if db == nil || userID <= 0 || storageRoot == "" {
		return seed, fmt.Errorf("knowledge seed source is incomplete")
	}
	var user model.User
	if err := db.WithContext(ctx).Where("id = ?", userID).First(&user).Error; err != nil {
		return seed, fmt.Errorf("evaluation user not found")
	}
	seed.UserID = user.ID
	seed.UserName = user.Username
	var rows []model.KnowledgeDocument
	if err := db.WithContext(ctx).Where("user_id = ? AND deleted_at IS NULL", userID).Order("id").Find(&rows).Error; err != nil {
		return seed, err
	}
	storage, err := knowledge.NewLocalFileStorage(storageRoot)
	if err != nil {
		return seed, err
	}
	for _, row := range rows {
		if row.ActiveName == nil || row.ActiveHash == nil {
			return seed, fmt.Errorf("document %s has no active version", row.ID)
		}
		file, openErr := storage.Open(row.UserID, row.ID, row.StoragePath)
		if openErr != nil {
			return seed, fmt.Errorf("document %s file is unavailable: %w", row.ID, openErr)
		}
		content, readErr := io.ReadAll(io.LimitReader(file, 64<<20+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(content) > 64<<20 {
			return seed, fmt.Errorf("document %s file could not be read", row.ID)
		}
		if knowledge.ContentHash(content) != row.ContentHash || int64(len(content)) != row.FileSize {
			return seed, fmt.Errorf("document %s file does not match its hash", row.ID)
		}
		seed.Documents = append(seed.Documents, KnowledgeDocument{
			ID: row.ID, UserID: row.UserID, OriginalName: row.OriginalName,
			ActiveName: *row.ActiveName, ActiveHash: *row.ActiveHash,
			StoragePath: row.StoragePath, FileType: row.FileType,
			ContentHash: row.ContentHash, FileSize: row.FileSize,
			Version: row.Version, ActiveVersion: row.ActiveVersion,
			ChunkCount: row.ChunkCount, IndexStatus: string(row.IndexStatus),
			ActivePipelineFingerprint: row.ActivePipelineFingerprint,
			ProcessingJobID:           row.ProcessingJobID, ActiveExecutionID: row.ActiveExecutionID,
			CleanupVersion: row.CleanupVersion, CleanupStoragePath: row.CleanupStoragePath,
			CleanupAttempts: row.CleanupAttempts,
			CreatedAt:       row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return seed, nil
}

// CopyStorageTree copies the storage root of a built index so that later runs
// read an immutable copy instead of the directory the builder wrote.
func CopyStorageTree(source, target string) error {
	if source == "" || target == "" {
		return fmt.Errorf("storage copy needs two paths")
	}
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if info.IsDir() {
			return os.MkdirAll(destination, 0o750)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, content, 0o640)
	})
}
