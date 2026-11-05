package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// The whiteboard is the current effective state of one session: what the learner
// is doing now, which constraints are in force, which plan and numbers are
// current, and what is still open.
//
// It is rewritten rather than extended. A value that changed replaces the old
// one under the same key, so the board never accumulates history it has already
// superseded - the failure mode that made a rolling summary grow until it could
// not be written at all.
//
// The board is sent to the model on every turn, so its size is a fixed cost
// rather than a growing one. Every limit here exists to keep that cost bounded.
const (
	// SessionWhiteboardSchemaV1 is the version of the whiteboard document.
	SessionWhiteboardSchemaV1 = 1

	// SessionWhiteboardItemLimit is how many entries one board category may hold.
	// A category that reaches the limit has its oldest entries merged away, and
	// the removal is recorded as a change so the superseded value stays
	// answerable from the summary segments.
	SessionWhiteboardItemLimit = 20

	// SessionWhiteboardTextMaxRunes bounds one entry's text. A board entry is a
	// sentence, not a paragraph.
	SessionWhiteboardTextMaxRunes = 200

	// SessionWhiteboardEvidenceLimit is how many source message ids one entry may
	// cite. An entry citing more than this is describing a stretch of history
	// rather than a current fact.
	SessionWhiteboardEvidenceLimit = 8

	// SessionWhiteboardTokenLimit bounds the whole board. The board travels with
	// every turn, so its ceiling is what keeps the per-turn cost flat.
	SessionWhiteboardTokenLimit = 1200
)

// SessionWhiteboardEntry is one current fact on the board.
//
// Key is what makes overwriting possible: two entries about the same thing carry
// the same key, and the newer one replaces the older. An entry without a key
// cannot be overwritten and can only be added.
type SessionWhiteboardEntry struct {
	// Key identifies what the entry is about, so a changed value replaces the old
	// one instead of joining it. It is optional; an entry without one is
	// additive only.
	Key string `json:"key,omitempty"`
	// Text is the human-readable statement of the entry.
	Text string `json:"text"`
	// Value is the literal value of the entry when it has one, such as a
	// duration or a name. It is what a later change is compared against.
	Value string `json:"value,omitempty"`
	// State is the entry's lifecycle marker. Only plan entries use it.
	State string `json:"state,omitempty"`
	// SourceMessageIDs are the learner's own messages this entry came from.
	// They are what authorises reading the original wording back.
	SourceMessageIDs []uint `json:"source_message_ids,omitempty"`
	// UpdatedAtMessageID is the last message that changed this entry.
	UpdatedAtMessageID uint `json:"updated_at_message_id,omitempty"`
}

// SessionWhiteboardGoal is the learner's current goal. It is a single entry
// rather than a list because a session has one goal at a time; keeping older
// goals would be history, and history belongs to the summary segments.
type SessionWhiteboardGoal struct {
	Text               string `json:"text"`
	SourceMessageIDs   []uint `json:"source_message_ids,omitempty"`
	UpdatedAtMessageID uint   `json:"updated_at_message_id,omitempty"`
}

// ErrWhiteboardNotFound means the session has no board yet.
//
// It lives here rather than in the repository because the reader that has to
// tell "no board yet" from "a board I may not read" is not the only one that
// needs to know: the writer compares a version it read against the version it
// is about to write, and it has to recognise the same condition.
var ErrWhiteboardNotFound = errors.New("session whiteboard not found")

// ErrWhiteboardConflict means another writer moved the board on.
//
// It is not a failure: the caller re-reads and decides. The work that was
// done is not lost, only the write was refused.
var ErrWhiteboardConflict = errors.New("session whiteboard version conflict")

// SessionWhiteboardDocument is the stored board.
type SessionWhiteboardDocument struct {
	SchemaVersion int                      `json:"schema_version"`
	Goal          *SessionWhiteboardGoal   `json:"goal,omitempty"`
	Constraints   []SessionWhiteboardEntry `json:"constraints,omitempty"`
	Plan          []SessionWhiteboardEntry `json:"plan,omitempty"`
	OpenItems     []SessionWhiteboardEntry `json:"open_items,omitempty"`
}

// DecodeSessionWhiteboardDocument parses a stored board.
//
// Unknown fields are refused rather than ignored: a board that grows a field
// without this decoder knowing about it is a board whose meaning has changed
// underneath the code that reads it.
func DecodeSessionWhiteboardDocument(content string) (SessionWhiteboardDocument, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return SessionWhiteboardDocument{}, fmt.Errorf("session whiteboard is empty")
	}
	if !strings.HasPrefix(content, "{") {
		return SessionWhiteboardDocument{}, fmt.Errorf("session whiteboard must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var document SessionWhiteboardDocument
	if err := decoder.Decode(&document); err != nil {
		return SessionWhiteboardDocument{}, fmt.Errorf("decode session whiteboard: %w", err)
	}
	if err := ensureWhiteboardJSONEOF(decoder); err != nil {
		return SessionWhiteboardDocument{}, err
	}
	if document.SchemaVersion != SessionWhiteboardSchemaV1 {
		return SessionWhiteboardDocument{}, fmt.Errorf("unsupported session whiteboard schema version %d", document.SchemaVersion)
	}
	if err := document.Validate(); err != nil {
		return SessionWhiteboardDocument{}, err
	}
	return document, nil
}

func ensureWhiteboardJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing session whiteboard: %w", err)
	}
	return fmt.Errorf("session whiteboard contains trailing JSON")
}

