package knowledge

import (
	"context"
	"fmt"
	"testing"
	"time"

	"GopherAI/internal/testdb"
	"GopherAI/model"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Frozen legacy schema projection, intentionally lacking all candidate fields.
type legacyDocument struct {
	ID                 string                        `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID             int64                         `gorm:"not null;index:idx_knowledge_user_updated,priority:1;index:idx_knowledge_user_content_hash,priority:1;uniqueIndex:uidx_knowledge_active_name,priority:1;uniqueIndex:uidx_knowledge_active_hash,priority:1" json:"user_id"`
	OriginalName       string                        `gorm:"type:varchar(255);not null" json:"original_name"`
	ActiveName         *string                       `gorm:"type:varchar(255);uniqueIndex:uidx_knowledge_active_name,priority:2" json:"-"`
	ActiveHash         *string                       `gorm:"type:char(64);uniqueIndex:uidx_knowledge_active_hash,priority:2" json:"-"`
	StoragePath        string                        `gorm:"type:varchar(512);not null" json:"storage_path"`
	FileType           string                        `gorm:"type:varchar(16);not null" json:"file_type"`
	ContentHash        string                        `gorm:"type:char(64);not null;index:idx_knowledge_user_content_hash,priority:2" json:"content_hash"`
	FileSize           int64                         `gorm:"not null" json:"file_size"`
	Version            int                           `gorm:"not null;default:1" json:"version"`
	ActiveVersion      int                           `gorm:"not null;default:0" json:"active_version"`
	ChunkCount         int                           `gorm:"not null;default:0" json:"chunk_count"`
	IndexStatus        model.KnowledgeDocumentStatus `gorm:"type:varchar(32);not null;index" json:"index_status"`
	ErrorMessage       string                        `gorm:"type:text" json:"error_message,omitempty"`
	CleanupVersion     int                           `gorm:"not null;default:0" json:"-"`
	CleanupStoragePath string                        `gorm:"type:varchar(512);not null;default:''" json:"-"`
	CleanupAttempts    int                           `gorm:"not null;default:0" json:"-"`
	CleanupAfter       *time.Time                    `gorm:"index" json:"-"`
	CreatedAt          time.Time                     `json:"created_at"`
	UpdatedAt          time.Time                     `gorm:"index:idx_knowledge_user_updated,priority:2" json:"updated_at"`
	DeletedAt          gorm.DeletedAt                `gorm:"index" json:"-"`
}

func (legacyDocument) TableName() string { return "knowledge_documents" }

// exerciseUnmigratedLegacy runs the real legacy repository against a schema
// that never had the candidate columns. It is the backwards-compatibility
// check for installations created before the candidate schema existed.
//
// It used to live in the candidate-only document-executor package, where only
// candidate tests could reach it. The public release keeps the scenario.
func exerciseUnmigratedLegacy(d *gorm.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d = d.WithContext(ctx)
	if err := d.AutoMigrate(&model.User{}, &legacyDocument{}); err != nil {
		return err
	}
	for _, col := range []string{"processing_job_id", "active_execution_id", "active_pipeline_fingerprint"} {
		if d.Migrator().HasColumn(&model.KnowledgeDocument{}, col) {
			return fmt.Errorf("legacy test requires absent column %s", col)
		}
	}
	user := model.User{ID: 101, Username: "b05-r1-legacy"}
	if err := d.Create(&user).Error; err != nil {
		return err
	}
	repo := NewRepository(d)
	first := model.KnowledgeDocument{ID: "b05-legacy", UserID: 101, OriginalName: "legacy.txt", StoragePath: "101/legacy.txt", FileType: "txt", ContentHash: fmt.Sprintf("%064d", 1), FileSize: 4, Version: 1, ActiveVersion: 1, IndexStatus: model.KnowledgeStatusReady}
	if err := repo.Create(ctx, &first); err != nil {
		return fmt.Errorf("unmigrated Create: %w", err)
	}
	second := model.KnowledgeDocument{ID: "b05-legacy-limited", UserID: 101, OriginalName: "limit.txt", StoragePath: "101/limit.txt", FileType: "txt", ContentHash: fmt.Sprintf("%064d", 2), FileSize: 4}
	if err := repo.CreateWithinLimits(ctx, &second, 10, 1000); err != nil {
		return fmt.Errorf("unmigrated CreateWithinLimits: %w", err)
	}
	staged, err := repo.StageUpdate(ctx, first.ID, 101, 1, DocumentUpdate{OriginalName: "legacy-v2.txt", StoragePath: "101/legacy-v2.txt", FileType: "txt", ContentHash: fmt.Sprintf("%064d", 3), FileSize: 8}, 1000)
	if err != nil {
		return fmt.Errorf("unmigrated StageUpdate: %w", err)
	}
	if staged.Version != 2 || staged.ActiveVersion != 1 {
		return fmt.Errorf("version semantics changed")
	}
	ok, err := repo.ClaimForIndexing(ctx, first.ID, 101, 2)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("unmigrated claim false")
	}
	if err = repo.CompleteIndexing(ctx, first.ID, 101, 2, model.KnowledgeStatusReady, 2, ""); err != nil {
		return fmt.Errorf("unmigrated CompleteIndexing: %w", err)
	}
	got, err := repo.GetByIDAndUserID(ctx, first.ID, 101)
	if err != nil {
		return err
	}
	if got.Version != 2 || got.ActiveVersion != 2 || got.ChunkCount != 2 {
		return fmt.Errorf("unmigrated version update not retained")
	}
	return nil
}

// TestLegacyRepositoryWorksOnUnmigratedSchema is the public-release form of
// the scenario above: the legacy DAO must still work on a database that was
// created before any candidate-mode schema was introduced.
func TestLegacyRepositoryWorksOnUnmigratedSchema(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := exerciseUnmigratedLegacy(db); err != nil {
		t.Fatalf("legacy repository on an unmigrated schema: %v", err)
	}
}
