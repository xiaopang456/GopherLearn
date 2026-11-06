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

// A summary segment covers one stretch of a session's turns and is written once.
//
// Segments are not rewritten. The board beside them carries what is true now;
// a segment carries what happened then. Keeping the two apart is what stops a
// summary from growing with the conversation, and it is what lets a superseded
// value stay answerable: the segment that recorded the old value is still there,
// marked as superseded rather than deleted.
const (
	// SessionSummarySegmentSchemaV1 is the version of a segment's content.
	SessionSummarySegmentSchemaV1 = 1

	// SessionSummarySegmentPointLimit is how many points one segment may carry.
	SessionSummarySegmentPointLimit = 20

	// SessionSummarySegmentPointMaxRunes bounds one point's text.
	SessionSummarySegmentPointMaxRunes = 500

	// SessionSummarySegmentTermLimit is how many names or numbers one segment
	// may list for exact retrieval.
	SessionSummarySegmentTermLimit = 32
)

// Index states of a segment's derived search vector.
const (
	SessionSummarySegmentIndexPending = "pending"
	SessionSummarySegmentIndexIndexed = "indexed"
	SessionSummarySegmentIndexFailed  = "failed"
)

// SessionSummarySegmentPoint is one thing that happened in the segment.
type SessionSummarySegmentPoint struct {
	Text               string `json:"text"`
	EvidenceMessageIDs []uint `json:"evidence_message_ids,omitempty"`
}

// SessionSummarySegmentContent is the stored body of a segment.
type SessionSummarySegmentContent struct {
	SchemaVersion int                          `json:"schema_version"`
	Topic         string                       `json:"topic"`
	Points        []SessionSummarySegmentPoint `json:"points"`
	Names         []string                     `json:"names,omitempty"`
	Numbers       []string                     `json:"numbers,omitempty"`
}

// SessionSummarySegmentChange records that a segment changed one board entry.
//
// The change is computed by comparing the board before and after the segment was
// generated, not written by the model. A model that says it changed something
// cannot be believed about what the board looks like afterwards; two boards can
// be compared.
type SessionSummarySegmentChange struct {
	Key           string `json:"key"`
	OldValue      string `json:"old_value,omitempty"`
	NewValue      string `json:"new_value,omitempty"`
	OldMessageIDs []uint `json:"old_message_ids,omitempty"`
	NewMessageIDs []uint `json:"new_message_ids,omitempty"`
	Removed       bool   `json:"removed,omitempty"`
	// Suspected marks a change the engine inferred rather than observed: the
	// board still carries a value that the learner's own messages look like
	// they replaced. The engine does not know the correct value, so it records
	// the suspicion and leaves the board alone.
	Suspected bool `json:"suspected,omitempty"`
}

// SessionSummarySegment is one written-once stretch of a session's turns.
//
// EndMessageID is the segment's high-water mark: turns at or below it are
// covered by this segment and later ones are not.
type SessionSummarySegment struct {
	ID                uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID            int64     `gorm:"not null;uniqueIndex:idx_summary_segments_user_session,priority:1" json:"user_id"`
	SessionID         string    `gorm:"type:varchar(36);not null;uniqueIndex:idx_summary_segments_user_session,priority:2" json:"session_id"`
	Seq               int       `gorm:"not null;uniqueIndex:idx_summary_segments_user_session,priority:3" json:"seq"`
	StartMessageID    uint      `gorm:"not null;index" json:"start_message_id"`
	EndMessageID      uint      `gorm:"not null;index" json:"end_message_id"`
	StartAt           time.Time `gorm:"not null" json:"start_at"`
	EndAt             time.Time `gorm:"not null" json:"end_at"`
	Topic             string    `gorm:"type:varchar(200)" json:"topic"`
	Content           string    `gorm:"type:longtext;not null" json:"content"`
	NamesAndNumbers   string    `gorm:"type:text" json:"names_and_numbers"`
	WhiteboardChanges string    `gorm:"type:json" json:"whiteboard_changes"`
	TokenEstimate     int       `gorm:"not null;default:0" json:"token_estimate"`
	IndexStatus       string    `gorm:"type:varchar(16);not null;default:pending;index" json:"index_status"`
	CreatedAt         time.Time `json:"created_at"`
	Session           Session   `gorm:"foreignKey:SessionID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"-"`
}

