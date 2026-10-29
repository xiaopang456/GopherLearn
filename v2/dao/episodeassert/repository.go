// Package episodeassert is the persistence behind assertion time governance.
//
// It is a real repository rather than the in-memory fake the service tests use,
// because three of the guarantees this feature rests on are properties of the
// database and not of Go code: the optimistic version has to be enforced by a
// conditional UPDATE, the readable-Episode rule has to be the same join the
// rest of the system uses, and a store failure has to surface as a failure
// rather than as an empty result that reads like "no governance recorded".
package episodeassert

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"GopherAI/model"

	"gorm.io/gorm"
)

// ErrEpisodeUnavailable means the card cannot be read by this learner: it does
// not exist, it is archived or deleted, its session is gone, or it belongs to
// someone else. The reasons are deliberately not distinguished to the caller.
var ErrEpisodeUnavailable = errors.New("episode is not readable")

type Repository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) (*Repository, error) {
	if db == nil {
		return nil, fmt.Errorf("assertion state repository needs a database")
	}
	return &Repository{db: db}, nil
}

// LoadStates reads governance rows for one learner.
//
// A missing row is simply absent from the map, which the model reads as
// unknown. An error is returned as an error: a database failure that came back
// as an empty map would silently downgrade every replaced assertion to unknown,
// and unknown is permissive enough to be quoted as history.
func (r *Repository) LoadStates(ctx context.Context, userID int64,
	assertionIDs []string) (map[string]model.EpisodeAssertionState, error) {
	out := map[string]model.EpisodeAssertionState{}
	if userID <= 0 || len(assertionIDs) == 0 {
		return out, nil
	}
	var rows []model.EpisodeAssertionState
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND assertion_id IN ?", userID, assertionIDs).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load assertion states: %w", err)
	}
	for _, row := range rows {
		out[row.AssertionID] = row
	}
	return out, nil
}

// LoadEpisode reads one Episode the learner is entitled to see.
//
// The conditions are the ones the retrieval layer already applies - owner,
// active status, indexed, and a session that still exists - so a card that
// memory_search would refuse cannot become the target of a relation through
// this door instead.
func (r *Repository) LoadEpisode(ctx context.Context, userID int64,
	episodeID string) (model.EpisodeMemory, error) {
	var row model.EpisodeMemory
	err := r.db.WithContext(ctx).Model(&model.EpisodeMemory{}).
		Select("episode_memories.*").
		Joins("JOIN sessions ON sessions.id = episode_memories.session_id"+
			" AND sessions.user_id = episode_memories.user_id AND sessions.deleted_at IS NULL").
		Where("episode_memories.user_id = ? AND episode_memories.id = ?"+
			" AND episode_memories.status = ? AND episode_memories.index_status = ?",
			userID, episodeID, model.EpisodeStatusActive, model.EpisodeIndexIndexed).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.EpisodeMemory{}, ErrEpisodeUnavailable
	}
	if err != nil {
		return model.EpisodeMemory{}, fmt.Errorf("load episode: %w", err)
	}
	if row.UserID != userID {
		// Defence in depth: the query is scoped, and a scoped query that ever
		// returned a foreign row would be a silent cross-tenant read.
		return model.EpisodeMemory{}, ErrEpisodeUnavailable
	}
	return row, nil
}

// GovernedAssertions is what a re-projection of one card must not lose.
//
// Two sets, and both are relations pointing at this card's assertions: the
// assertions that carry a state of their own, and the assertions some other
// assertion names as its replacement. The second is the one that is easy to
// forget - an assertion with no row of its own can still be the thing three
// other decisions were superseded by, and deleting it would leave all three
// saying "replaced by" nothing at all.
//
// Assertions whose only state is unknown are deliberately not included: an
// unknown row records the absence of a relation, and refusing a projection to
// protect it would freeze every legacy card against ever being re-derived.
func (r *Repository) GovernedAssertions(ctx context.Context, userID int64,
	episodeID string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if userID <= 0 || strings.TrimSpace(episodeID) == "" {
		return out, nil
	}
	var stated []string
	if err := r.db.WithContext(ctx).Model(&model.EpisodeAssertionState{}).
		Where("user_id = ? AND episode_id = ? AND state IN ?", userID, episodeID,
			[]model.AssertionState{model.AssertionCurrent, model.AssertionReplaced}).
		Pluck("assertion_id", &stated).Error; err != nil {
		return nil, fmt.Errorf("read governed assertions: %w", err)
	}
	var targeted []string
	if err := r.db.WithContext(ctx).Model(&model.EpisodeAssertionState{}).
		Where("user_id = ? AND replaced_by_episode_id = ? AND replaced_by_assertion_id <> ''",
			userID, episodeID).
		Pluck("replaced_by_assertion_id", &targeted).Error; err != nil {
		return nil, fmt.Errorf("read replacement targets: %w", err)
	}
	for _, group := range [][]string{stated, targeted} {
		for _, id := range group {
			if strings.TrimSpace(id) != "" {
				out[id] = struct{}{}
			}
		}
	}
	return out, nil
}