// Validate checks the board's shape and bounds.
//
// The checks here are the ones a reader can rely on: a board that passed them
// has a bounded size, and every entry it carries can be traced to messages.
func (d *SessionWhiteboardDocument) Validate() error {
	if d == nil {
		return fmt.Errorf("session whiteboard is nil")
	}
	if d.SchemaVersion != SessionWhiteboardSchemaV1 {
		return fmt.Errorf("session whiteboard schema version must be %d", SessionWhiteboardSchemaV1)
	}
	if d.Goal != nil {
		if err := validateWhiteboardGoal(*d.Goal); err != nil {
			return err
		}
	}
	for _, group := range [][]SessionWhiteboardEntry{d.Constraints, d.Plan, d.OpenItems} {
		if len(group) > SessionWhiteboardItemLimit {
			return fmt.Errorf("session whiteboard has too many entries")
		}
		for _, entry := range group {
			if err := entry.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateWhiteboardGoal(goal SessionWhiteboardGoal) error {
	if len([]rune(strings.TrimSpace(goal.Text))) > SessionWhiteboardTextMaxRunes {
		return fmt.Errorf("session whiteboard goal is too long")
	}
	return validateWhiteboardEvidence(goal.SourceMessageIDs)
}

// Validate checks one entry.
func (e SessionWhiteboardEntry) Validate() error {
	if strings.TrimSpace(e.Text) == "" {
		return fmt.Errorf("session whiteboard entry has no text")
	}
	if len([]rune(strings.TrimSpace(e.Text))) > SessionWhiteboardTextMaxRunes {
		return fmt.Errorf("session whiteboard entry is too long")
	}
	if len([]rune(strings.TrimSpace(e.Key))) > SessionWhiteboardTextMaxRunes {
		return fmt.Errorf("session whiteboard entry key is too long")
	}
	if len([]rune(strings.TrimSpace(e.Value))) > SessionWhiteboardTextMaxRunes {
		return fmt.Errorf("session whiteboard entry value is too long")
	}
	switch e.State {
	case "", "open", "done":
	default:
		return fmt.Errorf("session whiteboard entry has an unknown state %q", e.State)
	}
	return validateWhiteboardEvidence(e.SourceMessageIDs)
}

func validateWhiteboardEvidence(ids []uint) error {
	if len(ids) > SessionWhiteboardEvidenceLimit {
		return fmt.Errorf("session whiteboard entry cites too many messages")
	}
	seen := make(map[uint]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			return fmt.Errorf("session whiteboard evidence id must be positive")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate session whiteboard evidence id %d", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// EvidenceMessageIDs returns every message id the board cites, sorted and
// deduplicated. It is what authorises reading the learner's own words back.
func (d SessionWhiteboardDocument) EvidenceMessageIDs() []uint {
	seen := make(map[uint]struct{})
	if d.Goal != nil {
		for _, id := range d.Goal.SourceMessageIDs {
			seen[id] = struct{}{}
		}
	}
	for _, group := range [][]SessionWhiteboardEntry{d.Constraints, d.Plan, d.OpenItems} {
		for _, entry := range group {
			for _, id := range entry.SourceMessageIDs {
				seen[id] = struct{}{}
			}
		}
	}
	ids := make([]uint, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// SessionWhiteboard is the stored board of one session.
//
// It is one row per session, replaced in place. Version is the compare-and-set
// token: a writer that built a board on version N can only commit if the stored
// row is still at N, so a slow generation cannot overwrite a newer one.
type SessionWhiteboard struct {
	ID               uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID           int64     `gorm:"not null;uniqueIndex:idx_session_whiteboards_user_session,priority:1" json:"user_id"`
	SessionID        string    `gorm:"type:varchar(36);not null;uniqueIndex:idx_session_whiteboards_user_session,priority:2" json:"session_id"`
	Content          string    `gorm:"type:longtext;not null" json:"content"`
	CoveredMessageID uint      `gorm:"not null;default:0;index" json:"covered_message_id"`
	TokenEstimate    int       `gorm:"not null;default:0" json:"token_estimate"`
	Version          uint64    `gorm:"not null;default:1" json:"version"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Session          Session   `gorm:"foreignKey:SessionID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"-"`
}

// Validate checks the stored row.
func (w *SessionWhiteboard) Validate() error {
	if w == nil {
		return fmt.Errorf("session whiteboard is nil")
	}
	if w.UserID <= 0 || strings.TrimSpace(w.SessionID) == "" {
		return fmt.Errorf("session whiteboard identity is required")
	}
	if strings.TrimSpace(w.Content) == "" {
		return fmt.Errorf("session whiteboard content is required")
	}
	if w.TokenEstimate < 0 {
		return fmt.Errorf("session whiteboard token estimate cannot be negative")
	}
	return nil
}

// MarshalSessionWhiteboardDocument encodes a board for storage.
//
// The encoding is canonical so that two boards with the same content produce the
// same bytes, which is what lets a writer tell "nothing changed" from "changed
// and changed back".
func MarshalSessionWhiteboardDocument(document SessionWhiteboardDocument) (string, error) {
	if err := document.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode session whiteboard: %w", err)
	}
	return string(encoded), nil
}

// jsonEquals reports whether two documents encode to the same bytes.
func jsonEquals(left, right any) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return bytes.Equal(leftBytes, rightBytes)
}
