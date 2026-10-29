package model

import (
	"crypto/sha256"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// AGENT-018 replaced the original four memory types with two.
//
// The old taxonomy classified on "what is this about", a semantic axis where a
// single sentence can belong to several types at once. EXP-002B Pipeline
// Scale-1 measured the cost: 25 of 80 expected `experience` events were
// classified as a perfectly legal `project_fact`, and `project_fact` had the
// worst tag overlap of the four at 16/80.
//
// The two remaining types split on a question the server can answer without a
// model: does this memory belong to a technical topic, or to the user?
//
// The other two types left because their content already had a structured
// source. Mastery lives in TopicProgress, and project facts live in the user's
// own notes, which the knowledge base already retrieves.
// MemoryScope is the consumption scope of one UserMemory. It is deliberately
// not called a type: global and topic rows share one lifecycle and governance
// model; the scope only decides whether the row applies to every answer or to
// one strongly identified technical topic.
type MemoryScope string

const (
	// MemoryTypeProfile holds facts about the user that are not bound to any
	// technical topic: background, teaching preferences, goals, prohibitions.
	// Injected in full rather than retrieved, because its relevance does not
	// correlate with the current question.
	MemoryScopeGlobal MemoryScope = "global"
	// MemoryTypeTopicNote holds the user's own understanding of one topic:
	// analogies, confusions, pitfalls, personal terminology. This is the only
	// type that is actually retrieved.
	MemoryScopeTopic MemoryScope = "topic"
)

// The pre-AGENT-018 types. Existing rows keep them so that no user content is
// destroyed by the migration, but they are not valid for new writes and are
// not part of any recall type set, so they can never re-enter an answer.
const (
	MemoryScopeLegacyUserProfile   MemoryScope = "user_profile"
	MemoryScopeLegacyLearningState MemoryScope = "learning_state"
	MemoryScopeLegacyProjectFact   MemoryScope = "project_fact"
	MemoryScopeLegacyExperience    MemoryScope = "experience"
)

func (s MemoryScope) Valid() bool {
	switch s {
	case MemoryScopeGlobal, MemoryScopeTopic:
		return true
	default:
		return false
	}
}

// Legacy reports a retired type that may still exist in storage. It is kept
// separate from Valid so that a read path can recognise an old row without
// accidentally making it writable or recallable again.
func (s MemoryScope) Legacy() bool {
	switch s {
	case MemoryScopeLegacyUserProfile, MemoryScopeLegacyLearningState, MemoryScopeLegacyProjectFact, MemoryScopeLegacyExperience:
		return true
	default:
		return false
	}
}

// Deprecated source aliases keep old experiment harnesses buildable while all
// production entities and APIs migrate to Scope. They resolve to the new
// persisted values and therefore cannot create a fresh profile/topic_note row.
type MemoryType = MemoryScope

const (
	MemoryTypeProfile             = MemoryScopeGlobal
	MemoryTypeTopicNote           = MemoryScopeTopic
	MemoryTypeLegacyUserProfile   = MemoryScopeLegacyUserProfile
	MemoryTypeLegacyLearningState = MemoryScopeLegacyLearningState
	MemoryTypeLegacyProjectFact   = MemoryScopeLegacyProjectFact
	MemoryTypeLegacyExperience    = MemoryScopeLegacyExperience
)

// MemoryDurability separates preferences that hold indefinitely from goals and
// plans that expire. Both are `profile`: they are consumed the same way, so the
// difference is a field rather than a type. Making it a type would have
// recreated the ambiguity AGENT-018 removed, since "I want to interview in
// August" is equally readable as a preference and as a goal.
type MemoryDurability string

const (
	MemoryDurabilityStable    MemoryDurability = "stable"
	MemoryDurabilityTimeBound MemoryDurability = "time_bound"
)

func (d MemoryDurability) Valid() bool {
	return d == MemoryDurabilityStable || d == MemoryDurabilityTimeBound
}

const (
	// MemorySubjectMaxRunes bounds the one-line summary that doubles as the
	// retrieval unit, which is why no separate description field exists.
	MemorySubjectMaxRunes = 200
	// MemoryContentMaxRunes was 2000 before AGENT-018. Five memories at the old
	// limit could not fit the 2000-token memory budget and were always going to
	// be truncated; anything longer than this belongs in several separate
	// conclusions rather than one memory.
	MemoryContentMaxRunes = 400
)

type MemoryJobStatus string

const (
	MemoryJobPending    MemoryJobStatus = "pending"
	MemoryJobProcessing MemoryJobStatus = "processing"
	MemoryJobCompleted  MemoryJobStatus = "completed"
	MemoryJobRejected   MemoryJobStatus = "rejected"
	MemoryJobFailed     MemoryJobStatus = "failed"
)

func (s MemoryJobStatus) Valid() bool {
	switch s {
	case MemoryJobPending, MemoryJobProcessing, MemoryJobCompleted, MemoryJobRejected, MemoryJobFailed:
		return true
	default:
		return false
	}
}

type MemoryEventType string

const (
	MemoryEventAgentRun         MemoryEventType = "agent_run"
	MemoryEventExplicitRemember MemoryEventType = "explicit_remember"
	MemoryEventLearningComplete MemoryEventType = "learning_completed"
	MemoryEventSummaryCompacted MemoryEventType = "summary_compacted"
)

func (t MemoryEventType) Valid() bool {
	switch t {
	case MemoryEventAgentRun, MemoryEventExplicitRemember, MemoryEventLearningComplete, MemoryEventSummaryCompacted:
		return true
	default:
		return false
	}
}

type MemoryCandidateStatus string

const (
	MemoryCandidatePending    MemoryCandidateStatus = "pending"
	MemoryCandidateProcessing MemoryCandidateStatus = "processing"
	MemoryCandidateApplied    MemoryCandidateStatus = "applied"
	MemoryCandidateRejected   MemoryCandidateStatus = "rejected"
)

func (s MemoryCandidateStatus) Valid() bool {
	switch s {
	case MemoryCandidatePending, MemoryCandidateProcessing, MemoryCandidateApplied, MemoryCandidateRejected:
		return true
	default:
		return false
	}
}

type MemorySensitivity string

const MemorySensitivitySafe MemorySensitivity = "safe"

var allowedMemoryTagList = []string{
	"go", "gin", "gorm", "mysql", "redis", "rabbitmq", "etcd", "watch",
	"rpc", "protocol", "docker", "gopherai", "rag", "embedding", "agent", "memory",
	"context", "learning", "testing", "deployment", "concurrency", "network", "security",
	"debugging", "performance", "api", "database", "backend", "preference", "project",
}

var allowedMemoryTags = func() map[string]struct{} {
	tags := make(map[string]struct{}, len(allowedMemoryTagList))
	for _, tag := range allowedMemoryTagList {
		tags[tag] = struct{}{}
	}
	return tags
}()

// AllowedMemoryTags returns a copy of the canonical vocabulary. Keeping the
// extractor prompt and the write/read validation boundaries on this one list
// prevents a prompt-only tag from becoming an invalid persisted candidate.
func AllowedMemoryTags() []string {
	return append([]string(nil), allowedMemoryTagList...)
}

func ValidMemoryTag(tag string) bool {
	_, ok := allowedMemoryTags[strings.ToLower(strings.TrimSpace(tag))]
	return ok
}

type LongTermMemoryStatus string

const (
	LongTermMemoryActive     LongTermMemoryStatus = "active"
	LongTermMemoryArchived   LongTermMemoryStatus = "archived"
	LongTermMemorySuperseded LongTermMemoryStatus = "superseded"
	LongTermMemoryDeleted    LongTermMemoryStatus = "deleted"
)

func (s LongTermMemoryStatus) Valid() bool {
	switch s {
	case LongTermMemoryActive, LongTermMemoryArchived, LongTermMemorySuperseded, LongTermMemoryDeleted:
		return true
	default:
		return false
	}
}

type MemoryDreamAction string

const (
	MemoryDreamAdd       MemoryDreamAction = "add"
	MemoryDreamMerge     MemoryDreamAction = "merge"
	MemoryDreamSupersede MemoryDreamAction = "supersede"
	MemoryDreamArchive   MemoryDreamAction = "archive"
	MemoryDreamReject    MemoryDreamAction = "reject"
	MemoryUserCorrect    MemoryDreamAction = "correct"
	MemoryUserDelete     MemoryDreamAction = "delete"
)

func (a MemoryDreamAction) Valid() bool {
	switch a {
	case MemoryDreamAdd, MemoryDreamMerge, MemoryDreamSupersede, MemoryDreamArchive, MemoryDreamReject, MemoryUserCorrect, MemoryUserDelete:
		return true
	default:
		return false
	}
}

type MemoryDreamRunStatus string

const (
	MemoryDreamRunProcessing MemoryDreamRunStatus = "processing"
	MemoryDreamRunCompleted  MemoryDreamRunStatus = "completed"
	MemoryDreamRunFailed     MemoryDreamRunStatus = "failed"
)

func (s MemoryDreamRunStatus) Valid() bool {
	return s == MemoryDreamRunProcessing || s == MemoryDreamRunCompleted || s == MemoryDreamRunFailed
}

type MemoryRevisionActor string

const (
	MemoryRevisionDream  MemoryRevisionActor = "dream"
	MemoryRevisionUser   MemoryRevisionActor = "user"
	MemoryRevisionSystem MemoryRevisionActor = "system"
)

func (a MemoryRevisionActor) Valid() bool {
	return a == MemoryRevisionDream || a == MemoryRevisionUser || a == MemoryRevisionSystem
}

type MemoryDraftStatus string

const (
	MemoryDraftPending   MemoryDraftStatus = "pending"
	MemoryDraftConfirmed MemoryDraftStatus = "confirmed"
	MemoryDraftCancelled MemoryDraftStatus = "cancelled"
)

func (s MemoryDraftStatus) Valid() bool {
	return s == MemoryDraftPending || s == MemoryDraftConfirmed || s == MemoryDraftCancelled
}

type StringList []string

func (s StringList) Value() (driver.Value, error) {
	if s == nil {
		s = StringList{}
	}
	return json.Marshal(s)
}

func (s *StringList) Scan(value any) error {
	if value == nil {
		*s = StringList{}
		return nil
	}
	var data []byte
	switch v := value.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return fmt.Errorf("scan string list: unsupported type %T", value)
	}
	if err := json.Unmarshal(data, s); err != nil {
		return err
	}
	return nil
}