// DecodeSessionSummarySegmentContent parses a segment body.
func DecodeSessionSummarySegmentContent(content string) (SessionSummarySegmentContent, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return SessionSummarySegmentContent{}, fmt.Errorf("summary segment content is empty")
	}
	if !strings.HasPrefix(content, "{") {
		return SessionSummarySegmentContent{}, fmt.Errorf("summary segment must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var document SessionSummarySegmentContent
	if err := decoder.Decode(&document); err != nil {
		return SessionSummarySegmentContent{}, fmt.Errorf("decode summary segment: %w", err)
	}
	if err := ensureSegmentJSONEOF(decoder); err != nil {
		return SessionSummarySegmentContent{}, err
	}
	if document.SchemaVersion != SessionSummarySegmentSchemaV1 {
		return SessionSummarySegmentContent{}, fmt.Errorf("unsupported summary segment schema version %d", document.SchemaVersion)
	}
	if err := document.Validate(); err != nil {
		return SessionSummarySegmentContent{}, err
	}
	return document, nil
}

func ensureSegmentJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing summary segment: %w", err)
	}
	return fmt.Errorf("summary segment contains trailing JSON")
}

// Validate checks a segment body's shape and bounds.
func (c SessionSummarySegmentContent) Validate() error {
	if c.SchemaVersion != SessionSummarySegmentSchemaV1 {
		return fmt.Errorf("summary segment schema version must be %d", SessionSummarySegmentSchemaV1)
	}
	if len([]rune(strings.TrimSpace(c.Topic))) > 200 {
		return fmt.Errorf("summary segment topic is too long")
	}
	if len(c.Points) > SessionSummarySegmentPointLimit {
		return fmt.Errorf("summary segment has too many points")
	}
	for _, point := range c.Points {
		if err := point.Validate(); err != nil {
			return err
		}
	}
	if len(c.Names) > SessionSummarySegmentTermLimit {
		return fmt.Errorf("summary segment has too many names")
	}
	if len(c.Numbers) > SessionSummarySegmentTermLimit {
		return fmt.Errorf("summary segment has too many numbers")
	}
	for _, term := range append(append([]string{}, c.Names...), c.Numbers...) {
		if len([]rune(strings.TrimSpace(term))) > 100 {
			return fmt.Errorf("summary segment term is too long")
		}
	}
	return nil
}

// Validate checks one point.
func (p SessionSummarySegmentPoint) Validate() error {
	if strings.TrimSpace(p.Text) == "" {
		return fmt.Errorf("summary segment point has no text")
	}
	if len([]rune(strings.TrimSpace(p.Text))) > SessionSummarySegmentPointMaxRunes {
		return fmt.Errorf("summary segment point is too long")
	}
	if len(p.EvidenceMessageIDs) > SessionSummaryEvidenceLimit {
		return fmt.Errorf("summary segment point cites too many messages")
	}
	seen := make(map[uint]struct{}, len(p.EvidenceMessageIDs))
	for _, id := range p.EvidenceMessageIDs {
		if id == 0 {
			return fmt.Errorf("summary segment evidence id must be positive")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate summary segment evidence id %d", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// EvidenceMessageIDs returns every message id the segment cites, sorted and
// deduplicated.
func (c SessionSummarySegmentContent) EvidenceMessageIDs() []uint {
	seen := make(map[uint]struct{})
	for _, point := range c.Points {
		for _, id := range point.EvidenceMessageIDs {
			seen[id] = struct{}{}
		}
	}
	ids := make([]uint, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// MarshalSessionSummarySegmentContent encodes a segment body for storage.
func MarshalSessionSummarySegmentContent(content SessionSummarySegmentContent) (string, error) {
	if err := content.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("encode summary segment: %w", err)
	}
	return string(encoded), nil
}

// SegmentChanges is the stored list of board changes a segment caused.
type SegmentChanges []SessionSummarySegmentChange

// UnmarshalJSON accepts an absent list and the stored object form.
func (l *SegmentChanges) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*l = nil
		return nil
	}
	var values []SessionSummarySegmentChange
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return fmt.Errorf("decode summary segment changes: %w", err)
	}
	*l = values
	return nil
}

// Validate checks the stored row.
func (s *SessionSummarySegment) Validate() error {
	if s == nil {
		return fmt.Errorf("summary segment is nil")
	}
	if s.UserID <= 0 || strings.TrimSpace(s.SessionID) == "" {
		return fmt.Errorf("summary segment identity is required")
	}
	if s.Seq <= 0 {
		return fmt.Errorf("summary segment sequence must be positive")
	}
	if s.EndMessageID == 0 {
		return fmt.Errorf("summary segment coverage is required")
	}
	if s.StartMessageID > s.EndMessageID {
		return fmt.Errorf("summary segment coverage is inverted")
	}
	if strings.TrimSpace(s.Content) == "" {
		return fmt.Errorf("summary segment content is required")
	}
	if s.TokenEstimate < 0 {
		return fmt.Errorf("summary segment token estimate cannot be negative")
	}
	switch s.IndexStatus {
	case "", SessionSummarySegmentIndexPending, SessionSummarySegmentIndexIndexed, SessionSummarySegmentIndexFailed:
	default:
		return fmt.Errorf("summary segment has an unknown index status %q", s.IndexStatus)
	}
	return nil
}
