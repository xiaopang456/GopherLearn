package gormmysql

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// The SQL half of the topic-memory consent. It follows the rolling summary rather than the
// sessions table on purpose: the conversation row stays immutable, and a decision a learner can
// change mid-conversation gets its own row, its own commit verb, and its own cleanup when the
// conversation is deleted.
//
// Neither method reads SessionRecord.mode. Which conversations may hold this permission is a
// service rule; storage enforces ownership, and the recall path checks the mode again, so the
// two cannot agree to grant something neither will grant alone.

// GetTopicMemoryPreference reads one conversation's consent.
//
// A conversation that has never been asked answers Enabled=false with no error, which is the
// whole point of putting this next to the recall path: a caller cannot mistake "no row" for a
// reason to read topic memory. A conversation that is somebody else's, and one that never
// existed, are both ErrNotFound - the same pair this repository refuses to distinguish anywhere
// else.
func (r *Repository) GetTopicMemoryPreference(ctx context.Context, userID, sessionID string) (domain.TopicMemoryPreference, error) {
	if err := checkContext(ctx); err != nil {
		return domain.TopicMemoryPreference{}, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return domain.TopicMemoryPreference{}, repository.ErrInvalidArgument
	}

	var record TopicMemoryRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireOwnedSession(tx, userID, sessionID); err != nil {
			return err
		}
		lookup := tx.Where("session_id = ?", sessionID).Take(&record).Error
		if errors.Is(lookup, gorm.ErrRecordNotFound) {
			// No decision yet. The identity is filled in so a caller reads a permission that
			// says no, not an empty value it has to interpret.
			record = TopicMemoryRecord{SessionID: sessionID, UserID: userID}
			return nil
		}
		return lookup
	})
	if err != nil {
		return domain.TopicMemoryPreference{}, translate(err)
	}
	return record.toDomain(), nil
}

// CommitTopicMemoryPreference stores the learner's answer for one conversation.
//
// Repeating the same answer writes nothing at all, so a double click cannot move the timestamp
// that says when the learner decided. The one race worth naming is two concurrent *first*
// commits: the loser's insert hits the primary key, and it retries once against a row that now
// exists, so both callers end up reporting the same stored state instead of one of them
// meeting a conflict for clicking twice.
func (r *Repository) CommitTopicMemoryPreference(ctx context.Context, userID, sessionID string,
	enabled bool) (*domain.TopicMemoryPreference, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, repository.ErrInvalidArgument
	}

	preference, err := r.commitTopicMemoryOnce(ctx, userID, sessionID, enabled)
	if errors.Is(translate(err), repository.ErrConflict) {
		preference, err = r.commitTopicMemoryOnce(ctx, userID, sessionID, enabled)
	}
	if err != nil {
		return nil, translate(err)
	}
	return preference, nil
}

func (r *Repository) commitTopicMemoryOnce(ctx context.Context, userID, sessionID string,
	enabled bool) (*domain.TopicMemoryPreference, error) {
	var record TopicMemoryRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireOwnedSession(tx, userID, sessionID); err != nil {
			return err
		}
		lookup := tx.Where("session_id = ?", sessionID).Take(&record).Error
		if errors.Is(lookup, gorm.ErrRecordNotFound) {
			now := time.Now().UTC()
			record = TopicMemoryRecord{
				SessionID: sessionID, UserID: userID, Enabled: enabled,
				CreatedAt: now, UpdatedAt: now,
			}
			return tx.Create(&record).Error
		}
		if lookup != nil {
			return lookup
		}
		if record.Enabled == enabled {
			// Already the answer being committed: no write, no moved timestamp.
			return nil
		}
		// A map, not a struct: GORM skips zero values in a struct update, so switching the
		// permission *off* - the one direction a learner is entitled to trust - would be
		// silently dropped on the floor.
		return tx.Model(&TopicMemoryRecord{}).
			Where("session_id = ? AND user_id = ?", sessionID, userID).
			Updates(map[string]any{"enabled": enabled, "updated_at": time.Now().UTC()}).Error
	})
	if err != nil {
		return nil, err
	}
	// Read the row back rather than assembling what we meant to write: the answer this method
	// reports must be the stored state, including under the concurrent-first-commit retry above.
	var stored TopicMemoryRecord
	err = r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireOwnedSession(tx, userID, sessionID); err != nil {
			return err
		}
		return tx.Where("session_id = ?", sessionID).Take(&stored).Error
	})
	if err != nil {
		return nil, translate(err)
	}
	preference := stored.toDomain()
	return &preference, nil
}