type MemoryExtractionJob struct {
	ID              string          `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID          int64           `gorm:"not null;index:idx_memory_jobs_user_status,priority:1;uniqueIndex:idx_memory_jobs_event,priority:1" json:"user_id"`
	EventType       MemoryEventType `gorm:"type:varchar(32);not null;uniqueIndex:idx_memory_jobs_event,priority:2" json:"event_type"`
	SourceID        string          `gorm:"type:varchar(128);not null;uniqueIndex:idx_memory_jobs_event,priority:3" json:"source_id"`
	SourceSessionID string          `gorm:"type:varchar(36);index" json:"source_session_id"`
	SourceRunID     string          `gorm:"type:varchar(36);index" json:"source_run_id"`
	EventPayload    string          `gorm:"type:text" json:"-"`
	Status          MemoryJobStatus `gorm:"type:varchar(24);not null;index:idx_memory_jobs_user_status,priority:2" json:"status"`
	Attempts        int             `gorm:"not null;default:0" json:"attempts"`
	LeaseToken      string          `gorm:"type:varchar(36)" json:"-"`
	LeaseEndsAt     *time.Time      `gorm:"index" json:"-"`
	AvailableAt     time.Time       `gorm:"not null;index" json:"available_at"`
	ErrorCode       string          `gorm:"type:varchar(64)" json:"error_code"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	CompletedAt     *time.Time      `json:"completed_at,omitempty"`
}

