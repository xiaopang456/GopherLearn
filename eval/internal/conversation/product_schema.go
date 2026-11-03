package conversation

import (
	"context"
	"fmt"
	"io"
	"strings"

	"GopherAI/common/mysql"
	"GopherAI/model"
	knowledge "GopherAI/service/knowledge"
	driver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// InitProductSchema uses the product's complete migration in a new owned DB.
// Migration DDL is not transactional in MySQL; failure leaves this disposable
// instance unusable and the caller must destroy it instead of retrying in place.
func (s *Sandbox) InitProductSchema(ctx context.Context) error {
	if err := s.verify(ctx); err != nil {
		return err
	}
	if err := (guardedStore{s}).with(ctx, func(tx *gorm.DB) error {
		var n int64
		if err := tx.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name <> 'eval_instance_guard'").Scan(&n).Error; err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("product initialization requires a fresh database")
		}
		return nil
	}); err != nil {
		return err
	}
	return mysql.InitializeSchema(s.db.WithContext(ctx))
}

// SeedProduct copies only explicitly supplied evaluation identity and immutable
// document metadata. Files must already exist with their exact hash; no live
// application database is a seed source. Redis indexes are not copied or reset.
func (s *Sandbox) SeedProduct(ctx context.Context, user model.User, docs []model.KnowledgeDocument, storageRoot string) error {
	if user.ID <= 0 || strings.TrimSpace(user.Username) == "" {
		return fmt.Errorf("invalid evaluation identity")
	}
	for _, d := range docs {
		if d.UserID != user.ID || d.ID == "" || d.IndexStatus != model.KnowledgeStatusReady || d.Version != d.ActiveVersion || d.ActiveVersion < 1 || d.ProcessingJobID != "" || d.ActiveExecutionID != "" || d.CleanupVersion != 0 || d.CleanupAfter != nil || d.DeletedAt.Valid {
			return fmt.Errorf("document seed is not immutable and ready")
		}
		storage, err := knowledge.NewLocalFileStorage(storageRoot)
		if err != nil {
			return err
		}
		file, err := storage.Open(d.UserID, d.ID, d.StoragePath)
		if err != nil {
			return fmt.Errorf("seed document unavailable: %w", err)
		}
		bytes, readErr := io.ReadAll(io.LimitReader(file, 64<<20+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(bytes) > 64<<20 {
			return fmt.Errorf("seed document read failed or exceeds limit")
		}
		if knowledge.ContentHash(bytes) != d.ContentHash || int64(len(bytes)) != d.FileSize || d.ActiveHash == nil || *d.ActiveHash != d.ContentHash || d.ActiveName == nil || *d.ActiveName != d.OriginalName {
			return fmt.Errorf("document seed hash or identity mismatch")
		}
	}
	if err := s.verify(ctx); err != nil {
		return err
	}
	return guardedStore{s}.with(ctx, func(tx *gorm.DB) error {
		for _, table := range []string{"users", "sessions", "messages", "session_summaries", "knowledge_documents"} {
			var n int64
			if err := tx.Table(table).Count(&n).Error; err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("seed requires empty product tables")
			}
		}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		for _, d := range docs {
			if err := tx.Create(&d).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// openProductDB gives the product a distinct pool. Closing the complete product
// therefore cannot close the journal observer before its final evidence read.
func (s *Sandbox) openProductDB(ctx context.Context) (*gorm.DB, error) {
	c, err := s.ConnectionInfo(ctx)
	if err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=UTC", c.User, c.Password, c.Host, c.Port, c.Database)
	db, err := gorm.Open(driver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("open isolated product pool failed")
	}
	if err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return s.checkMarker(tx) }); err != nil {
		pool, _ := db.DB()
		pool.Close()
		return nil, err
	}
	return db, nil
}
