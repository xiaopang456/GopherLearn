package model

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	EpisodeTitleMaxRunes        = 160
	EpisodeSynopsisMaxRunes     = 1200
	EpisodeEntryTextMaxRunes    = 400
	EpisodeSearchTextMaxRunes   = 12000
	EpisodeTopicLimit           = 12
	EpisodeEntryLimit           = 12
	EpisodeEvidenceLimit        = 32
	EpisodeSourceHandleLimit    = 64
	EpisodeSourceHandleMaxRunes = 200
)

type EpisodeStatus string

const (
	EpisodeStatusActive   EpisodeStatus = "active"
	EpisodeStatusArchived EpisodeStatus = "archived"
	EpisodeStatusDeleted  EpisodeStatus = "deleted"
	// EpisodeStatusAbstained is a session the projector read and decided held
	// nothing worth remembering.
	//
	// It is a row rather than an absence on purpose. Writing nothing cannot be
	// told apart from three other things - the projector never ran, it ran and
	// failed, or the job is still queued - and "we looked and there was nothing"
	// is a different fact from all three. Every memory read filters on
	// EpisodeStatusActive, so an abstained row is invisible to recall while
	// still answering "why does this session have no memory".
	EpisodeStatusAbstained EpisodeStatus = "abstained"
)

func (s EpisodeStatus) Valid() bool {
	switch s {
	case EpisodeStatusActive, EpisodeStatusArchived, EpisodeStatusDeleted,
		EpisodeStatusAbstained:
		return true
	default:
		return false
	}
}

// AbstainReason is the closed vocabulary for why a projection produced no
// memory. It is never free text from a model: this string is persisted and
// read back by operators, and a model sentence here would carry the learner's
// own words into an operational field.
type AbstainReason string

const (
	// AbstainNoDurableAssertion is the ordinary case: the session was read and
	// held no decision, open loop, correction, preference or durable
	// experience.
	AbstainNoDurableAssertion AbstainReason = "no_durable_assertion"
	// AbstainAllAssertionsTrimmed is the case where the model proposed
	// assertions and the server removed every one of them. It is separate
	// because the two say different things about where to look: the first is
	// about the session, the second is about the generation.
	AbstainAllAssertionsTrimmed AbstainReason = "all_assertions_trimmed"
)

func (r AbstainReason) Valid() bool {
	switch r {
	case AbstainNoDurableAssertion, AbstainAllAssertionsTrimmed:
		return true
	default:
		return false
	}
}

type EpisodeIndexStatus string

const (
	EpisodeIndexPending EpisodeIndexStatus = "pending"
	EpisodeIndexIndexed EpisodeIndexStatus = "indexed"
	EpisodeIndexFailed  EpisodeIndexStatus = "failed"
)

func (s EpisodeIndexStatus) Valid() bool {
	switch s {
	case EpisodeIndexPending, EpisodeIndexIndexed, EpisodeIndexFailed:
		return true
	default:
		return false
	}
}

type EpisodeProjectionStatus string

const (
	EpisodeProjectionPending    EpisodeProjectionStatus = "pending"
	EpisodeProjectionProcessing EpisodeProjectionStatus = "processing"
	EpisodeProjectionCompleted  EpisodeProjectionStatus = "completed"
	EpisodeProjectionFailed     EpisodeProjectionStatus = "failed"
)

func (s EpisodeProjectionStatus) Valid() bool {
	switch s {
	case EpisodeProjectionPending, EpisodeProjectionProcessing, EpisodeProjectionCompleted, EpisodeProjectionFailed:
		return true
	default:
		return false
	}
}

// EpisodeTopic is a stable topic handle plus its user-facing label. Topic keys
// are retrieval metadata, not a model-owned namespace; the projector added in
// AGENT-024 must validate them against its own deterministic rules.
type EpisodeTopic struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// EpisodeEvidence records one session fact and the complete messages that
// support it. A projector may lose detail, but it must never claim a decision
// after seeing only part of a message.
type EpisodeEvidence struct {
	// ID is the server-issued stable identity of this assertion.
	//
	// It is persisted in the document because a governance relation points at
	// it and must survive re-projection. It is omitempty so that a legacy row
	// written before identities existed still decodes; such a row is given a
	// derived identity at read time and is never silently treated as new.
	ID                 string `json:"id,omitempty"`
	Text               string `json:"text"`
	EvidenceMessageIDs []uint `json:"evidence_message_ids"`
	// OccurredAt is when this assertion was made, derived from the messages it
	// cites rather than from the Episode's last activity. A card's timestamp
	// says when the session ended, which is not when a decision inside it was
	// taken, and the replacement ordering rule compares assertions.
	OccurredAt time.Time `json:"occurred_at,omitempty"`
}

