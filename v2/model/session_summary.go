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

const (
	SessionSummarySchemaV2      = 2
	SessionSummaryItemLimit     = 20
	SessionSummaryItemMaxRunes  = 1000
	SessionSummaryEvidenceLimit = 32
	SessionSummarySourceLimit   = 64
)

// SessionSummaryEvidence is the auditable form used by summary schema v2.
// Empty EvidenceMessageIDs are accepted only for legacy v1 items; producers
// must separately prove that such an item was preserved from an old summary.
type SessionSummaryEvidence struct {
	Text               string `json:"text"`
	EvidenceMessageIDs []uint `json:"evidence_message_ids"`
}

type SessionSummaryEvidenceList []SessionSummaryEvidence

// UnmarshalJSON accepts both the v1 string arrays already stored in MySQL and
// v2 evidence objects. MarshalJSON always emits the v2 object representation.
func (l *SessionSummaryEvidenceList) UnmarshalJSON(data []byte) error {
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	items := make(SessionSummaryEvidenceList, 0, len(values))
	for _, value := range values {
		var legacy string
		if err := json.Unmarshal(value, &legacy); err == nil {
			items = append(items, SessionSummaryEvidence{Text: legacy})
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.DisallowUnknownFields()
		var item SessionSummaryEvidence
		if err := decoder.Decode(&item); err != nil {
			return err
		}
		if err := ensureSummaryJSONEOF(decoder); err != nil {
			return err
		}
		items = append(items, item)
	}
	*l = items
	return nil
}

type SessionSummaryDocument struct {
	SchemaVersion      int                        `json:"schema_version,omitempty"`
	CurrentGoal        string                     `json:"current_goal"`
	ConfirmedDecisions SessionSummaryEvidenceList `json:"confirmed_decisions"`
	CompletedItems     []string                   `json:"completed_items"`
	OpenLoops          SessionSummaryEvidenceList `json:"open_loops"`
	UserCorrections    SessionSummaryEvidenceList `json:"user_corrections"`
	KeyErrors          []string                   `json:"key_errors"`
	References         []string                   `json:"references"`
}

func DecodeSessionSummaryDocument(content string) (SessionSummaryDocument, error) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "{") {
		return SessionSummaryDocument{}, fmt.Errorf("session summary must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var document SessionSummaryDocument
	if err := decoder.Decode(&document); err != nil {
		return SessionSummaryDocument{}, fmt.Errorf("decode session summary: %w", err)
	}
	if err := ensureSummaryJSONEOF(decoder); err != nil {
		return SessionSummaryDocument{}, err
	}
	if document.SchemaVersion != 0 && document.SchemaVersion != SessionSummarySchemaV2 {
		return SessionSummaryDocument{}, fmt.Errorf("unsupported session summary schema version %d", document.SchemaVersion)
	}
	if err := document.Validate(); err != nil {
		return SessionSummaryDocument{}, err
	}
	return document, nil
}

func ensureSummaryJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing session summary: %w", err)
	}
	return fmt.Errorf("session summary contains trailing JSON")
}

func (d *SessionSummaryDocument) Validate() error {
	if d == nil || len([]rune(strings.TrimSpace(d.CurrentGoal))) > SessionSummaryItemMaxRunes {
		return fmt.Errorf("invalid session summary goal")
	}
	for _, group := range []SessionSummaryEvidenceList{d.ConfirmedDecisions, d.OpenLoops, d.UserCorrections} {
		if len(group) > SessionSummaryItemLimit {
			return fmt.Errorf("session summary has too many evidence items")
		}
		for _, item := range group {
			if err := item.Validate(); err != nil {
				return err
			}
		}
	}
	for _, group := range [][]string{d.CompletedItems, d.KeyErrors, d.References} {
		if len(group) > SessionSummaryItemLimit {
			return fmt.Errorf("session summary has too many items")
		}
		for _, item := range group {
			if len([]rune(strings.TrimSpace(item))) > SessionSummaryItemMaxRunes {
				return fmt.Errorf("session summary item is too long")
			}
		}
	}
	if len(d.EvidenceMessageIDs()) > SessionSummarySourceLimit {
		return fmt.Errorf("session summary has too many evidence messages")
	}
	return nil
}

func (e SessionSummaryEvidence) Validate() error {
	if strings.TrimSpace(e.Text) == "" || len([]rune(strings.TrimSpace(e.Text))) > SessionSummaryItemMaxRunes || len(e.EvidenceMessageIDs) > SessionSummaryEvidenceLimit {
		return fmt.Errorf("invalid session summary evidence")
	}
	seen := make(map[uint]struct{}, len(e.EvidenceMessageIDs))
	for _, id := range e.EvidenceMessageIDs {
		if id == 0 {
			return fmt.Errorf("session summary evidence id must be positive")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate session summary evidence id %d", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (d SessionSummaryDocument) EvidenceMessageIDs() []uint {
	seen := make(map[uint]struct{})
	for _, group := range []SessionSummaryEvidenceList{d.ConfirmedDecisions, d.OpenLoops, d.UserCorrections} {
		for _, item := range group {
			for _, id := range item.EvidenceMessageIDs {
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

// SessionSummary is a lossy, session-scoped compression of older messages.
// CoveredMessageID is the high-water mark: messages at or below it must not be
// summarized again or injected as recent history.
type SessionSummary struct {
	ID               uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID           int64     `gorm:"not null;index:idx_session_summaries_user_session,unique,priority:1" json:"user_id"`
	SessionID        string    `gorm:"type:varchar(36);not null;index:idx_session_summaries_user_session,unique,priority:2;index" json:"session_id"`
	Content          string    `gorm:"type:longtext;not null" json:"content"`
	CoveredMessageID uint      `gorm:"not null;default:0;index" json:"covered_message_id"`
	TokenEstimate    int       `gorm:"not null;default:0" json:"token_estimate"`
	Version          uint64    `gorm:"not null;default:1" json:"version"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	Session          Session   `gorm:"foreignKey:SessionID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"-"`
}

func (s *SessionSummary) Validate() error {
	if s == nil {
		return fmt.Errorf("session summary is nil")
	}
	if s.UserID <= 0 || strings.TrimSpace(s.SessionID) == "" {
		return fmt.Errorf("session summary identity is required")
	}
	if strings.TrimSpace(s.Content) == "" || s.CoveredMessageID == 0 {
		return fmt.Errorf("session summary content and coverage are required")
	}
	if s.TokenEstimate < 0 {
		return fmt.Errorf("session summary token estimate cannot be negative")
	}
	return nil
}
