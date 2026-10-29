package episode

import (
	"GopherAI/common/mysql"
	"GopherAI/model"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrEpisodeNotFound = errors.New("episode memory not found")
	ErrEpisodeConflict = errors.New("episode memory version conflict")
	ErrJobNotFound     = errors.New("episode projection job not found")
	ErrJobConflict     = errors.New("episode projection job conflict")
)

type Repository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

func DefaultRepository() *Repository {
	return NewRepository(mysql.DB)
}

// GetBySession returns an Episode only while its source Session is active.
// A GORM soft delete cannot trigger a database cascade, so the active-session
// join is part of the privacy boundary rather than a cleanup optimization.
func (r *Repository) GetBySession(ctx context.Context, userID int64, sessionID string) (*model.EpisodeMemory, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("episode repository is not initialized")
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, ErrEpisodeNotFound
	}
	var episode model.EpisodeMemory
	err := activeEpisodeQuery(r.db.WithContext(ctx)).
		Where("episode_memories.user_id = ? AND episode_memories.session_id = ?", userID, sessionID).
		First(&episode).Error
	return episodeResult(&episode, err)
}

func (r *Repository) GetByID(ctx context.Context, userID int64, episodeID string) (*model.EpisodeMemory, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("episode repository is not initialized")
	}
	if userID <= 0 || strings.TrimSpace(episodeID) == "" {
		return nil, ErrEpisodeNotFound
	}
	var episode model.EpisodeMemory
	err := activeEpisodeQuery(r.db.WithContext(ctx)).
		Where("episode_memories.user_id = ? AND episode_memories.id = ?", userID, episodeID).
		First(&episode).Error
	return episodeResult(&episode, err)
}

func activeEpisodeQuery(db *gorm.DB) *gorm.DB {
	return db.Model(&model.EpisodeMemory{}).
		Select("episode_memories.*").
		Joins("JOIN sessions ON sessions.id = episode_memories.session_id AND sessions.user_id = episode_memories.user_id AND sessions.deleted_at IS NULL")
}

func episodeResult(episode *model.EpisodeMemory, err error) (*model.EpisodeMemory, error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrEpisodeNotFound
	}
	if err != nil {
		return nil, err
	}
	return episode, nil
}

// Save publishes a new Episode or advances an existing Episode with optimistic
// concurrency. expectedVersion=0 creates; later writes must preserve the same
// ID and move the complete-message watermark forward.
func (r *Repository) Save(ctx context.Context, episode *model.EpisodeMemory, expectedVersion uint64) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("episode repository is not initialized")
	}
	if episode == nil {
		return fmt.Errorf("episode memory is required")
	}
	next := *episode
	if expectedVersion == 0 {
		next.Version = 1
	} else {
		next.Version = expectedVersion + 1
	}
	if err := next.Validate(); err != nil {
		return err
	}
	documentJSON, err := json.Marshal(next.Document)
	if err != nil {
		return fmt.Errorf("encode episode document: %w", err)
	}
	topicJSON, err := json.Marshal(next.TopicKeys)
	if err != nil {
		return fmt.Errorf("encode episode topic keys: %w", err)
	}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireActiveSession(tx, next.UserID, next.SessionID, true); err != nil {
			return ErrEpisodeNotFound
		}
		if expectedVersion == 0 {
			if err := tx.Create(&next).Error; err != nil {
				if isDuplicateKey(err) {
					return ErrEpisodeConflict
				}
				return err
			}
			return nil
		}
		result := tx.Model(&model.EpisodeMemory{}).
			Where("id = ? AND user_id = ? AND session_id = ? AND version = ? AND source_last_message_id < ?",
				next.ID, next.UserID, next.SessionID, expectedVersion, next.SourceLastMessageID).
			Updates(map[string]any{
				"title": next.Title, "content_json": string(documentJSON), "search_text": next.SearchText,
				"topic_keys": string(topicJSON), "source_summary_version": next.SourceSummaryVersion,
				"source_summary_watermark": next.SourceSummaryWatermark, "source_last_message_id": next.SourceLastMessageID,
				"last_message_at": next.LastMessageAt, "version": next.Version, "status": next.Status, "index_status": next.IndexStatus,
				// The reason has to move with the status. A row that stopped
				// being an abstention while still carrying why it abstained
				// would be memory that also explains why there is none.
				"abstain_reason": next.AbstainReason,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEpisodeConflict
		}
		return nil
	})
	if err != nil {
		return err
	}
	episode.Version = next.Version
	episode.CreatedAt = next.CreatedAt
	episode.UpdatedAt = next.UpdatedAt
	return nil
}

// DeleteBySession removes both derived Episode rows. It is safe to call before
// a future soft-delete workflow; hard deletes are additionally protected by
// database ON DELETE CASCADE constraints.
func (r *Repository) DeleteBySession(ctx context.Context, userID int64, sessionID string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("episode repository is not initialized")
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("episode owner and session are required")
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND session_id = ?", userID, sessionID).Delete(&model.EpisodeProjectionJob{}).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ? AND session_id = ?", userID, sessionID).Delete(&model.EpisodeMemory{}).Error
	})
}

func requireActiveSession(db *gorm.DB, userID int64, sessionID string, lock bool) error {
	query := db.Model(&model.Session{})
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var session model.Session
	err := query.Where("id = ? AND user_id = ?", sessionID, userID).First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrEpisodeNotFound
	}
	return err
}

func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	value := err.Error()
	for _, marker := range []string{"Error 1062", "Duplicate entry", "UNIQUE constraint failed"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}
