package model

import (
	"time"

	"gorm.io/gorm"
)

type KnowledgeDocumentStatus string

const (
	KnowledgeStatusPending        KnowledgeDocumentStatus = "pending"
	KnowledgeStatusIndexing       KnowledgeDocumentStatus = "indexing"
	KnowledgeStatusReady          KnowledgeDocumentStatus = "ready"
	KnowledgeStatusFailed         KnowledgeDocumentStatus = "failed"
	KnowledgeStatusCleanupPending KnowledgeDocumentStatus = "cleanup_pending"
	KnowledgeStatusDeleting       KnowledgeDocumentStatus = "deleting"
	KnowledgeStatusDeleted        KnowledgeDocumentStatus = "deleted"
)

func (s KnowledgeDocumentStatus) Valid() bool {
	switch s {
	case KnowledgeStatusPending, KnowledgeStatusIndexing, KnowledgeStatusReady,
		KnowledgeStatusFailed, KnowledgeStatusCleanupPending,
		KnowledgeStatusDeleting, KnowledgeStatusDeleted:
		return true
	default:
		return false
	}
}

// KnowledgeDocument stores metadata and lifecycle state. Original content is
// kept in file storage, while chunks and vectors are stored in Redis.
type KnowledgeDocument struct {
	ID                        string                  `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID                    int64                   `gorm:"not null;index:idx_knowledge_user_updated,priority:1;index:idx_knowledge_user_content_hash,priority:1;uniqueIndex:uidx_knowledge_active_name,priority:1;uniqueIndex:uidx_knowledge_active_hash,priority:1" json:"user_id"`
	OriginalName              string                  `gorm:"type:varchar(255);not null" json:"original_name"`
	ActiveName                *string                 `gorm:"type:varchar(255);uniqueIndex:uidx_knowledge_active_name,priority:2" json:"-"`
	ActiveHash                *string                 `gorm:"type:char(64);uniqueIndex:uidx_knowledge_active_hash,priority:2" json:"-"`
	StoragePath               string                  `gorm:"type:varchar(512);not null" json:"storage_path"`
	FileType                  string                  `gorm:"type:varchar(16);not null" json:"file_type"`
	ContentHash               string                  `gorm:"type:char(64);not null;index:idx_knowledge_user_content_hash,priority:2" json:"content_hash"`
	FileSize                  int64                   `gorm:"not null" json:"file_size"`
	Version                   int                     `gorm:"not null;default:1" json:"version"`
	ActiveVersion             int                     `gorm:"not null;default:0" json:"active_version"`
	ProcessingJobID           string                  `gorm:"type:varchar(64);not null;default:''" json:"-"`
	ActiveExecutionID         string                  `gorm:"type:varchar(64);not null;default:''" json:"-"`
	ActivePipelineFingerprint string                  `gorm:"type:char(64);not null;default:''" json:"-"`
	ChunkCount                int                     `gorm:"not null;default:0" json:"chunk_count"`
	IndexStatus               KnowledgeDocumentStatus `gorm:"type:varchar(32);not null;index" json:"index_status"`
	ErrorMessage              string                  `gorm:"type:text" json:"error_message,omitempty"`
	CleanupVersion            int                     `gorm:"not null;default:0" json:"-"`
	CleanupStoragePath        string                  `gorm:"type:varchar(512);not null;default:''" json:"-"`
	CleanupAttempts           int                     `gorm:"not null;default:0" json:"-"`
	CleanupAfter              *time.Time              `gorm:"index" json:"-"`
	CreatedAt                 time.Time               `json:"created_at"`
	UpdatedAt                 time.Time               `gorm:"index:idx_knowledge_user_updated,priority:2" json:"updated_at"`
	DeletedAt                 gorm.DeletedAt          `gorm:"index" json:"-"`
}

func (d *KnowledgeDocument) PrepareForCreate() {
	activeName := d.OriginalName
	d.ActiveName = &activeName
	activeHash := d.ContentHash
	d.ActiveHash = &activeHash
	if d.Version == 0 {
		d.Version = 1
	}
	if d.IndexStatus == "" {
		d.IndexStatus = KnowledgeStatusPending
	}
}