func (j *MemoryExtractionJob) ValidateForCreate() error {
	if j == nil || strings.TrimSpace(j.ID) == "" || j.UserID <= 0 || !j.EventType.Valid() || strings.TrimSpace(j.SourceID) == "" || !j.Status.Valid() {
		return fmt.Errorf("invalid memory extraction job")
	}
	if j.Status != MemoryJobPending || j.AvailableAt.IsZero() || utf8.RuneCountInString(j.SourceID) > 128 {
		return fmt.Errorf("invalid initial memory extraction job state")
	}
	return nil
}

type MemoryCandidate struct {
	ID              string                `gorm:"primaryKey;type:varchar(36)" json:"id"`
	JobID           string                `gorm:"type:varchar(36);not null;uniqueIndex" json:"job_id"`
	UserID          int64                 `gorm:"not null;index" json:"user_id"`
	Scope           MemoryScope           `gorm:"column:memory_scope;type:varchar(32);not null;index" json:"scope"`
	Subject         string                `gorm:"type:varchar(200);not null" json:"subject"`
	ProposedContent string                `gorm:"type:text;not null" json:"proposed_content"`
	ContentHash     string                `gorm:"type:char(64);not null;index" json:"content_hash"`
	StructuredJSON  string                `gorm:"type:json;not null" json:"structured_json"`
	Tags            StringList            `gorm:"type:json;not null" json:"tags"`
	TopicKey        string                `gorm:"type:varchar(160);index" json:"topic_key,omitempty"`
	Durability      MemoryDurability      `gorm:"type:varchar(16)" json:"durability,omitempty"`
	ValidUntil      *time.Time            `gorm:"index" json:"valid_until,omitempty"`
	SourceRefs      StringList            `gorm:"type:json;not null" json:"source_refs"`
	Confidence      float64               `gorm:"not null" json:"confidence"`
	Importance      int                   `gorm:"not null" json:"importance"`
	Sensitivity     MemorySensitivity     `gorm:"type:varchar(16);not null" json:"sensitivity"`
	Status          MemoryCandidateStatus `gorm:"type:varchar(24);not null;index" json:"status"`
	DreamRunID      string                `gorm:"type:varchar(36);index" json:"dream_run_id,omitempty"`
	RejectionReason string                `gorm:"type:varchar(128)" json:"rejection_reason"`
	CreatedAt       time.Time             `json:"created_at"`
	UpdatedAt       time.Time             `json:"updated_at"`
	ProcessedAt     *time.Time            `json:"processed_at,omitempty"`
}