// EpisodeDocument is the structured, cross-session projection of one chat
// session. It is deliberately narrower than SessionSummary: only durable
// history handles used by cross-session recall belong here.
type EpisodeDocument struct {
	Synopsis        string            `json:"synopsis"`
	Topics          []EpisodeTopic    `json:"topics"`
	Decisions       []EpisodeEvidence `json:"decisions"`
	OpenLoops       []EpisodeEvidence `json:"open_loops"`
	UserCorrections []EpisodeEvidence `json:"user_corrections"`
	// Preferences and DurableExperiences are the two slots added in
	// STABILIZE-065 Phase 2-C1.
	//
	// They exist because the learner's clearest durable statements had nowhere
	// correct to go. "From now on explain code in Go" is not a decision about
	// the project and "I understand that TCP has no message boundaries" is not
	// a correction; both were landing in Decisions, which made every count of
	// decisions wrong as well as losing the distinction.
	//
	// Both are omitempty so a document written before they existed decodes to
	// nil and behaves exactly as it did. There is no migration and no backfill.
	Preferences        []EpisodeEvidence `json:"preferences,omitempty"`
	DurableExperiences []EpisodeEvidence `json:"durable_experiences,omitempty"`
	SourceHandles      []string          `json:"source_handles"`
}

// AssertionGroups is every slot that carries a real assertion, in the order a
// reader sees them. Synopsis is not here: it asserts nothing.
//
// It is one function rather than five literal lists scattered across the
// codebase, because adding the two new slots meant finding all of them, and
// the next kind should not have to.
func (d *EpisodeDocument) AssertionGroups() []struct {
	Kind    AssertionKind
	Entries *[]EpisodeEvidence
} {
	return []struct {
		Kind    AssertionKind
		Entries *[]EpisodeEvidence
	}{
		{AssertionDecision, &d.Decisions},
		{AssertionOpenLoop, &d.OpenLoops},
		{AssertionCorrection, &d.UserCorrections},
		{AssertionPreference, &d.Preferences},
		{AssertionDurableExperience, &d.DurableExperiences},
	}
}

// AssertionCount is how many real assertions this document carries.
//
// It is the question "is there anything to remember here", and it is asked
// before an Episode is persisted at all.
func (d EpisodeDocument) AssertionCount() int {
	total := 0
	for _, group := range d.AssertionGroups() {
		for _, entry := range *group.Entries {
			if strings.TrimSpace(entry.Text) != "" {
				total++
			}
		}
	}
	return total
}

func (d EpisodeDocument) Value() (driver.Value, error) {
	return json.Marshal(d)
}

func (d *EpisodeDocument) Scan(value any) error {
	if value == nil {
		*d = EpisodeDocument{}
		return nil
	}
	var data []byte
	switch typed := value.(type) {
	case []byte:
		data = typed
	case string:
		data = []byte(typed)
	default:
		return fmt.Errorf("scan episode document: unsupported type %T", value)
	}
	decoded, err := DecodeEpisodeDocument(data)
	if err != nil {
		return fmt.Errorf("scan episode document: %w", err)
	}
	*d = decoded
	return nil
}

// DecodeEpisodeDocument is the single strict decoder shared by storage tests
// now and by the model projector in AGENT-024. Unknown fields and trailing JSON
// are rejected instead of being silently persisted as an expanded protocol.
func DecodeEpisodeDocument(data []byte) (EpisodeDocument, error) {
	var document EpisodeDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return EpisodeDocument{}, fmt.Errorf("decode episode document: %w", err)
	}
	if err := ensureEpisodeJSONEOF(decoder); err != nil {
		return EpisodeDocument{}, err
	}
	if err := document.Validate(); err != nil {
		return EpisodeDocument{}, err
	}
	return document, nil
}

func ensureEpisodeJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing episode document: %w", err)
	}
	return fmt.Errorf("episode document contains trailing JSON")
}

