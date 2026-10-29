package memory

import (
	"GopherAI/common/mysql"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrJobNotFound = errors.New("memory extraction job not found")
	ErrJobConflict = errors.New("memory extraction job conflict")
)

type Repository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }
func DefaultRepository() *Repository        { return NewRepository(mysql.DB) }

func (r *Repository) Enqueue(ctx context.Context, job *model.MemoryExtractionJob) (bool, error) {
	if r == nil || r.db == nil {
		return false, fmt.Errorf("memory repository is not initialized")
	}
	if err := job.ValidateForCreate(); err != nil {
		return false, err
	}
	result := r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(job)
	return result.RowsAffected == 1, result.Error
}

func (r *Repository) ListReady(ctx context.Context, limit int, now time.Time) ([]model.MemoryExtractionJob, error) {
	if r == nil || r.db == nil || limit < 1 || limit > 200 {
		return nil, fmt.Errorf("invalid memory job query")
	}
	var jobs []model.MemoryExtractionJob
	err := r.db.WithContext(ctx).
		Where("status = ? AND available_at <= ?", model.MemoryJobPending, now).
		Order("available_at ASC, created_at ASC").Limit(limit).Find(&jobs).Error
	return jobs, err
}

func (r *Repository) Claim(ctx context.Context, jobID string, now time.Time, lease time.Duration, maxAttempts int) (*model.MemoryExtractionJob, error) {
	if r == nil || r.db == nil || lease <= 0 || maxAttempts < 1 {
		return nil, fmt.Errorf("invalid memory job claim")
	}
	token := uuid.NewString()
	ends := now.Add(lease)
	result := r.db.WithContext(ctx).Model(&model.MemoryExtractionJob{}).
		Where("id = ? AND status = ? AND available_at <= ? AND attempts < ?", jobID, model.MemoryJobPending, now, maxAttempts).
		Updates(map[string]any{"status": model.MemoryJobProcessing, "attempts": gorm.Expr("attempts + 1"), "lease_token": token, "lease_ends_at": ends, "error_code": ""})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrJobConflict
	}
	return r.get(ctx, jobID, token)
}

func (r *Repository) Complete(ctx context.Context, job *model.MemoryExtractionJob, candidate *model.MemoryCandidate, terminal model.MemoryJobStatus, errorCode string, now time.Time) error {
	if r == nil || r.db == nil || job == nil || (terminal != model.MemoryJobCompleted && terminal != model.MemoryJobRejected) {
		return fmt.Errorf("invalid memory job completion")
	}
	if terminal == model.MemoryJobCompleted {
		if candidate == nil || candidate.JobID != job.ID || candidate.UserID != job.UserID {
			return fmt.Errorf("memory candidate does not match job")
		}
		if err := candidate.Validate(); err != nil {
			return err
		}
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if candidate != nil {
			if err := tx.Create(candidate).Error; err != nil {
				return err
			}
		}
		result := tx.Model(&model.MemoryExtractionJob{}).
			Where("id = ? AND user_id = ? AND status = ? AND lease_token = ?", job.ID, job.UserID, model.MemoryJobProcessing, job.LeaseToken).
			Updates(map[string]any{"status": terminal, "error_code": errorCode, "lease_token": "", "lease_ends_at": nil, "completed_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrJobConflict
		}
		return nil
	})
}

func (r *Repository) Retry(ctx context.Context, job *model.MemoryExtractionJob, errorCode string, availableAt time.Time, maxAttempts int) error {
	if r == nil || r.db == nil || job == nil {
		return fmt.Errorf("invalid memory job retry")
	}
	status := model.MemoryJobPending
	var completedAt any
	if job.Attempts >= maxAttempts {
		status = model.MemoryJobFailed
		completedAt = time.Now()
	}
	result := r.db.WithContext(ctx).Model(&model.MemoryExtractionJob{}).
		Where("id = ? AND user_id = ? AND status = ? AND lease_token = ?", job.ID, job.UserID, model.MemoryJobProcessing, job.LeaseToken).
		Updates(map[string]any{"status": status, "error_code": errorCode, "available_at": availableAt, "lease_token": "", "lease_ends_at": nil, "completed_at": completedAt})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrJobConflict
	}
	return nil
}

func (r *Repository) RecoverExpired(ctx context.Context, now time.Time, maxAttempts int) (int64, error) {
	if r == nil || r.db == nil || maxAttempts < 1 {
		return 0, fmt.Errorf("invalid memory job recovery")
	}
	var affected int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		failed := tx.Model(&model.MemoryExtractionJob{}).
			Where("status = ? AND lease_ends_at IS NOT NULL AND lease_ends_at <= ? AND attempts >= ?", model.MemoryJobProcessing, now, maxAttempts).
			Updates(map[string]any{"status": model.MemoryJobFailed, "lease_token": "", "lease_ends_at": nil, "error_code": "worker_interrupted", "completed_at": now})
		if failed.Error != nil {
			return failed.Error
		}
		pending := tx.Model(&model.MemoryExtractionJob{}).
			Where("status = ? AND lease_ends_at IS NOT NULL AND lease_ends_at <= ? AND attempts < ?", model.MemoryJobProcessing, now, maxAttempts).
			Updates(map[string]any{"status": model.MemoryJobPending, "lease_token": "", "lease_ends_at": nil, "available_at": now, "error_code": "worker_interrupted"})
		if pending.Error != nil {
			return pending.Error
		}
		affected = failed.RowsAffected + pending.RowsAffected
		return nil
	})
	return affected, err
}

func (r *Repository) get(ctx context.Context, id, token string) (*model.MemoryExtractionJob, error) {
	var job model.MemoryExtractionJob
	err := r.db.WithContext(ctx).Where("id = ? AND lease_token = ?", id, token).First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrJobNotFound
	}
	return &job, err
}