func (c *MemoryCandidate) Validate() error {
	if c == nil || strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.JobID) == "" || c.UserID <= 0 || !c.Scope.Valid() {
		return fmt.Errorf("invalid memory candidate identity")
	}
	return c.ValidatePayload()
}

// ValidatePayload validates model-produced fields before a worker binds the
// candidate to a persistent extraction job. Validate additionally checks the
// database identity fields required at commit time.
func (c *MemoryCandidate) ValidatePayload() error {
	if c == nil || c.UserID <= 0 || !c.Scope.Valid() {
		return fmt.Errorf("invalid memory candidate payload identity")
	}
	if strings.TrimSpace(c.Subject) == "" || strings.TrimSpace(c.ProposedContent) == "" ||
		utf8.RuneCountInString(c.Subject) > MemorySubjectMaxRunes || utf8.RuneCountInString(c.ProposedContent) > MemoryContentMaxRunes {
		return fmt.Errorf("invalid memory candidate content")
	}
	if c.ContentHash != MemoryContentHash(c.ProposedContent) {
		return fmt.Errorf("invalid memory candidate content hash")
	}
	if !validMemoryTags(c.Tags, 0, 5) || len(c.SourceRefs) == 0 || len(c.SourceRefs) > 12 || c.Confidence < 0 || c.Confidence > 1 || c.Importance < 1 || c.Importance > 10 {
		return fmt.Errorf("invalid memory candidate metadata")
	}
	if !json.Valid([]byte(c.StructuredJSON)) || c.Status != MemoryCandidatePending || c.Sensitivity != MemorySensitivitySafe {
		return fmt.Errorf("invalid memory candidate state")
	}
	return validateMemoryShape(c.Scope, c.TopicKey, c.Durability, c.ValidUntil, c.CreatedAt)
}

