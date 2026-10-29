// Package gormmysql implements repository.Repository on top of GORM. The
// production target is MySQL; the repository itself only uses portable GORM
// clauses so the same code path is exercised by the test suite without a live
// database.
package gormmysql

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// UserRecord is the users table. Email is nullable and unique: the in-memory
// repository allows several users without an email, and a UNIQUE index over a
// nullable column keeps that behaviour because NULLs never collide.
type UserRecord struct {
	ID           string    `gorm:"column:id;type:varchar(96);primaryKey"`
	Email        *string   `gorm:"column:email;type:varchar(320);uniqueIndex:idx_users_email"`
	DisplayName  string    `gorm:"column:display_name;type:varchar(100)"`
	Username     string    `gorm:"column:username;type:varchar(100)"`
	PasswordHash string    `gorm:"column:password_hash;type:varchar(128)"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

// TableName pins the table name so it never follows a struct rename.
func (UserRecord) TableName() string { return "users" }

// SessionRecord is the sessions table. Mode is stored as plain text and
// validated on both write and read: it is a server-owned fact with no update
// path, and a value outside the closed set must never reach the domain.
type SessionRecord struct {
	ID        string    `gorm:"column:id;type:varchar(96);primaryKey"`
	UserID    string    `gorm:"column:user_id;type:varchar(96);not null;index:idx_sessions_user"`
	Title     string    `gorm:"column:title;type:varchar(200)"`
	Mode      string    `gorm:"column:mode;type:varchar(32);not null"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (SessionRecord) TableName() string { return "sessions" }

// MessageRecord is the messages table. Content is mediumtext because the API
// accepts up to 128 KiB per turn, which a MySQL TEXT column (64 KiB) cannot
// hold. Timestamp columns carry no explicit type so each dialect picks its own
// (MySQL datetime(3)); append order therefore cannot rely on created_at alone
// and is recovered from (created_at, id): generated identifiers embed a
// fixed-width UTC nanosecond timestamp, so lexical order matches creation order
// even for rows that share a millisecond.
type MessageRecord struct {
	ID        string    `gorm:"column:id;type:varchar(96);primaryKey"`
	SessionID string    `gorm:"column:session_id;type:varchar(96);not null;index:idx_messages_session_created,priority:1"`
	Role      string    `gorm:"column:role;type:varchar(16);not null"`
	Content   string    `gorm:"column:content;type:mediumtext"`
	CreatedAt time.Time `gorm:"column:created_at;not null;index:idx_messages_session_created,priority:2"`
}

func (MessageRecord) TableName() string { return "messages" }

// MessageSourceRecord is one citation of one assistant message, in the order the
// answer cites it. A separate table rather than a JSON column on the message:
// sources are read back as a set, they are counted and cleaned up per message,
// and a JSON blob cannot be joined or indexed by document if the audit question
// ever becomes "which citations point at this chunk?".
//
// (message_id, position) is unique so a duplicate insert is a conflict instead of
// a silently repeated citation. There is deliberately no foreign key: the session
// delete cascade in Repository is explicit and runs the same way on every engine.
type MessageSourceRecord struct {
	MessageID    string `gorm:"column:message_id;type:varchar(96);not null;uniqueIndex:idx_message_sources,priority:1"`
	Position     int    `gorm:"column:position;type:int;not null;uniqueIndex:idx_message_sources,priority:2"`
	SourceID     string `gorm:"column:source_id;type:varchar(128)"`
	DocumentID   string `gorm:"column:document_id;type:varchar(96)"`
	CollectionID string `gorm:"column:collection_id;type:varchar(96)"`
	ChunkID      string `gorm:"column:chunk_id;type:varchar(128)"`
	Title        string `gorm:"column:title;type:varchar(200)"`
	Section      string `gorm:"column:section;type:varchar(500)"`
	Snippet      string `gorm:"column:snippet;type:varchar(512)"`
}

func (MessageSourceRecord) TableName() string { return "message_sources" }

// SessionSummaryRecord is the rolling summary of one session. session_id is the
// primary key rather than an index: one session has at most one summary row, and a
// second insert for the same session is a conflict instead of a second history the
// reader would have to guess between.
//
// version is the compare-and-set token. Every accepted commit moves it forward by one
// and rewrites covered_message_id together with content, so the two can never be read
// in a state where the boundary has advanced past the text.
type SessionSummaryRecord struct {
	SessionID        string    `gorm:"column:session_id;type:varchar(96);primaryKey"`
	UserID           string    `gorm:"column:user_id;type:varchar(96);not null;index:idx_session_summaries_user"`
	Content          string    `gorm:"column:content;type:mediumtext"`
	CoveredMessageID string    `gorm:"column:covered_message_id;type:varchar(96);not null"`
	Version          int64     `gorm:"column:version;type:bigint;not null"`
	UpdatedAt        time.Time `gorm:"column:updated_at;not null"`
}

func (SessionSummaryRecord) TableName() string { return "session_summaries" }

// TopicMemoryRecord is one conversation's topic-memory consent. session_id is the primary key
// for the same reason the summary row uses it: one conversation has at most one decision, so a
// second insert is a conflict rather than a second history the reader would have to guess
// between, and the commit path is an upsert over that key.
//
// There is no enabled-by-absence column trick here: no row means off, and a row that says false
// means the learner said so. Both read as off, and only the second one carries a timestamp that
// can answer "when did you turn this on".
type TopicMemoryRecord struct {
	SessionID string    `gorm:"column:session_id;type:varchar(96);primaryKey"`
	UserID    string    `gorm:"column:user_id;type:varchar(96);not null;index:idx_session_topic_memory_user"`
	Enabled   bool      `gorm:"column:enabled;type:tinyint(1);not null"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

func (TopicMemoryRecord) TableName() string { return "session_topic_memory" }

func (r TopicMemoryRecord) toDomain() domain.TopicMemoryPreference {
	return domain.TopicMemoryPreference{
		UserID:    r.UserID,
		SessionID: r.SessionID,
		Enabled:   r.Enabled,
		CreatedAt: r.CreatedAt.UTC(),
		UpdatedAt: r.UpdatedAt.UTC(),
	}
}

func (r SessionSummaryRecord) toDomain() domain.SessionSummary {
	return domain.SessionSummary{
		SessionID:        r.SessionID,
		UserID:           r.UserID,
		Content:          r.Content,
		CoveredMessageID: r.CoveredMessageID,
		Version:          r.Version,
		UpdatedAt:        r.UpdatedAt.UTC(),
	}
}

// MemoryRecord is one long-term memory in whichever lifecycle state it currently holds.
//
// The lookup key that matters is (user_id, scope, topic_key, status): confirming a memory
// has to find "the one that is active for this subject right now", and only an index over
// those columns can answer that without scanning every memory the user owns.
//
// There is no foreign key from source_session_id to sessions, and no cascade in either
// direction. A memory is meant to outlive the conversation it came from; deleting a
// session must not be able to delete confirmed facts, and the reference it recorded stays
// readable so the dangling pointer is visible instead of silent.
type MemoryRecord struct {
	MemoryID string `gorm:"column:memory_id;type:varchar(96);primaryKey"`
	UserID   string `gorm:"column:user_id;type:varchar(96);not null;index:idx_memories_subject,priority:1"`
	Scope    string `gorm:"column:scope;type:varchar(16);not null;index:idx_memories_subject,priority:2"`
	TopicKey string `gorm:"column:topic_key;type:varchar(192);not null;index:idx_memories_subject,priority:3"`
	Status   string `gorm:"column:status;type:varchar(16);not null;index:idx_memories_subject,priority:4"`
	Content  string `gorm:"column:content;type:text"`
	// Origin is nullable on purpose. A row written before this column existed has no
	// author recorded, and inventing one would be a claim about history: an empty value
	// reads back as the only shape that ever existed here - a draft with provenance,
	// made active by someone's confirmation. See domain.Memory.AuthoredBy.
	Origin          string    `gorm:"column:origin;type:varchar(24)"`
	Confidence      float64   `gorm:"column:confidence;type:double;not null"`
	SourceSessionID string    `gorm:"column:source_session_id;type:varchar(96);not null;index:idx_memories_source_session"`
	CreatedAt       time.Time `gorm:"column:created_at;not null"`
	UpdatedAt       time.Time `gorm:"column:updated_at;not null"`
	// SupersededAt is nullable: a live memory has never been replaced, and storing the
	// zero time would make "when" indistinguishable from "not applicable" on a database
	// that rounds timestamps.
	SupersededAt *time.Time `gorm:"column:superseded_at"`
}

func (MemoryRecord) TableName() string { return "memories" }

// MemorySourceRecord is one cited message of one memory, in the order it was cited.
//
// A child table rather than a packed column for the same reason citations are one: the
// list is read back as a set, it is ordered by data rather than by string-splitting luck,
// and "which memories claim this message as evidence" has to be answerable with a query -
// that mapping is the reference record which makes deleting a session an explicit decision
// instead of a cascade nobody thought about.
type MemorySourceRecord struct {
	MemoryID  string `gorm:"column:memory_id;type:varchar(96);not null;uniqueIndex:idx_memory_sources,priority:1"`
	Position  int    `gorm:"column:position;type:int;not null;uniqueIndex:idx_memory_sources,priority:2"`
	MessageID string `gorm:"column:message_id;type:varchar(96);not null;index:idx_memory_sources_message"`
}

func (MemorySourceRecord) TableName() string { return "memory_sources" }

func (r MemoryRecord) toDomain(messageIDs []string) (domain.Memory, error) {
	scope := domain.MemoryScope(r.Scope)
	if !scope.IsValid() {
		return domain.Memory{}, fmt.Errorf("%w: memory %q has scope %q", errCorruptStoredData, r.MemoryID, r.Scope)
	}
	status := domain.MemoryStatus(r.Status)
	if !status.IsValid() {
		return domain.Memory{}, fmt.Errorf("%w: memory %q has status %q", errCorruptStoredData, r.MemoryID, r.Status)
	}
	memory := domain.Memory{
		MemoryID:        r.MemoryID,
		UserID:          r.UserID,
		Scope:           scope,
		TopicKey:        r.TopicKey,
		Content:         r.Content,
		Status:          status,
		Origin:          domain.MemoryOrigin(r.Origin),
		Confidence:      r.Confidence,
		SourceSessionID: r.SourceSessionID,
		CreatedAt:       r.CreatedAt.UTC(),
		UpdatedAt:       r.UpdatedAt.UTC(),
	}
	if r.SupersededAt != nil {
		memory.SupersededAt = r.SupersededAt.UTC()
	}
	for _, id := range messageIDs {
		memory.SourceMessageIDs = append(memory.SourceMessageIDs, id)
	}
	return memory, nil
}

func memorySourceRecords(memoryID string, messageIDs []string) []MemorySourceRecord {
	records := make([]MemorySourceRecord, 0, len(messageIDs))
	for position, messageID := range messageIDs {
		records = append(records, MemorySourceRecord{
			MemoryID:  memoryID,
			Position:  position,
			MessageID: messageID,
		})
	}
	return records
}

func (r MessageSourceRecord) toDomain() domain.Source {
	return domain.Source{
		SourceID:     r.SourceID,
		DocumentID:   r.DocumentID,
		CollectionID: r.CollectionID,
		ChunkID:      r.ChunkID,
		Title:        r.Title,
		Section:      r.Section,
		Snippet:      r.Snippet,
	}
}

// messageSourceRecords numbers the citations by their answer order, so position
// 0 is the first thing the answer cites and the read-back order is data, not
// insertion luck.
func messageSourceRecords(messageID string, sources []domain.Source) []MessageSourceRecord {
	records := make([]MessageSourceRecord, 0, len(sources))
	for position, source := range sources {
		records = append(records, MessageSourceRecord{
			MessageID:    messageID,
			Position:     position,
			SourceID:     source.SourceID,
			DocumentID:   source.DocumentID,
			CollectionID: source.CollectionID,
			ChunkID:      source.ChunkID,
			Title:        source.Title,
			Section:      source.Section,
			Snippet:      source.Snippet,
		})
	}
	return records
}

var idSequence uint32

// newID builds a lexicographically ordered identifier: fixed-width
// Unix-nanosecond timestamp, then process id, then a per-process counter.
func newID(prefix string) string {
	sequence := atomic.AddUint32(&idSequence, 1)
	return fmt.Sprintf("%s-%016x-%04x-%04x", prefix, time.Now().UTC().UnixNano(), os.Getpid()&0xffff, sequence&0xffff)
}

func (r UserRecord) toDomain() domain.User {
	user := domain.User{
		ID:           r.ID,
		DisplayName:  r.DisplayName,
		Username:     r.Username,
		PasswordHash: r.PasswordHash,
		CreatedAt:    r.CreatedAt.UTC(),
	}
	if r.Email != nil {
		user.Email = *r.Email
	}
	return user
}

func userRecordFrom(user domain.User) UserRecord {
	record := UserRecord{
		ID:           user.ID,
		DisplayName:  user.DisplayName,
		Username:     user.Username,
		PasswordHash: user.PasswordHash,
		CreatedAt:    user.CreatedAt.UTC(),
	}
	// Email is stored normalized so the UNIQUE index cannot be bypassed by case
	// or surrounding space, which is what the in-memory repository's index key
	// already guarantees.
	if email := normalizeEmail(user.Email); email != "" {
		record.Email = &email
	}
	return record
}

func (r SessionRecord) toDomain() (domain.Session, error) {
	mode := domain.SessionMode(r.Mode)
	if !mode.IsValid() {
		return domain.Session{}, fmt.Errorf("%w: stored session %q has mode %q", errCorruptStoredData, r.ID, r.Mode)
	}
	return domain.Session{
		ID:        r.ID,
		UserID:    r.UserID,
		Title:     r.Title,
		Mode:      mode,
		CreatedAt: r.CreatedAt.UTC(),
		UpdatedAt: r.UpdatedAt.UTC(),
	}, nil
}

func sessionRecordFrom(session domain.Session) SessionRecord {
	return SessionRecord{
		ID:        session.ID,
		UserID:    session.UserID,
		Title:     session.Title,
		Mode:      string(session.Mode),
		CreatedAt: session.CreatedAt.UTC(),
		UpdatedAt: session.UpdatedAt.UTC(),
	}
}

func (r MessageRecord) toDomain() domain.Message {
	return domain.Message{
		ID:        r.ID,
		SessionID: r.SessionID,
		Role:      r.Role,
		Content:   r.Content,
		CreatedAt: r.CreatedAt.UTC(),
	}
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
