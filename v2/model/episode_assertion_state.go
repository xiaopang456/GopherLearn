package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrAssertionVersionConflict is raised when a governance write loses the
// optimistic race. It lives here so the DAO that raises it and the service that
// classifies it can share one sentinel without importing each other.
var ErrAssertionVersionConflict = errors.New("assertion state version conflict")

// EpisodeAssertionState is the governance row for one assertion.
//
// It is a table rather than a field inside the Episode's content_json for four
// reasons, and each of them is a defect avoided rather than a preference:
//
//  1. The document is derived. It is rebuilt from Messages, so a relation
//     stored inside it is destroyed by the next re-projection.
//  2. The write cadence differs. Governance is written by a user action or, one
//     day, by a write-side evaluator; projection happens when a session moves.
//  3. Concurrency granularity. Changing one assertion's state must not bump the
//     whole Episode's Version and collide with edits to its other slots.
//  4. Queryability. "What replaced this" is a join, not a JSON scan.
type EpisodeAssertionState struct {
	AssertionID string         `gorm:"primaryKey;type:varchar(200)" json:"assertion_id"`
	UserID      int64          `gorm:"not null;index:idx_assertion_owner_episode,priority:1" json:"user_id"`
	EpisodeID   string         `gorm:"type:varchar(36);not null;index:idx_assertion_owner_episode,priority:2" json:"episode_id"`
	Kind        AssertionKind  `gorm:"type:varchar(16);not null" json:"kind"`
	State       AssertionState `gorm:"type:varchar(16);not null;index" json:"state"`
	// ReplacedByAssertionID and ReplacedByEpisodeID are set together or not at
	// all. The Episode id is denormalised so a reader can reject a relation
	// whose target card has since been archived without a second lookup.
	ReplacedByAssertionID string    `gorm:"type:varchar(200)" json:"replaced_by_assertion_id,omitempty"`
	ReplacedByEpisodeID   string    `gorm:"type:varchar(36)" json:"replaced_by_episode_id,omitempty"`
	OccurredAt            time.Time `gorm:"not null;index" json:"occurred_at"`
	// Version is optimistic concurrency for this row alone.
	Version   uint64    `gorm:"not null;default:1" json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (EpisodeAssertionState) TableName() string { return "episode_assertion_states" }

// Validate checks one row in isolation. Relation-level checks that need other
// rows - existence, ordering, cycles - live in the governance service, because
// they cannot be answered by looking at this row alone.
func (s *EpisodeAssertionState) Validate() error {
	if s == nil {
		return fmt.Errorf("assertion state is nil")
	}
	if strings.TrimSpace(s.AssertionID) == "" || s.UserID <= 0 || strings.TrimSpace(s.EpisodeID) == "" {
		return fmt.Errorf("assertion state needs an id, an owner and an episode")
	}
	// The identity is opaque in v1.1, so the episode and the kind come from
	// their own columns rather than from parsing the id. That is the point of
	// making it opaque: nothing can be inferred from an identity, so nothing
	// breaks when a document is rearranged.
	if !ValidAssertionID(s.AssertionID) {
		return fmt.Errorf("assertion id %q was not issued by this server", s.AssertionID)
	}
	if !s.Kind.Valid() || !s.State.Valid() {
		return fmt.Errorf("assertion state has an unknown kind or state")
	}
	if s.OccurredAt.IsZero() || s.Version == 0 {
		return fmt.Errorf("assertion state needs an occurrence time and a version")
	}
	hasTarget := strings.TrimSpace(s.ReplacedByAssertionID) != ""
	if hasTarget != (strings.TrimSpace(s.ReplacedByEpisodeID) != "") {
		return fmt.Errorf("replaced_by assertion and episode must be set together")
	}
	if s.State == AssertionReplaced && !hasTarget {
		return fmt.Errorf("a replaced assertion must name what replaced it")
	}
	if s.State != AssertionReplaced && hasTarget {
		return fmt.Errorf("only a replaced assertion may name a replacement")
	}
	if hasTarget {
		if s.ReplacedByAssertionID == s.AssertionID {
			return fmt.Errorf("an assertion cannot replace itself")
		}
		if !ValidAssertionID(s.ReplacedByAssertionID) {
			return fmt.Errorf("replacement id %q was not issued by this server", s.ReplacedByAssertionID)
		}
	}
	return nil
}

// NewAssertionState builds an unknown-state row for a resolved assertion.
//
// Unknown is the only state this constructor can produce, on purpose: a
// migration or a backfill that could mint `current` rows would be a way to
// declare a batch of unverified legacy assertions to be the present.
func NewAssertionState(item Assertion) EpisodeAssertionState {
	return EpisodeAssertionState{
		AssertionID: item.ID, UserID: item.UserID, EpisodeID: item.EpisodeID,
		Kind: item.Kind, State: AssertionUnknown, OccurredAt: item.OccurredAt, Version: 1,
	}
}