// validateMemoryShape enforces the per-type fields that make the two-type
// taxonomy decidable. A profile that carries a topic, or a topic note without
// one, would blur exactly the boundary AGENT-018 exists to keep sharp.
func validateMemoryShape(scope MemoryScope, topicKey string, durability MemoryDurability, validUntil *time.Time, createdAt time.Time) error {
	topicKey = strings.TrimSpace(topicKey)
	if utf8.RuneCountInString(topicKey) > 160 {
		return fmt.Errorf("memory topic key exceeds the column limit")
	}
	switch scope {
	case MemoryScopeGlobal:
		if topicKey != "" {
			return fmt.Errorf("global memory must not be bound to a technical topic")
		}
	case MemoryScopeTopic:
		if topicKey == "" {
			return fmt.Errorf("topic memory requires a topic key")
		}
	default:
		return fmt.Errorf("unsupported memory scope %q", scope)
	}
	if !durability.Valid() {
		return fmt.Errorf("memory requires stable or time_bound durability")
	}
	switch durability {
	case MemoryDurabilityStable:
		if validUntil != nil {
			return fmt.Errorf("stable memory must not carry valid_until")
		}
	case MemoryDurabilityTimeBound:
		if validUntil == nil || validUntil.IsZero() {
			return fmt.Errorf("time_bound memory requires valid_until")
		}
		if !createdAt.IsZero() && !validUntil.After(createdAt) {
			return fmt.Errorf("memory valid_until must be later than created_at")
		}
	}
	return nil
}

// LongTermMemory is the recallable representation. AGENT-011 defines and
// queries it; AGENT-012 owns promotion, merge, supersede and user governance.
type LongTermMemory struct {
	ID                string               `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID            int64                `gorm:"not null;index:idx_long_memories_scope_recall,priority:1" json:"user_id"`
	OriginCandidateID *string              `gorm:"type:varchar(36);uniqueIndex" json:"origin_candidate_id,omitempty"`
	Scope             MemoryScope          `gorm:"column:memory_scope;type:varchar(32);not null;index:idx_long_memories_scope_recall,priority:3" json:"scope"`
	Subject           string               `gorm:"type:varchar(200);not null" json:"subject"`
	Content           string               `gorm:"type:text;not null" json:"content"`
	ContentHash       string               `gorm:"type:char(64);not null;index:idx_long_memories_hash,priority:3" json:"content_hash"`
	OriginContentHash string               `gorm:"type:char(64);index:idx_long_memories_origin_hash" json:"origin_content_hash,omitempty"`
	StructuredJSON    string               `gorm:"type:json;not null" json:"structured_json"`
	Tags              StringList           `gorm:"type:json;not null" json:"tags"`
	TopicKey          string               `gorm:"type:varchar(160);index" json:"topic_key"`
	Durability        MemoryDurability     `gorm:"type:varchar(16)" json:"durability,omitempty"`
	ValidUntil        *time.Time           `gorm:"index" json:"valid_until,omitempty"`
	Verified          bool                 `gorm:"not null;default:false" json:"verified"`
	SourceRefs        StringList           `gorm:"type:json;not null" json:"source_refs"`
	Confidence        float64              `gorm:"not null;index" json:"confidence"`
	Importance        int                  `gorm:"not null" json:"importance"`
	Status            LongTermMemoryStatus `gorm:"type:varchar(24);not null;index:idx_long_memories_scope_recall,priority:2" json:"status"`
	UserConfirmed     bool                 `gorm:"not null;default:false" json:"user_confirmed"`
	TombstoneHash     string               `gorm:"type:char(64);index:idx_long_memories_hash,priority:2" json:"tombstone_hash,omitempty"`
	LastUsedAt        *time.Time           `gorm:"index" json:"last_used_at,omitempty"`
	UsageCount        uint64               `gorm:"not null;default:0" json:"usage_count"`
	CreatedAt         time.Time            `json:"created_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
	DeletedAt         *time.Time           `json:"deleted_at,omitempty"`
}