// SaveState writes one governance row under optimistic concurrency.
//
// The version check is a condition on the UPDATE rather than a read-then-write,
// because two callers reading the same version and both writing is exactly the
// race this guards. expectedVersion zero means "there is no row yet", and the
// insert fails if one appeared in the meantime.
func (r *Repository) SaveState(ctx context.Context, state model.EpisodeAssertionState,
	expectedVersion uint64) error {
	if err := state.Validate(); err != nil {
		return err
	}
	if state.Version != expectedVersion+1 {
		return fmt.Errorf("assertion state version must advance by exactly one")
	}
	now := time.Now().UTC()
	if expectedVersion == 0 {
		state.CreatedAt, state.UpdatedAt = now, now
		err := r.db.WithContext(ctx).Create(&state).Error
		if err != nil && isDuplicate(err) {
			return errVersionConflict()
		}
		return err
	}
	state.UpdatedAt = now
	result := r.db.WithContext(ctx).Model(&model.EpisodeAssertionState{}).
		Where("assertion_id = ? AND user_id = ? AND version = ?",
			state.AssertionID, state.UserID, expectedVersion).
		Updates(map[string]any{
			"state":                    state.State,
			"kind":                     state.Kind,
			"episode_id":               state.EpisodeID,
			"replaced_by_assertion_id": state.ReplacedByAssertionID,
			"replaced_by_episode_id":   state.ReplacedByEpisodeID,
			"occurred_at":              state.OccurredAt,
			"version":                  state.Version,
			"updated_at":               state.UpdatedAt,
		})
	if result.Error != nil {
		return fmt.Errorf("save assertion state: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return errVersionConflict()
	}
	return nil
}

// MessageTimes dates the learner messages an assertion cites.
//
// Only this learner's messages are dated. A message id that belongs to someone
// else simply produces no entry, and the assertion is then time-unknown rather
// than dated from a stranger's timeline.
func (r *Repository) MessageTimes(ctx context.Context, userID int64,
	messageIDs []uint) (map[uint]time.Time, error) {
	out := map[uint]time.Time{}
	if userID <= 0 || len(messageIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		ID        uint
		CreatedAt time.Time
	}
	if err := r.db.WithContext(ctx).Model(&model.Message{}).
		Select("messages.id, messages.created_at").
		Joins("JOIN sessions ON sessions.id = messages.session_id AND sessions.deleted_at IS NULL").
		Where("sessions.user_id = ? AND messages.id IN ?", userID, messageIDs).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("date assertion evidence: %w", err)
	}
	for _, row := range rows {
		out[row.ID] = row.CreatedAt.UTC()
	}
	return out, nil
}

// isDuplicate recognises "a row already exists", which for an insert with
// expectedVersion zero means another writer got there first.
//
// The sentinel comes first: with TranslateError on, GORM reports
// gorm.ErrDuplicatedKey whose message is "duplicated key not allowed" and
// matches none of the driver strings. Relying on the strings alone let a lost
// insert race surface as a raw driver error instead of a version conflict, and
// the integration test caught it.
func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	return containsAny(err.Error(), "Duplicate entry", "duplicate key", "UNIQUE constraint")
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if len(needle) > 0 && len(value) >= len(needle) && indexOf(value, needle) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(haystack, needle string) int {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return index
		}
	}
	return -1
}

func errVersionConflict() error { return model.ErrAssertionVersionConflict }