func (d *EpisodeDocument) Validate() error {
	if d == nil || strings.TrimSpace(d.Synopsis) == "" || utf8.RuneCountInString(d.Synopsis) > EpisodeSynopsisMaxRunes {
		return fmt.Errorf("invalid episode synopsis")
	}
	if len(d.Topics) > EpisodeTopicLimit {
		return fmt.Errorf("episode document exceeds entry limits")
	}
	for _, group := range d.AssertionGroups() {
		if len(*group.Entries) > EpisodeEntryLimit {
			return fmt.Errorf("episode document exceeds entry limits")
		}
	}
	seenTopics := make(map[string]struct{}, len(d.Topics))
	for _, topic := range d.Topics {
		key := strings.TrimSpace(topic.Key)
		label := strings.TrimSpace(topic.Label)
		if key == "" || label == "" || utf8.RuneCountInString(key) > EpisodeTitleMaxRunes || utf8.RuneCountInString(label) > EpisodeTitleMaxRunes {
			return fmt.Errorf("invalid episode topic")
		}
		if _, exists := seenTopics[key]; exists {
			return fmt.Errorf("duplicate episode topic %q", key)
		}
		seenTopics[key] = struct{}{}
	}
	for _, group := range d.AssertionGroups() {
		for _, entry := range *group.Entries {
			if err := validateEpisodeEvidence(entry); err != nil {
				return err
			}
		}
	}
	if len(d.SourceHandles) == 0 || len(d.SourceHandles) > EpisodeSourceHandleLimit {
		return fmt.Errorf("invalid episode source handle count")
	}
	seenHandles := make(map[string]struct{}, len(d.SourceHandles))
	for _, handle := range d.SourceHandles {
		handle = strings.TrimSpace(handle)
		if handle == "" || utf8.RuneCountInString(handle) > EpisodeSourceHandleMaxRunes {
			return fmt.Errorf("invalid episode source handle")
		}
		if _, exists := seenHandles[handle]; exists {
			return fmt.Errorf("duplicate episode source handle %q", handle)
		}
		seenHandles[handle] = struct{}{}
	}
	return nil
}