func (m *LongTermMemory) Validate() error {
	if m == nil || strings.TrimSpace(m.ID) == "" || m.UserID <= 0 || !m.Scope.Valid() || !m.Status.Valid() {
		return fmt.Errorf("invalid long-term memory identity")
	}
	if m.Status == LongTermMemoryDeleted {
		if m.Content != "" || m.Subject != "" || len(m.Tags) != 0 || len(m.SourceRefs) != 0 || m.DeletedAt == nil || strings.TrimSpace(m.TombstoneHash) == "" || m.TombstoneHash != m.ContentHash {
			return fmt.Errorf("invalid deleted memory tombstone")
		}
		return nil
	}
	if strings.TrimSpace(m.Subject) == "" || strings.TrimSpace(m.Content) == "" ||
		utf8.RuneCountInString(m.Subject) > MemorySubjectMaxRunes || utf8.RuneCountInString(m.Content) > MemoryContentMaxRunes {
		return fmt.Errorf("invalid long-term memory content")
	}
	if m.ContentHash != MemoryContentHash(m.Content) || strings.TrimSpace(m.TombstoneHash) != "" || m.DeletedAt != nil {
		return fmt.Errorf("invalid long-term memory content hash or tombstone")
	}
	if !validMemoryTags(m.Tags, 0, 5) || len(m.SourceRefs) == 0 || len(m.SourceRefs) > 32 || m.Confidence < 0 || m.Confidence > 1 || m.Importance < 1 || m.Importance > 10 {
		return fmt.Errorf("invalid long-term memory metadata")
	}
	if !json.Valid([]byte(m.StructuredJSON)) {
		return fmt.Errorf("invalid long-term memory state")
	}
	// Everything in this type is self-reported by the user in conversation.
	// A verified conclusion is a graded attempt, which lives in TopicProgress
	// and is read live rather than copied here where it would go stale.
	if m.Scope == MemoryScopeTopic && m.Verified {
		return fmt.Errorf("topic note memory cannot claim to be verified")
	}
	return validateMemoryShape(m.Scope, m.TopicKey, m.Durability, m.ValidUntil, m.CreatedAt)
}

// SemanticMemoryVersion is the version number a semantic memory carries once it
// leaves the database.
//
// LongTermMemory has no version column: an update rewrites the row in place, so
// "which revision did the model actually read" has to be derived. UpdatedAt in
// whole seconds is that derivation, and it is a single function rather than the
// same expression written twice because two readers now have to agree on it.
// The candidate layer stamps it on every semantic card, and the v2 topic
// bootstrap stamps it on every record it injects; the tool dedupes one against
// the other by kind, id and version, and two copies of this line that drifted
// would silently turn one record into two citable ones.
func SemanticMemoryVersion(memory LongTermMemory) uint64 {
	return uint64(memory.UpdatedAt.UTC().Unix())
}

func (m LongTermMemory) ExpiredAt(now time.Time) bool {
	return m.Durability == MemoryDurabilityTimeBound && m.ValidUntil != nil && !m.ValidUntil.After(now)
}

