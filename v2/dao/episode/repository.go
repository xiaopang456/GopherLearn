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

// GetBySession returns the Episode of a session while its source Session is
// active. A GORM soft delete cannot trigger a database cascade, so the
// active-session join is part of the privacy boundary rather than a cleanup
// optimization.
//
// The kind is part of the lookup because one session may hold more than one
// derived row: an Episode written before the session-card path existed and a
// session card written after it live side by side, and neither overwrites the
// other.
func (r *Repository) GetBySession(ctx context.Context, userID int64, sessionID string) (*model.EpisodeMemory, error) {
	return r.GetBySessionKind(ctx, userID, sessionID, model.EpisodeContentEpisode)
}

// GetCardBySession returns the session card of a session.
//
// It is the same lookup as GetBySession with a different kind, and it is a
// separate function rather than a parameter at every call site: a caller that
// wants a card and gets an Episode would render the wrong shape, and the
// mistake is easier to make with one argument than with two.
func (r *Repository) GetCardBySession(ctx context.Context, userID int64, sessionID string) (*model.EpisodeMemory, error) {
	return r.GetBySessionKind(ctx, userID, sessionID, model.EpisodeContentSessionCard)
}

// GetBySessionKind returns one derived row of a session by its content kind.
func (r *Repository) GetBySessionKind(ctx context.Context, userID int64, sessionID string,
	kind model.EpisodeContentKind) (*model.EpisodeMemory, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("episode repository is not initialized")
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, ErrEpisodeNotFound
	}
	kind = kind.Normalized()
	if !kind.Valid() {
		return nil, ErrEpisodeNotFound
	}
	var episode model.EpisodeMemory
	err := activeEpisodeQuery(r.db.WithContext(ctx)).
		Where("episode_memories.user_id = ? AND episode_memories.session_id = ? AND episode_memories.content_kind = ?",
			userID, sessionID, string(kind)).
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

// Save publishes a new derived row or advances an existing one with optimistic
// concurrency. expectedVersion=0 creates; later writes must preserve the same
// ID and move the complete-message watermark forward.
//
// The thin card is the one row that may be rewritten without moving forward:
// see watermarkAdvance for why, and for the checks that are kept.
//
// The two content kinds are stored in separate columns, and each row carries
// exactly one of them: the discriminator column says which, the other column
// is left empty. A row that carried both would leave every reader asking
// which document it had.
func (r *Repository) Save(ctx context.Context, episode *model.EpisodeMemory, expectedVersion uint64) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("episode repository is not initialized")
	}
	if episode == nil {
		return fmt.Errorf("episode memory is required")
	}
	next := *episode
	next.ContentKind = next.ContentKind.Normalized()
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
	cardJSON, err := json.Marshal(next.Card)
	if err != nil {
		return fmt.Errorf("encode session card document: %w", err)
	}
	topicJSON, err := json.Marshal(next.TopicKeys)
	if err != nil {
		return fmt.Errorf("encode episode topic keys: %w", err)
	}
	// The column that does not belong to this row's kind is written as NULL
	// rather than as an empty document, so "this row carries no card" and
	// "this row carries a card with nothing in it" stay different facts.
	var contentValue any = string(documentJSON)
	var cardValue any
	if next.ContentKind == model.EpisodeContentSessionCard {
		contentValue = nil
		cardValue = string(cardJSON)
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
			Where("id = ? AND user_id = ? AND session_id = ? AND content_kind = ? AND version = ? AND "+watermarkAdvance(next),
				next.ID, next.UserID, next.SessionID, string(next.ContentKind), expectedVersion, next.SourceLastMessageID).
			Updates(map[string]any{
				"title": next.Title, "content_json": contentValue, "card_json": cardValue,
				"search_text": next.SearchText,
				"topic_keys":  string(topicJSON), "source_summary_version": next.SourceSummaryVersion,
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

// watermarkAdvance is how far a row's watermark has to move for this write to
// be allowed.
//
// The rule for every row but the thin card is that the write has to reach at
// least as far as the row does: a projection that read less than the row
// already holds would replace a wider reach with a narrower one.
//
// The thin card is the one exception, and the reason is specific to what it
// is. A card's clues are its topics and its segment refs, and a NEW SUMMARY
// SEGMENT can land without the session having moved: the segment summarizes
// turns the card has already been told about. The session's last message id
// is the same, the card's watermark is the same, and the card has to be
// rewritten anyway - otherwise the card keeps pointing at the session's older
// state and the summary that was just written is invisible to every reader
// that goes through the card.
//
// The version check is NOT relaxed: a stale writer still loses, and the
// watermark still may not go backwards. Only the "strictly further" half of
// the watermark check becomes "at least as far".
func watermarkAdvance(next model.EpisodeMemory) string {
	if next.ContentKind.Normalized() == model.EpisodeContentSessionCard && next.Card.IsThin() {
		return "source_last_message_id <= ?"
	}
	return "source_last_message_id < ?"
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
