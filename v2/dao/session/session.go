package session

import (
	"GopherAI/common/mysql"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrSessionNotFound = errors.New("session not found")

var ErrSessionRequestConflict = errors.New("session creation request conflict")

var ErrSessionLimitReached = errors.New("session limit reached")

type Repository struct {
	db *gorm.DB
}

// DeleteOwned permanently removes one owned conversation together with its
// message history, Agent run facts, Summary, Episode, projection jobs and
// not-yet-independent memory pipeline artifacts in one MySQL transaction.
// UserMemory is not part of this aggregate: an explicitly confirmed memory has
// its own user-governed delete lifecycle. Returned Episode IDs let the service
// remove the rebuildable Redis view after MySQL has become authoritative.
func (r *Repository) DeleteOwned(ctx context.Context, userID int64, sessionID string) ([]string, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("session repository is not initialized")
	}
	if userID <= 0 || sessionID == "" {
		return nil, ErrSessionNotFound
	}
	var episodeIDs []string
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var owned model.Session
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ?", sessionID, userID).First(&owned).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrSessionNotFound
			}
			return err
		}
		if err := tx.Model(&model.EpisodeMemory{}).
			Where("user_id = ? AND session_id = ?", userID, sessionID).Pluck("id", &episodeIDs).Error; err != nil {
			return err
		}
		// Lock the extraction lineage before deleting it. This serializes session
		// deletion with extractor/dream workers: after this transaction commits,
		// no candidate derived from the deleted conversation remains claimable.
		var memoryJobIDs []string
		if err := tx.Model(&model.MemoryExtractionJob{}).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND source_session_id = ?", userID, sessionID).
			Pluck("id", &memoryJobIDs).Error; err != nil {
			return err
		}
		var memoryCandidateIDs []string
		if len(memoryJobIDs) > 0 {
			if err := tx.Model(&model.MemoryCandidate{}).Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("user_id = ? AND job_id IN ?", userID, memoryJobIDs).
				Pluck("id", &memoryCandidateIDs).Error; err != nil {
				return err
			}
		}
		runIDs := tx.Model(&model.AgentRun{}).Select("id").Where("user_id = ? AND session_id = ?", userID, sessionID)
		deletions := make([]func() error, 0, 12)
		if len(memoryCandidateIDs) > 0 {
			deletions = append(deletions,
				func() error {
					return tx.Where("user_id = ? AND candidate_id IN ?", userID, memoryCandidateIDs).Delete(&model.MemoryDreamRun{}).Error
				},
				func() error {
					return tx.Where("user_id = ? AND id IN ?", userID, memoryCandidateIDs).Delete(&model.MemoryCandidate{}).Error
				},
			)
		}
		if len(memoryJobIDs) > 0 {
			deletions = append(deletions, func() error {
				return tx.Where("user_id = ? AND id IN ?", userID, memoryJobIDs).Delete(&model.MemoryExtractionJob{}).Error
			})
		}
		deletions = append(deletions,
			func() error {
				return tx.Where("user_id = ? AND session_id = ?", userID, sessionID).Delete(&model.MemoryDraft{}).Error
			},
			func() error {
				return tx.Where("user_id = ? AND run_id IN (?)", userID, runIDs).Delete(&model.AgentStep{}).Error
			},
			func() error {
				return tx.Where("user_id = ? AND session_id = ?", userID, sessionID).Delete(&model.AgentRun{}).Error
			},
			func() error {
				return tx.Where("user_id = ? AND session_id = ?", userID, sessionID).Delete(&model.SessionSummary{}).Error
			},
			func() error {
				return tx.Where("user_id = ? AND session_id = ?", userID, sessionID).Delete(&model.EpisodeProjectionJob{}).Error
			},
			func() error {
				return tx.Where("user_id = ? AND session_id = ?", userID, sessionID).Delete(&model.EpisodeMemory{}).Error
			},
			func() error { return tx.Where("session_id = ?", sessionID).Delete(&model.Message{}).Error },
			func() error {
				return tx.Unscoped().Where("id = ? AND user_id = ?", sessionID, userID).Delete(&model.Session{}).Error
			},
		)
		for _, deletion := range deletions {
			if err := deletion(); err != nil {
				return err
			}
		}
		return nil
	})
	return episodeIDs, err
}

func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

func DefaultRepository() *Repository {
	return NewRepository(mysql.DB)
}

func (r *Repository) Create(ctx context.Context, session *model.Session) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("session repository is not initialized")
	}
	if err := session.ValidateForCreate(); err != nil {
		return err
	}
	_, _, err := r.createOrGetLocked(ctx, session, false)
	return err
}

// CreateOrGet makes Agent-session creation retry-safe. The nullable unique key
// leaves every legacy/non-Agent creation path unchanged.
func (r *Repository) CreateOrGet(ctx context.Context, session *model.Session) (*model.Session, bool, error) {
	if r == nil || r.db == nil {
		return nil, false, fmt.Errorf("session repository is not initialized")
	}
	if err := session.ValidateForCreate(); err != nil {
		return nil, false, err
	}
	return r.createOrGetLocked(ctx, session, true)
}

// createOrGetLocked serializes all session creation for one account on the
// durable user row. This makes the quota exact and keeps an idempotent Agent
// creation replay valid even when the account has reached the limit.
func (r *Repository) createOrGetLocked(ctx context.Context, session *model.Session, allowReplay bool) (*model.Session, bool, error) {
	var result *model.Session
	created := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("id = ?", session.UserID).First(&user).Error; err != nil {
			return err
		}
		if allowReplay && session.CreationRequestKey != nil {
			var existing model.Session
			err := tx.Where("user_id = ? AND creation_request_key = ?", session.UserID, *session.CreationRequestKey).First(&existing).Error
			if err == nil {
				if existing.Mode != session.Mode || existing.CreationInputHash != session.CreationInputHash {
					return ErrSessionRequestConflict
				}
				result = &existing
				return nil
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		var count int64
		if err := tx.Model(&model.Session{}).Where("user_id = ?", session.UserID).Count(&count).Error; err != nil {
			return err
		}
		if count >= model.MaxSessionsPerUser {
			return ErrSessionLimitReached
		}
		if err := tx.Create(session).Error; err != nil {
			return err
		}
		result, created = session, true
		return nil
	})
	return result, created, err
}

func (r *Repository) ListByUserID(ctx context.Context, userID int64) ([]model.Session, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("session repository is not initialized")
	}
	if userID <= 0 {
		return nil, fmt.Errorf("user id must be positive")
	}
	var sessions []model.Session
	err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("updated_at DESC").
		Find(&sessions).Error
	return sessions, err
}

func (r *Repository) ListOwned(ctx context.Context) ([]model.Session, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("session repository is not initialized")
	}
	var sessions []model.Session
	err := r.db.WithContext(ctx).
		Where("user_id > 0").
		Order("created_at ASC").
		Find(&sessions).Error
	return sessions, err
}

func (r *Repository) GetByIDAndUserID(ctx context.Context, sessionID string, userID int64) (*model.Session, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("session repository is not initialized")
	}
	if sessionID == "" || userID <= 0 {
		return nil, ErrSessionNotFound
	}
	var result model.Session
	err := r.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", sessionID, userID).
		First(&result).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &result, nil
}