func validateEpisodeEvidence(entry EpisodeEvidence) error {
	if strings.TrimSpace(entry.Text) == "" || utf8.RuneCountInString(entry.Text) > EpisodeEntryTextMaxRunes || len(entry.EvidenceMessageIDs) == 0 || len(entry.EvidenceMessageIDs) > EpisodeEvidenceLimit {
		return fmt.Errorf("invalid episode evidence")
	}
	seen := make(map[uint]struct{}, len(entry.EvidenceMessageIDs))
	for _, id := range entry.EvidenceMessageIDs {
		if id == 0 {
			return fmt.Errorf("episode evidence message id must be positive")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate episode evidence message id %d", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// EpisodeMemory is one updateable, cross-session projection per Session. The
// raw Message rows remain authoritative; source watermarks make every derived
// version auditable and prevent stale projectors from moving coverage back.
type EpisodeMemory struct {
	ID                     string            `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID                 int64             `gorm:"not null;index:idx_episode_owner_session,unique,priority:1;index:idx_episode_owner_time,priority:1" json:"user_id"`
	SessionID              string            `gorm:"type:varchar(36);not null;index:idx_episode_owner_session,unique,priority:2;index" json:"session_id"`
	Title                  string            `gorm:"type:varchar(160);not null" json:"title"`
	Document               EpisodeDocument   `gorm:"column:content_json;type:json;not null" json:"content"`
	SearchText             string            `gorm:"type:text;not null" json:"-"`
	TopicKeys              StringList        `gorm:"type:json;not null" json:"topic_keys"`
	SourceEpisodes         EpisodeSourceRefs `gorm:"type:json" json:"-"`
	SourceSummaryVersion   uint64            `gorm:"not null;default:0" json:"source_summary_version"`
	SourceSummaryWatermark uint              `gorm:"not null;default:0" json:"source_summary_watermark"`
	SourceLastMessageID    uint              `gorm:"not null;index" json:"source_last_message_id"`
	LastMessageAt          time.Time         `gorm:"not null;index:idx_episode_owner_time,priority:2" json:"last_message_at"`
	Version                uint64            `gorm:"not null;default:1" json:"version"`
	Status                 EpisodeStatus     `gorm:"type:varchar(16);not null;index" json:"status"`
	// AbstainReason is set only on an abstained row and is empty otherwise, so
	// a row cannot claim both to be memory and to be a reason there is none.
	AbstainReason AbstainReason      `gorm:"type:varchar(32);not null;default:''" json:"abstain_reason,omitempty"`
	IndexStatus   EpisodeIndexStatus `gorm:"type:varchar(16);not null;index" json:"index_status"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
	Session       Session            `gorm:"foreignKey:SessionID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"-"`
}

func (m *EpisodeMemory) Validate() error {
	if m == nil || strings.TrimSpace(m.ID) == "" || m.UserID <= 0 || strings.TrimSpace(m.SessionID) == "" {
		return fmt.Errorf("invalid episode memory identity")
	}
	if strings.TrimSpace(m.Title) == "" || utf8.RuneCountInString(m.Title) > EpisodeTitleMaxRunes || strings.TrimSpace(m.SearchText) == "" || utf8.RuneCountInString(m.SearchText) > EpisodeSearchTextMaxRunes {
		return fmt.Errorf("invalid episode memory content")
	}
	if err := m.Document.Validate(); err != nil {
		return err
	}
	if len(m.TopicKeys) > EpisodeTopicLimit {
		return fmt.Errorf("episode memory exceeds topic key limit")
	}
	if err := m.SourceEpisodes.Validate(); err != nil {
		return err
	}
	for _, source := range m.SourceEpisodes {
		if source.EpisodeID == m.ID {
			return fmt.Errorf("episode memory cannot depend on itself")
		}
	}
	seenTopics := make(map[string]struct{}, len(m.TopicKeys))
	for _, topic := range m.TopicKeys {
		topic = strings.TrimSpace(topic)
		if topic == "" || utf8.RuneCountInString(topic) > EpisodeTitleMaxRunes {
			return fmt.Errorf("invalid episode memory topic key")
		}
		if _, exists := seenTopics[topic]; exists {
			return fmt.Errorf("duplicate episode memory topic key %q", topic)
		}
		seenTopics[topic] = struct{}{}
	}
	if len(seenTopics) != len(m.Document.Topics) {
		return fmt.Errorf("episode topic keys do not match document topics")
	}
	for _, topic := range m.Document.Topics {
		if _, exists := seenTopics[strings.TrimSpace(topic.Key)]; !exists {
			return fmt.Errorf("episode topic keys do not match document topics")
		}
	}
	if m.SourceLastMessageID == 0 || m.LastMessageAt.IsZero() || m.Version == 0 || !m.Status.Valid() || !m.IndexStatus.Valid() {
		return fmt.Errorf("invalid episode memory state")
	}
	if m.SourceSummaryWatermark > m.SourceLastMessageID || (m.SourceSummaryVersion == 0) != (m.SourceSummaryWatermark == 0) {
		return fmt.Errorf("invalid episode summary source watermark")
	}
	for _, group := range m.Document.AssertionGroups() {
		for _, entry := range *group.Entries {
			for _, messageID := range entry.EvidenceMessageIDs {
				if messageID > m.SourceLastMessageID {
					return fmt.Errorf("episode evidence exceeds source message watermark")
				}
			}
		}
	}
	return nil
}

// EpisodeProjectionJob is a durable debounce and lease record. DesiredMessageID
// may advance while a worker owns ClaimedMessageID; completion then requeues the
// same row instead of losing the newly arrived tail.
type EpisodeProjectionJob struct {
	ID               string                  `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID           int64                   `gorm:"not null;index:idx_episode_jobs_owner_session,unique,priority:1;index:idx_episode_jobs_status,priority:2" json:"user_id"`
	SessionID        string                  `gorm:"type:varchar(36);not null;index:idx_episode_jobs_owner_session,unique,priority:2;index" json:"session_id"`
	DesiredMessageID uint                    `gorm:"not null" json:"desired_message_id"`
	ClaimedMessageID uint                    `gorm:"not null;default:0" json:"claimed_message_id"`
	Status           EpisodeProjectionStatus `gorm:"type:varchar(16);not null;index:idx_episode_jobs_status,priority:1" json:"status"`
	Attempts         int                     `gorm:"not null;default:0" json:"attempts"`
	AvailableAt      time.Time               `gorm:"not null;index" json:"available_at"`
	LeaseToken       string                  `gorm:"type:varchar(36)" json:"-"`
	LeaseEndsAt      *time.Time              `gorm:"index" json:"-"`
	ErrorCode        string                  `gorm:"type:varchar(64)" json:"error_code"`
	CreatedAt        time.Time               `json:"created_at"`
	UpdatedAt        time.Time               `json:"updated_at"`
	CompletedAt      *time.Time              `json:"completed_at,omitempty"`
	Session          Session                 `gorm:"foreignKey:SessionID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"-"`
}

func (j *EpisodeProjectionJob) ValidateForCreate() error {
	if j == nil || strings.TrimSpace(j.ID) == "" || j.UserID <= 0 || strings.TrimSpace(j.SessionID) == "" || j.DesiredMessageID == 0 || j.AvailableAt.IsZero() {
		return fmt.Errorf("invalid episode projection job identity")
	}
	if j.Status != EpisodeProjectionPending || j.Attempts != 0 || j.ClaimedMessageID != 0 || j.LeaseToken != "" || j.LeaseEndsAt != nil || j.CompletedAt != nil || j.ErrorCode != "" {
		return fmt.Errorf("invalid initial episode projection job state")
	}
	return nil
}