type MemoryRevision struct {
	ID             string              `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID         int64               `gorm:"not null;index:idx_memory_revisions_owner,priority:1;uniqueIndex:idx_memory_revision_request,priority:1" json:"user_id"`
	MemoryID       string              `gorm:"type:varchar(36);index:idx_memory_revisions_owner,priority:2" json:"memory_id,omitempty"`
	CandidateID    string              `gorm:"type:varchar(36);index" json:"candidate_id,omitempty"`
	DreamRunID     string              `gorm:"type:varchar(36);index" json:"dream_run_id,omitempty"`
	RequestKey     *string             `gorm:"type:varchar(128);uniqueIndex:idx_memory_revision_request,priority:2" json:"-"`
	Action         MemoryDreamAction   `gorm:"type:varchar(24);not null;index" json:"action"`
	Actor          MemoryRevisionActor `gorm:"type:varchar(16);not null" json:"actor"`
	Reason         string              `gorm:"type:varchar(500);not null" json:"reason"`
	BeforeSnapshot string              `gorm:"type:longtext" json:"before_snapshot,omitempty"`
	AfterSnapshot  string              `gorm:"type:longtext" json:"after_snapshot,omitempty"`
	BeforeHash     string              `gorm:"type:char(64)" json:"before_hash,omitempty"`
	AfterHash      string              `gorm:"type:char(64)" json:"after_hash,omitempty"`
	CreatedAt      time.Time           `json:"created_at"`
}

// MemoryDraft is a durable request for user authorization. A model may propose
// these fields, but it cannot choose owner, lifecycle status, revision actor or
// a database action. Only the explicit confirmation API can promote it.
type MemoryDraft struct {
	ID             string            `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID         int64             `gorm:"not null;uniqueIndex:idx_memory_drafts_idempotency,priority:1;index:idx_memory_drafts_owner_status,priority:1" json:"user_id"`
	SessionID      string            `gorm:"type:varchar(36);not null;index" json:"session_id"`
	RunID          string            `gorm:"type:varchar(36);not null;index" json:"run_id"`
	IdempotencyKey string            `gorm:"type:varchar(128);not null;uniqueIndex:idx_memory_drafts_idempotency,priority:2" json:"idempotency_key"`
	Scope          MemoryScope       `gorm:"column:memory_scope;type:varchar(32);not null" json:"scope"`
	Subject        string            `gorm:"type:varchar(200);not null" json:"subject"`
	Content        string            `gorm:"type:text;not null" json:"content"`
	ContentHash    string            `gorm:"type:char(64);not null;index" json:"-"`
	Tags           StringList        `gorm:"type:json;not null" json:"tags"`
	TopicKey       string            `gorm:"type:varchar(160);index" json:"topic_key,omitempty"`
	Durability     MemoryDurability  `gorm:"type:varchar(16);not null" json:"durability"`
	ValidUntil     *time.Time        `gorm:"index" json:"valid_until,omitempty"`
	ConflictIDs    StringList        `gorm:"type:json;not null" json:"conflict_ids"`
	Status         MemoryDraftStatus `gorm:"type:varchar(16);not null;index:idx_memory_drafts_owner_status,priority:2" json:"status"`
	MemoryID       string            `gorm:"type:varchar(36);index" json:"memory_id,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	ResolvedAt     *time.Time        `json:"resolved_at,omitempty"`
}

func (d *MemoryDraft) Validate() error {
	if d == nil || strings.TrimSpace(d.ID) == "" || d.UserID <= 0 || strings.TrimSpace(d.SessionID) == "" || strings.TrimSpace(d.RunID) == "" ||
		strings.TrimSpace(d.IdempotencyKey) == "" || utf8.RuneCountInString(d.IdempotencyKey) > 128 || !d.Scope.Valid() || !d.Status.Valid() {
		return fmt.Errorf("invalid memory draft identity")
	}
	if utf8.RuneCountInString(d.SessionID) > 36 || utf8.RuneCountInString(d.RunID) > 36 {
		return fmt.Errorf("memory draft source identity exceeds column limit")
	}
	if strings.TrimSpace(d.Subject) == "" || strings.TrimSpace(d.Content) == "" || utf8.RuneCountInString(d.Subject) > MemorySubjectMaxRunes || utf8.RuneCountInString(d.Content) > MemoryContentMaxRunes || d.ContentHash != MemoryContentHash(d.Content) {
		return fmt.Errorf("invalid memory draft content")
	}
	if !validMemoryTags(d.Tags, 0, 5) || len(d.ConflictIDs) > 20 {
		return fmt.Errorf("invalid memory draft metadata")
	}
	if err := validateMemoryShape(d.Scope, d.TopicKey, d.Durability, d.ValidUntil, d.CreatedAt); err != nil {
		return err
	}
	if d.Status == MemoryDraftPending && (d.MemoryID != "" || d.ResolvedAt != nil) {
		return fmt.Errorf("pending memory draft cannot be resolved")
	}
	if d.Status == MemoryDraftConfirmed && (d.MemoryID == "" || d.ResolvedAt == nil) {
		return fmt.Errorf("confirmed memory draft requires memory result")
	}
	if d.Status == MemoryDraftCancelled && (d.MemoryID != "" || d.ResolvedAt == nil) {
		return fmt.Errorf("cancelled memory draft has invalid result")
	}
	return nil
}

func (r *MemoryRevision) Validate() error {
	if r == nil || strings.TrimSpace(r.ID) == "" || r.UserID <= 0 || !r.Action.Valid() || !r.Actor.Valid() || strings.TrimSpace(r.Reason) == "" || utf8.RuneCountInString(r.Reason) > 500 {
		return fmt.Errorf("invalid memory revision")
	}
	if r.RequestKey != nil && (strings.TrimSpace(*r.RequestKey) == "" || utf8.RuneCountInString(*r.RequestKey) > 128) {
		return fmt.Errorf("invalid memory revision request key")
	}
	if r.Action != MemoryDreamReject && strings.TrimSpace(r.MemoryID) == "" {
		return fmt.Errorf("memory revision target is required")
	}
	return nil
}

type MemoryDreamRun struct {
	ID             string               `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID         int64                `gorm:"not null;index:idx_memory_dream_runs_owner,priority:1" json:"user_id"`
	CandidateID    string               `gorm:"type:varchar(36);not null;index:idx_memory_dream_runs_owner,priority:2" json:"candidate_id"`
	Status         MemoryDreamRunStatus `gorm:"type:varchar(24);not null;index" json:"status"`
	Action         MemoryDreamAction    `gorm:"type:varchar(24)" json:"action,omitempty"`
	TargetMemoryID string               `gorm:"type:varchar(36);index" json:"target_memory_id,omitempty"`
	ResultMemoryID string               `gorm:"type:varchar(36);index" json:"result_memory_id,omitempty"`
	ErrorCode      string               `gorm:"type:varchar(64)" json:"error_code,omitempty"`
	StartedAt      time.Time            `gorm:"not null" json:"started_at"`
	CompletedAt    *time.Time           `json:"completed_at,omitempty"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
}

func (r *MemoryDreamRun) ValidateForCreate() error {
	if r == nil || strings.TrimSpace(r.ID) == "" || r.UserID <= 0 || strings.TrimSpace(r.CandidateID) == "" || r.Status != MemoryDreamRunProcessing || r.StartedAt.IsZero() {
		return fmt.Errorf("invalid memory dream run")
	}
	return nil
}

func MemoryContentHash(content string) string {
	value := strings.TrimSpace(content)
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:])
}

// validMemoryTags allows an empty tag set since AGENT-018.
//
// Before it, a per-type fallback tag guaranteed at least one entry, so the
// floor of one was never reached. Removing that fallback (BUG-AGENT-024, where
// `project` was stamped on every project_fact and stopped discriminating) also
// removed the guarantee, which would have made any memory whose wording misses
// the fixed vocabulary impossible to store at all.
//
// Neither type depends on tags to be reachable: `profile` is injected wholesale
// and never retrieved by tag, and `topic_note` always carries a topic key.
// A memory with no tag is simply harder for tag matching to find, which is a
// true statement about the method rather than a reason to reject the memory.
func validMemoryTags(tags []string, minCount, maxCount int) bool {
	if len(tags) < minCount || len(tags) > maxCount {
		return false
	}
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		canonical := strings.ToLower(strings.TrimSpace(tag))
		if tag != canonical || !ValidMemoryTag(canonical) {
			return false
		}
		if _, duplicate := seen[canonical]; duplicate {
			return false
		}
		seen[canonical] = struct{}{}
	}
	return true
}
